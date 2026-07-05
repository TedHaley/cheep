//go:build unix

package worktree

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryLock opens path and takes a non-blocking exclusive flock on it. The lock
// is released by closing the file (or by process death, which is the point:
// no stale-pid bookkeeping). Returns nil, false when the lock is held by
// someone else.
func tryLock(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

const poolSupported = true
