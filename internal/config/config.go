// Package config is cheep's on-disk configuration.
//
// Both the orchestrator and the executors are described the same way: an
// endpoint, an access key, and a model. cheep does not care how the model behind
// an endpoint is served. Either role can be an Anthropic endpoint or any
// OpenAI-compatible endpoint. If no executors are configured, the orchestrator
// does the work itself.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/TedHaley/cheep/internal/mcp"
)

// WindowLookup is wired to pricing.Window at startup (main), letting
// ApplyDefaults auto-fill a model's context window from the LiteLLM dataset
// without config importing pricing (which imports config). nil = no auto-fill.
var WindowLookup func(model string) (int, bool)

func lookupWindow(model string) (int, bool) {
	if WindowLookup == nil {
		return 0, false
	}
	return WindowLookup(model)
}

// Agent describes one model endpoint (orchestrator or executor).
type Agent struct {
	Name        string `json:"name,omitempty"`     // executors only
	Provider    string `json:"provider"`           // "anthropic" | "openai"
	Endpoint    string `json:"endpoint,omitempty"` // base URL; blank ok for Anthropic
	APIKey      string `json:"api_key,omitempty"`
	Model       string `json:"model"`
	MaxTurns    int    `json:"max_turns,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty"` // executor: stop a run past this many input tokens

	// ContextWindow is the model's real context size in tokens. When known it
	// drives self-compaction (compact at ~75% of it) and per-agent context
	// gauges, so a small local window never surprises the run. 0 = unknown.
	ContextWindow int `json:"context_window,omitempty"`

	// ContextBudget is the self-compaction trigger: when estimated context
	// exceeds this many tokens, older history is summarized and replaced.
	// 0 derives from ContextWindow (or a default). Applies to both roles.
	ContextBudget int `json:"context_budget,omitempty"`

	// Executor supervision.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"` // wall-clock abort per run
	MaxResumes     int `json:"max_resumes,omitempty"`     // resume-with-summary attempts

	// Optional cost overrides for the spend estimate, in USD per 1M tokens.
	// When unset, cheep uses a built-in table (and treats local models as free).
	PriceIn  float64 `json:"price_in,omitempty"`
	PriceOut float64 `json:"price_out,omitempty"`
}

type Config struct {
	Orchestrator Agent                 `json:"orchestrator"`
	Executors    []Agent               `json:"executors"`
	MCP          map[string]mcp.Server `json:"mcp,omitempty"` // name -> MCP server
	KeepTabs     bool                  `json:"keep_tabs,omitempty"`

	// MouseOff releases mouse capture in the full-screen UI so terminal-native
	// text selection works (scroll with PgUp/PgDn). Default: capture on, the
	// wheel scrolls the focused tab. Toggle live (and persist) with /mouse.
	MouseOff bool `json:"mouse_off,omitempty"`

	// Inline opts out of the full-screen tab UI: the conversation prints into
	// the terminal's own scrollback (Claude Code style) with native scrolling
	// and selection, executor output prefixed instead of tabbed.
	Inline bool `json:"inline,omitempty"`

	// DisableEscalate turns off cheap-first escalation (retrying a failed subtask
	// on a more capable executor). Escalation is on by default.
	DisableEscalate bool `json:"disable_escalate,omitempty"`

	// DisablePool turns off pooled worktree reuse; every subtask then gets a
	// fresh temporary worktree (pre-pool behavior).
	DisablePool bool `json:"disable_pool,omitempty"`

	// BudgetUSD caps estimated session spend in US dollars (0 = no cap). cheep
	// warns at 80% and stops the run at 100%.
	BudgetUSD float64 `json:"budget_usd,omitempty"`

	// Budgets caps spend per project, keyed by absolute working directory. A
	// project entry overrides BudgetUSD; otherwise BudgetUSD applies everywhere.
	Budgets map[string]float64 `json:"budgets,omitempty"`

	// PiExtensions are pi coding-agent extensions (https://pi.dev) to run via
	// the bundled Node bridge: npm package names installed with `cheep pi add`,
	// or local paths. Their registered tools join the agents' tool set like any
	// MCP server's.
	PiExtensions []string `json:"pi_extensions,omitempty"`

	// NoMistakes is firstmate's strictest safety mode: every shared write and
	// shell command asks first, and NOTHING merges into the local checkout
	// until the user approves the branch diff. Headless runs (no approver)
	// keep all work on branches.
	NoMistakes bool `json:"no_mistakes,omitempty"`

	// Delivery is how validated worktree changes land: "merge" (default) merges
	// them into the local checkout; "pr" pushes each subtask branch and opens a
	// pull request with the gh CLI instead.
	Delivery string `json:"delivery,omitempty"`

	// ApprovalMode gates risky tool calls in the shared workspace:
	// "yolo" (nothing), "auto" (file writes ask; default), "approve"
	// (writes and shell commands ask). Worktree work is never gated.
	ApprovalMode string `json:"approval_mode,omitempty"`

	// Validation configures the pre-merge pipeline run on each delegated
	// subtask's worktree (checks from AGENTS.md '## Validation', then a
	// fresh-context review). Zero value = enabled with defaults.
	Validation Validation `json:"validation,omitempty"`

	// Reviewer optionally overrides which model reviews branch diffs during
	// validation; nil uses the orchestrator's provider/model.
	Reviewer *Agent `json:"reviewer,omitempty"`
}

