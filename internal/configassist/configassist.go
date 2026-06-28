// Package configassist builds a small agent that helps the user configure cheep
// by chatting — powered by whichever already-configured agent is reachable. It
// can probe an endpoint for its model and write the config, so one working agent
// bootstraps the others.
package configassist

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/provider"
)

const system = `You are cheep's setup assistant. You help the user configure cheep's agents:
one orchestrator (the lead that plans and delegates) and zero or more executors (workers).
Each agent is just an endpoint + optional access key + a model.

How to work:
- When the user gives you an endpoint, call discover_models first — it confirms the endpoint
  is reachable and reports the model(s) it serves. Use the discovered model unless the user
  names a specific one.
- Record agents with set_orchestrator / add_executor, then call save to persist.
- For a Claude/Anthropic orchestrator use provider="anthropic" with a blank endpoint; for any
  other endpoint use provider="openai" with the endpoint.
- Call show_config to confirm the setup, and tell the user clearly once it's saved.
Be concise and DO the work via tools rather than only describing it.`

// State is the working configuration the assistant edits.
type State struct {
	Cfg   config.Config
	Saved bool
}

func str(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

// reachable returns the first configured agent that responds to a ping.
func reachable(c config.Config) (config.Agent, string, bool) {
	ping := func(a config.Agent) bool {
		if a.Model == "" {
			return false
		}
		p := provider.For(a.Provider, a.Endpoint, a.APIKey, 64)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := p.Complete(ctx, a.Model, "Reply with: ok", []core.Message{{Role: "user", Text: "ok"}}, nil)
		return err == nil
	}
	if o := c.Orchestrator; !(o.Provider == "anthropic" && o.APIKey == "") && ping(o) {
		return o, "orchestrator (" + o.Model + ")", true
	}
	for _, e := range c.Executors {
		if ping(e) {
			return e, "executor " + e.Name + " (" + e.Model + ")", true
		}
	}
	return config.Agent{}, "", false
}

// Build returns a setup-assistant agent powered by a reachable configured agent,
// the mutable State it edits, and a label naming which agent drives it.
func Build(cfg config.Config, onEvent core.EventFunc) (*agent.Agent, *State, string, error) {
	ag, label, ok := reachable(cfg)
	if !ok {
		return nil, nil, "", fmt.Errorf("no reachable agent to drive setup — configure one with /config first")
	}
	prov := provider.For(ag.Provider, ag.Endpoint, ag.APIKey, 4096)
	model := ag.Model
	st := &State{Cfg: cfg}

	obj := func(props map[string]any, req ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			m["required"] = req
		}
		return m
	}
	s := map[string]any{"type": "string"}

	tools := []core.Tool{
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
			Description: `Set the orchestrator. provider is "anthropic" (Claude, blank endpoint) or "openai".`,
			Parameters:  obj(map[string]any{"provider": s, "endpoint": s, "api_key": s, "model": s}, "provider", "model"),
			Func: func(_ context.Context, a map[string]any) string {
				st.Cfg.Orchestrator = config.Agent{
					Provider: str(a, "provider"), Endpoint: str(a, "endpoint"),
					APIKey: str(a, "api_key"), Model: str(a, "model"),
				}
				return "orchestrator set to " + str(a, "model")
			},
		},
		{
			Name:        "add_executor",
			Description: "Add or replace an executor by name (OpenAI-compatible endpoint).",
			Parameters:  obj(map[string]any{"name": s, "endpoint": s, "api_key": s, "model": s}, "name", "endpoint", "model"),
			Func: func(_ context.Context, a map[string]any) string {
				e := config.Agent{
					Name: str(a, "name"), Provider: "openai", Endpoint: str(a, "endpoint"),
					APIKey: str(a, "api_key"), Model: str(a, "model"),
				}
				for i := range st.Cfg.Executors {
					if st.Cfg.Executors[i].Name == e.Name {
						st.Cfg.Executors[i] = e
						return "executor " + e.Name + " updated to " + e.Model
					}
				}
				st.Cfg.Executors = append(st.Cfg.Executors, e)
				return "executor " + e.Name + " added (" + e.Model + ")"
			},
		},
		{
			Name:        "remove_executor",
			Description: "Remove an executor by name.",
			Parameters:  obj(map[string]any{"name": s}, "name"),
			Func: func(_ context.Context, a map[string]any) string {
				out := st.Cfg.Executors[:0]
				for _, e := range st.Cfg.Executors {
					if e.Name != str(a, "name") {
						out = append(out, e)
					}
				}
				st.Cfg.Executors = out
				return "removed " + str(a, "name")
			},
		},
		{
			Name:        "show_config",
			Description: "Show the current pending configuration.",
			Parameters:  obj(map[string]any{}),
			Func:        func(context.Context, map[string]any) string { return render(st.Cfg) },
		},
		{
			Name:        "save",
			Description: "Persist the configuration to disk.",
			Parameters:  obj(map[string]any{}),
			Func: func(context.Context, map[string]any) string {
				if err := config.Save(st.Cfg); err != nil {
					return "ERROR: " + err.Error()
				}
				st.Saved = true
				p, _ := config.Path()
				return "saved to " + p
			},
		},
	}

	return agent.New("setup", prov, model, system, tools, 20, 0, onEvent), st, label, nil
}

func render(c config.Config) string {
	o := c.Orchestrator
	out := fmt.Sprintf("orchestrator: provider=%s model=%s endpoint=%s\n", o.Provider, o.Model, o.Endpoint)
	if len(c.Executors) == 0 {
		out += "executors: (none — solo mode)"
		return out
	}
	out += "executors:\n"
	for _, e := range c.Executors {
		out += fmt.Sprintf("  - %s: model=%s endpoint=%s\n", e.Name, e.Model, e.Endpoint)
	}
	return out
}
