// Package jobs is cheep's scheduled-task registry: recurring `cheep run`
// invocations described by an interval or a cron expression, persisted under
// ~/.cheep/jobs and fired by `cheep daemon`. It stores the schedule and last
// outcome; the daemon and CLI drive execution.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/config"
)

// Job is one scheduled task.
type Job struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Task       string    `json:"task"`
	Workdir    string    `json:"workdir"`
	Schedule   string    `json:"schedule"` // Go duration ("30m", "24h") or 5-field cron ("0 9 * * *")
	Enabled    bool      `json:"enabled"`
	Created    time.Time `json:"created"`
	LastRun    time.Time `json:"last_run,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
	Runs       int       `json:"runs,omitempty"`
}

// Dir is ~/.cheep/jobs.
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "jobs"), nil
}

// NewID returns a short sortable id from a timestamp.
func NewID(t time.Time) string { return "job-" + t.UTC().Format("20060102-150405.000000") }

// Validate checks the schedule parses and required fields are set.
func (j Job) Validate() error {
	if strings.TrimSpace(j.Task) == "" {
		return fmt.Errorf("job needs a task")
	}
	if _, err := time.ParseDuration(j.Schedule); err == nil {
		return nil
	}
	if err := validateCron(j.Schedule); err != nil {
		return fmt.Errorf("schedule %q is neither a duration (e.g. 30m, 24h) nor a cron expression: %w", j.Schedule, err)
	}
	return nil
}

// Due reports whether the job should run at now given its schedule and LastRun.
func (j Job) Due(now time.Time) bool {
	if !j.Enabled {
		return false
	}
	if d, err := time.ParseDuration(j.Schedule); err == nil {
		return j.LastRun.IsZero() || now.Sub(j.LastRun) >= d
	}
	if ok, err := cronMatch(j.Schedule, now); err == nil && ok {
		// Fire at most once per matching minute.
		return now.Sub(j.LastRun) >= 59*time.Second
	}
	return false
}

// Next returns the next time the job is scheduled to run after t (best-effort;
// for display). ok is false when it can't be computed.
func (j Job) Next(after time.Time) (time.Time, bool) {
	if d, err := time.ParseDuration(j.Schedule); err == nil {
		base := j.LastRun
		if base.IsZero() {
			return after, true
		}
		return base.Add(d), true
	}
	// Cron: scan minute by minute up to a year out.
	t := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if ok, err := cronMatch(j.Schedule, t); err != nil {
			return time.Time{}, false
		} else if ok {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

func List() ([]Job, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, nil // no jobs yet
	}
	var out []Job
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var j Job
		if json.Unmarshal(b, &j) == nil {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Find resolves a job by id or (case-insensitive) name.
func Find(idOrName string) (Job, error) {
	all, err := List()
	if err != nil {
		return Job{}, err
	}
	for _, j := range all {
		if j.ID == idOrName || strings.EqualFold(j.Name, idOrName) {
			return j, nil
		}
	}
	return Job{}, fmt.Errorf("no job %q", idOrName)
}

func Save(j Job) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	return os.WriteFile(filepath.Join(d, j.ID+".json"), b, 0o600)
}

func Remove(idOrName string) error {
	j, err := Find(idOrName)
	if err != nil {
		return err
	}
	d, _ := Dir()
	return os.Remove(filepath.Join(d, j.ID+".json"))
}

// AppendLog records a run's outcome to the job's log (~/.cheep/jobs/<id>.log).
func AppendLog(id, line string) {
	d, err := Dir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(d, id+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

// Log returns the last n lines of a job's log.
func Log(id string, n int) []string {
	d, err := Dir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(d, id+".log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
