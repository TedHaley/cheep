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

// Agent describes one model endpoint (orchestrator or executor).
type Agent struct {
	Name        string `json:"name,omitempty"`     // executors only
	Provider    string `json:"provider"`           // "anthropic" | "openai"
	Endpoint    string `json:"endpoint,omitempty"` // base URL; blank ok for Anthropic
	APIKey      string `json:"api_key,omitempty"`
	Model       string `json:"model"`
	MaxTurns    int    `json:"max_turns,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty"` // executor: stop a run past this many input tokens

	// Orchestrator self-compaction: when its estimated context exceeds this many
	// tokens, older history is summarized and replaced. 0 picks a default.
	ContextBudget int `json:"context_budget,omitempty"`

	// Executor supervision.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"` // wall-clock abort per run
	MaxResumes     int `json:"max_resumes,omitempty"`     // resume-with-summary attempts
}

type Config struct {
	Orchestrator Agent                 `json:"orchestrator"`
	Executors    []Agent               `json:"executors"`
	MCP          map[string]mcp.Server `json:"mcp,omitempty"` // name -> MCP server
	KeepTabs     bool                  `json:"keep_tabs,omitempty"`
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
	if o.ContextBudget == 0 {
		o.ContextBudget = 120000 // est. tokens; self-compaction trigger
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
