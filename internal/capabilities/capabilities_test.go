package capabilities

import (
	"testing"

	"github.com/TedHaley/cheep/internal/config"
)

func TestFindAndInstall(t *testing.T) {
	c, ok := Find("Fetch") // case-insensitive
	if !ok || c.Name != "fetch" {
		t.Fatalf("Find(Fetch) = %v, %v", c, ok)
	}
	if _, ok := Find("nonexistent"); ok {
		t.Fatal("unknown capability should not be found")
	}

	var cfg config.Config
	if Installed(cfg, "fetch") {
		t.Fatal("nothing should be installed on an empty config")
	}
	Install(&cfg, c)
	if !Installed(cfg, "fetch") {
		t.Fatal("Install should add the MCP to config")
	}
	if got := cfg.MCP["fetch"].Command; got != "uvx" {
		t.Fatalf("installed server command = %q, want uvx", got)
	}
}

func TestAvailableExcludesInstalled(t *testing.T) {
	var cfg config.Config
	full := len(Available(cfg))
	if full != len(Catalog) {
		t.Fatalf("all %d should be available initially, got %d", len(Catalog), full)
	}
	Install(&cfg, Catalog[0])
	if got := len(Available(cfg)); got != full-1 {
		t.Fatalf("installing one should leave %d available, got %d", full-1, got)
	}
	for _, c := range Available(cfg) {
		if c.Name == Catalog[0].Name {
			t.Fatal("installed capability should not be listed as available")
		}
	}
}
