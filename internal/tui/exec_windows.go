//go:build windows

package tui

import "os/exec"

// killProcessGroup is a no-op on Windows, which has no process groups or
// syscall.Kill; exec.CommandContext's default cancel already kills the process.
func killProcessGroup(c *exec.Cmd) {}