// Validation configures the pre-merge validation pipeline.
type Validation struct {
	// Disable skips the whole pipeline (pre-validation behavior).
	Disable bool `json:"disable,omitempty"`
	// MaxFixRounds bounds repair loops per subtask (default 2).
	MaxFixRounds int `json:"max_fix_rounds,omitempty"`
	// SkipReview keeps the checks but drops the reviewer agent (cheaper).
	SkipReview bool `json:"skip_review,omitempty"`
	// Strict makes reviewer failures (unparseable verdict, errors) block the
	// merge instead of falling open.
	Strict bool `json:"strict,omitempty"`
}

// Budget resolves the active budget for a working directory: the per-project
// entry if set, else the global BudgetUSD.
func (c Config) Budget(workdir string) float64 {
	if v, ok := c.Budgets[workdir]; ok {
		return v
	}
	return c.BudgetUSD
}

// Home is cheep's root directory (~/.cheep by default; override with CHEEP_HOME).
// It holds config.json and keys.env.
func Home() (string, error) {
	if h := os.Getenv("CHEEP_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cheep"), nil
}

func Dir() (string, error) {
	if p := os.Getenv("CHEEP_CONFIG"); p != "" {
		return filepath.Dir(p), nil
	}
	return Home()
}

// Path is the config file location. Override it with CHEEP_CONFIG to keep
// multiple profiles (e.g. a local-only setup vs a Claude-orchestrated one).
func Path() (string, error) {
	if p := os.Getenv("CHEEP_CONFIG"); p != "" {
		return p, nil
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "config.json"), nil
}

// KeysPath is the central key store (~/.cheep/keys.env).
func KeysPath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "keys.env"), nil
}

// Init prepares the cheep home: migrates any legacy config and loads keys.env
// into the environment. Call once at startup.
func Init() {
	migrateLegacy()
	loadKeys()
}

