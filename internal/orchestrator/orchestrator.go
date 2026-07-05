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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/approve"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configtools"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/dispatch"
	"github.com/TedHaley/cheep/internal/history"
	"github.com/TedHaley/cheep/internal/inflight"
	"github.com/TedHaley/cheep/internal/jobs"
	"github.com/TedHaley/cheep/internal/pricing"
	"github.com/TedHaley/cheep/internal/project"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/skills"
	"github.com/TedHaley/cheep/internal/tool"
	"github.com/TedHaley/cheep/internal/validate"
	"github.com/TedHaley/cheep/internal/worktree"
)

const reviewerSystem = `You are a fresh-context code reviewer. You did not write these changes
and have no stake in them. Judge the unified diff below on correctness, safety, and fit with
the project's stated conventions; use your read-only tools (read_file, list_dir) to check
surrounding code when the diff alone is ambiguous. Do not request stylistic rewrites of
working code. Be decisive.

End your reply with EXACTLY ONE JSON object and nothing after it:
{"verdict":"approve"} or
{"verdict":"revise","summary":"<one line>","issues":[{"severity":"high|medium|low","file":"<path>","description":"<what and why>"}]}
Only "revise" for issues that matter (bugs, broken behavior, unmet requirements, convention
violations declared in the project instructions) — not preferences.`

// NewReviewer builds the fresh-context review callback used by the validation
// pipeline: a new session per call (no bias from the implementing agent),
// read-only tools confined to the worktree, judging the branch diff. Exported
// so `cheep validate --review` shares the exact same reviewer.
func NewReviewer(cfg config.Config, onEvent core.EventFunc) func(ctx context.Context, diff, dir string) (validate.Verdict, error) {
	ra := cfg.Orchestrator
	if cfg.Reviewer != nil {
		ra = *cfg.Reviewer
	}
	if !usable(ra) {
		return nil
	}
	prov := provider.For(ra.Provider, ra.Endpoint, ra.APIKey, 4096)
	maxTurns := ra.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 15
	}
	return func(ctx context.Context, diff, dir string) (validate.Verdict, error) {
		sess := agent.New("reviewer", prov, ra.Model, reviewerSystem+project.InstructionsBlock(dir),
			tool.MakeReadOnly(dir), maxTurns, 0, onEvent).NewSession()
		r := sess.SendCtx(ctx, "Review this diff:\n\n```diff\n"+diff+"\n```")
		if r.Status != "completed" {
			return validate.Verdict{}, fmt.Errorf("reviewer ended with status %s", r.Status)
		}
		return validate.ExtractVerdict(r.Output)
	}
}

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
	ModeLoop Mode = "loop" // auto + plan: agree on measurable goals, then loop until met or plateaued
)

// ParseMode returns the mode for a name, or false if unknown.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeChat, ModePlan, ModeAuto, ModeLoop:
		return Mode(s), true
	}
	return "", false
}

// NextMode cycles chat → plan → auto → loop → chat.
func NextMode(m Mode) Mode {
	switch m {
	case ModeChat:
		return ModePlan
	case ModePlan:
		return ModeAuto
	case ModeAuto:
		return ModeLoop
	default:
		return ModeChat
	}
}

// loopSystem layers the LOOP-mode protocol on top of the normal auto prompt:
// agree on measurable goals first, then iterate toward them, stopping only on
// goal-reached or plateau.
const loopSystem = `

LOOP MODE is ON — you work toward a GOAL, iterating until it's met or progress plateaus.
A goal can be NUMERIC or a COVERAGE goal; pick the shape that fits and confirm it with the
user (at most one short round of questions — propose one yourself when they're vague):

  • NUMERIC — a shell command whose output is a number, a direction, and a target
    (coverage %, test pass count, lint warnings, p95 latency, bundle bytes, ns/op).
  • COVERAGE — "do X for every item in a set that isn't fully known up front"
    (e.g. replicate every capability of a website, port every endpoint, cover every case).
    Here the "metric" is items-remaining → 0. You MAINTAIN THE SET as a checklist file
    (update_todos plus a tracked doc), because the set grows as you discover more.

Protocol, in order:
1. AGREE ON THE GOAL and its done-definition (target number, or "the checklist is empty").
2. BASELINE. Measure / enumerate what exists before changing anything; report it.
3. LOOP one chunk at a time:
   • NUMERIC → prefer iterate_metric (improve→measure rounds, auto-stops on target/plateau);
     iterate_until for plain pass/fail.
   • COVERAGE → each round: pick the next unfinished checklist item, delegate ONE narrow
     self-contained subtask for it (sized to the executor's context window), verify it, mark
     it done, and re-scan for newly discovered items. Executors are fresh each subtask and
     save their own progress, so a big set survives across many small sessions.
4. STOP only when: the target is met / the checklist is empty, progress has PLATEAUED (two
   consecutive rounds add nothing done and nothing new discovered), or budget/user stops you.
   Never stop because a round "seems good" — the goal decides, not vibes.
5. REPORT the trajectory (baseline → rounds → final), what moved it most, and — if plateaued
   short — the bottleneck and what would unblock it.

If the WORK IS IN A DIFFERENT DIRECTORY than your workspace, don't fight the sandbox — tell
the user to run /cd <path> (or relaunch there); your file tools are scoped to the workspace.`

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
  {"executor": "<name>", "subtask": "<full instructions>", "kind": "ship"|"scout"}. Updating
  todos is NOT doing the work — only delegate (then verify) actually does it.
