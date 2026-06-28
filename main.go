// Command cheep is an interactive multi-agent coding shell. A lead "orchestrator"
// agent plans and verifies; optional "executor" agents do the work in parallel.
// Any role can point at an Anthropic or OpenAI-compatible endpoint.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/orchestrator"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/tui"
	"github.com/TedHaley/cheep/internal/worktree"

	"golang.org/x/term"
)

// version is stamped at build time by GoReleaser (-X main.version=...).
var version = "dev"

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cCyan   = "\033[1;36m"
	cGreen  = "\033[1;32m"
	cYellow = "\033[33m"
	cDim    = "\033[2m"
	cRed    = "\033[1;31m"
)

const bannerArt = `
 ██████╗██╗  ██╗███████╗███████╗██████╗ ██╗
██╔════╝██║  ██║██╔════╝██╔════╝██╔══██╗██║
██║     ███████║█████╗  █████╗  ██████╔╝██║
██║     ██╔══██║██╔══╝  ██╔══╝  ██╔═══╝ ╚═╝
╚██████╗██║  ██║███████╗███████╗██║     ██╗
 ╚═════╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝     ╚═╝`

func main() {
	config.Init() // migrate legacy config + load ~/.cheep/keys.env into the env
	if len(os.Args) < 2 {
		cmdChat()
		return
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "check":
		cmdCheck()
	case "config":
		cmdConfig(os.Args[2:])
	case "setup":
		cmdSetup()
	case "keys":
		cmdKeys()
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
	fmt.Print(`cheep — an interactive multi-agent coding shell.

Usage:
  cheep                          start the interactive shell (default)
  cheep run "<task>" [--workdir] run a single task non-interactively
  cheep check                    ping the orchestrator and every executor
  cheep config [show|path]       set up or inspect your agents (manual wizard)
  cheep setup                    configure agents by chatting with a working one
  cheep keys                     show the key store (~/.cheep/keys.env)
  cheep version                  print the version

On first use, cheep walks you through choosing an orchestrator and, optionally,
one or more executors. Configuration lives in a single JSON file (cheep config path).
`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "%scheep: %v%s\n", cRed, err, cReset)
	os.Exit(1)
}

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

// ---- interactive shell ----------------------------------------------------

func cmdChat() {
	cfg := ensureConfig()
	workdir, _ := os.Getwd()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if err := tui.Run(cfg, workdir); err != nil {
			fatal(err)
		}
		return
	}
	lineREPL(cfg, workdir)
}

// lineREPL is the non-interactive fallback (piped stdin / no TTY): a simple
// line-based loop with slash commands. The rich tabbed view is in package tui.
func lineREPL(cfg config.Config, workdir string) {
	onEvent := printer()
	mode := orchestrator.ModeAuto
	var session *agent.Session
	var buildErr error
	rebuild := func(keepHistory bool) {
		var hist []core.Message
		if keepHistory && session != nil {
			hist = session.History()
		}
		orch, err := orchestrator.Build(cfg, workdir, true, mode, onEvent)
		buildErr = err
		if err != nil {
			session = nil
			return
		}
		session = orch.Resume(hist)
	}
	setMode := func(m orchestrator.Mode) {
		mode = m
		rebuild(true)
		fmt.Printf("%smode: %s%s\n", cDim, mode, cReset)
	}

	printStatus(cfg, workdir)
	rebuild(false)
	if buildErr != nil {
		fmt.Printf("%s%v%s\n", cYellow, buildErr, cReset)
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("\n%s%s ›%s ", cBold, mode, cReset)
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			switch fields := strings.Fields(line); fields[0] {
			case "/exit", "/quit", "/q":
				return
			case "/help", "/?":
				fmt.Print(`commands:
  /chat /plan /auto   switch mode (talk · investigate+plan · do the work)
  /mode               cycle chat → plan → auto
  /setup              configure the other service by chatting with a working one
  /config             run the manual setup wizard
  /status             show the current setup
  /clear              start a fresh conversation
  /exit               quit
`)
			case "/chat":
				setMode(orchestrator.ModeChat)
			case "/plan":
				setMode(orchestrator.ModePlan)
			case "/auto":
				setMode(orchestrator.ModeAuto)
			case "/mode":
				setMode(orchestrator.NextMode(mode))
			case "/setup":
				if c, ok := runSetupAssistant(cfg, in); ok {
					cfg = c
					rebuild(true)
					printStatus(cfg, workdir)
				}
			case "/status":
				printStatus(cfg, workdir)
			case "/config":
				if c, err := runWizard(); err != nil {
					fmt.Printf("%sconfig: %v%s\n", cRed, err, cReset)
				} else {
					cfg = c
					rebuild(true)
					printStatus(cfg, workdir)
				}
			case "/clear":
				session = nil
				rebuild(false)
				fmt.Printf("%s(new conversation)%s\n", cDim, cReset)
			default:
				fmt.Printf("%sunknown command %q — try /help%s\n", cYellow, fields[0], cReset)
			}
			continue
		}

		if session == nil {
			fmt.Printf("%scan't start a session: %v%s\n", cYellow, buildErr, cReset)
			continue
		}
		r := session.Send(line)
		fmt.Printf("%s[%s · %s · %d turns · %d→%d tokens]%s\n",
			cDim, mode, r.Status, r.Turns, r.InputTokens, r.OutputTokens, cReset)
	}
}

