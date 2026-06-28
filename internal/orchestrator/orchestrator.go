// Package orchestrator wires the orchestrator agent and the tools it uses.
//
// With executors configured, the orchestrator decomposes work and delegates it
// across them in parallel (each in its own git worktree when isolation is on),
// then verifies and integrates the results. With no executors, it runs solo:
// the same agent does the work directly with the full tool set.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/tool"
	"github.com/TedHaley/cheep/internal/worktree"
)

const executorSystem = `You are an executor agent: a focused, capable coding/execution worker.
You receive one concrete subtask and complete it using your tools (read_file, write_file,
list_dir, run_bash). Work autonomously and efficiently.

For multi-step work, call update_todos to lay out the steps up front, then mark each
in_progress/done as you go so your progress is visible.

When the subtask is fully done, STOP calling tools and reply with a short summary of exactly
what you did and how it can be verified. If you get blocked, stop and explain what is
blocking you and why.`

const soloSystem = `You are cheep, a capable autonomous coding agent. Complete the user's task
directly using your tools (read_file, write_file, list_dir, run_bash). Plan briefly, make the
changes, and verify them (read files back, run tests/commands). When the task is done, stop
calling tools and give a short summary of what you did and how to verify it.`

const chatSystem = `You are cheep in CHAT MODE. You have no tools and make no changes to the
workspace. Discuss, brainstorm, explain, and help the user think through their project and
tasks. If they want you to investigate the code, suggest switching to plan mode; if they want
work done, suggest auto mode.`

const planSystem = `You are cheep in PLAN MODE. Investigate the workspace using your read-only
tools (read_file, list_dir, and run_bash for READ-ONLY commands like ls/cat/grep/git status).
Do NOT modify anything: no file writes and no state-changing commands. When you understand the
task, STOP calling tools and present a concrete, numbered, step-by-step plan for the user to
review. Make no changes — the user will approve the plan and switch to auto mode to execute it.`

// Mode controls the orchestrator's tools and behavior.
type Mode string

const (
	ModeChat Mode = "chat" // conversation only, no tools
	ModePlan Mode = "plan" // read-only investigation, produces a plan
	ModeAuto Mode = "auto" // full autonomy (delegate / edit), today's behavior
)

// ParseMode returns the mode for a name, or false if unknown.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeChat, ModePlan, ModeAuto:
		return Mode(s), true
	}
	return "", false
}

// NextMode cycles chat → plan → auto → chat.
func NextMode(m Mode) Mode {
	switch m {
	case ModeChat:
		return ModePlan
	case ModePlan:
		return ModeAuto
	default:
		return ModeChat
	}
}

const orchestratorSystemTmpl = `You are the orchestrator. You coordinate a fleet of cheaper
executor agents to accomplish the user's task. You are expensive; the executors are cheap.
Be economical: plan and delegate rather than doing the work yourself.

%s
- PLAN with update_todos: lay out the subtasks as a checklist and mark each
  in_progress/done as you delegate and verify, so the user can watch progress.
- DECOMPOSE the task into concrete, self-contained subtasks.
- DELEGATE with the "delegate" tool. It takes a LIST of tasks and runs them in PARALLEL,
  so dispatch independent subtasks together in one call. Each task is
  {"executor": "<name>", "subtask": "<full instructions>"}.
- ROUTE each subtask to the executor whose model is best suited to it, based on the models
  listed above. If there is no obvious best fit, pick any executor.
- Executors share NO memory or context with you or each other; every subtask must contain
  all the detail it needs to be done in isolation.
- DELEGATE all execution to executors — especially web access, research, API calls, and
  data gathering. NEVER fetch web pages, scrape, or call external services yourself (no
  curl/wget/web requests in run_bash); hand that to an executor. Do not write or edit files
  yourself either.
- VERIFY every result yourself with read_file, list_dir and run_bash. Your own run_bash is
  ONLY for verification of finished work (reading files, running local tests/builds) — never
  for doing the task or reaching the network. Never trust a "done" report without checking.
- RECOVER when an executor returns a status other than "completed" (max_turns, looping,
  context_exhausted, error): split the subtask smaller, clarify it, or fix the blocker,
  then delegate again.
- Plan, delegate, and verify — that is your whole job.

When the entire task is verified complete, stop calling tools and give a final summary.`

type execRuntime struct {
	name       string
	model      string
	provider   core.Provider
	maxTurns   int
	budget     int
	timeout    time.Duration
	maxResumes int
	extra      []core.Tool
	onEvent    core.EventFunc
}

func (e execRuntime) newSession(workdir, label string) *agent.Session {
	tools := append(tool.Make(workdir, true), e.extra...)
	return agent.New(label, e.provider, e.model, executorSystem,
		tools, e.maxTurns, e.budget, e.onEvent).NewSession()
}

