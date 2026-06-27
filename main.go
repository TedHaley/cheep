// Command cheep is a Claude orchestrator that coordinates a fleet of cheaper
// executor agents: it decomposes a task, delegates self-contained subtasks to
// executors (in parallel), verifies their work, and recovers them when stuck.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

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
		cmdCheck()
	case "config":
		cmdConfig(os.Args[2:])
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
	fmt.Print(`cheep — a Claude orchestrator that coordinates a fleet of cheaper executor agents.

Usage:
  cheep run "<task>" [--workdir DIR]    decompose, delegate to executors, verify
  cheep check                           ping the orchestrator and every executor
  cheep config [show|path]              set up or inspect your agents
  cheep version                         print the version

On first use, cheep walks you through choosing an orchestrator and one or more
executors. Configuration lives in a single JSON file (see "cheep config path").
`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "%scheep: %v%s\n", cRed, err, cReset)
	os.Exit(1)
}

// ensureConfig loads the config, running the first-time setup wizard if none exists.
func ensureConfig() config.Config {
	if !config.Exists() {
		fmt.Println("No configuration found yet — let's set up your agents.")
		fmt.Println()
		c, err := runWizard()
		if err != nil {
			fatal(err)
		}
		return c
	}
	c, err := config.Load()
	if err != nil {
		fatal(fmt.Errorf("reading config: %w", err))
	}
	return c
}

// ---- setup wizard ---------------------------------------------------------

func runWizard() (config.Config, error) {
	in := bufio.NewScanner(os.Stdin)
	ask := func(prompt, def string) string {
		if def != "" {
			fmt.Printf("%s [%s]: ", prompt, def)
		} else {
			fmt.Printf("%s: ", prompt)
		}
		if !in.Scan() {
			return def
		}
		if v := strings.TrimSpace(in.Text()); v != "" {
			return v
		}
		return def
	}

	var c config.Config
	fmt.Println("── Orchestrator ──  the smart model that plans, delegates and verifies")
	c.Orchestrator.Provider = "anthropic"
	c.Orchestrator.Model = ask("Orchestrator model", "claude-sonnet-4-6")
	keyPrompt := "Anthropic API key"
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		keyPrompt += " (blank = use ANTHROPIC_API_KEY)"
	}
	c.Orchestrator.APIKey = ask(keyPrompt, "")

	fmt.Println()
	fmt.Println("── Executors ──  the cheap workers; give each an endpoint and access key")
	for {
		fmt.Println()
		name := ask("Executor name", fmt.Sprintf("executor-%d", len(c.Executors)+1))
		endpoint := ask("Endpoint URL", "")
		for endpoint == "" {
			fmt.Println("  an endpoint is required.")
			endpoint = ask("Endpoint URL", "")
		}
		key := ask("Access key (blank if none)", "")

		fmt.Print("  detecting available model… ")
		base, models, err := provider.DiscoverModels(endpoint, key)
		model := ""
		switch {
		case err != nil || len(models) == 0:
			fmt.Printf("%scould not detect%s (%s)\n", cYellow, cReset, errText(err))
			model = ask("  Model id to use", "")
			base = endpoint
		case len(models) == 1:
			model = models[0]
			fmt.Printf("%sfound %q%s\n", cGreen, model, cReset)
		default:
			fmt.Printf("%sfound: %s%s\n", cGreen, strings.Join(models, ", "), cReset)
			model = ask("  Which model", models[0])
		}

		c.Executors = append(c.Executors, config.Executor{
			Name: name, BaseURL: base, APIKey: key, Model: model,
		})

		if !strings.HasPrefix(strings.ToLower(ask("Add another executor? (y/N)", "n")), "y") {
			break
		}
	}

	if err := config.Save(c); err != nil {
		return c, err
	}
	p, _ := config.Path()
	c.ApplyDefaults()
	fmt.Printf("\n%s✓ Saved to %s%s\n\n", cGreen, p, cReset)
	return c, nil
}

func errText(err error) string {
	if err == nil {
		return "no models returned"
	}
	return err.Error()
}

// ---- config command -------------------------------------------------------

func cmdConfig(argv []string) {
	sub := ""
	if len(argv) > 0 {
		sub = argv[0]
	}
	switch sub {
	case "", "edit", "setup":
		if _, err := runWizard(); err != nil {
			fatal(err)
		}
	case "path":
		p, err := config.Path()
		if err != nil {
			fatal(err)
		}
		fmt.Println(p)
	case "show":
		c, err := config.Load()
		if err != nil {
			fatal(err)
		}
		redact(&c)
		fmt.Printf("Orchestrator: %s (%s)\n", c.Orchestrator.Model, c.Orchestrator.Provider)
		fmt.Println("Executors:")
		for _, e := range c.Executors {
			fmt.Printf("  - %s  model=%s  endpoint=%s\n", e.Name, e.Model, e.BaseURL)
		}
	default:
		fatal(fmt.Errorf("unknown config subcommand %q (use show|path|edit)", sub))
	}
}

func redact(c *config.Config) {
	if c.Orchestrator.APIKey != "" {
		c.Orchestrator.APIKey = "••••"
	}
	for i := range c.Executors {
		if c.Executors[i].APIKey != "" {
			c.Executors[i].APIKey = "••••"
		}
	}
}

// ---- run ------------------------------------------------------------------

func cmdRun(argv []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workdir := fs.String("workdir", ".", "workspace directory the agents operate in")
	isolate := fs.Bool("isolate", true, "run each parallel subtask in its own git worktree (if workdir is a git repo)")
	_ = fs.Parse(argv)

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fatal(fmt.Errorf(`no task provided — try: cheep run "fix the failing test"`))
	}

	cfg := ensureConfig()
	orch, err := orchestrator.Build(cfg, *workdir, *isolate, printer())
	if err != nil {
		fatal(err)
	}

	fmt.Println("──────────── cheep ────────────")
	r := orch.Run(task)
	fmt.Println("──────────── done ────────────")
	fmt.Printf("Status: %s\n", r.Status)
	fmt.Printf("Orchestrator (%s) tokens: in=%d out=%d\n\n", cfg.Orchestrator.Model, r.InputTokens, r.OutputTokens)
	fmt.Println(r.Output)
}

// ---- check ----------------------------------------------------------------

func cmdCheck() {
	cfg := ensureConfig()

	fmt.Printf("Orchestrator  %-22s ", cfg.Orchestrator.Model)
	if cfg.Orchestrator.APIKey == "" {
		fmt.Printf("%sno API key (set ANTHROPIC_API_KEY or run `cheep config`)%s\n", cRed, cReset)
	} else {
		ap := provider.NewAnthropic(cfg.Orchestrator.APIKey, 16)
		ping(ap, cfg.Orchestrator.Model)
	}

	for _, e := range cfg.Executors {
		label := fmt.Sprintf("%s (%s)", e.Name, e.Model)
		fmt.Printf("Executor      %-22s ", label)
		ep := provider.NewOpenAI(e.BaseURL, e.APIKey, 16)
		ping(ep, e.Model)
	}
}

func ping(p core.Provider, model string) {
	_, err := p.Complete(model, "Reply with: ok", []core.Message{{Role: "user", Text: "ok"}}, nil)
	if err != nil {
		fmt.Printf("%sunreachable: %v%s\n", cRed, err, cReset)
		return
	}
	fmt.Printf("%sreachable%s\n", cGreen, cReset)
}

// ---- live output ----------------------------------------------------------

func printer() core.EventFunc {
	var mu sync.Mutex // executors run concurrently; serialize their output
	return func(e core.Event) {
		mu.Lock()
		defer mu.Unlock()
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