// loadKeys reads keys.env (KEY=value per line) and sets each into the
// environment, without overwriting variables already set in the shell.
func loadKeys() {
	p, err := KeysPath()
	if err != nil {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// migrateLegacy copies a config.json from the old location
// (UserConfigDir/cheep) into the cheep home, once, if the new one is absent.
func migrateLegacy() {
	if os.Getenv("CHEEP_CONFIG") != "" {
		return
	}
	np, err := Path()
	if err != nil {
		return
	}
	if _, err := os.Stat(np); err == nil {
		return // new config already present
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(base, "cheep", "config.json"))
	if err != nil {
		return
	}
	if h, err := Home(); err == nil {
		_ = os.MkdirAll(h, 0o700)
		_ = os.WriteFile(np, b, 0o600)
	}
}

// EnsureKeysTemplate creates keys.env with a starter template if it is missing,
// and returns its path.
func EnsureKeysTemplate() (string, error) {
	p, err := KeysPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if h, err := Home(); err == nil {
		_ = os.MkdirAll(h, 0o700)
	}
	tmpl := "# cheep keys — one KEY=value per line. Loaded into the environment on startup.\n" +
		"# Anything you set in your shell takes precedence over these.\n\n" +
		"# Anthropic / Claude (orchestrator):\n" +
		"# ANTHROPIC_API_KEY=sk-ant-...\n"
	return p, os.WriteFile(p, []byte(tmpl), 0o600)
}

// SetKey writes name=value to keys.env (replacing any existing entry) and sets it
// in the current process environment, so it takes effect without a restart.
func SetKey(name, value string) error {
	p, err := KeysPath()
	if err != nil {
		return err
	}
	var kept []string
	if b, e := os.ReadFile(p); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "#") {
				body := strings.TrimPrefix(trimmed, "export ")
				if k, _, ok := strings.Cut(body, "="); ok && strings.TrimSpace(k) == name {
					continue // drop the old entry
				}
			}
			kept = append(kept, line)
		}
	}
	kept = append(kept, name+"="+value)
	if h, e := Home(); e == nil {
		_ = os.MkdirAll(h, 0o700)
	}
	if err := os.WriteFile(p, []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Setenv(name, value)
}

func Exists() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func Load() (Config, error) {
	var c Config
	p, err := Path()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	c.ApplyDefaults()
	return c, nil
}

// ApplyDefaults fills in zero values. An Anthropic role with no key falls back to
// the ANTHROPIC_API_KEY env var.
func (c *Config) ApplyDefaults() {
	o := &c.Orchestrator
	if o.Provider == "" {
		o.Provider = "anthropic"
	}
	if o.Model == "" && o.Provider == "anthropic" {
		o.Model = "claude-sonnet-4-6"
	}
	if o.MaxTurns == 0 {
		o.MaxTurns = 30
	}
	if o.ContextWindow == 0 {
		if w, ok := lookupWindow(o.Model); ok {
			o.ContextWindow = w
		}
	}
	if o.ContextBudget == 0 {
		if o.ContextWindow > 0 {
			o.ContextBudget = o.ContextWindow * 3 / 4
		} else {
			o.ContextBudget = 120000 // est. tokens; self-compaction trigger
		}
	}
	if o.Provider == "anthropic" && o.APIKey == "" {
		o.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	for i := range c.Executors {
		e := &c.Executors[i]
		if e.Provider == "" {
			e.Provider = "openai"
		}
		if e.MaxTurns == 0 {
			e.MaxTurns = 20
		}
		if e.ContextWindow == 0 {
			if w, ok := lookupWindow(e.Model); ok {
				e.ContextWindow = w
			}
		}
		// Self-compact well before the window so a chunk gets summarized and
		// continued instead of dying; keep the hard stop just under the window.
		if e.ContextWindow > 0 {
			if e.ContextBudget == 0 {
				e.ContextBudget = e.ContextWindow * 3 / 4
			}
			if e.TokenBudget == 0 {
				e.TokenBudget = e.ContextWindow * 95 / 100
			}
		}
		if e.TokenBudget == 0 {
			e.TokenBudget = 100000
		}
		if e.TimeoutSeconds == 0 {
			e.TimeoutSeconds = 300
		}
		if e.MaxResumes == 0 {
			e.MaxResumes = 2
		}
		if e.Provider == "anthropic" && e.APIKey == "" {
			e.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
	}
}

func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
