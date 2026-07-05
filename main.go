// Command cheep is an interactive multi-agent coding shell. A lead "orchestrator"
// agent plans and verifies; optional "executor" agents do the work in parallel.
// Any role can point at an Anthropic or OpenAI-compatible endpoint.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/mcp"
	"github.com/TedHaley/cheep/internal/orchestrator"
	"github.com/TedHaley/cheep/internal/piext"
	"github.com/TedHaley/cheep/internal/pricing"
	"github.com/TedHaley/cheep/internal/project"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/tui"
	"github.com/TedHaley/cheep/internal/validate"
	"github.com/TedHaley/cheep/internal/worktree"

	"golang.org/x/term"
)

// version is stamped at build time by GoReleaser (-X main.version=...).
var version = "v0.0.1"

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
	config.Init()          // migrate legacy config + load ~/.cheep/keys.env into the env
	pricing.Load()         // load cached per-token prices (no network)
	pricing.MaybeRefresh() // refresh the price dataset in the background if stale
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
	case "worktree":
		cmdWorktree(os.Args[2:])
	case "init":
		cmdInit(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	case "pi":
		cmdPi(os.Args[2:])
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
  cheep run "<task>" [--workdir] [--json]
                                 run a single task non-interactively; --json
                                 streams JSONL events for scripting/CI
  cheep check                    ping the orchestrator and every executor
  cheep config [show|path]       set up or inspect your agents (manual wizard)
  cheep setup                    configure agents by chatting with a working one
  cheep keys                     show the key store (~/.cheep/keys.env)
  cheep init [--force] [--no-assist]
                                 set up the current project for agentic work:
                                 AGENTS.md (+ CLAUDE.md link), detected
                                 validation checks, git init, toolbelt skill
  cheep validate [--review] [--json]
                                 run the project's AGENTS.md '## Validation'
                                 checks (and optionally a fresh-context AI
                                 review of the branch diff); exits 0 on pass
  cheep worktree <acquire|release|list> [--json]
                                 manage the pooled git worktrees (toolbelt for
                                 other harnesses; cheep uses the pool itself)
  cheep pi <add|remove|list>     run pi coding-agent extensions (pi.dev): their
                                 registered tools join the agents' tool set
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

// startMCP launches configured MCP servers (plus the pi-extension bridge when
// pi_extensions is set) and returns their role-scoped tools + a session to
// close on exit. Errors are reported but never fatal.
func startMCP(cfg config.Config) (mcp.Tools, *mcp.Session) {
	servers := cfg.MCP
	if s, err := piext.Server(cfg.PiExtensions); err != nil {
		fmt.Fprintf(os.Stderr, "%spi extensions disabled: %v%s\n", cDim, err, cReset)
	} else if s != nil {
		merged := map[string]mcp.Server{}
		for k, v := range servers {
			merged[k] = v
		}
		name := "pi"
		if _, taken := merged[name]; taken {
			name = "pi_ext"
		}
		merged[name] = *s
		servers = merged
	}
	return mcp.Start(servers, func(e core.Event) {
		fmt.Fprintf(os.Stderr, "%s%s%s\n", cDim, e.Status, cReset)
	})
}

// cmdPi manages pi coding-agent extensions run through the bundled bridge.
func cmdPi(args []string) {
	usage := func() {
		fmt.Print(`cheep pi — run pi coding-agent extensions (pi.dev) inside cheep.

Usage:
  cheep pi add <npm-package|path>   install an extension and register it
  cheep pi remove <npm-package|path>
  cheep pi list                     show registered extensions

Only the TOOLS an extension registers cross the bridge; pi event hooks,
commands, and custom UI need pi's own runtime and are skipped.
`)
	}
	if len(args) == 0 {
		usage()
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			usage()
			os.Exit(2)
		}
		if err := piext.Add(args[1]); err != nil {
			fatal(err)
		}
		fmt.Printf("%s✓%s %s registered — its tools load on the next cheep start\n", cGreen, cReset, args[1])
	case "remove", "rm":
		if len(args) < 2 {
			usage()
			os.Exit(2)
		}
		if err := piext.Remove(args[1]); err != nil {
			fatal(err)
		}
		fmt.Printf("%s✓%s %s removed\n", cGreen, cReset, args[1])
	case "list", "ls":
		cfg, err := config.Load()
		if err != nil {
			fatal(err)
		}
		if len(cfg.PiExtensions) == 0 {
			fmt.Println("no pi extensions registered — add one with `cheep pi add <npm-package>`")
			return
		}
		for _, e := range cfg.PiExtensions {
			fmt.Println("  " + e)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func cmdChat() {
	workdir, _ := os.Getwd()
	tty := term.IsTerminal(int(os.Stdin.Fd()))

	// On a TTY with no config, launch the TUI straight into its discovery
	// configurator instead of the line-based wizard.
	if tty {
		firstRun := !config.Exists()
		var cfg config.Config
		if !firstRun {
			c, err := config.Load()
			if err != nil {
				fatal(fmt.Errorf("reading config: %w", err))
			}
			cfg = c
		}
		mt, mcpSess := startMCP(cfg)
		defer mcpSess.Close()
		if err := tui.Run(cfg, workdir, version, mt.Orchestrator, mt.Executor, firstRun); err != nil {
			fatal(err)
		}
		return
	}

	cfg := ensureConfig() // no TTY: fall back to the line wizard + REPL
	mt, mcpSess := startMCP(cfg)
	defer mcpSess.Close()
	lineREPL(cfg, workdir, mt.Orchestrator, mt.Executor)
}

// lineREPL is the non-interactive fallback (piped stdin / no TTY): a simple
// line-based loop with slash commands. The rich tabbed view is in package tui.
func lineREPL(cfg config.Config, workdir string, extraOrch, extraExec []core.Tool) {
	onEvent := printer()
	mode := orchestrator.ModeAuto
	var session *agent.Session
	var buildErr error
	rebuild := func(keepHistory bool) {
		var hist []core.Message
		if keepHistory && session != nil {
			hist = session.History()
		}
		orch, err := orchestrator.Build(cfg, workdir, orchestrator.Options{
			Isolate: true, Mode: mode, ExtraOrch: extraOrch, ExtraExec: extraExec, OnEvent: onEvent,
		})
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
			case "/loop":
				setMode(orchestrator.ModeLoop)
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
	asJSON := fs.Bool("json", false, "emit JSONL events on stdout (human chrome goes to stderr)")
	// Go's flag package stops at the first positional, but the documented
	// shape is `cheep run "<task>" [--flags]` — so partition args ourselves.
	var flags, positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if name == "workdir" && !strings.Contains(a, "=") && i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	_ = fs.Parse(flags)

	task := strings.TrimSpace(strings.Join(positional, " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, `cheep: no task provided — try: cheep run "fix the failing test"`)
		os.Exit(2)
	}

	cfg := ensureConfig()
	mt, mcpSess := startMCP(cfg)
	defer mcpSess.Close()

	// --json: one {"type":"event",...} line per core.Event on stdout, then a
	// final {"type":"result",...} line. Treat this shape as a public API.
	onEvent := printer()
	if *asJSON {
		var mu sync.Mutex
		enc := json.NewEncoder(os.Stdout)
		onEvent = func(e core.Event) {
			mu.Lock()
			defer mu.Unlock()
			_ = enc.Encode(map[string]any{"type": "event", "ts": time.Now().UTC().Format(time.RFC3339), "event": e})
		}
	}

	orch, err := orchestrator.Build(cfg, *workdir, orchestrator.Options{
		Isolate: *isolate, Mode: orchestrator.ModeAuto, ExtraOrch: mt.Orchestrator, ExtraExec: mt.Executor, OnEvent: onEvent,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%scheep: %v%s\n", cRed, err, cReset)
		os.Exit(2)
	}
	if !*asJSON {
		fmt.Println("──────────── cheep ────────────")
	}
	r := orch.Run(task)
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type": "result", "status": r.Status, "output": r.Output, "turns": r.Turns,
			"input_tokens": r.InputTokens, "output_tokens": r.OutputTokens,
		})
	} else {
		fmt.Println("──────────── done ────────────")
		fmt.Printf("Status: %s | tokens in=%d out=%d\n", r.Status, r.InputTokens, r.OutputTokens)
	}
	if r.Status != "completed" {
		os.Exit(1)
	}
}

// ---- init -------------------------------------------------------------------

// cmdInit is the project launchpad: deterministic scaffolding first (AGENTS.md
// with detected checks, CLAUDE.md symlink, toolbelt skill, git init), then an
// optional agent-assisted pass that explores the repo and rewrites the
// template's prose sections to be accurate.
func cmdInit(argv []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "project directory")
	force := fs.Bool("force", false, "overwrite an existing AGENTS.md")
	noAssist := fs.Bool("no-assist", false, "skip the agent-assisted rewrite of AGENTS.md")
	_ = fs.Parse(argv)
	tty := term.IsTerminal(int(os.Stdin.Fd()))

	if !worktree.IsRepo(*dir) {
		ok := true
		if tty {
			fmt.Print("Not a git repository — run `git init`? [Y/n] ")
			var ans string
			fmt.Scanln(&ans)
			ok = ans == "" || strings.HasPrefix(strings.ToLower(ans), "y")
		}
		if ok {
			if out, err := exec.Command("git", "-C", *dir, "init").CombinedOutput(); err != nil {
				fatal(fmt.Errorf("git init: %s", strings.TrimSpace(string(out))))
			}
			fmt.Println("initialized git repository")
		}
	}

	detected := project.DetectCommands(*dir)
	wrote, err := project.Scaffold(*dir, detected, *force)
	if err != nil {
		fatal(err)
	}
	for _, w := range wrote {
		fmt.Println("wrote", w)
	}
	if len(wrote) == 0 {
		fmt.Println("nothing to scaffold (files exist — use --force to regenerate AGENTS.md)")
	}

	if *noAssist || !tty {
		fmt.Println("\nNext: fill in AGENTS.md's Overview and Conventions, and verify the checks under '## Validation'.")
		return
	}

	// Agent-assisted pass: a solo agent (executors stripped — this is quick
	// investigative work, not a fleet job) explores the repo and rewrites the
	// template prose. Reuses the normal Build machinery end to end.
	fmt.Println("\nExploring the project to fill in AGENTS.md (ctrl-c to skip)…")
	cfg := ensureConfig()
	cfg.Executors = nil
	orch, err := orchestrator.Build(cfg, *dir, orchestrator.Options{
		Mode: orchestrator.ModeAuto, OnEvent: printer(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%sskipping assisted pass: %v%s\n", cYellow, err, cReset)
		return
	}
	r := orch.Run(`Explore this repository (read key files, directory layout, build/config files),
then rewrite AGENTS.md so its prose matches reality:
- Replace the Overview comment with 2-4 sentences on what the project is and how it's structured.
- Make "## Build & Run" accurate, including any required setup.
- Verify each command in "## Validation" actually works here; fix or remove wrong ones and keep
  the fenced check-block format exactly (they are machine-parsed).
- Note real conventions you can see in the code under "## Conventions".
- Keep "## Lessons" and the overall section structure intact.
Do not invent facts you cannot verify from the files.`)
	if r.Status != "completed" {
		fmt.Fprintf(os.Stderr, "%sassisted pass ended: %s%s\n", cYellow, r.Status, cReset)
	}
	fmt.Println("\nProject initialized. Review AGENTS.md, then run `cheep validate` to confirm the checks.")
}

// ---- validate ---------------------------------------------------------------

// cmdValidate runs the validation pipeline headlessly against the current
// directory: the AGENTS.md '## Validation' checks, plus (with --review) the
// same fresh-context reviewer cheep uses before merging subtask worktrees.
// Exit codes: 0 pass, 1 fail, 2 usage/config error. Other harnesses shell out
// to this — keep the --json shape stable.
func cmdValidate(argv []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to validate")
	review := fs.Bool("review", false, "also run a fresh-context AI review of the diff against --base")
	base := fs.String("base", "", "ref to diff against for --review (default: merge-base with the default branch)")
	asJSON := fs.Bool("json", false, "machine-readable result")
	_ = fs.Parse(argv)

	proj := project.Load(*dir)
	if len(proj.Checks) == 0 && !*review {
		fmt.Fprintln(os.Stderr, "cheep: no checks declared — add a '## Validation' section to AGENTS.md (see cheep init)")
		os.Exit(2)
	}

	runner := validate.Runner{Checks: proj.Checks}
	baseRef := *base
	if *review {
		cfg := ensureConfig()
		runner.Reviewer = orchestrator.NewReviewer(cfg, printer())
		runner.Strict = cfg.Validation.Strict
		if runner.Reviewer == nil {
			fmt.Fprintln(os.Stderr, "cheep: no usable reviewer model configured")
			os.Exit(2)
		}
		if baseRef == "" {
			baseRef = defaultBase(*dir)
			if baseRef == "" {
				fmt.Fprintln(os.Stderr, "cheep: cannot determine a base ref for --review; pass --base")
				os.Exit(2)
			}
		}
	}
	if !*asJSON {
		runner.OnEvent = printer()
	}

	res := runner.Run(context.Background(), *dir, baseRef)
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, c := range res.Checks {
			mark := "✓"
			if c.Exit != 0 {
				mark = "✗"
			}
			fmt.Printf("%s %s (exit %d)\n", mark, c.Name, c.Exit)
		}
		if res.Review != nil {
			fmt.Printf("review: %s %s\n", res.Review.Verdict, res.Review.Summary)
			for _, i := range res.Review.Issues {
				fmt.Printf("  - [%s] %s %s\n", i.Severity, i.Description, i.File)
			}
		}
		if res.Note != "" {
			fmt.Println("note:", res.Note)
		}
	}
	if !res.Passed {
		os.Exit(1)
	}
}

// defaultBase finds the merge-base of HEAD with the repo's default branch
// (origin/HEAD, falling back to main/master), or "" when none applies.
func defaultBase(dir string) string {
	try := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	for _, ref := range []string{try("symbolic-ref", "--short", "refs/remotes/origin/HEAD"), "main", "master"} {
		if ref == "" {
			continue
		}
		if mb := try("merge-base", "HEAD", ref); mb != "" {
			return mb
		}
	}
	return ""
}

// ---- worktree ---------------------------------------------------------------

// cmdWorktree is the headless pool interface, usable by other harnesses:
//
//	cheep worktree acquire --name fix-login [--json]   → prints the worktree path
//	cheep worktree release --path <p> [--landed]
//	cheep worktree list [--json]
//
// CLI acquires hold a lease in the slot's state file (a process that exits
// can't hold a flock); release it with `cheep worktree release`.
func cmdWorktree(argv []string) {
	if len(argv) == 0 {
		fatal(fmt.Errorf("usage: cheep worktree <acquire|release|list> [flags]"))
	}
	sub, rest := argv[0], argv[1:]
	fs := flag.NewFlagSet("worktree", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository the pool belongs to")
	name := fs.String("name", "task", "short task name used in the branch name")
	path := fs.String("path", "", "slot path (release)")
	landed := fs.Bool("landed", false, "the slot's work is merged/landed (release)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(rest)

	pool, err := worktree.OpenPool(*repo)
	if err != nil {
		fatal(err)
	}
	switch sub {
	case "acquire":
		t, err := pool.Acquire(*name, int(time.Now().Unix()%100000), true)
		if err != nil {
			fatal(err)
		}
		if *asJSON {
			b, _ := json.Marshal(map[string]string{"path": t.Path, "branch": t.Branch, "base": t.Base})
			fmt.Println(string(b))
		} else {
			fmt.Println(t.Path)
		}
	case "release":
		if *path == "" {
			fatal(fmt.Errorf("release needs --path"))
		}
		if err := pool.ReleaseByPath(*path, *landed); err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "released")
	case "list":
		slots := pool.List()
		if *asJSON {
			b, _ := json.MarshalIndent(slots, "", "  ")
			fmt.Println(string(b))
			return
		}
		if len(slots) == 0 {
			fmt.Println("no slots yet")
			return
		}
		for _, s := range slots {
			fmt.Printf("%-12s %-30s %s\n", s.State, s.Branch, s.Path)
		}
	default:
		fatal(fmt.Errorf("unknown worktree subcommand %q", sub))
	}
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
