// Package piext runs pi coding-agent extensions (https://pi.dev) inside
// cheep. Pi extensions are TypeScript/JavaScript modules; cheep can't host
// them natively, so a small embedded Node bridge loads them and serves the
// custom tools they register over MCP stdio — cheep's existing MCP client
// does the rest. Only the tool surface crosses the bridge: event hooks,
// commands, renderers, and providers need pi's own runtime and are skipped
// (the bridge reports each skip on startup).
//
// Extensions are named in config.json under "pi_extensions": npm package
// names (installed into ~/.cheep/pi via `cheep pi add`) or local paths.
package piext

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/mcp"
)

//go:embed bridge.mjs
var bridgeJS []byte

// Dir is the bridge home (~/.cheep/pi): bridge.mjs plus the npm prefix that
// holds installed extension packages.
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "pi"), nil
}

// Server materializes the bridge and returns the synthetic MCP server that
// runs the given extensions. Returns nil when no extensions are configured.
func Server(extensions []string) (*mcp.Server, error) {
	if len(extensions) == 0 {
		return nil, nil
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("pi extensions need node on PATH")
	}
	bridge, err := ensureBridge()
	if err != nil {
		return nil, err
	}
	return &mcp.Server{Command: node, Args: append([]string{bridge}, extensions...)}, nil
}

// ensureBridge writes the embedded bridge (and a package.json so node module
// resolution anchors here) into ~/.cheep/pi, refreshing it every start so
// upgrades take effect.
func ensureBridge() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(d, "bridge.mjs")
	if err := os.WriteFile(p, bridgeJS, 0o600); err != nil {
		return "", err
	}
	pkg := filepath.Join(d, "package.json")
	if _, err := os.Stat(pkg); os.IsNotExist(err) {
		_ = os.WriteFile(pkg, []byte("{\n  \"name\": \"cheep-pi\",\n  \"private\": true\n}\n"), 0o600)
	}
	return p, nil
}

// Add npm-installs a pi extension package into the bridge home (plus jiti,
// pi's TypeScript loader) and records it in the config.
func Add(pkg string) error {
	if _, err := ensureBridge(); err != nil {
		return err
	}
	d, _ := Dir()
	cmd := exec.Command("npm", "install", "--omit=dev", "--prefix", d, pkg, "jiti")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install %s: %w", pkg, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, e := range cfg.PiExtensions {
		if e == pkg {
			return nil // already registered
		}
	}
	cfg.PiExtensions = append(cfg.PiExtensions, pkg)
	return config.Save(cfg)
}

// Remove npm-uninstalls a package and drops it from the config.
func Remove(pkg string) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	cmd := exec.Command("npm", "uninstall", "--prefix", d, pkg)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run() // uninstall of a local-path spec is a no-op; keep going
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var kept []string
	for _, e := range cfg.PiExtensions {
		if e != pkg {
			kept = append(kept, e)
		}
	}
	cfg.PiExtensions = kept
	return config.Save(cfg)
}
