package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfine(t *testing.T) {
	wd := t.TempDir()
	if _, err := confine(wd, "a/b.txt"); err != nil {
		t.Fatalf("relative path should be allowed: %v", err)
	}
	if _, err := confine(wd, "/etc/passwd"); err == nil {
		t.Fatal("absolute path should be rejected")
	}
	if _, err := confine(wd, "../escape"); err == nil {
		t.Fatal("parent-escape should be rejected")
	}
	if p, err := confine(wd, "x.txt"); err != nil || filepath.Dir(p) != wd {
		t.Fatalf("expected %s/x.txt, got %s (%v)", wd, p, err)
	}
}

func TestWriteFileConfined(t *testing.T) {
	wd := t.TempDir()
	var write func(context.Context, map[string]any) string
	for _, tl := range Make(wd, true) {
		if tl.Name == "write_file" {
			write = tl.Func
		}
	}
	ctx := context.Background()
	if got := write(ctx, map[string]any{"path": "../evil.txt", "content": "x"}); !strings.HasPrefix(got, "ERROR") {
		t.Fatalf("escape write should error, got %q", got)
	}
	if got := write(ctx, map[string]any{"path": "ok.txt", "content": "hi"}); strings.HasPrefix(got, "ERROR") {
		t.Fatalf("in-workspace write should succeed, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(wd, "ok.txt")); err != nil {
		t.Fatalf("file not written inside workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(wd), "evil.txt")); err == nil {
		t.Fatal("escape file was written outside workspace")
	}
}
