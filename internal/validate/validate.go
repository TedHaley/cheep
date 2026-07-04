// Package validate is the pre-merge validation pipeline: it runs the
// project's declared checks (AGENTS.md "## Validation") inside a subtask's
// worktree, has a fresh-context reviewer agent judge the branch diff, and
// loops bounded fix rounds back through an executor before any merge.
//
// Checks are fail-closed: they must exit 0 for the work to land. The reviewer
// is fail-open on malformed output (a chatty model must not wedge every
// merge) unless Strict is set. The package holds no LLM machinery itself —
// the orchestrator injects Reviewer and Fix callbacks — so it is equally
// usable headlessly via `cheep validate`.
package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/project"
)

// CheckResult is one check's outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Exit   int    `json:"exit"`
	Output string `json:"output,omitempty"`
}

// Issue is one reviewer finding.
type Issue struct {
	Severity    string `json:"severity,omitempty"`
	File        string `json:"file,omitempty"`
	Description string `json:"description"`
}

// Verdict is the reviewer's structured judgment.
type Verdict struct {
	Verdict string  `json:"verdict"` // "approve" | "revise"
	Summary string  `json:"summary,omitempty"`
	Issues  []Issue `json:"issues,omitempty"`
}

// Result is the pipeline outcome for one branch.
type Result struct {
	Passed bool          `json:"passed"`
	Rounds int           `json:"rounds"`
	Checks []CheckResult `json:"checks,omitempty"`
	Review *Verdict      `json:"review,omitempty"`
	Note   string        `json:"note,omitempty"` // human-readable wrinkle (reviewer parse failure, etc.)
}

// Runner drives the pipeline. Reviewer and Fix are optional: a nil Reviewer
// skips review, a nil Fix makes the first failure final (single round).
type Runner struct {
	Checks    []project.Check
	MaxRounds int // fix rounds after the initial attempt; default 2
	Strict    bool
	// Reviewer judges a unified diff, exploring worktreeDir read-only.
	Reviewer func(ctx context.Context, diff, worktreeDir string) (Verdict, error)
	// Fix asks an executor to repair the worktree given a failure description
	// (and must leave the result committed).
	Fix     func(ctx context.Context, worktreeDir, task string) error
	OnEvent core.EventFunc
}

// Run validates the work in worktreeDir (committed on branch, forked at
// baseRef). It never touches the primary checkout.
func (r Runner) Run(ctx context.Context, worktreeDir, baseRef string) Result {
	maxRounds := r.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 2
	}
	res := Result{}
	for round := 0; ; round++ {
		res.Rounds = round
		if ctx.Err() != nil {
			res.Note = "cancelled"
			return res
		}

		res.Checks = r.runChecks(ctx, worktreeDir)
		if failed := failedChecks(res.Checks); len(failed) > 0 {
			if round >= maxRounds || r.Fix == nil {
				r.event("validation: checks still failing after %d fix round(s)", round)
				return res
			}
			r.event("validation: %d check(s) failed — fix round %d/%d", len(failed), round+1, maxRounds)
			if err := r.Fix(ctx, worktreeDir, fixChecksTask(failed)); err != nil {
				res.Note = "fix round failed: " + err.Error()
				return res
			}
			continue
		}

		if r.Reviewer == nil {
			res.Passed = true
			return res
		}
		diff, err := branchDiff(ctx, worktreeDir, baseRef)
		if err != nil {
			res.Note = "diff unavailable: " + err.Error()
			res.Passed = !r.Strict
			return res
		}
		if strings.TrimSpace(diff) == "" { // nothing to review
			res.Passed = true
			return res
		}
		r.event("validation: fresh-context review (round %d)", round)
		v, err := r.Reviewer(ctx, diff, worktreeDir)
		if err != nil {
			// Fail-open: a broken reviewer must not block every merge.
			res.Note = "review skipped: " + err.Error()
			res.Passed = !r.Strict
			return res
		}
		res.Review = &v
		if v.Verdict == "approve" {
			res.Passed = true
			return res
		}
		if round >= maxRounds || r.Fix == nil {
			r.event("validation: reviewer still requests changes after %d round(s)", round)
			return res
		}
		r.event("validation: reviewer requested changes — fix round %d/%d", round+1, maxRounds)
		if err := r.Fix(ctx, worktreeDir, fixReviewTask(v)); err != nil {
			res.Note = "fix round failed: " + err.Error()
			return res
		}
	}
}

