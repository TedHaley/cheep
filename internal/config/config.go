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
)

// Agent describes one model endpoint (orchestrator or executor).
type Agent struct {
	Name        string `json:"name,omitempty"`     // executors only
	Provider    string `json:"provider"`           // "anthropic" | "openai"
	Endpoint    string `json:"endpoint,omitempty"` // base URL; blank ok for Anthropic
	APIKey      string `json:"api_key,omitempty"`
	Model       string `json:"model"`
	MaxTurns    int    `json:"max_turns,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty"`
}

type Config struct {
	Orchestrator Agent   `json:"orchestrator"`
	Executors    []Agent `json:"executors"`
}

func Dir() (string, error) {
	if p := os.Getenv("CHEEP_CONFIG"); p != "" {
		return filepath.Dir(p), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cheep"), nil
}

// Path is the config file location. Override it with CHEEP_CONFIG to keep
// multiple profiles (e.g. a local-only setup vs a Claude-orchestrated one).
func Path() (string, error) {
	if p := os.Getenv("CHEEP_CONFIG"); p != "" {
		return p, nil
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
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
		if e.Provider == "anthropic" && e.APIKey == "" {
			e.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
	}
}

func Save(c Config) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "config.json"), b, 0o600)
}
