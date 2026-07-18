// Package plugins is cheep's optional-capability manager. A plugin is a
// companion binary (like cheep-hivemind) that unlocks functionality when
// installed and enabled. Binaries are fetched on demand from the GitHub release
// (checksum-verified, reusing internal/update) into ~/.cheep/plugins, so the
// base cheep install stays lean and only pulls a plugin's weight if you opt in.
package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/update"
)

// Plugin describes an optional companion-binary capability.
type Plugin struct {
	Name    string // registry id, e.g. "hivemind"
	Summary string // one-line description
	Unlocks string // what enabling it turns on
	Binary  string // companion binary name, e.g. "cheep-hivemind"
}

// Registry is the built-in catalog of known plugins.
var Registry = []Plugin{
	{
		Name:    "hivemind",
		Summary: "Decentralized peer findings — share distilled insights P2P.",
		Unlocks: "/hivemind side-context browser",
		Binary:  "cheep-hivemind",
	},
}

// Find returns the registered plugin with the given name.
func Find(name string) (Plugin, bool) {
	for _, p := range Registry {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// Dir is where plugin binaries are installed: ~/.cheep/plugins. Kept separate
// from the cheep binary's own directory, which may be read-only (Homebrew).
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "plugins"), nil
}

// binName is the on-disk binary name for the current OS.
func (p Plugin) binName() string {
	if runtime.GOOS == "windows" {
		return p.Binary + ".exe"
	}
	return p.Binary
}

// BinaryPath is where this plugin's binary lives once installed.
func (p Plugin) BinaryPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, p.binName()), nil
}

// Installed reports whether the plugin's binary is present on disk.
func (p Plugin) Installed() bool {
	path, err := p.BinaryPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// archiveName is the release artifact that carries this plugin's binary, matching
// GoReleaser's per-binary archive naming.
func (p Plugin) archiveName() string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s.%s", p.Binary, runtime.GOOS, runtime.GOARCH, ext)
}

// Install downloads the plugin's companion binary from the latest release
// (checksum-verified) and writes it into the plugins dir, atomically.
func (p Plugin) Install(ctx context.Context) error {
	bin, err := update.FetchBinary(ctx, p.archiveName(), p.Binary)
	if err != nil {
		return err
	}
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	path, _ := p.BinaryPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove deletes the installed binary (no-op if absent).
func (p Plugin) Remove() error {
	path, err := p.BinaryPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Enabled reports whether the plugin is switched on in config. A plugin only
// takes effect when it is both Installed() and Enabled().
func Enabled(cfg config.Config, name string) bool {
	return cfg.Plugins[name]
}

// SetEnabled records the enable/disable choice in cfg (the caller persists it).
func SetEnabled(cfg *config.Config, name string, on bool) {
	if cfg.Plugins == nil {
		cfg.Plugins = map[string]bool{}
	}
	cfg.Plugins[name] = on
}

// Active reports whether a plugin is ready to use (installed AND enabled).
func Active(cfg config.Config, p Plugin) bool {
	return p.Installed() && Enabled(cfg, p.Name)
}
