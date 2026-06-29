// Package configtools exposes cheep's configuration to an agent as tools, so the
// orchestrator (or, in rescue mode, an executor) can get the user set up and
// reconfigure cheep — discover local servers and API keys, then apply them —
// instead of editing files via the shell.
package configtools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/provider"
)

var (
	foundMu  sync.Mutex
	foundKey = map[string]string{} // name -> value located by discover (for copy_key)
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

func mask(v string) string {
	if len(v) <= 10 {
		return "•••"
	}
	return v[:6] + "…" + v[len(v)-4:]
}

// ---- discovery -------------------------------------------------------------

type keyHit struct {
	Name     string `json:"name"`
	FoundIn  string `json:"found_in"`
	Preview  string `json:"preview"`
	Provider string `json:"suggested_provider,omitempty"` // how to configure it
	Endpoint string `json:"suggested_endpoint,omitempty"`
	value    string
}

func looksLikeKey(name string) bool {
	return strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_KEY") || strings.HasSuffix(name, "_TOKEN")
}

// suggestProvider maps a known key name to how cheep should use it. Anthropic is
// native; the rest are OpenAI-compatible endpoints.
func suggestProvider(name string) (provider, endpoint string) {
	switch name {
	case "ANTHROPIC_API_KEY":
		return "anthropic", ""
	case "OPENAI_API_KEY":
		return "openai", "https://api.openai.com/v1"
	case "DEEPSEEK_API_KEY":
		return "openai", "https://api.deepseek.com/v1"
	case "XAI_API_KEY", "GROK_API_KEY":
		return "openai", "https://api.x.ai/v1"
	case "GROQ_API_KEY":
		return "openai", "https://api.groq.com/openai/v1"
	case "OPENROUTER_API_KEY":
		return "openai", "https://openrouter.ai/api/v1"
	case "MISTRAL_API_KEY":
		return "openai", "https://api.mistral.ai/v1"
	case "TOGETHER_API_KEY":
		return "openai", "https://api.together.xyz/v1"
	case "PERPLEXITY_API_KEY":
		return "openai", "https://api.perplexity.ai"
	case "GEMINI_API_KEY", "GOOGLE_API_KEY":
		return "openai", "https://generativelanguage.googleapis.com/v1beta/openai"
	}
	return "", ""
}

// scanKeys collects API-key-looking variables from the environment and dotfiles.
func scanKeys() []keyHit {
	seen := map[string]bool{}
	var out []keyHit
	add := func(name, val, src string) {
		name = strings.TrimSpace(name)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if name == "" || val == "" || seen[name] || !looksLikeKey(name) {
			return
		}
		seen[name] = true
		prov, ep := suggestProvider(name)
		out = append(out, keyHit{Name: name, FoundIn: src, Preview: mask(val), Provider: prov, Endpoint: ep, value: val})
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			add(k, v, "environment")
		}
	}
	files := []string{}
	if home, _ := os.UserHomeDir(); home != "" {
		for _, f := range []string{
			".zshrc", ".zshenv", ".zprofile", ".zlogin",
			".bashrc", ".bash_profile", ".bash_login", ".profile", ".envrc", ".env",
		} {
			files = append(files, filepath.Join(home, f))
		}
	}
	if cwd, _ := os.Getwd(); cwd != "" {
		files = append(files, filepath.Join(cwd, ".env"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "#") {
				continue
			}
			t = strings.TrimPrefix(t, "export ")
			if k, v, ok := strings.Cut(t, "="); ok {
				add(k, v, f)
			}
		}
	}
	return out
}

type serverHit struct {
	Endpoint string   `json:"endpoint"`
	Models   []string `json:"models"`
}

// scanServers probes common local LLM server ports for OpenAI-compatible models.
func scanServers() []serverHit {
	candidates := []string{
		"http://127.0.0.1:11434", "http://127.0.0.1:1234", "http://127.0.0.1:8000",
		"http://127.0.0.1:8080", "http://127.0.0.1:4000", "http://127.0.0.1:5000",
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var hits []serverHit
	for _, c := range candidates {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			if base, models, err := provider.DiscoverModels(c, ""); err == nil && len(models) > 0 {
				mu.Lock()
				hits = append(hits, serverHit{base, models})
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return hits
}

// ServerInfo / Found are the exported shapes returned by Discover for the UI.
type ServerInfo struct {
	Endpoint string
	Models   []string
}

type Found struct {
	Name, Source, Preview, Provider, Endpoint, Value string
}

// Discover returns reachable local servers and API keys (with raw values) for
// the setup UI to act on.
func Discover() ([]ServerInfo, []Found) {
	var servers []ServerInfo
	for _, s := range scanServers() {
		servers = append(servers, ServerInfo{Endpoint: s.Endpoint, Models: s.Models})
	}
	var keys []Found
	for _, k := range scanKeys() {
		keys = append(keys, Found{Name: k.Name, Source: k.FoundIn, Preview: k.Preview, Provider: k.Provider, Endpoint: k.Endpoint, Value: k.value})
	}
	return servers, keys
}

// Tools returns the configuration + discovery tools.
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
			Name:        "discover",
			Description: "Auto-discover what's available to set up cheep: running local LLM servers (with their models) and API keys found in the environment/dotfiles (masked). Saves NOTHING. Use this first when helping a user get set up — then propose a configuration, ASK the user, and apply it with set_orchestrator / add_executor / copy_key.",
			Parameters:  obj(map[string]any{}),
			Func: func(context.Context, map[string]any) string {
				keys := scanKeys()
				foundMu.Lock()
				for _, k := range keys {
					foundKey[k.Name] = k.value
				}
				foundMu.Unlock()
				b, _ := json.MarshalIndent(map[string]any{
					"local_servers": scanServers(),
					"api_keys":      keys,
					"note":          "propose a setup, ask the user, then apply with set_orchestrator/add_executor and copy_key for a found key",
				}, "", "  ")
				return string(b)
			},
		},
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
			Description: "Probe a specific endpoint (and optional access key) for the models it serves; confirms reachability.",
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
		{
			Name:        "copy_key",
			Description: "Save an API key that discover found into cheep (~/.cheep/keys.env) and the live session. ONLY call after the user grants permission.",
			Parameters:  obj(map[string]any{"name": s}, "name"),
			Func: func(_ context.Context, a map[string]any) string {
				name := str(a, "name")
				foundMu.Lock()
				v := foundKey[name]
				foundMu.Unlock()
				if v == "" {
					return "no value for " + name + " has been discovered — run discover first"
				}
				if err := config.SetKey(name, v); err != nil {
					return "ERROR: " + err.Error()
				}
				return name + " saved to cheep and active now (no restart needed)"
			},
		},
		{
			Name:        "set_key",
			Description: "Set an API key directly when the user provides the value. Saves to ~/.cheep/keys.env and the live session.",
			Parameters:  obj(map[string]any{"name": s, "value": s}, "name", "value"),
			Func: func(_ context.Context, a map[string]any) string {
				if err := config.SetKey(str(a, "name"), str(a, "value")); err != nil {
					return "ERROR: " + err.Error()
				}
				return str(a, "name") + " saved and active now"
			},
		},
	}
}
