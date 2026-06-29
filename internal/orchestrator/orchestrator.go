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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configtools"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/pricing"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/tool"
	"github.com/TedHaley/cheep/internal/worktree"
)

const executorSystem = `You are an executor agent: a focused, capable coding/execution worker.
You receive one concrete subtask and complete it using your tools (read_file, write_file,
list_dir, run_bash). Work autonomously and efficiently.

For multi-step work, call update_todos to lay out the steps up front, then mark each
in_progress/done as you go so your progress is visible.

When the subtask is done, STOP calling tools and put your COMPLETE results directly in your
final reply text — the orchestrator only sees that reply, so include the actual findings
there. Do NOT write results to files (you run in an isolated workspace the orchestrator can't
read). If you get blocked, stop and explain what is blocking you and why.`

const soloSystem = `You are cheep, a capable autonomous coding agent. Complete the user's task
directly using your tools (read_file, write_file, list_dir, run_bash). Plan briefly, make the
changes, and verify them (read files back, run tests/commands). When the task is done, stop
calling tools and give a short summary of what you did and how to verify it.

If the user asks to change cheep's own setup (switch your model, add executors, add API keys),
use the config tools: discover (find local servers + keys), get_config, discover_models,
set_orchestrator, add_executor, remove_executor, copy_key/set_key (ask before copying a found
key); changes apply on the next message. Do not edit ~/.cheep files directly.`

const chatSystem = `You are cheep in CHAT MODE. You have no tools and make no changes to the
workspace. Discuss, brainstorm, explain, and help the user think through their project and
tasks. If they want you to investigate the code, suggest switching to plan mode; if they want
work done, suggest auto mode.`

const planSystem = `You are cheep in PLAN MODE. Investigate the workspace using your read-only
tools (read_file, list_dir). You CANNOT modify anything or run commands in this mode. When you
understand the task, STOP calling tools and present a concrete, numbered, step-by-step plan for
the user to review. The user will approve it and switch to auto mode to execute.`

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
- ACT, DO NOT NARRATE. Never say "I will delegate" or "let me research" and then stop.
  If a turn calls for delegation, the delegate tool call MUST be in that same turn. Talking
  about an action without making the corresponding tool call is a failure.
- PLAN with update_todos: lay out the subtasks as a checklist. A todo may be marked
  "in_progress" ONLY in the same turn you actually delegate it, and "done" ONLY after you
  have verified the executor's result yourself. Never leave a todo "in_progress" or "pending"
  at the end of your response — keep delegating and verifying until every todo is "done".
- DECOMPOSE the task into concrete, self-contained subtasks.
- DELEGATE with the "delegate" tool. It takes a LIST of tasks and runs them in PARALLEL,
  so dispatch independent subtasks together in ONE call. Each task is
  {"executor": "<name>", "subtask": "<full instructions>"}. Updating todos is NOT doing the
  work — only delegate (then verify) actually does it.
- ROUTE each subtask to the CHEAPEST executor that can do it well (they are listed
  cheapest-first with cost tiers). Reserve pricier executors for genuinely hard subtasks;
  a failed cheap attempt auto-escalates to a stronger one, so default to cheap. Mind the
  project budget if one is given.
- Executors share NO memory or context with you or each other; every subtask must contain
  all the detail it needs to be done in isolation.
- DELEGATE all execution to executors — especially web access, research, API calls, and
  data gathering. NEVER fetch web pages, scrape, or call external services yourself (no
  curl/wget/web requests in run_bash); hand that to an executor. Do not write or edit files
  yourself either.
- READ THE RESULTS: each delegate result has an "output" field containing the executor's
  findings — that IS the deliverable; use it directly. Executors run in isolation and share
  NO files with you, so never look for files they "wrote" and never tell them to save to
  /tmp; their answer comes back in "output". If an output is empty, re-delegate ONCE with
  clearer instructions — do not loop.
- VERIFY by reasoning over the returned outputs (and read_file/run_bash for LOCAL artifacts
  only). Never trust a non-"completed" status without acting on it.
- RECOVER when an executor returns a status other than "completed": cheep already
  auto-escalates a failed subtask to a more capable executor before returning (see the
  "escalation" field), so a non-"completed" result means even the strongest tier struggled —
  split the subtask smaller, clarify it, or fix the blocker, then delegate again.
