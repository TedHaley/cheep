// Package config is cheep's on-disk configuration: which model orchestrates,
// and which executor endpoints do the work.
//
// Executors are described purely by endpoint + access key — cheep does not care
// (or ask) how the model behind an endpoint is served. The model each executor
// runs is detected from the endpoint and recorded so the orchestrator can route
// work to the most suitable one.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Orchestrator struct {
	Provider string `json:"provider"` // currently always "anthropic"
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	MaxTurns int    `json:"max_turns,omitempty"`
}

type Executor struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key,omitempty"`
	Model       string `json:"model,omitempty"` // detected at setup time
	MaxTurns    int    `json:"max_turns,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty"`
}

type Config struct {
	Orchestrator Orchestrator `json:"orchestrator"`
	Executors    []Executor   `json:"executors"`
}

// Dir is the cheep config directory (created lazily on Save).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cheep"), nil
}

func Path() (string, error) {
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

// ApplyDefaults fills in zero values and falls back to the ANTHROPIC_API_KEY env
// var when the orchestrator key was left blank in the file.
func (c *Config) ApplyDefaults() {
	if c.Orchestrator.Provider == "" {
		c.Orchestrator.Provider = "anthropic"
	}
	if c.Orchestrator.Model == "" {
		c.Orchestrator.Model = "claude-sonnet-4-6"
	}
	if c.Orchestrator.MaxTurns == 0 {
		c.Orchestrator.MaxTurns = 30
	}
	if c.Orchestrator.APIKey == "" {
		c.Orchestrator.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	for i := range c.Executors {
		if c.Executors[i].MaxTurns == 0 {
			c.Executors[i].MaxTurns = 20
		}
		if c.Executors[i].TokenBudget == 0 {
			c.Executors[i].TokenBudget = 100000
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
