//go:build windows

package worktree

import "os"

// The pool relies on flock semantics (auto-release on process death) that
// have no direct equivalent here; on Windows cheep just uses ephemeral
// worktrees, which behave exactly as before.
func tryLock(string) (*os.File, bool) { return nil, false }

const poolSupported = false
