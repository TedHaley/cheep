// Package capabilities is the curated catalog of MCP-backed skills that
// auto-improve can add to a struggling agent. Installs come ONLY from this
// vetted catalog — never arbitrary registry servers — so "the agent downloads
// an MCP to improve itself" can't pull untrusted code onto your machine.
package capabilities

import (
	"context"
	"strings"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/mcp"
)

// Capability is a vetted MCP server that grants agents a skill (e.g. web
// research). Installing one adds its server to config.MCP under Name.
type Capability struct {
	Name     string     // config.MCP key, e.g. "fetch"
	Summary  string     // what it grants
	Triggers []string   // struggle keywords that hint at needing it (fallback matcher)
	Server   mcp.Server // the vetted MCP spec
	NeedsEnv []string   // env vars it needs to work (e.g. API keys)
}

// Catalog is the curated starter set. Keep it small and trustworthy.
var Catalog = []Capability{
	{
		Name:     "fetch",
		Summary:  "Fetch and read web pages by URL — the fastest research boost (no API key).",
		Triggers: []string{"fetch", "url", "web page", "read the link", "scrape", "docs", "documentation", "research"},
		Server:   mcp.Server{Command: "uvx", Args: []string{"mcp-server-fetch"}, Roles: []string{"executor", "orchestrator"}},
	},
	{
		Name:     "web-search",
		Summary:  "Search the web (Brave). Needs BRAVE_API_KEY.",
		Triggers: []string{"search", "look up", "find online", "latest", "who is", "what is", "research"},
		Server:   mcp.Server{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-brave-search"}, Roles: []string{"executor", "orchestrator"}},
		NeedsEnv: []string{"BRAVE_API_KEY"},
	},
	{
		Name:     "github",
		Summary:  "Read GitHub repos, issues, and PRs. Needs GITHUB_TOKEN.",
		Triggers: []string{"github", "pull request", "issue", "repository", "repo"},
		Server:   mcp.Server{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"}, Roles: []string{"executor", "orchestrator"}},
		NeedsEnv: []string{"GITHUB_TOKEN"},
	},
}

// Find returns the catalog capability with the given name (case-insensitive).
func Find(name string) (Capability, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, c := range Catalog {
		if c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}

// Installed reports whether the capability's MCP is already configured.
func Installed(cfg config.Config, name string) bool {
	_, ok := cfg.MCP[name]
	return ok
}

// Install adds the capability's MCP server to cfg (the caller persists and
// rebuilds to wire the new tools in).
func Install(cfg *config.Config, c Capability) {
	if cfg.MCP == nil {
		cfg.MCP = map[string]mcp.Server{}
	}
	cfg.MCP[c.Name] = c.Server
}

// Available returns catalog capabilities not yet installed.
func Available(cfg config.Config) []Capability {
	var out []Capability
	for _, c := range Catalog {
		if !Installed(cfg, c.Name) {
			out = append(out, c)
		}
	}
	return out
}

// Detect asks the (cheap) model whether a struggling turn would have benefited
// from an uninstalled capability, returning the best fit or ok=false. This is
// the "notice the model struggling" step of auto-improve.
func Detect(ctx context.Context, prov core.Provider, model, task, result string, cfg config.Config) (Capability, bool) {
	avail := Available(cfg)
	if len(avail) == 0 {
		return Capability{}, false
	}
	var list strings.Builder
	for _, c := range avail {
		list.WriteString("- " + c.Name + ": " + c.Summary + "\n")
	}
	sys := "An agent just worked a task and may have struggled for lack of a TOOL. Installable tools:\n" +
		list.String() +
		"\nReply with the SINGLE tool name that would most help this agent, or exactly NONE if the outcome " +
		"was fine or no tool would have helped. Output only the name."
	msg := "TASK:\n" + clip(task, 800) + "\n\nRESULT:\n" + clip(result, 1500)
	turn, err := prov.Complete(ctx, model, sys, []core.Message{{Role: "user", Text: msg}}, nil)
	if err != nil {
		return Capability{}, false
	}
	name := strings.Trim(strings.ToLower(strings.TrimSpace(turn.Message.Text)), ".`\"' ")
	if name == "" || name == "none" {
		return Capability{}, false
	}
	c, ok := Find(name)
	if !ok || Installed(cfg, c.Name) {
		return Capability{}, false
	}
	return c, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