func printStatus(cfg config.Config, workdir string) {
	lines := []string{
		"cheep " + version,
		"",
		fmt.Sprintf("orchestrator  %s  [%s]", cfg.Orchestrator.Model, cfg.Orchestrator.Provider),
	}
	if len(cfg.Executors) == 0 {
		lines = append(lines, "executors     (none — solo mode)")
	} else {
		for i, e := range cfg.Executors {
			label := "executors"
			if i > 0 {
				label = ""
			}
			lines = append(lines, fmt.Sprintf("%-13s %s → %s", label, e.Name, e.Model))
		}
	}
	git := ""
	if worktree.IsRepo(workdir) {
		git = "  (git: worktree isolation on)"
	}
	lines = append(lines, "", "workspace     "+workdir+git)
	fmt.Println(boxed(lines))
}

func boxed(lines []string) string {
	w := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > w {
			w = n
		}
	}
	var b strings.Builder
	b.WriteString("┌" + strings.Repeat("─", w+2) + "┐\n")
	for _, l := range lines {
		b.WriteString("│ " + l + strings.Repeat(" ", w-utf8.RuneCountInString(l)) + " │\n")
	}
	b.WriteString("└" + strings.Repeat("─", w+2) + "┘")
	return b.String()
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
	fmt.Println("── Orchestrator ──  the lead agent that plans, delegates and verifies")
	endpoint := ask("Orchestrator endpoint URL (blank = Anthropic / Claude)", "")
	if endpoint == "" || strings.Contains(endpoint, "anthropic") {
		c.Orchestrator.Provider = "anthropic"
		c.Orchestrator.Model = ask("Model", "claude-sonnet-4-6")
		kp := "API key"
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			kp += " (blank = ANTHROPIC_API_KEY)"
		}
		c.Orchestrator.APIKey = ask(kp, "")
	} else {
		key := ask("Access key (blank if none)", "")
		base, model := detectModel(endpoint, key, ask)
		c.Orchestrator.Provider = "openai"
		c.Orchestrator.Endpoint = base
		c.Orchestrator.Model = model
		c.Orchestrator.APIKey = key
	}

	fmt.Println()
	if strings.HasPrefix(strings.ToLower(ask("Add separate executor agents? (y/N)", "n")), "y") {
		fmt.Println("── Executors ──  cheap workers the orchestrator delegates to")
		for {
			fmt.Println()
			name := ask("Executor name", fmt.Sprintf("executor-%d", len(c.Executors)+1))
			ep := ask("Endpoint URL", "")
			for ep == "" {
				fmt.Println("  an endpoint is required.")
				ep = ask("Endpoint URL", "")
			}
			key := ask("Access key (blank if none)", "")
			base, model := detectModel(ep, key, ask)
			c.Executors = append(c.Executors, config.Agent{
				Name: name, Provider: "openai", Endpoint: base, APIKey: key, Model: model,
			})
			if !strings.HasPrefix(strings.ToLower(ask("Add another executor? (y/N)", "n")), "y") {
				break
			}
		}
	} else {
		fmt.Printf("%sSolo mode: the orchestrator will do everything itself.%s\n", cDim, cReset)
	}

	if err := config.Save(c); err != nil {
		return c, err
	}
	c.ApplyDefaults()
	p, _ := config.Path()
	fmt.Printf("\n%s✓ Saved to %s%s\n", cGreen, p, cReset)
	return c, nil
}