- SCOUT vs SHIP: mark investigation, audit, research, and planning subtasks "kind":"scout" —
  a scout's file changes are discarded and its findings come back in "output" (also saved to
  the "report" path). Use "ship" (the default) only when the subtask must change files.
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
- LOOP to convergence: when work has an objective check (tests pass, build succeeds, lint
  clean), call iterate_until{"subtask","check","executor"} instead of re-delegating by hand —
  it re-runs the work on a cheap executor and re-runs the check until it passes (bounded).
  Iterating on cheap/local executors is nearly free, so prefer it for fix-until-green loops.
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
	compact    int // self-compaction trigger (executor context management)
	timeout    time.Duration
	maxResumes int
	extra      []core.Tool
	onEvent    core.EventFunc
	projectFn  func() string // project instructions, re-read per session
	gate       *approve.Gate
	root       string // the user's real workdir (worktrees differ from it)
}

func (e execRuntime) newSession(workdir, label string) *agent.Session {
	// Only work in the SHARED workspace is gated; isolated worktrees answer
	// to the validation pipeline and the merge boundary instead.
	shared := workdir == e.root
	tools := append(e.gate.Wrap(tool.Make(workdir, true), shared, workdir), e.extra...)
	system := executorSystem
	if e.projectFn != nil {
		// Read from the repo root (not the worktree copy) so uncommitted
		// AGENTS.md edits and freshly recorded lessons reach new sessions.
		system += e.projectFn()
	}
	a := agent.New(label, e.provider, e.model, system,
		tools, e.maxTurns, e.budget, e.onEvent)
	a.CompactBudget = e.compact // compress-and-continue instead of hard-stopping
	a.CompactNote = func(sum string) {
		history.AppendRunNote(e.root, "**"+label+" compacted mid-task — progress saved**\n\n"+sum)
	}
	return a.NewSession()
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
	anySmall := false
	for _, e := range sorted {
		model := e.Model
		if model == "" {
			model = "unknown model"
		}
		ctx := ""
		if e.ContextWindow > 0 {
			ctx = fmt.Sprintf("  ~%dk ctx", e.ContextWindow/1000)
			if e.ContextWindow < 32000 {
				anySmall = true
			}
		}
		fmt.Fprintf(&b, "  - %q runs %q  [%s]%s\n", e.Name, model, costTier(e), ctx)
	}
	if anySmall {
		b.WriteString("Some executors have a SMALL context window (shown above). Size each subtask to fit: " +
			"give one narrow, self-contained job per delegate, never \"read this whole site/repo\" — " +
			"split by page/file/section so no single executor session overflows.\n")
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

// scheduleTool lets the agent create a recurring background job when the user
// asks to do something "every X" or "at X time". The agent translates natural
// language into the schedule string (duration or cron).
func scheduleTool(workdir string) core.Tool {
	return core.Tool{
		Name: "schedule_task",
		Description: "Schedule a task to run automatically on a recurring basis. Use when the user asks " +
			"to do something repeatedly (\"every 2 hours\", \"at 9am daily\", \"every weekday morning\"). " +
			"The \"schedule\" is EITHER a Go duration for intervals (\"30m\", \"2h\", \"24h\") OR a 5-field " +
			"cron expression for clock times: \"0 9 * * *\" = 9am daily, \"0 9 * * 1-5\" = 9am weekdays, " +
			"\"*/15 * * * *\" = every 15 minutes, \"0 */2 * * *\" = every 2 hours on the hour. Jobs run via " +
			"the `cheep daemon` process — after scheduling, tell the user to run `cheep daemon` (e.g. in tmux) " +
			"if it isn't already running. Manage jobs with /scheduled.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task":     map[string]any{"type": "string", "description": "The task to run each time (self-contained, as if typed at the prompt)."},
				"schedule": map[string]any{"type": "string", "description": "Interval (\"2h\") or 5-field cron (\"0 9 * * *\")."},
				"name":     map[string]any{"type": "string", "description": "Short label for the job (optional)."},
			},
			"required": []string{"task", "schedule"},
		},
		Func: func(_ context.Context, args map[string]any) string {
			task := strArg(args, "task")
			sched := strArg(args, "schedule")
			j := jobs.Job{
				ID: jobs.NewID(time.Now()), Name: strArg(args, "name"), Task: task,
				Workdir: workdir, Schedule: sched, Enabled: true, Created: time.Now(),
			}
			if err := j.Validate(); err != nil {
				return "ERROR: " + err.Error()
			}
			if err := jobs.Save(j); err != nil {
				return "ERROR: couldn't save job: " + err.Error()
			}
			next := ""
			if n, ok := j.Next(time.Now()); ok {
				next = " · next run " + n.Local().Format("Mon Jan 2 15:04")
			}
			return "Scheduled job " + j.ID + " (" + sched + ")" + next +
				". It runs when `cheep daemon` is active; manage jobs with /scheduled."
		},
	}
}

