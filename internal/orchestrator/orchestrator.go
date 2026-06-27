// Package orchestrator wires the orchestrator agent and the tools it uses to
// command a fleet of executors.
//
// The orchestrator (Claude) knows the roster of executors and which model each
// one runs, so it can route subtasks to the most suitable worker. Its `delegate`
// tool accepts a list of subtasks and runs them in PARALLEL across the named
// executors, each a fresh agent with no shared context. Each executor returns a
// status the orchestrator uses to verify or recover the work.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/tool"
)

const executorSystem = `You are an executor agent: a focused, capable coding/execution worker.
You receive one concrete subtask from an orchestrator and complete it using your tools
(read_file, write_file, list_dir, run_bash). Work autonomously and efficiently.

When the subtask is fully done, STOP calling tools and reply with a short summary of
exactly what you did and how it can be verified. If you get blocked, stop and explain
clearly what is blocking you and why.`

const orchestratorSystemTmpl = `You are the orchestrator. You coordinate a fleet of cheaper
executor agents to accomplish the user's overall task. You are expensive; the executors are
cheap. Be economical: plan and delegate rather than doing the work yourself.

%s
- DECOMPOSE the task into concrete, self-contained subtasks.
- DELEGATE with the "delegate" tool. It takes a LIST of tasks and runs them in PARALLEL,
  so dispatch independent subtasks together in one call. Each task is
  {"executor": "<name>", "subtask": "<full instructions>"}.
- ROUTE each subtask to the executor whose model is best suited to it, based on the models
  listed above. If a subtask has no obvious best fit, pick any executor.
- Executors share NO memory or context with you or each other; every subtask must contain
  all the detail it needs to be done in isolation.
- VERIFY every result yourself with read_file, list_dir and run_bash. Never trust a "done"
  report without checking.
- RECOVER when an executor returns a status other than "completed" (max_turns, looping,
  context_exhausted, error): split the subtask smaller, clarify it, or fix the blocker,
  then delegate again.
- Do NOT write code or edit files yourself. Plan, delegate, and verify.

When the entire task is verified complete, stop calling tools and give a final summary.`

type execRuntime struct {
	name     string
	model    string
	provider core.Provider
	maxTurns int
	budget   int
	workdir  string
	onEvent  core.EventFunc
}

func (e execRuntime) run(subtask string) agent.RunResult {
	a := agent.New("executor:"+e.name, e.provider, e.model, executorSystem,
		tool.Make(e.workdir, true), e.maxTurns, e.budget, e.onEvent)
	return a.Run(subtask)
}

func roster(execs []config.Executor) string {
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
func Build(cfg config.Config, workdir string, onEvent core.EventFunc) (*agent.Agent, error) {
	if cfg.Orchestrator.APIKey == "" {
		return nil, fmt.Errorf("no orchestrator API key (set ANTHROPIC_API_KEY or run `cheep config`)")
	}
	if len(cfg.Executors) == 0 {
		return nil, fmt.Errorf("no executors configured (run `cheep config`)")
	}

	runtimes := map[string]execRuntime{}
	var order []string
	for _, e := range cfg.Executors {
		runtimes[e.Name] = execRuntime{
			name:     e.Name,
			model:    e.Model,
			provider: provider.NewOpenAI(e.BaseURL, e.APIKey, 4096),
			maxTurns: e.MaxTurns,
			budget:   e.TokenBudget,
			workdir:  workdir,
			onEvent:  onEvent,
		}
		order = append(order, e.Name)
	}
	defaultExec := order[0]

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
		for i, j := range jobs {
			wg.Add(1)
			go func(i int, j job) {
				defer wg.Done()
				rt := runtimes[j.executor]
				r := rt.run(j.subtask)
				results[i] = map[string]any{
					"executor":      rt.name,
					"model":         rt.model,
					"status":        r.Status,
					"turns":         r.Turns,
					"input_tokens":  r.InputTokens,
					"output_tokens": r.OutputTokens,
					"output":        r.Output,
				}
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
	tools := append(tool.Make(workdir, false), delegateTool)
	return agent.New(
		"orchestrator",
		provider.NewAnthropic(cfg.Orchestrator.APIKey, 4096),
		cfg.Orchestrator.Model,
		system,
		tools,
		cfg.Orchestrator.MaxTurns,
		0,
		onEvent,
	), nil
}
