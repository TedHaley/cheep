package approve

import (
	"fmt"
	"strings"
)

// maxDiffLines caps the LCS table; beyond it the preview degrades to a
// summary rather than an O(n·m) blowup.
const maxDiffLines = 3000

// Diff renders a line diff of old → new with -/+ prefixes and 3 lines of
// context around changes (unchanged runs collapse to a "···" marker). The UI
// colors lines by their first byte.
func Diff(old, new string) string {
	if old == new {
		return "(no change)"
	}
	a, b := splitLines(old), splitLines(new)
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return fmt.Sprintf("(file too large to preview: %d → %d lines)", len(a), len(b))
	}
	ops := lcsDiff(a, b)
	return renderHunks(ops, 3)
}

type op struct {
	kind byte // ' ', '-', '+'
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lcsDiff computes a line diff via a classic LCS table — simple and plenty
// fast at the sizes gated writes actually have.
func lcsDiff(a, b []string) []op {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:], b[j:]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// renderHunks keeps ctx unchanged lines around each change and collapses the
// rest to a single "···" marker line.
func renderHunks(ops []op, ctx int) string {
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		lo, hi := i-ctx, i+ctx
		if lo < 0 {
			lo = 0
		}
		if hi >= len(ops) {
			hi = len(ops) - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}
	var b strings.Builder
	skipping := false
	for i, o := range ops {
		if !keep[i] {
			if !skipping {
				b.WriteString("···\n")
				skipping = true
			}
			continue
		}
		skipping = false
		b.WriteByte(o.kind)
		b.WriteByte(' ')
		b.WriteString(o.text)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}