// usable reports whether an agent can actually run.
func usable(a config.Agent) bool {
	return a.Model != "" && !(a.Provider == "anthropic" && a.APIKey == "")
}

func strArg(a map[string]any, k string) string { s, _ := a[k].(string); return s }

func intArg(a map[string]any, k string, def int) int {
	if v, ok := a[k].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…(truncated)"
	}
	return s
}

// parseMetric extracts the LAST number in a measure command's output.
func parseMetric(out string) (float64, error) {
	ms := metricRe.FindAllString(out, -1)
	if len(ms) == 0 {
		return 0, fmt.Errorf("no number in output")
	}
	return strconv.ParseFloat(strings.TrimSuffix(ms[len(ms)-1], "%"), 64)
}

var metricRe = regexp.MustCompile(`-?\d+(?:\.\d+)?%?`)

// runCheck runs a shell predicate in dir and returns its combined output + exit code.
func runCheck(ctx context.Context, dir, cmd string) (string, int) {
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out) + err.Error(), -1
	}
	return string(out), 0
}

func iterRes(status string, rounds int, exName, check, out, note string) string {
	res := map[string]any{
		"status": status, "rounds": rounds, "executor": exName, "check": check,
		"output": clip(strings.TrimSpace(out), 4000),
	}
	if note != "" {
		res["note"] = note
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return string(b)
}

// writeReport persists a scout task's findings to ~/.cheep/reports and returns
// the file path (firstmate's data/<id>/report.md, adapted).
func writeReport(executor string, id int, subtask, output string) (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, "reports")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%d.md", time.Now().UTC().Format("20060102-150405"), executor, id)
	path := filepath.Join(d, name)
	md := "# Scout report — " + executor + "\n\n" + time.Now().Local().Format("2006-01-02 15:04") +
		"\n\n## Subtask\n\n" + strings.TrimSpace(subtask) + "\n\n## Findings\n\n" + strings.TrimSpace(output) + "\n"
	return path, os.WriteFile(path, []byte(md), 0o600)
}

// batchNote renders one delegate batch as a run-notes entry.
func batchNote(subtasks []string, results []map[string]any) string {
	var b strings.Builder
	for i, r := range results {
		if r == nil {
			continue
		}
		sub := ""
		if i < len(subtasks) {
			sub = clip(strings.ReplaceAll(strings.TrimSpace(subtasks[i]), "\n", " "), 140)
		}
		fmt.Fprintf(&b, "- **%v** (%v): %s\n", r["status"], r["executor"], sub)
		if integ, ok := r["integration"].(string); ok && integ != "" {
			fmt.Fprintf(&b, "  - integration: %s\n", integ)
		}
		if v, ok := r["validation"].(validate.Result); ok {
			state := "failed"
			if v.Passed {
				state = "passed"
			}
			fmt.Fprintf(&b, "  - validation: %s (%d checks, %d fix rounds)\n", state, len(v.Checks), v.Rounds)
			if v.Review != nil && v.Review.Summary != "" {
				fmt.Fprintf(&b, "  - review: %s — %s\n", v.Review.Verdict, v.Review.Summary)
			}
		}
		if esc, ok := r["escalation"].(string); ok && esc != "" {
			fmt.Fprintf(&b, "  - escalation: %s\n", esc)
		}
		if rep, ok := r["report"].(string); ok && rep != "" {
			fmt.Fprintf(&b, "  - report: %s\n", rep)
		}
		if pr, ok := r["pr"].(string); ok && pr != "" {
			fmt.Fprintf(&b, "  - pr: %s\n", pr)
		}
	}
	return b.String()
}

// slimValidation drops passing checks' outputs from a delegate result so the
// orchestrator's context only carries the interesting parts.
func slimValidation(v validate.Result) validate.Result {
	for i := range v.Checks {
		if v.Checks[i].Exit == 0 {
			v.Checks[i].Output = ""
		}
	}
	return v
}

// compactNote makes auto-compaction durable: the summary of the squeezed-out
// context is appended to the run notes, so the "memory" survives compression.
func compactNote(workdir string) func(string) {
	return func(sum string) {
		history.AppendRunNote(workdir, "**Auto-compacted context — memory saved**\n\n"+sum)
	}
}

