package inflight

import (
	"testing"
	"time"
)

func TestMarkClearStale(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())

	// A cleared marker never comes back.
	j := Mark(Job{Workdir: "/w", Executor: "qwen", Subtask: "do things", Started: time.Now()})
	j.Clear()
	if got := Stale("/w"); len(got) != 0 {
		t.Fatalf("cleared marker resurfaced: %+v", got)
	}

	// A marker from a dead process is stale — and reaped exactly once.
	j2 := Mark(Job{Workdir: "/w", Executor: "qwen", Kind: "scout", Subtask: "audit", Branch: "cheep/qwen-1", Started: time.Now()})
	_ = j2
	// Simulate the owning process being gone: our own PID is treated as not-alive
	// by design (alive() excludes os.Getpid()), so the marker reads as stale.
	got := Stale("/w")
	if len(got) != 1 || got[0].Branch != "cheep/qwen-1" || got[0].Kind != "scout" {
		t.Fatalf("want the one stale job back, got %+v", got)
	}
	if again := Stale("/w"); len(again) != 0 {
		t.Fatalf("stale marker not reaped: %+v", again)
	}

	// Markers for other workdirs stay untouched.
	Mark(Job{Workdir: "/other", Executor: "e", Subtask: "s", Started: time.Now()})
	if got := Stale("/w"); len(got) != 0 {
		t.Fatalf("cross-workdir leak: %+v", got)
	}
	if got := Stale("/other"); len(got) != 1 {
		t.Fatalf("other workdir's marker missing: %+v", got)
	}
}
