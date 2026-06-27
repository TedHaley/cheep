// Package orchestrator wires the orchestrator agent and its
// delegate_to_executor tool.
//
// The orchestrator is just an Agent with read-only tools plus one extra tool
// that spawns a fresh executor Agent and runs it to completion. The executor's
// RunResult (status + summary + token usage) is handed straight back to the
// orchestrator as the tool result, so it can verify and, if the status is not
// "completed", recover.
package orchestrator

import (
	"encoding/json"

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

const orchestratorSystem = `You are the orchestrator. You coordinate executor agents to
accomplish the user's overall task. You are expensive; the executors are cheap and local.
Therefore:

- DECOMPOSE the task into concrete, self-contained subtasks.
- DELEGATE each subtask with delegate_to_executor. The executor has NO memory of prior
  subtasks or of this conversation, so every delegation must contain all context it needs.
- VERIFY every executor's work yourself with read_file, list_dir and run_bash (read the
  files it claims to have written, run the tests, inspect the output). Never trust a
  "done" report without checking.
- RECOVER when an executor returns a status other than "completed" (max_turns, looping,
  context_exhausted, error): split the subtask into smaller pieces, clarify the
  instructions, or remove the blocker, then delegate again.
- Do NOT write code or edit files yourself. Your job is to plan, delegate, and verify.

When the entire task is verified complete, stop calling tools and give a final summary.`

func Build(s config.Settings, onEvent core.EventFunc) *agent.Agent {
	execProvider := provider.NewOpenAI(s.Executor.BaseURL, s.Executor.APIKey, 4096)

	delegate := func(args map[string]any) string {
		subtask, _ := args["subtask"].(string)
		ex := agent.New(
			"executor:"+s.Executor.Model,
			execProvider,
			s.Executor.Model,
			executorSystem,
			tool.Make(s.Workdir, true),
			s.Executor.MaxTurns,
			s.Executor.TokenBudget,
			onEvent,
		)
		r := ex.Run(subtask)
		out, _ := json.MarshalIndent(map[string]any{
			"status":        r.Status,
			"turns":         r.Turns,
			"input_tokens":  r.InputTokens,
			"output_tokens": r.OutputTokens,
			"output":        r.Output,
		}, "", "  ")
		return string(out)
	}

	delegateTool := core.Tool{
		Name: "delegate_to_executor",
		Description: "Delegate one concrete, self-contained subtask to a fresh executor agent " +
			"(local Qwen). The executor has no prior context, so include every detail it needs. " +
			"Returns the executor's final status and summary as JSON.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subtask": map[string]any{
					"type":        "string",
					"description": "Complete, self-contained instructions for the executor.",
				},
			},
			"required": []string{"subtask"},
		},
		Func: delegate,
	}

	tools := append(tool.Make(s.Workdir, false), delegateTool)
	return agent.New(
		"orchestrator",
		provider.NewAnthropic(s.Orchestrator.APIKey, 4096),
		s.Orchestrator.Model,
		orchestratorSystem,
		tools,
		s.Orchestrator.MaxTurns,
		0,
		onEvent,
	)
}