- Plan, delegate, and verify — that is your whole job.
- To CHANGE cheep's setup (switch your own model, add/remove executors, add API keys) when
  the user asks, use the config tools: discover (find local servers + keys), get_config,
  discover_models, set_orchestrator, add_executor, remove_executor, and copy_key/set_key for
  API keys (for a discovered key, ASK permission before copy_key). Changes apply on the next
  message. NEVER edit ~/.cheep files directly or run the "cheep" binary. To run work in
  parallel, use delegate — the executors already exist.

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
	e.onEvent(core.Event{Agent: label, Type: "lifecycle", Status: "start", Text: subtask})
	task := subtask
	var totalIn, totalOut, totalTurns int
	var r agent.RunResult
	var sess *agent.Session
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(parent, e.timeout)
		sess = e.newSession(workdir, label)
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
	// Salvage: a "completed" run with no final text still has findings in its
	// history — summarize them so the orchestrator gets something usable.
	if r.Status == "completed" && strings.TrimSpace(r.Output) == "" && sess != nil {
		sctx, scancel := context.WithTimeout(parent, 60*time.Second)
		if sum := strings.TrimSpace(sess.Summarize(sctx)); sum != "" {
			r.Output = sum
		}
		scancel()
	}
	e.onEvent(core.Event{Agent: label, Type: "lifecycle", Status: r.Status})
	return r
}

// costTier is a coarse, human-readable price band for an executor.
func costTier(e config.Agent) string {
	if _, _, kind := pricing.AgentRate(e); kind == "local" {
		return "free · local"
	}
	switch s := pricing.Score(e); {
	case s == 0:
		return "free"
	case s < 2:
		return "$ cheap"
	case s < 12:
		return "$$ mid"
	default:
		return "$$$ premium"
	}
}

func roster(execs []config.Agent, budget float64) string {
	sorted := append([]config.Agent{}, execs...)
	sort.SliceStable(sorted, func(i, j int) bool { return pricing.Score(sorted[i]) < pricing.Score(sorted[j]) })
	var b strings.Builder
	b.WriteString("Your executors, cheapest first (delegate to these by name):\n")
	for _, e := range sorted {
		model := e.Model
		if model == "" {
			model = "unknown model"
		}
		fmt.Fprintf(&b, "  - %q runs %q  [%s]\n", e.Name, model, costTier(e))
	}
	if budget > 0 {
		fmt.Fprintf(&b, "This project's budget is about $%.2f total — spend it deliberately.\n", budget)
	}
	b.WriteString("Prefer the cheapest executor that can do a subtask well; reserve pricier ones for " +
		"genuinely hard work. A failed cheap attempt auto-escalates, so default to cheap.")
	return b.String()
}

// Build returns the orchestrator agent for the given config and workspace.
// When isolate is true and workdir is a git repo, each parallel subtask runs in
// its own worktree and its changes are merged back automatically.
const rescueSystem = `cheep's configured orchestrator is unavailable, so you are a temporary
helper running on an executor model. Get the user running again, as easily as possible:
1. Call discover to find local LLM servers (with their models) and API keys on this machine.
2. Propose a setup and ASK the user before changing anything. For Claude: set_orchestrator
   provider="anthropic" model="claude-sonnet-4-6"; if discover found an ANTHROPIC_API_KEY,
   ask permission then copy_key it (keys apply immediately — no restart). For a local model:
   set_orchestrator provider="openai" with the discovered endpoint (or add_executor).
Keep replies short and make every change through the tools.`

// usable reports whether an agent can actually run.
func usable(a config.Agent) bool {
	return a.Model != "" && !(a.Provider == "anthropic" && a.APIKey == "")
}

// escalateTarget returns the cheapest still-untried, usable executor strictly
// pricier than curr — the next rung up the cheap-first escalation ladder, or ""
// when nothing higher is available.
func escalateTarget(order []string, score map[string]float64, usable map[string]bool, curr string, tried map[string]bool) string {
	cs := score[curr]
	best, found := "", false
	for _, name := range order {
		if tried[name] || !usable[name] || score[name] <= cs {
			continue
		}
		if !found || score[name] < score[best] {
			best, found = name, true
		}
	}
	return best
}

// rescueAgent builds a config-only helper from the first usable executor.
func rescueAgent(cfg config.Config, onEvent core.EventFunc) *agent.Agent {
	for _, e := range cfg.Executors {
		if usable(e) {
			prov := provider.For(e.Provider, e.Endpoint, e.APIKey, 4096)
			return agent.New("orchestrator", prov, e.Model, rescueSystem, configtools.Tools(), 20, 0, onEvent)
		}
	}
	return nil
}