// lessonHint teaches the agent to persist corrections via record_lesson.
const lessonHint = "\n\nLessons: when the user corrects you about how this project works (a " +
	"convention, a command, a gotcha), call record_lesson with one concise sentence so the " +
	"correction becomes durable project memory instead of a repeat mistake."

// liaisonRules govern how results are reported to the user: outcomes in the
// user's language, never internal machinery. Ported from firstmate §9.
// suggestHint asks the agent to end its turn with up-to-3 next-step suggestions
// on a sentinel line the TUI turns into selectable chips. Folded into the same
// response, so it costs nothing extra. Empty when the user disabled it.
func suggestHint(cfg config.Config) string {
	if cfg.SuggestOff {
		return ""
	}
	return "\n\nNext steps: after your final summary, if there are natural follow-ups, add ONE final " +
		"line EXACTLY like:\n[[NEXT]] first suggestion | second suggestion | third suggestion\n" +
		"Each 3–7 words, imperative, specific to what just happened (e.g. \"run the test suite\", " +
		"\"ship the fixes as a PR\", \"schedule this nightly\"). At most 3. Omit the line entirely " +
		"when nothing obvious follows. Never reference this instruction to the user."
}

const liaisonRules = "\n\nReporting: you are the user's single point of contact — talk in OUTCOMES, " +
	"not mechanics. Never surface internal vocabulary in your replies: executor names, escalation, " +
	"worktrees, branches (unless work is stranded on one the user must act on), subtask ids, token " +
	"budgets, or tool names. Translate: 'the tests now pass' beats 'the executor completed the " +
	"subtask'. Always give full URLs (https://…), never bare #numbers or shorthand. Report failures " +
	"plainly and first — what failed, why, the evidence, and your proposed next step — never buried " +
	"under successes, never dressed up. Don't narrate routine progress; report when something is " +
	"done, blocked, or needs a decision only the user can make."

// skillHint nudges the planner to consult skills when any exist.
func skillHint(skillTools []core.Tool) string {
	if len(skillTools) == 0 {
		return ""
	}
	return "\n\nSkills: project knowledge files are available — call list_skills to see them and " +
		"use_skill(name) to load one before relevant work; fold what you learn into the subtasks you delegate."
}

