package jobs

import (
	"testing"
	"time"
)

func TestCronMatch(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		spec string
		when string
		want bool
	}{
		{"0 9 * * *", "2026-07-06 09:00", true},
		{"0 9 * * *", "2026-07-06 09:01", false},
		{"*/15 * * * *", "2026-07-06 10:30", true},
		{"*/15 * * * *", "2026-07-06 10:31", false},
		{"0 9 * * 1-5", "2026-07-06 09:00", true},  // 2026-07-06 is a Monday
		{"0 9 * * 1-5", "2026-07-04 09:00", false}, // Saturday
		{"0 0,12 * * *", "2026-07-06 12:00", true},
		{"30 8 1 * *", "2026-07-01 08:30", true},
		{"30 8 1 * *", "2026-07-02 08:30", false},
	}
	for _, c := range cases {
		got, err := cronMatch(c.spec, at(c.when))
		if err != nil {
			t.Errorf("cronMatch(%q,%q) error: %v", c.spec, c.when, err)
			continue
		}
		if got != c.want {
			t.Errorf("cronMatch(%q, %q) = %v, want %v", c.spec, c.when, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []string{"30m", "24h", "0 9 * * *", "*/5 * * * *", "0 9 * * 1-5"}
	bad := []string{"", "nonsense", "0 9 * *", "99 9 * * *", "0 9 * * 8"}
	for _, s := range ok {
		if err := (Job{Task: "t", Schedule: s}).Validate(); err != nil {
			t.Errorf("Validate(%q) unexpected error: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := (Job{Task: "t", Schedule: s}).Validate(); err == nil {
			t.Errorf("Validate(%q) should have failed", s)
		}
	}
}

func TestDueInterval(t *testing.T) {
	now := time.Now()
	j := Job{Task: "t", Schedule: "1h", Enabled: true}
	if !j.Due(now) {
		t.Error("never-run interval job should be due")
	}
	j.LastRun = now.Add(-30 * time.Minute)
	if j.Due(now) {
		t.Error("interval job run 30m ago should not be due for 1h")
	}
	j.LastRun = now.Add(-90 * time.Minute)
	if !j.Due(now) {
		t.Error("interval job run 90m ago should be due for 1h")
	}
	j.Enabled = false
	if j.Due(now) {
		t.Error("disabled job is never due")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	j := Job{ID: NewID(time.Now()), Name: "nightly", Task: "run tests", Schedule: "24h",
		Enabled: true, Created: time.Now()}
	if err := Save(j); err != nil {
		t.Fatal(err)
	}
	got, err := Find("nightly")
	if err != nil || got.Task != "run tests" {
		t.Fatalf("Find by name: %v / %+v", err, got)
	}
	if _, err := Find(j.ID); err != nil {
		t.Fatalf("Find by id: %v", err)
	}
	if err := Remove("nightly"); err != nil {
		t.Fatal(err)
	}
	if all, _ := List(); len(all) != 0 {
		t.Errorf("job not removed: %+v", all)
	}
}
