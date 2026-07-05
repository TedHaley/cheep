package orchestrator

// End-to-end delegate tests: a fake OpenAI-compatible server plays both the
// orchestrator (delegates once, then summarizes) and the executor (writes a
// file, then reports), driving the real Build → delegate → worktree →
// validation → integration pipeline against a real git repo.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TedHaley/cheep/internal/approve"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

// fakeLLM speaks just enough chat-completions for the agent loop: the model
// field selects the role script, and "has a tool result yet" selects the turn.
func fakeLLM(t *testing.T, delegateArgs string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		afterTool := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				afterTool = true
			}
		}
		var msg string
		switch {
		case req.Model == "orch" && !afterTool:
			msg = fmt.Sprintf(`{"tool_calls":[{"id":"c1","type":"function","function":{"name":"delegate","arguments":%q}}]}`, delegateArgs)
		case req.Model == "orch":
			msg = `{"content":"all done"}`
		case req.Model == "exec" && !afterTool:
			msg = `{"tool_calls":[{"id":"w1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"artifact.txt\",\"content\":\"made by executor\"}"}}]}`
		default:
			msg = `{"content":"FINDINGS: the artifact was written"}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, msg)
	}))
}

// testRepo creates a git repo with one commit and a local committer identity.
func testRepo(t *testing.T) string {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "test")
	run("config", "user.email", "test@localhost")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func testConfig(url string) config.Config {
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: url, Model: "orch"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: url, Model: "exec"}},
		Validation:   config.Validation{SkipReview: true},
		DisablePool:  true, // ephemeral worktrees keep the test self-contained
	}
	cfg.ApplyDefaults()
	return cfg
}

func runTask(t *testing.T, cfg config.Config, repo string, opts ...func(*Options)) string {
	t.Helper()
	o := Options{Isolate: true, Mode: ModeAuto, OnEvent: func(core.Event) {}}
	for _, f := range opts {
		f(&o)
	}
	orch, err := Build(cfg, repo, o)
	if err != nil {
		t.Fatal(err)
	}
	r := orch.Run("do the task")
	if r.Status != "completed" {
		t.Fatalf("orchestrator ended %q: %s", r.Status, r.Output)
	}
	return r.Output
}

func gitOut(t *testing.T, dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func TestDelegateShipMergesValidatedWork(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	srv := fakeLLM(t, `{"tasks":[{"executor":"e1","subtask":"write artifact.txt","kind":"ship"}]}`)
	defer srv.Close()
	repo := testRepo(t)

	runTask(t, testConfig(srv.URL), repo)

	b, err := os.ReadFile(filepath.Join(repo, "artifact.txt"))
	if err != nil || string(b) != "made by executor" {
		t.Fatalf("executor's work not merged into the repo: %v %q", err, b)
	}
	if !strings.Contains(gitOut(t, repo, "log", "--oneline"), "cheep: merge") {
		t.Errorf("expected a merge commit:\n%s", gitOut(t, repo, "log", "--oneline"))
	}
}

func TestDelegateScoutDiscardsChangesAndWritesReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHEEP_HOME", home)
	srv := fakeLLM(t, `{"tasks":[{"executor":"e1","subtask":"audit the repo","kind":"scout"}]}`)
	defer srv.Close()
	repo := testRepo(t)

	runTask(t, testConfig(srv.URL), repo)

	if _, err := os.Stat(filepath.Join(repo, "artifact.txt")); !os.IsNotExist(err) {
		t.Errorf("scout file changes must be discarded, but artifact.txt landed")
	}
	reports, _ := os.ReadDir(filepath.Join(home, "reports"))
	if len(reports) != 1 {
		t.Fatalf("want 1 scout report, got %d", len(reports))
	}
	b, _ := os.ReadFile(filepath.Join(home, "reports", reports[0].Name()))
	if !strings.Contains(string(b), "FINDINGS: the artifact was written") {
		t.Errorf("report missing findings:\n%s", b)
	}
}

func TestDelegateNoMistakesHoldsMergeWithoutApproval(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	srv := fakeLLM(t, `{"tasks":[{"executor":"e1","subtask":"write artifact.txt","kind":"ship"}]}`)
	defer srv.Close()
	repo := testRepo(t)

	cfg := testConfig(srv.URL)
	cfg.NoMistakes = true
	runTask(t, cfg, repo) // nil Gate = headless: no approver, so nothing may land

	if _, err := os.Stat(filepath.Join(repo, "artifact.txt")); !os.IsNotExist(err) {
		t.Errorf("no-mistakes without an approver must not merge, but artifact.txt landed")
	}
	branches := gitOut(t, repo, "branch", "--list", "cheep/*")
	if !strings.Contains(branches, "cheep/e1") {
		t.Errorf("held work should survive on its branch, got branches:\n%s", branches)
	}
	if !strings.Contains(gitOut(t, repo, "show", "cheep/e1-1:artifact.txt"), "made by executor") {
		t.Errorf("branch should contain the executor's commit")
	}
}

func TestDelegateNoMistakesMergesWhenApproved(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	srv := fakeLLM(t, `{"tasks":[{"executor":"e1","subtask":"write artifact.txt","kind":"ship"}]}`)
	defer srv.Close()
	repo := testRepo(t)

	gate := approve.New(approve.ModeApprove)
	go func() { // the "captain": approve every merge request, checking the preview
		for r := range gate.Requests {
			if r.Tool == "merge" && strings.Contains(r.Diff, "made by executor") {
				r.Resp <- approve.Allow
			} else {
				r.Resp <- approve.Deny
			}
		}
	}()

	cfg := testConfig(srv.URL)
	cfg.NoMistakes = true
	runTask(t, cfg, repo, func(o *Options) { o.Gate = gate })

	b, err := os.ReadFile(filepath.Join(repo, "artifact.txt"))
	if err != nil || string(b) != "made by executor" {
		t.Fatalf("approved no-mistakes merge should land: %v %q", err, b)
	}
}