func (r Runner) runChecks(ctx context.Context, dir string) []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		o, code := runShell(ctx, dir, c.Script)
		out = append(out, CheckResult{Name: c.Name, Exit: code, Output: clip(strings.TrimSpace(o), 4000)})
		r.event("validation: check %s → exit %d", c.Name, code)
	}
	return out
}

func failedChecks(cs []CheckResult) []CheckResult {
	var out []CheckResult
	for _, c := range cs {
		if c.Exit != 0 {
			out = append(out, c)
		}
	}
	return out
}

func fixChecksTask(failed []CheckResult) string {
	var b strings.Builder
	b.WriteString("The project's validation checks fail on the current state of this workspace. " +
		"Fix the underlying causes so every check passes. Do not weaken or delete the checks " +
		"or tests to make them pass.\n")
	for _, c := range failed {
		fmt.Fprintf(&b, "\nCheck %q exited %d:\n%s\n", c.Name, c.Exit, c.Output)
	}
	return b.String()
}

func fixReviewTask(v Verdict) string {
	var b strings.Builder
	b.WriteString("A code review of the current changes found issues that must be addressed:\n")
	if v.Summary != "" {
		b.WriteString("\nSummary: " + v.Summary + "\n")
	}
	for _, i := range v.Issues {
		fmt.Fprintf(&b, "- [%s] %s %s\n", orDash(i.Severity), i.Description, parenthetical(i.File))
	}
	b.WriteString("\nAddress each issue. If an issue is factually wrong, leave the code as is " +
		"and note why in your reply.")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parenthetical(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// branchDiff returns the unified diff of the worktree's HEAD against baseRef.
func branchDiff(ctx context.Context, dir, baseRef string) (string, error) {
	c := exec.CommandContext(ctx, "git", "diff", baseRef, "HEAD")
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff: %s", strings.TrimSpace(string(out)))
	}
	return clip(string(out), 60_000), nil
}

// runShell runs script via bash -lc in dir, mirroring how executors run
// commands, and returns combined output + exit code.
func runShell(ctx context.Context, dir, script string) (string, int) {
	c := exec.CommandContext(ctx, "bash", "-lc", script)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out) + err.Error(), -1
	}
	return string(out), 0
}

// ExtractVerdict parses the LAST balanced JSON object in a reviewer's reply —
// models often lead with prose. Returns an error when nothing parses or the
// verdict field is missing/unknown.
func ExtractVerdict(reply string) (Verdict, error) {
	for end := len(reply); end > 0; {
		close := strings.LastIndex(reply[:end], "}")
		if close < 0 {
			break
		}
		depth := 0
		start := -1
		for i := close; i >= 0; i-- {
			switch reply[i] {
			case '}':
				depth++
			case '{':
				depth--
				if depth == 0 {
					start = i
				}
			}
			if start >= 0 {
				break
			}
		}
		if start >= 0 {
			var v Verdict
			if err := json.Unmarshal([]byte(reply[start:close+1]), &v); err == nil {
				switch v.Verdict {
				case "approve", "revise":
					return v, nil
				}
			}
		}
		end = close
	}
	return Verdict{}, fmt.Errorf("no verdict JSON found in reviewer reply")
}

func (r Runner) event(format string, args ...any) {
	if r.OnEvent != nil {
		r.OnEvent(core.Event{Agent: "cheep", Type: "status", Status: fmt.Sprintf(format, args...)})
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…(truncated)"
	}
	return s
}
