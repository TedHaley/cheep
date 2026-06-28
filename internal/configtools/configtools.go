// Package configtools exposes cheep's own configuration to an agent as tools, so
// the orchestrator can reconfigure cheep (switch its model, add/remove executors)
// when the user asks — instead of editing files via the shell. Each tool
// load-modifies-saves the config file; the shell reloads it after the turn.
package configtools

import (
	"context"
	"encoding/json"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/provider"
)

func str(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func load() config.Config { c, _ := config.Load(); return c }

func redacted(c config.Config) config.Config {
	if c.Orchestrator.APIKey != "" {
		c.Orchestrator.APIKey = "set"
	}
	for i := range c.Executors {
		if c.Executors[i].APIKey != "" {
			c.Executors[i].APIKey = "set"
		}
	}
	return c
}

// Tools returns the configuration-editing tools.
func Tools() []core.Tool {
	obj := func(props map[string]any, req ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			m["required"] = req
		}
		return m
	}
	s := map[string]any{"type": "string"}

	return []core.Tool{
		{
			Name:        "get_config",
			Description: "Show cheep's current configuration (orchestrator + executors).",
			Parameters:  obj(map[string]any{}),
			Func: func(context.Context, map[string]any) string {
				b, _ := json.MarshalIndent(redacted(load()), "", "  ")
				return string(b)
			},
		},
		{
			Name:        "discover_models",
			Description: "Probe an endpoint (and optional access key) for the models it serves; confirms reachability.",
			Parameters:  obj(map[string]any{"endpoint": s, "api_key": s}, "endpoint"),
			Func: func(_ context.Context, a map[string]any) string {
				base, models, err := provider.DiscoverModels(str(a, "endpoint"), str(a, "api_key"))
				if err != nil {
					return "ERROR: " + err.Error()
				}
				b, _ := json.Marshal(map[string]any{"resolved_endpoint": base, "models": models})
				return string(b)
			},
		},
		{
			Name:        "set_orchestrator",
			Description: `Change the lead orchestrator. provider="anthropic" (Claude; blank endpoint, needs ANTHROPIC_API_KEY) or "openai" (with endpoint). Applies on the next message.`,
			Parameters:  obj(map[string]any{"provider": s, "endpoint": s, "api_key": s, "model": s}, "provider", "model"),
			Func: func(_ context.Context, a map[string]any) string {
				c := load()
				c.Orchestrator = config.Agent{
					Provider: str(a, "provider"), Endpoint: str(a, "endpoint"),
					APIKey: str(a, "api_key"), Model: str(a, "model"),
				}
				if err := config.Save(c); err != nil {
					return "ERROR: " + err.Error()
				}
				return "orchestrator set to " + str(a, "model") + " — applies on the next message"
			},
		},
		{
			Name:        "add_executor",
			Description: "Add or replace an executor by name (OpenAI-compatible endpoint). Applies on the next message.",
			Parameters:  obj(map[string]any{"name": s, "endpoint": s, "api_key": s, "model": s}, "name", "endpoint", "model"),
			Func: func(_ context.Context, a map[string]any) string {
				c := load()
				e := config.Agent{
					Name: str(a, "name"), Provider: "openai", Endpoint: str(a, "endpoint"),
					APIKey: str(a, "api_key"), Model: str(a, "model"),
				}
				replaced := false
				for i := range c.Executors {
					if c.Executors[i].Name == e.Name {
						c.Executors[i], replaced = e, true
					}
				}
				if !replaced {
					c.Executors = append(c.Executors, e)
				}
				if err := config.Save(c); err != nil {
					return "ERROR: " + err.Error()
				}
				return "executor " + e.Name + " saved — applies on the next message"
			},
		},
		{
			Name:        "remove_executor",
			Description: "Remove an executor by name. Applies on the next message.",
			Parameters:  obj(map[string]any{"name": s}, "name"),
			Func: func(_ context.Context, a map[string]any) string {
				c := load()
				out := c.Executors[:0]
				for _, e := range c.Executors {
					if e.Name != str(a, "name") {
						out = append(out, e)
					}
				}
				c.Executors = out
				if err := config.Save(c); err != nil {
					return "ERROR: " + err.Error()
				}
				return "removed " + str(a, "name") + " — applies on the next message"
			},
		},
	}
}
