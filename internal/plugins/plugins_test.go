package plugins

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TedHaley/cheep/internal/config"
)

func TestRegistryHasHivemind(t *testing.T) {
	p, ok := Find("hivemind")
	if !ok {
		t.Fatal("hivemind should be registered")
	}
	if p.Binary != "cheep-hivemind" {
		t.Fatalf("hivemind binary = %q", p.Binary)
	}
	if _, ok := Find("nope"); ok {
		t.Fatal("unknown plugin should not be found")
	}
}

func TestBinaryPathAndInstalled(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	p, _ := Find("hivemind")

	if p.Installed() {
		t.Fatal("plugin should not be installed in a fresh home")
	}
	path, err := p.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an install by dropping the binary in place.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !p.Installed() {
		t.Fatal("plugin should be detected as installed once the binary exists")
	}

	if err := p.Remove(); err != nil {
		t.Fatal(err)
	}
	if p.Installed() {
		t.Fatal("plugin should be gone after Remove")
	}
	if err := p.Remove(); err != nil {
		t.Fatal("Remove on an absent plugin should be a no-op, got:", err)
	}
}

func TestEnabledAndActive(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	p, _ := Find("hivemind")
	var cfg config.Config

	if Enabled(cfg, "hivemind") {
		t.Fatal("should default to disabled")
	}
	SetEnabled(&cfg, "hivemind", true)
	if !Enabled(cfg, "hivemind") {
		t.Fatal("SetEnabled(true) should enable")
	}
	// Enabled but not installed → not active.
	if Active(cfg, p) {
		t.Fatal("enabled-but-not-installed should not be active")
	}
	// Install, then it's active.
	path, _ := p.BinaryPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("x"), 0o755)
	if !Active(cfg, p) {
		t.Fatal("installed + enabled should be active")
	}
	SetEnabled(&cfg, "hivemind", false)
	if Active(cfg, p) {
		t.Fatal("disabled should not be active")
	}
}

func TestArchiveNameMatchesGoReleaser(t *testing.T) {
	p, _ := Find("hivemind")
	got := p.archiveName()
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	want := "cheep-hivemind_" + runtime.GOOS + "_" + runtime.GOARCH + "." + ext
	if got != want {
		t.Fatalf("archiveName = %q, want %q", got, want)
	}
}
