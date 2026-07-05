package tui

// clipboardCopy puts text on the system clipboard via the platform's CLI tool
// (pbcopy / wl-copy / xclip / xsel / clip.exe), so /copy works without cgo.

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func clipboardCopy(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "windows":
		candidates = [][]string{{"clip"}}
	default:
		candidates = [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard tool found (pbcopy/wl-copy/xclip/xsel)")
}