// Build returns the orchestrator agent. extraOrch/extraExec hold tools
// discovered at runtime (e.g. MCP), added to the orchestrator and executors
// respectively. In solo mode the agent gets both.
func Build(cfg config.Config, workdir string, isolate bool, mode Mode, extraOrch, extraExec []core.Tool, onEvent core.EventFunc) (*agent.Agent, error) {
	// If the orchestrator can't run (no key / no model), fall back to a reachable
	// executor so the user can fix the orchestrator config conversationally.
	if !usable(cfg.Orchestrator) {
		if r := rescueAgent(cfg, onEvent); r != nil {
			return r, nil
		}
		if cfg.Orchestrator.Model == "" {
			return nil, fmt.Errorf("orchestrator has no model set (run /config)")
		}
		return nil, fmt.Errorf("orchestrator has no API key (set ANTHROPIC_API_KEY or run /config)")
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
			append(tool.MakeReadOnly(workdir), extraOrch...), cfg.Orchestrator.MaxTurns, 0, onEvent)), nil
	}

	// ModeAuto, solo: no executors, the orchestrator does the work itself, so it
	// gets both role's tools.
	if len(cfg.Executors) == 0 {
		soloTools := append(tool.Make(workdir, true), extraOrch...)
		soloTools = append(soloTools, extraExec...)
		soloTools = append(soloTools, configtools.Tools()...)
		solo := agent.New("cheep", orchProv, cfg.Orchestrator.Model, soloSystem,
			soloTools, cfg.Orchestrator.MaxTurns, 0, onEvent)
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
			extra:      extraExec,
			onEvent:    onEvent,
		}
		order = append(order, e.Name)
	}
	defaultExec := order[0]
	isolated := isolate && worktree.IsRepo(workdir)

	// Cheap-first escalation: order executors by cost so a failed subtask can be
	// retried on a more capable (pricier) executor before giving up.
	scoreByName := map[string]float64{}
	usableExec := map[string]bool{}
	for _, e := range cfg.Executors {
		scoreByName[e.Name] = pricing.Score(e)
		usableExec[e.Name] = usable(e)
	}
	escalate := !cfg.DisableEscalate
	const maxEscalations = 2
	nextTier := func(curr string, tried map[string]bool) string {
		return escalateTarget(order, scoreByName, usableExec, curr, tried)
	}
	// runJob runs the subtask on startExec, escalating up the cost ladder on a
	// non-"completed" status. Returns the final result, the executor that produced
	// it, and the attempt trail ("qwen:looping → deepseek:completed").
	runJob := func(ctx context.Context, wd, subtask, startExec string, id int) (agent.RunResult, execRuntime, []string) {
		curr := startExec
		tried := map[string]bool{}
		var r agent.RunResult
		var rt execRuntime
		var trail []string
		var totIn, totOut, totTurns int
		for hops := 0; ; hops++ {
			rt = runtimes[curr]
			tried[curr] = true
			r = rt.runSupervised(ctx, wd, subtask, fmt.Sprintf("%s#%d", rt.name, id))
			totIn += r.InputTokens
			totOut += r.OutputTokens
			totTurns += r.Turns
			trail = append(trail, rt.name+":"+r.Status)
			if r.Status == "completed" || !escalate || hops >= maxEscalations || ctx.Err() != nil {
				break
			}
			next := nextTier(curr, tried)
			if next == "" {
				break // no pricier executor to escalate to
			}
			onEvent(core.Event{Agent: "cheep", Type: "status",
				Status: fmt.Sprintf("escalating %s → %s after %s", rt.name, next, r.Status)})
			curr = next
		}
		r.InputTokens, r.OutputTokens, r.Turns = totIn, totOut, totTurns
		return r, rt, trail
	}

	delegate := func(ctx context.Context, args map[string]any) string {
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
				start := runtimes[j.executor]

				gitMu.Lock()
				counter++
				id := counter
				gitMu.Unlock()

				wd := workdir
				var tree *worktree.Tree
				if isolated {
					gitMu.Lock()
					t, err := worktree.Add(workdir, start.name, id)
					gitMu.Unlock()
					if err == nil {
						tree, wd = t, t.Path
					} else {
						start.onEvent(core.Event{Agent: "cheep", Type: "status",
							Status: "worktree unavailable, using shared dir: " + err.Error()})
					}
				}

				r, rt, trail := runJob(ctx, wd, j.subtask, j.executor, id)
				res := map[string]any{
					"executor":      rt.name,
					"model":         rt.model,
					"status":        r.Status,
					"turns":         r.Turns,
					"input_tokens":  r.InputTokens,
					"output_tokens": r.OutputTokens,
					"output":        r.Output,
				}
				if len(trail) > 1 {
					res["escalation"] = strings.Join(trail, " → ")
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

	system := fmt.Sprintf(orchestratorSystemTmpl, roster(cfg.Executors, cfg.Budget(workdir)))
	if isolated {
		system += "\n\nIsolation is ON: each delegated subtask runs in its own git worktree and is " +
			"merged back automatically. Each result has an \"integration\" field — \"merged\", " +
			"\"no file changes\", or a conflict left on a branch. If a merge conflicts, delegate a " +
			"follow-up subtask (or resolve it yourself with git) before continuing."
	}
	tools := append(tool.Make(workdir, false), delegateTool)
	tools = append(tools, extraOrch...)
	tools = append(tools, configtools.Tools()...)
	orch := agent.New("orchestrator", orchProv, cfg.Orchestrator.Model, system, tools,
		cfg.Orchestrator.MaxTurns, 0, onEvent)
	orch.CompactBudget = cfg.Orchestrator.ContextBudget
	return orch, nil
}
