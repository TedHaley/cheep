// Package config loads settings from environment variables with sensible
// defaults: Claude (Sonnet) as the orchestrator, a local Qwen served by Ollama
// as the executor. Override any of these via the CHEEP_* env vars.
package config

import (
	"os"
	"strconv"
)

type AgentConfig struct {
	Provider    string
	Model       string
	BaseURL     string
	APIKey      string
	MaxTurns    int
	TokenBudget int // stop the loop if cumulative input tokens exceed this (0 = unlimited)
}

type Settings struct {
	Orchestrator AgentConfig
	Executor     AgentConfig
	Workdir      string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func FromEnv(workdir string) Settings {
	return Settings{
		Orchestrator: AgentConfig{
			Provider: "anthropic",
			Model:    envOr("CHEEP_ORCHESTRATOR_MODEL", "claude-sonnet-4-6"),
			APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
			MaxTurns: envInt("CHEEP_ORCHESTRATOR_MAX_TURNS", 30),
		},
		Executor: AgentConfig{
			Provider:    "openai",
			Model:       envOr("CHEEP_EXECUTOR_MODEL", "qwen2.5-coder"),
			BaseURL:     envOr("CHEEP_EXECUTOR_BASE_URL", "http://localhost:11434/v1"),
			APIKey:      envOr("CHEEP_EXECUTOR_API_KEY", "ollama"),
			MaxTurns:    envInt("CHEEP_EXECUTOR_MAX_TURNS", 20),
			TokenBudget: envInt("CHEEP_EXECUTOR_TOKEN_BUDGET", 100000),
		},
		Workdir: workdir,
	}
}
