package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Detected holds build/test/lint commands inferred from a project's files —
// the seed for a scaffolded AGENTS.md's Build & Run / Validation sections.
type Detected struct {
	Build []string
	Test  []string
	Lint  []string
}

// DetectCommands probes dir for well-known project files and returns likely
// commands. Best-effort: wrong guesses are cheap because the agent-assisted
// init pass (and the human) review the generated AGENTS.md.
func DetectCommands(dir string) Detected {
	var d Detected
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}

	if exists("go.mod") {
		d.Build = append(d.Build, "go build ./...")
		d.Test = append(d.Test, "go test ./...")
		d.Lint = append(d.Lint, "go vet ./...")
	}
	if exists("Cargo.toml") {
		d.Build = append(d.Build, "cargo build")
		d.Test = append(d.Test, "cargo test")
		d.Lint = append(d.Lint, "cargo clippy -- -D warnings")
	}
	if exists("package.json") {
		scripts := npmScripts(filepath.Join(dir, "package.json"))
		runner := "npm run"
		if exists("pnpm-lock.yaml") {
			runner = "pnpm"
		} else if exists("yarn.lock") {
			runner = "yarn"
		} else if exists("bun.lockb") || exists("bun.lock") {
			runner = "bun run"
		}
		if scripts["build"] {
			d.Build = append(d.Build, runner+" build")
		}
		if scripts["test"] {
			d.Test = append(d.Test, runner+" test")
		}
		if scripts["lint"] {
			d.Lint = append(d.Lint, runner+" lint")
		}
		if scripts["typecheck"] {
			d.Lint = append(d.Lint, runner+" typecheck")
		}
	}
	if exists("pyproject.toml") || exists("setup.py") {
		switch {
		case exists("uv.lock"):
			d.Test = append(d.Test, "uv run pytest")
		case exists("poetry.lock"):
			d.Test = append(d.Test, "poetry run pytest")
		default:
			d.Test = append(d.Test, "pytest")
		}
		if hasTool(filepath.Join(dir, "pyproject.toml"), "ruff") {
			d.Lint = append(d.Lint, "ruff check .")
		}
	}
	if exists("Makefile") {
		targets := makeTargets(filepath.Join(dir, "Makefile"))
		// Makefile targets often wrap the ecosystem commands; prefer them.
		if targets["build"] && len(d.Build) == 0 {
			d.Build = append(d.Build, "make build")
		}
		if targets["test"] {
			d.Test = prepend(d.Test, "make test")
		}
		if targets["lint"] {
			d.Lint = prepend(d.Lint, "make lint")
		}
	}
	return d
}

func prepend(s []string, v string) []string { return append([]string{v}, s...) }

func npmScripts(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return out
	}
	for k := range pkg.Scripts {
		out[k] = true
	}
	return out
}

var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+)\s*:([^=]|$)`)

func makeTargets(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, m := range makeTargetRe.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

func hasTool(pyproject, tool string) bool {
	b, err := os.ReadFile(pyproject)
	return err == nil && strings.Contains(string(b), tool)
}