// runSupervised runs the subtask with a wall-clock timeout. If it ends short
// (timeout, looping, max_turns, context_exhausted), it summarizes the progress
// and resumes in a fresh session with that handoff, up to maxResumes times.
// label is the executor instance's display name (e.g. "qwen-local#2"); all its
// events carry it so the UI can group them into one tab.
func (e execRuntime) runSupervised(parent context.Context, workdir, subtask, label string) agent.RunResult {
	e.onEvent(core.Event{Agent: label, Type: "lifecycle", Status: "start"})
	task := subtask
	var totalIn, totalOut, totalTurns int
	var r agent.RunResult
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(parent, e.timeout)
		sess := e.newSession(workdir, label)
		r = sess.SendCtx(ctx, task)
		cancel()

		totalIn += r.InputTokens
		totalOut += r.OutputTokens
		totalTurns += r.Turns
		r.InputTokens, r.OutputTokens, r.Turns = totalIn, totalOut, totalTurns

		resumable := r.Status == "timeout" || r.Status == "max_turns" ||
			r.Status == "looping" || r.Status == "context_exhausted"
		if !resumable || attempt >= e.maxResumes || parent.Err() != nil {
			break
		}

		sctx, scancel := context.WithTimeout(parent, 60*time.Second)
		summary := sess.Summarize(sctx)
		scancel()
		e.onEvent(core.Event{Agent: label, Type: "status",
			Status: fmt.Sprintf("resuming after %s (attempt %d/%d)", r.Status, attempt+1, e.maxResumes)})
		task = subtask + "\n\nA previous attempt was interrupted (" + r.Status + ").\n" +
			"Progress so far:\n" + summary + "\n\nContinue from where it left off and finish the task."
	}
	e.onEvent(core.Event{Agent: label, Type: "lifecycle", Status: r.Status})
	return r
}

func roster(execs []config.Agent) string {
	var b strings.Builder
	b.WriteString("Your executors (delegate to these by name):\n")
	for _, e := range execs {
		model := e.Model
		if model == "" {
			model = "unknown model"
		}
		fmt.Fprintf(&b, "  - %q runs %q\n", e.Name, model)
	}
	return b.String()
}

