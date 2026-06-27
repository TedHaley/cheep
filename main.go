// Command cheep is a Claude orchestrator that coordinates local Qwen executor
// agents: it decomposes a task, delegates self-contained subtasks to executors,
// verifies their work, and recovers them when they get stuck.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/orchestrator"
	"github.com/TedHaley/cheep/internal/provider"
)

// version is stamped at build time by GoReleaser (-X main.version=...).
var version = "dev"

const (
	cReset  = "\033[0m"
	cCyan   = "\033[1;36m"
	cGreen  = "\033[1;32m"
	cYellow = "\033[33m"
	cDim    = "\033[2m"
	cRed    = "\033[1;31m"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "check":
		cmdCheck(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("cheep %s\n", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`cheep — a Claude orchestrator that coordinates local Qwen executor agents.

Usage:
  cheep run "<task>" [--workdir DIR]    decompose, delegate to executors, verify
  cheep check [--workdir DIR]           ping the Claude and Qwen endpoints
  cheep version                         print the version

Config (env):
  ANTHROPIC_API_KEY            required for the orchestrator
  CHEEP_ORCHESTRATOR_MODEL     default claude-sonnet-4-6
  CHEEP_EXECUTOR_MODEL         default qwen2.5-coder
  CHEEP_EXECUTOR_BASE_URL      default http://localhost:11434/v1
  CHEEP_EXECUTOR_API_KEY       default ollama
`)
}

func cmdRun(argv []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workdir := fs.String("workdir", ".", "workspace directory the agents operate in")
	_ = fs.Parse(argv)

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, "error: no task provided")
		os.Exit(1)
	}

	s := config.FromEnv(*workdir)
	orch := orchestrator.Build(s, printer())
	fmt.Println("──────────── cheep ────────────")
	r := orch.Run(task)
	fmt.Println("──────────── done ────────────")
	fmt.Printf("Status: %s\n", r.Status)
	fmt.Printf("Orchestrator (Claude) tokens: in=%d out=%d\n\n", r.InputTokens, r.OutputTokens)
	fmt.Println(r.Output)
}

func cmdCheck(argv []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	workdir := fs.String("workdir", ".", "workspace directory")
	_ = fs.Parse(argv)
	s := config.FromEnv(*workdir)

	fmt.Printf("Executor    (%s @ %s): ", s.Executor.Model, s.Executor.BaseURL)
	ep := provider.NewOpenAI(s.Executor.BaseURL, s.Executor.APIKey, 16)
	if _, err := ep.Complete(s.Executor.Model, "Reply with: ok",
		[]core.Message{{Role: "user", Text: "ok"}}, nil); err != nil {
		fmt.Printf("%sFAILED: %v%s\n", cRed, err, cReset)
	} else {
		fmt.Printf("%sreachable%s\n", cGreen, cReset)
	}

	fmt.Printf("Orchestrator (%s): ", s.Orchestrator.Model)
	if s.Orchestrator.APIKey == "" {
		fmt.Printf("%sFAILED: ANTHROPIC_API_KEY not set%s\n", cRed, cReset)
		return
	}
	ap := provider.NewAnthropic(s.Orchestrator.APIKey, 16)
	if _, err := ap.Complete(s.Orchestrator.Model, "Reply with: ok",
		[]core.Message{{Role: "user", Text: "ok"}}, nil); err != nil {
		fmt.Printf("%sFAILED: %v%s\n", cRed, err, cReset)
	} else {
		fmt.Printf("%sreachable%s\n", cGreen, cReset)
	}
}

func printer() core.EventFunc {
	return func(e core.Event) {
		color := cCyan
		if e.Agent != "orchestrator" {
			color = cGreen
		}
		switch e.Type {
		case "text":
			fmt.Printf("%s%s%s: %s\n", color, e.Agent, cReset, e.Text)
		case "tool_call":
			fmt.Printf("%s%s%s → %s%s%s(%s)\n",
				color, e.Agent, cReset, cYellow, e.Tool, cReset, shortArgs(e.Args))
		case "tool_result":
			fmt.Printf("%s%s ← %s: %s%s\n", cDim, e.Agent, e.Tool, shortText(e.Result), cReset)
		case "status":
			fmt.Printf("%s%s status: %s%s\n", cRed, e.Agent, e.Status, cReset)
		case "error":
			fmt.Printf("%s%s error: %s%s\n", cRed, e.Agent, e.Text, cReset)
		}
	}
}

func shortArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:60]
		}
		parts = append(parts, k+"="+s)
	}
	out := strings.Join(parts, ", ")
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func shortText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