func detectModel(endpoint, key string, ask func(string, string) string) (base, model string) {
	fmt.Print("  detecting available model… ")
	resolved, models, err := provider.DiscoverModels(endpoint, key)
	switch {
	case err != nil || len(models) == 0:
		fmt.Printf("%scould not detect%s (%s)\n", cYellow, cReset, errText(err))
		return endpoint, ask("  Model id to use", "")
	case len(models) == 1:
		fmt.Printf("%sfound %q%s\n", cGreen, models[0], cReset)
		return resolved, models[0]
	default:
		fmt.Printf("%sfound: %s%s\n", cGreen, strings.Join(models, ", "), cReset)
		return resolved, ask("  Which model", models[0])
	}
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
		fmt.Printf("Orchestrator: %s [%s]", c.Orchestrator.Model, c.Orchestrator.Provider)
		if c.Orchestrator.Endpoint != "" {
			fmt.Printf(" @ %s", c.Orchestrator.Endpoint)
		}
		fmt.Println()
		if len(c.Executors) == 0 {
			fmt.Println("Executors: (none — solo mode)")
			return
		}
		fmt.Println("Executors:")
		for _, e := range c.Executors {
			fmt.Printf("  - %s  model=%s  endpoint=%s\n", e.Name, e.Model, e.Endpoint)
		}
	default:
		fatal(fmt.Errorf("unknown config subcommand %q (use show|path|edit)", sub))
	}
}

// ---- setup assistant ------------------------------------------------------

// runSetupAssistant lets the user chat with a reachable agent to configure the
// others. Returns the (reloaded) config and whether it was saved.
func runSetupAssistant(cfg config.Config, in *bufio.Reader) (config.Config, bool) {
	asst, st, label, err := configassist.Build(cfg, printer())
	if err != nil {
		fmt.Printf("%s%v%s\n", cYellow, err, cReset)
		return cfg, false
	}
	fmt.Printf("%sSetup assistant (powered by %s). Describe what to configure; /done when finished, /cancel to abort.%s\n",
		cDim, label, cReset)
	session := asst.NewSession()
	for {
		fmt.Print("\nsetup › ")
		line, err := in.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/done", "/exit":
			goto finish
		case "/cancel":
			return cfg, false
		}
		r := session.Send(line)
		fmt.Printf("%s[%s · %d turns]%s\n", cDim, r.Status, r.Turns, cReset)
	}
finish:
	if !st.Saved {
		fmt.Printf("%s(nothing saved)%s\n", cDim, cReset)
		return cfg, false
	}
	updated, err := config.Load()
	if err != nil {
		return st.Cfg, true
	}
	return updated, true
}

func cmdSetup() {
	if !config.Exists() {
		fmt.Println("Configure your first agent with `cheep config`; then `cheep setup` can help add the rest.")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	in := bufio.NewReader(os.Stdin)
	if _, ok := runSetupAssistant(cfg, in); ok {
		fmt.Printf("%s✓ configuration updated%s\n", cGreen, cReset)
	}
}

// ---- keys -----------------------------------------------------------------

func cmdKeys() {
	p, err := config.EnsureKeysTemplate()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Key store: %s\n", p)
	fmt.Println("Add one KEY=value per line; it's loaded into the environment on startup.")
	fmt.Println("For Claude, add:  ANTHROPIC_API_KEY=sk-ant-...")
}

// ---- one-shot run ---------------------------------------------------------

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
	orch, err := orchestrator.Build(cfg, *workdir, *isolate, orchestrator.ModeAuto, printer())
	if err != nil {
		fatal(err)
	}
	fmt.Println("──────────── cheep ────────────")
	r := orch.Run(task)
	fmt.Println("──────────── done ────────────")
	fmt.Printf("Status: %s | tokens in=%d out=%d\n", r.Status, r.InputTokens, r.OutputTokens)
}

// ---- check ----------------------------------------------------------------

func cmdCheck() {
	cfg := ensureConfig()

	fmt.Printf("Orchestrator  %-24s ", cfg.Orchestrator.Model)
	if cfg.Orchestrator.Provider == "anthropic" && cfg.Orchestrator.APIKey == "" {
		fmt.Printf("%sno API key (set ANTHROPIC_API_KEY or run `cheep config`)%s\n", cRed, cReset)
	} else {
		ping(provider.For(cfg.Orchestrator.Provider, cfg.Orchestrator.Endpoint, cfg.Orchestrator.APIKey, 16),
			cfg.Orchestrator.Model)
	}
	for _, e := range cfg.Executors {
		fmt.Printf("Executor      %-24s ", fmt.Sprintf("%s (%s)", e.Name, e.Model))
		ping(provider.For(e.Provider, e.Endpoint, e.APIKey, 16), e.Model)
	}
}

func ping(p core.Provider, model string) {
	if _, err := p.Complete(context.Background(), model, "Reply with: ok", []core.Message{{Role: "user", Text: "ok"}}, nil); err != nil {
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
		if e.Agent != "orchestrator" && e.Agent != "cheep" {
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
