package validate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TedHaley/cheep/internal/project"
)

// repoWithBranch builds a repo whose worktree-like dir has one commit on a
// branch off base, plus a marker file the checks can probe.
func repoWithBranch(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", "-A")
	run("commit", "-m", "base")
	base = run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
	run("add", "-A")
	run("commit", "-m", "work")
	return dir, base[:40]
}

func TestRunChecksPassAndFail(t *testing.T) {
	dir, base := repoWithBranch(t)
	r := Runner{Checks: []project.Check{
		{Name: "ok", Script: "true"},
		{Name: "reads-file", Script: "test -f b.txt"},
	}}
	res := r.Run(context.Background(), dir, base)
	if !res.Passed || len(res.Checks) != 2 {
		t.Fatalf("expected pass, got %+v", res)
	}

	r = Runner{Checks: []project.Check{{Name: "bad", Script: "exit 3"}}}
	res = r.Run(context.Background(), dir, base)
	if res.Passed {
		t.Fatal("failing check must fail the pipeline (fail-closed)")
	}
	if res.Checks[0].Exit != 3 {
		t.Fatalf("exit code lost: %+v", res.Checks[0])
	}
}

func TestFixLoopBounded(t *testing.T) {
	dir, base := repoWithBranch(t)
	calls := 0
	r := Runner{
		Checks:    []project.Check{{Name: "never", Script: "false"}},
		MaxRounds: 2,
		Fix:       func(context.Context, string, string) error { calls++; return nil },
	}
	res := r.Run(context.Background(), dir, base)
	if res.Passed {
		t.Fatal("must not pass")
	}
	if calls != 2 || res.Rounds != 2 {
		t.Fatalf("fix rounds: calls=%d rounds=%d, want 2/2", calls, res.Rounds)
	}
}

func TestFixRepairsThenPasses(t *testing.T) {
	dir, base := repoWithBranch(t)
	marker := filepath.Join(dir, "fixed.txt")
	r := Runner{
		Checks:    []project.Check{{Name: "wants-marker", Script: "test -f fixed.txt"}},
		MaxRounds: 2,
		Fix: func(_ context.Context, d, task string) error {
			if task == "" {
				return errors.New("empty fix task")
			}
			return os.WriteFile(marker, []byte("ok"), 0o644)
		},
	}
	res := r.Run(context.Background(), dir, base)
	if !res.Passed || res.Rounds != 1 {
		t.Fatalf("expected pass after 1 fix round, got %+v", res)
	}
}

func TestReviewerReviseThenApprove(t *testing.T) {
	dir, base := repoWithBranch(t)
	call := 0
	r := Runner{
		MaxRounds: 2,
		Reviewer: func(_ context.Context, diff, _ string) (Verdict, error) {
			call++
			if diff == "" {
				return Verdict{}, errors.New("no diff passed to reviewer")
			}
			if call == 1 {
				return Verdict{Verdict: "revise", Summary: "s", Issues: []Issue{{Description: "d"}}}, nil
			}
			return Verdict{Verdict: "approve"}, nil
		},
		Fix: func(context.Context, string, string) error { return nil },
	}
	res := r.Run(context.Background(), dir, base)
	if !res.Passed || call != 2 {
		t.Fatalf("expected approve on round 2, got %+v (calls %d)", res, call)
	}
}

func TestReviewerFailOpenVsStrict(t *testing.T) {
	dir, base := repoWithBranch(t)
	broken := func(context.Context, string, string) (Verdict, error) {
		return Verdict{}, errors.New("model returned prose")
	}
	res := Runner{Reviewer: broken}.Run(context.Background(), dir, base)
	if !res.Passed || res.Note == "" {
		t.Fatalf("broken reviewer must fail open with a note, got %+v", res)
	}
	res = Runner{Reviewer: broken, Strict: true}.Run(context.Background(), dir, base)
	if res.Passed {
		t.Fatal("strict mode must fail closed on reviewer errors")
	}
}

func TestExtractVerdict(t *testing.T) {
	cases := []struct {
		in      string
		verdict string
		ok      bool
	}{
		{`{"verdict":"approve"}`, "approve", true},
		{"Looks good overall.\n\n{\"verdict\":\"approve\"}", "approve", true},
		{`I think {"maybe": 1} but finally {"verdict":"revise","summary":"x","issues":[]}`, "revise", true},
		{`{"verdict":"unsure"}`, "", false},
		{`no json at all`, "", false},
		{"```json\n{\"verdict\":\"approve\"}\n```", "approve", true},
	}
	for _, c := range cases {
		v, err := ExtractVerdict(c.in)
		if c.ok && (err != nil || v.Verdict != c.verdict) {
			t.Errorf("%q: got %+v err %v", c.in, v, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q: expected error, got %+v", c.in, v)
		}
	}
}

func TestEmptyDiffAutoApproves(t *testing.T) {
	dir, base := repoWithBranch(t)
	// Diff HEAD against HEAD → empty.
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	r := Runner{Reviewer: func(context.Context, string, string) (Verdict, error) {
		return Verdict{}, fmt.Errorf("reviewer must not be called for an empty diff")
	}}
	res := r.Run(context.Background(), dir, string(head[:40]))
	if !res.Passed {
		t.Fatalf("empty diff should pass: %+v", res)
	}
	_ = base
}