// Build returns the orchestrator agent for the given config and workspace.
// When isolate is true and workdir is a git repo, each parallel subtask runs in
// its own worktree and its changes are merged back automatically.
// Build returns the orchestrator agent. extra holds tools discovered at runtime
// (e.g. MCP) to add to the orchestrator and every executor.
func Build(cfg config.Config, workdir string, isolate bool, mode Mode, extra []core.Tool, onEvent core.EventFunc) (*agent.Agent, error) {
	if cfg.Orchestrator.Provider == "anthropic" && cfg.Orchestrator.APIKey == "" {
		return nil, fmt.Errorf("orchestrator has no API key (set ANTHROPIC_API_KEY or run /config)")
	}
	if cfg.Orchestrator.Model == "" {
		return nil, fmt.Errorf("orchestrator has no model set (run /config)")
	}
	orchProv := provider.For(cfg.Orchestrator.Provider, cfg.Orchestrator.Endpoint, cfg.Orchestrator.APIKey, 4096)

	withBudget := func(a *agent.Agent) *agent.Agent {
		a.CompactBudget = cfg.Orchestrator.ContextBudget
		return a
	}

	// Chat: no tools. Plan: read-only investigation + extra (no edits/delegation).
	switch mode {
	case ModeChat:
		return withBudget(agent.New("cheep", orchProv, cfg.Orchestrator.Model, chatSystem,
			nil, cfg.Orchestrator.MaxTurns, 0, onEvent)), nil
	case ModePlan:
		return withBudget(agent.New("cheep", orchProv, cfg.Orchestrator.Model, planSystem,
			append(tool.Make(workdir, false), extra...), cfg.Orchestrator.MaxTurns, 0, onEvent)), nil
	}

	// ModeAuto, solo: no executors, the orchestrator does the work itself.
	if len(cfg.Executors) == 0 {
		solo := agent.New("cheep", orchProv, cfg.Orchestrator.Model, soloSystem,
			append(tool.Make(workdir, true), extra...), cfg.Orchestrator.MaxTurns, 0, onEvent)
		solo.CompactBudget = cfg.Orchestrator.ContextBudget
		return solo, nil
	}

	runtimes := map[string]execRuntime{}
	var order []string
	for _, e := range cfg.Executors {
		runtimes[e.Name] = execRuntime{
			name:       e.Name,
			model:      e.Model,
			provider:   provider.For(e.Provider, e.Endpoint, e.APIKey, 4096),
			maxTurns:   e.MaxTurns,
			budget:     e.TokenBudget,
			timeout:    time.Duration(e.TimeoutSeconds) * time.Second,
			maxResumes: e.MaxResumes,
			extra:      extra,
			onEvent:    onEvent,
		}
		order = append(order, e.Name)
	}
	defaultExec := order[0]
	isolated := isolate && worktree.IsRepo(workdir)

	delegate := func(args map[string]any) string {
		rawTasks, _ := args["tasks"].([]any)
		if len(rawTasks) == 0 {
			return `ERROR: "tasks" must be a non-empty array of {"executor","subtask"}`
		}
		type job struct {
			executor, subtask string
		}
		jobs := make([]job, len(rawTasks))
		for i, rt := range rawTasks {
			m, _ := rt.(map[string]any)
			ex, _ := m["executor"].(string)
			st, _ := m["subtask"].(string)
			if _, ok := runtimes[ex]; !ok {
				ex = defaultExec // unknown/empty name falls back to the first executor
			}
			jobs[i] = job{executor: ex, subtask: st}
		}

		results := make([]map[string]any, len(jobs))
		var wg sync.WaitGroup
		var gitMu sync.Mutex // serialize git index operations (add/merge/remove)
		var counter int

		for i, j := range jobs {
			wg.Add(1)
			go func(i int, j job) {
				defer wg.Done()
				rt := runtimes[j.executor]

				gitMu.Lock()
				counter++
				id := counter
				gitMu.Unlock()
				label := fmt.Sprintf("%s#%d", rt.name, id)

				wd := workdir
				var tree *worktree.Tree
				if isolated {
					gitMu.Lock()
					t, err := worktree.Add(workdir, rt.name, id)
					gitMu.Unlock()
					if err == nil {
						tree, wd = t, t.Path
					} else {
						rt.onEvent(core.Event{Agent: "cheep", Type: "status",
							Status: "worktree unavailable, using shared dir: " + err.Error()})
					}
				}

				r := rt.runSupervised(context.Background(), wd, j.subtask, label)
				res := map[string]any{
					"executor":      rt.name,
					"model":         rt.model,
					"status":        r.Status,
					"turns":         r.Turns,
					"input_tokens":  r.InputTokens,
					"output_tokens": r.OutputTokens,
					"output":        r.Output,
				}

				if tree != nil {
					gitMu.Lock()
					committed, cErr := tree.CommitAll("cheep: " + rt.name + " subtask")
					switch {
					case cErr != nil:
						res["integration"] = "commit failed: " + cErr.Error()
						tree.Remove(true)
					case !committed:
						res["integration"] = "no file changes"
						tree.Remove(false)
					default:
						if mErr := tree.MergeInto(); mErr != nil {
							res["integration"] = mErr.Error() + " (kept on branch " + tree.Branch + ")"
							tree.Remove(true)
						} else {
							res["integration"] = "merged"
							tree.Remove(false)
						}
					}
					gitMu.Unlock()
				}

				results[i] = res
			}(i, j)
		}
		wg.Wait()
		out, _ := json.MarshalIndent(results, "", "  ")
		return string(out)
	}

	delegateTool := core.Tool{
		Name: "delegate",
		Description: "Delegate one or more self-contained subtasks to executor agents. The " +
			"tasks run IN PARALLEL, so batch independent work together. Each executor is a " +
			"fresh agent with no prior context. Returns an array of results (status + summary).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type":        "array",
					"description": "Subtasks to run in parallel.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"executor": map[string]any{
								"type":        "string",
								"description": "Name of the executor to run this subtask on.",
							},
							"subtask": map[string]any{
								"type":        "string",
								"description": "Complete, self-contained instructions for the executor.",
							},
						},
						"required": []string{"subtask"},
					},
				},
			},
			"required": []string{"tasks"},
		},
		Func: delegate,
	}

	system := fmt.Sprintf(orchestratorSystemTmpl, roster(cfg.Executors))
	if isolated {
		system += "\n\nIsolation is ON: each delegated subtask runs in its own git worktree and is " +
			"merged back automatically. Each result has an \"integration\" field — \"merged\", " +
			"\"no file changes\", or a conflict left on a branch. If a merge conflicts, delegate a " +
			"follow-up subtask (or resolve it yourself with git) before continuing."
	}
	tools := append(tool.Make(workdir, false), delegateTool)
	tools = append(tools, extra...)
	orch := agent.New("orchestrator", orchProv, cfg.Orchestrator.Model, system, tools,
		cfg.Orchestrator.MaxTurns, 0, onEvent)
	orch.CompactBudget = cfg.Orchestrator.ContextBudget
	return orch, nil
}
