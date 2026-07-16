//go:build !windows

package tui

import (
	"os/exec"
	"syscall"
)

// killProcessGroup makes an exec.CommandContext cancel kill the command's whole
// process group, so children of `sh -c` die with it instead of being orphaned.
// The command is placed in its own group (Setpgid) and Cancel signals the
// negative PID (the group).
func killProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