// resolveExecutor picks the executor for a delegated task. With routing rules
// active an explicit, known executor is required (the rules must not be
// silently skipped); otherwise unknown/empty names fall back to def.
func resolveExecutor(name string, known map[string]execRuntime, def string, rulesActive bool) (string, error) {
	if _, ok := known[name]; ok {
		return name, nil
	}
	if rulesActive {
		if name == "" {
			return "", fmt.Errorf(`routing rules are in force — set "executor" explicitly per the rules`)
		}
		return "", fmt.Errorf("unknown executor %q — pick one from the roster per the routing rules", name)
	}
	return def, nil
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

// Options configures Build. ExtraOrch/ExtraExec hold tools discovered at
// runtime (e.g. MCP), added to the orchestrator and executors respectively; in
// solo mode the agent gets both.
type Options struct {
	Isolate   bool
	Mode      Mode
	ExtraOrch []core.Tool
	ExtraExec []core.Tool
	OnEvent   core.EventFunc
	Gate      *approve.Gate // nil = no approval gating
}

// Build returns the orchestrator agent wired per opt.
func Build(cfg config.Config, workdir string, opt Options) (*agent.Agent, error) {
	isolate, mode := opt.Isolate, opt.Mode
	extraOrch, extraExec, onEvent := opt.ExtraOrch, opt.ExtraExec, opt.OnEvent
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

	// Standing instructions from AGENTS.md (global + project), injected into
	// every role so all agents share the project's rules.
	projBlock := project.Load(workdir).PromptBlock()

	withBudget := func(a *agent.Agent) *agent.Agent {
		a.CompactBudget = cfg.Orchestrator.ContextBudget
		return a
	}

	// Chat: no tools. Plan: read-only investigation + extra (no edits/delegation).
	switch mode {
	case ModeChat:
		return withBudget(agent.New("cheep", orchProv, cfg.Orchestrator.Model, chatSystem+projBlock,
			nil, cfg.Orchestrator.MaxTurns, 0, onEvent)), nil
	case ModePlan:
		skillTools := skills.Tools(workdir)
		planTools := append(tool.MakeReadOnly(workdir), extraOrch...)
		planTools = append(planTools, skillTools...)
		return withBudget(agent.New("cheep", orchProv, cfg.Orchestrator.Model, planSystem+skillHint(skillTools)+projBlock,
			planTools, cfg.Orchestrator.MaxTurns, 0, onEvent)), nil
	}

	// ModeAuto, solo: no executors, the orchestrator does the work itself, so it
	// gets both role's tools.
	if len(cfg.Executors) == 0 {
		skillTools := skills.Tools(workdir)
		soloTools := append(opt.Gate.Wrap(tool.Make(workdir, true), true, workdir), extraOrch...)
		soloTools = append(soloTools, extraExec...)
		soloTools = append(soloTools, configtools.Tools()...)
		soloTools = append(soloTools, scheduleTool(workdir))
		soloTools = append(soloTools, skillTools...)
		soloTools = append(soloTools, project.LessonTool(workdir))
		soloSys := soloSystem
		if mode == ModeLoop {
			soloSys += loopSystem
		}
		solo := agent.New("cheep", orchProv, cfg.Orchestrator.Model, soloSys+skillHint(skillTools)+lessonHint+liaisonRules+suggestHint(cfg)+projBlock,
			soloTools, cfg.Orchestrator.MaxTurns, 0, onEvent)
		solo.CompactBudget = cfg.Orchestrator.ContextBudget
		solo.CompactNote = compactNote(workdir)
		return solo, nil
	}

	runtimes := map[string]execRuntime{}
	var order []string
	projectFn := func() string { return project.InstructionsBlock(workdir) }
	for _, e := range cfg.Executors {
		runtimes[e.Name] = execRuntime{
			name:       e.Name,
			model:      e.Model,
			provider:   provider.For(e.Provider, e.Endpoint, e.APIKey, 4096),
			maxTurns:   e.MaxTurns,
			budget:     e.TokenBudget,
			compact:    e.ContextBudget,
			timeout:    time.Duration(e.TimeoutSeconds) * time.Second,
			maxResumes: e.MaxResumes,
			extra:      extraExec,
			onEvent:    onEvent,
			projectFn:  projectFn,
			gate:       opt.Gate,
			root:       workdir,
		}
		order = append(order, e.Name)
	}
	defaultExec := order[0]
	isolated := isolate && worktree.IsRepo(workdir)
	// A freshly-init'd repo has no commit, so worktrees can't branch from HEAD.
	// Establish one (including any existing files) rather than silently losing
	// isolation to the shared workspace.
	if isolated {
		if created, err := worktree.EnsureCommit(workdir); err != nil {
			onEvent(core.Event{Agent: "cheep", Type: "status", Status: "isolation off — couldn't initialize the repo: " + err.Error()})
			isolated = false
		} else if created {
			onEvent(core.Event{Agent: "cheep", Type: "status", Status: "made an initial git commit so isolated worktrees work"})
		}
	}

	// Pooled worktrees keep gitignored build artifacts warm across subtasks.
	// Nil pool (disabled, unsupported platform, or open failure) falls back to
	// ephemeral per-subtask worktrees.
	var pool *worktree.Pool
	if isolated && !cfg.DisablePool {
		if p, err := worktree.OpenPool(workdir); err == nil {
			pool = p
		}
	}
	// release returns a tree with its work either landed (merged / nothing to
	// keep) or preserved (quarantined slot or kept branch).
	release := func(t *worktree.Tree, landed bool) {
		if pool != nil {
			pool.Release(t, landed)
			return
		}
		t.Remove(!landed)
	}

	// Pre-merge validation: project checks + fresh-context review, with
	// bounded fix rounds, all inside the subtask's private worktree.
	validationOn := !cfg.Validation.Disable
	var reviewerFn func(context.Context, string, string) (validate.Verdict, error)
	if validationOn && !cfg.Validation.SkipReview {
		reviewerFn = NewReviewer(cfg, onEvent)
	}

	// Natural-language routing rules (the LLM matches; delegate() enforces
	// only that a choice was made while rules exist).
	home, _ := config.Home()
	rules, rerr := dispatch.Load(workdir, home)
	if rerr != nil {
		onEvent(core.Event{Agent: "cheep", Type: "status", Status: "dispatch rules ignored: " + rerr.Error()})
	}

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

	// cheapest executor — the default tier for loops.
	cheapest := order[0]
	for _, n := range order {
		if scoreByName[n] < scoreByName[cheapest] {
			cheapest = n
		}
	}

	// iterateUntil runs a subtask on a (cheap) executor and re-runs a shell check
	// until it passes, feeding each failure back in. Bounded by max_rounds and the
	// run context (so the budget cap stops it too). Loops on cheap executors are
	// nearly free — iterate aggressively.
	iterateUntil := func(ctx context.Context, args map[string]any) string {
		subtask, check := strArg(args, "subtask"), strArg(args, "check")
		if subtask == "" || check == "" {
			return `ERROR: "subtask" and "check" are required`
		}
		ex := strArg(args, "executor")
		if _, ok := runtimes[ex]; !ok {
			ex = cheapest
		}
		maxRounds := intArg(args, "max_rounds", 5)
		task := subtask
		var lastOut string
		for round := 1; round <= maxRounds; round++ {
			if ctx.Err() != nil {
				return iterRes("aborted", round-1, ex, check, lastOut, "cancelled")
			}
			_, rt, _ := runJob(ctx, workdir, task, ex, round)
			out, code := runCheck(ctx, workdir, check)
			lastOut = out
			onEvent(core.Event{Agent: "cheep", Type: "status",
				Status: fmt.Sprintf("iterate round %d/%d on %s — check exit %d", round, maxRounds, rt.name, code)})
			if code == 0 {
				return iterRes("passed", round, rt.name, check, out, "")
			}
			task = fmt.Sprintf("%s\n\nRound %d: the check `%s` failed (exit %d). Output:\n%s\n\n"+
				"Fix the underlying cause so the check passes.", subtask, round, check, code, clip(out, 4000))
		}
		return iterRes("failed", maxRounds, ex, check, lastOut, "check still failing after max_rounds")
	}

	iterateMetric := func(ctx context.Context, args map[string]any) string {
		subtask, measure := strArg(args, "subtask"), strArg(args, "measure")
		if subtask == "" || measure == "" {
			return `ERROR: "subtask" and "measure" are required`
		}
		ex := strArg(args, "executor")
		if _, ok := runtimes[ex]; !ok {
			ex = cheapest
		}
		down := strArg(args, "direction") == "down"
		target, hasTarget := args["target"].(float64)
		maxRounds := intArg(args, "max_rounds", 8)
		plateau := intArg(args, "plateau", 2)

		read := func() (float64, string) {
			out, _ := runCheck(ctx, workdir, measure)
			v, err := parseMetric(out)
			if err != nil {
				return 0, out
			}
			return v, ""
		}
		baseline, badOut := read()
		if badOut != "" {
			return `ERROR: measure command produced no number. Output: ` + clip(badOut, 800)
		}
		better := func(a, b float64) bool { // is a better than b?
			if down {
				return a < b
			}
			return a > b
		}
		met := func(v float64) bool {
			if !hasTarget {
				return false
			}
			if down {
				return v <= target
			}
			return v >= target
		}

		best := baseline
		trajectory := []float64{baseline}
		noImp := 0
		status, rounds := "max_rounds", 0
		if met(baseline) {
			status, rounds = "goal_already_met", 0
		} else {
			for round := 1; round <= maxRounds; round++ {
				if ctx.Err() != nil {
					status, rounds = "aborted", round-1
					break
				}
				task := fmt.Sprintf("%s\n\nGoal: move the metric `%s` %s (current best %.4g, latest %.4g%s). "+
					"Make the most impactful improvement you can this round.",
					subtask, measure, map[bool]string{true: "DOWN", false: "UP"}[down],
					best, trajectory[len(trajectory)-1],
					map[bool]string{true: fmt.Sprintf(", target %.4g", target), false: ""}[hasTarget])
				_, rt, _ := runJob(ctx, workdir, task, ex, round)
				v, bad := read()
				if bad != "" {
					status, rounds = "measure_failed", round
					break
				}
				trajectory = append(trajectory, v)
				rounds = round
				onEvent(core.Event{Agent: "cheep", Type: "status",
					Status: fmt.Sprintf("loop round %d/%d on %s — %s = %.4g (best %.4g)", round, maxRounds, rt.name, measure, v, best)})
				if better(v, best) {
					best, noImp = v, 0
				} else {
					noImp++
				}
				if met(v) {
					status = "goal_reached"
					break
				}
				if noImp >= plateau {
					status = "plateaued"
					break
				}
			}
		}
		res := map[string]any{
			"status": status, "rounds": rounds, "baseline": baseline, "best": best,
			"trajectory": trajectory, "measure": measure,
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		return string(b)
	}

	iterateMetricTool := core.Tool{
		Name: "iterate_metric",
		Description: "Optimize toward a NUMERIC goal: run improve→measure rounds on a cheap executor until " +
			"the target is reached or progress plateaus (no improvement for `plateau` consecutive rounds). " +
			"`measure` is a shell command whose output ends with the metric number. Use for coverage, " +
			"benchmark, size, and count goals; use iterate_until for plain pass/fail checks.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subtask":    map[string]any{"type": "string", "description": "What to improve each round (self-contained)."},
				"measure":    map[string]any{"type": "string", "description": "Shell command printing the metric; the LAST number in its output is used."},
				"direction":  map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "Which way is better (default up)."},
				"target":     map[string]any{"type": "number", "description": "Stop when the metric reaches this (optional — else run to plateau)."},
				"executor":   map[string]any{"type": "string", "description": "Executor to run on (default: the cheapest)."},
				"max_rounds": map[string]any{"type": "integer", "description": "Max improvement rounds (default 8)."},
				"plateau":    map[string]any{"type": "integer", "description": "Stop after this many rounds without improvement (default 2)."},
			},
			"required": []string{"subtask", "measure"},
		},
		Func: iterateMetric,
	}

	iterateTool := core.Tool{
		Name: "iterate_until",
		Description: "Run a subtask on a cheap executor and re-run a shell CHECK until it passes, " +
			"feeding each failure back in (bounded). Use for work with an objective pass/fail signal — " +
			"tests, build, lint. Cheaper than re-delegating by hand, and loops on cheap executors are nearly free.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subtask":    map[string]any{"type": "string", "description": "What to do each round (self-contained)."},
				"check":      map[string]any{"type": "string", "description": "Shell command that exits 0 when done, e.g. \"go test ./...\"."},
				"executor":   map[string]any{"type": "string", "description": "Executor to run on (default: the cheapest)."},
				"max_rounds": map[string]any{"type": "integer", "description": "Max iterations (default 5)."},
			},
			"required": []string{"subtask", "check"},
		},
		Func: iterateUntil,
	}

	delivery := cfg.Delivery
	noMistakes := cfg.NoMistakes

	delegate := func(ctx context.Context, args map[string]any) string {
		rawTasks, _ := args["tasks"].([]any)
		if len(rawTasks) == 0 {
			return `ERROR: "tasks" must be a non-empty array of {"executor","subtask"}`
		}
		type job struct {
			executor, subtask, kind string
		}
		jobs := make([]job, len(rawTasks))
		for i, rt := range rawTasks {
			m, _ := rt.(map[string]any)
			ex, _ := m["executor"].(string)
			st, _ := m["subtask"].(string)
			kind, _ := m["kind"].(string)
			if kind != "scout" {
				kind = "" // ship is the default
			}
			resolved, err := resolveExecutor(ex, runtimes, defaultExec, rules.Active())
			if err != nil {
				return fmt.Sprintf("ERROR: task %d: %v", i+1, err)
			}
			jobs[i] = job{executor: resolved, subtask: st, kind: kind}
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
					var t *worktree.Tree
					var err error
					if pool != nil {
						if t, err = pool.Acquire(start.name, id, false); err != nil {
							t, err = worktree.Add(workdir, start.name, id) // pool exhausted → ephemeral
						}
					} else {
						t, err = worktree.Add(workdir, start.name, id)
					}
					gitMu.Unlock()
					if err == nil {
						tree, wd = t, t.Path
					} else {
						start.onEvent(core.Event{Agent: "cheep", Type: "status",
							Status: "worktree unavailable, using shared dir: " + err.Error()})
					}
				}

				// Crash marker: if this process dies mid-delegation, the next
				// launch surfaces the interruption (the branch survives on its
				// quarantined slot regardless).
				mark := inflight.Job{Workdir: workdir, Executor: j.executor, Kind: j.kind,
					Subtask: j.subtask, Started: time.Now()}
				if tree != nil {
					mark.Branch = tree.Branch
				}
				mark = inflight.Mark(mark)
				defer mark.Clear()

				subtask := j.subtask
				if j.kind == "scout" {
					subtask = "SCOUT task — investigate and report only. Any file changes you make " +
						"will be DISCARDED, so put your COMPLETE findings in your final reply.\n\n" + subtask
				}
				r, rt, trail := runJob(ctx, wd, subtask, j.executor, id)
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

				if j.kind == "scout" {
					// Scouts deliver a report, not changes: discard whatever
					// they touched and persist the findings as an artifact.
					if path, err := writeReport(rt.name, id, j.subtask, r.Output); err == nil {
						res["report"] = path
					}
					if tree != nil {
						gitMu.Lock()
						tree.Discard()
						release(tree, true)
						gitMu.Unlock()
					}
					results[i] = res
					return
				}

				if tree != nil {
					gitMu.Lock()
					committed, cErr := tree.CommitAll("cheep: " + rt.name + " subtask")
					gitMu.Unlock()

					passed := true
					if cErr == nil && committed && validationOn {
						// Checks, review, and fix rounds run OUTSIDE gitMu:
						// they only touch this subtask's private worktree, and
						// long test runs must not serialize the other subtasks.
						fix := func(fctx context.Context, dir, task string) error {
							fr, _, _ := runJob(fctx, dir, task, j.executor, id)
							if fr.Status != "completed" {
								return fmt.Errorf("fix attempt ended with status %s", fr.Status)
							}
							gitMu.Lock()
							_, err := tree.CommitAll("cheep: validation fix (" + rt.name + ")")
							gitMu.Unlock()
							return err
						}
						runner := validate.Runner{
							Checks:    project.Load(workdir).Checks,
							MaxRounds: cfg.Validation.MaxFixRounds,
							Strict:    cfg.Validation.Strict,
							Reviewer:  reviewerFn,
							Fix:       fix,
							OnEvent:   onEvent,
						}
						vres := runner.Run(ctx, tree.Path, tree.Base)
						passed = vres.Passed
						res["validation"] = slimValidation(vres)
					}

					// No-mistakes: the user signs off on the diff BEFORE anything
					// merges. Asked outside gitMu so a pending approval never
					// blocks the other subtasks' integration; fails closed when
					// there is no approver (headless) or the run is cancelled.
					mergeOK := true
					if noMistakes && delivery != "pr" && cErr == nil && committed && passed {
						d := opt.Gate.Ask(ctx, approve.Request{Agent: rt.name, Tool: "merge",
							Path: tree.Branch, Diff: tree.Diff()})
						mergeOK = d == approve.Allow || d == approve.AllowSession
					}

					gitMu.Lock()
					switch {
					case cErr != nil:
						res["integration"] = "commit failed: " + cErr.Error()
						release(tree, false)
					case !committed:
						res["integration"] = "no file changes"
						release(tree, true)
					case !passed:
						res["integration"] = "failed validation (work kept on branch " + tree.Branch + ")"
						release(tree, false)
					case !mergeOK:
						res["integration"] = "no-mistakes: not merged — awaiting/declined approval (work kept on branch " + tree.Branch + ")"
						release(tree, false)
					case delivery == "pr":
						title := clip(strings.ReplaceAll(strings.TrimSpace(j.subtask), "\n", " "), 70)
						body := "Delegated subtask:\n\n" + clip(j.subtask, 2000) +
							"\n\n---\nOpened by cheep (executor: " + rt.name + ", validated pre-merge)."
						if url, prErr := tree.PushAndPR("cheep: "+title, body); prErr != nil {
							res["integration"] = "pr failed: " + prErr.Error() + " (work kept on branch " + tree.Branch + ")"
							release(tree, false)
						} else {
							res["integration"] = "pull request opened"
							res["pr"] = url
							release(tree, true)
						}
					default:
						if mErr := tree.MergeInto(); mErr != nil {
							res["integration"] = mErr.Error() + " (kept on branch " + tree.Branch + ")"
							release(tree, false)
						} else {
							res["integration"] = "merged"
							release(tree, true)
						}
					}
					gitMu.Unlock()
				}

				results[i] = res
			}(i, j)
		}
		wg.Wait()
		subs := make([]string, len(jobs))
		for i, j := range jobs {
			subs[i] = j.subtask
		}
		history.AppendRunNote(workdir, batchNote(subs, results))
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
							"kind": map[string]any{
								"type": "string",
								"enum": []string{"ship", "scout"},
								"description": "\"ship\" (default) delivers file changes. \"scout\" is " +
									"investigation/audit/planning only: its file changes are discarded and its " +
									"findings are saved as a report — use it for research so no merge machinery runs.",
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
	if mode == ModeLoop {
		system += loopSystem
	}
	if isolated {
		system += "\n\nIsolation is ON: each delegated subtask runs in its own git worktree and is " +
			"merged back automatically. Each result has an \"integration\" field — \"merged\", " +
			"\"no file changes\", or a conflict left on a branch. If a merge conflicts, delegate a " +
			"follow-up subtask (or resolve it yourself with git) before continuing."
		if cfg.NoMistakes && cfg.Delivery != "pr" {
			system += "\n\nNO-MISTAKES mode is ON: a validated subtask's branch merges ONLY after the " +
				"user approves its diff. A result whose integration says \"no-mistakes: not merged\" " +
				"is NOT a failure — the work is safe on the named branch awaiting the user's sign-off; " +
				"report it that way and do not re-delegate it."
		}
		if delivery := cfg.Delivery; delivery == "pr" {
			system += "\n\nDelivery is PR mode: validated ship subtasks are NOT merged locally — each " +
				"branch is pushed and opened as a pull request (the result's \"pr\" field has the URL). " +
				"Give the user the PR URLs; the local checkout is not modified."
		}
		if validationOn {
			system += "\n\nValidation is ON: before any merge, the project's declared checks run in the " +
				"subtask's worktree and a fresh-context reviewer judges the diff, with bounded automatic " +
				"fix rounds. A result whose \"integration\" says \"failed validation\" kept its work on " +
				"the named branch — read its \"validation\" field, decide whether the finding is real, " +
				"and either delegate a targeted fix on that branch or report the finding to the user. " +
				"Never re-delegate the whole subtask from scratch when a branch already holds the work."
		}
	}
	skillTools := skills.Tools(workdir)
	system += rules.PromptBlock() + skillHint(skillTools) + lessonHint + liaisonRules + suggestHint(cfg) + projBlock
	tools := append(opt.Gate.Wrap(tool.Make(workdir, false), true, workdir), delegateTool, iterateTool, iterateMetricTool)
	tools = append(tools, extraOrch...)
	tools = append(tools, configtools.Tools()...)
	tools = append(tools, scheduleTool(workdir))
	tools = append(tools, skillTools...)
	tools = append(tools, project.LessonTool(workdir))
	orch := agent.New("orchestrator", orchProv, cfg.Orchestrator.Model, system, tools,
		cfg.Orchestrator.MaxTurns, 0, onEvent)
	orch.CompactBudget = cfg.Orchestrator.ContextBudget
	orch.CompactNote = compactNote(workdir)
	return orch, nil
}
