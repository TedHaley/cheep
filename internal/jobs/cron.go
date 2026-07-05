package jobs

// A compact standard 5-field cron matcher: "min hour dom month dow".
// Each field supports *, N, A-B, */N, A-B/N, and comma-separated lists of
// those. Enough for real schedules ("0 9 * * 1-5", "*/15 * * * *") without a
// dependency. Day-of-month and day-of-week both restrict when both are set
// (the common, if slightly loose, interpretation).

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cronField struct{ min, max int }

var cronFields = []cronField{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

func validateCron(spec string) error {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return fmt.Errorf("expected 5 fields, got %d", len(f))
	}
	for i, part := range f {
		if _, err := parseCronField(part, cronFields[i]); err != nil {
			return fmt.Errorf("field %d (%q): %w", i+1, part, err)
		}
	}
	return nil
}

// cronMatch reports whether t satisfies the cron spec.
func cronMatch(spec string, t time.Time) (bool, error) {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return false, fmt.Errorf("expected 5 fields, got %d", len(f))
	}
	sets := make([]map[int]bool, 5)
	for i, part := range f {
		s, err := parseCronField(part, cronFields[i])
		if err != nil {
			return false, err
		}
		sets[i] = s
	}
	vals := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	if !sets[0][vals[0]] || !sets[1][vals[1]] || !sets[3][vals[3]] {
		return false, nil
	}
	// dom/dow: if either is restricted (not "*"), the day matches when either
	// restricted field matches; if both are "*", any day matches.
	domStar, dowStar := f[2] == "*", f[4] == "*"
	switch {
	case domStar && dowStar:
		return true, nil
	case domStar:
		return sets[4][vals[4]], nil
	case dowStar:
		return sets[2][vals[2]], nil
	default:
		return sets[2][vals[2]] || sets[4][vals[4]], nil
	}
}

// parseCronField expands one field into the set of matching integers.
func parseCronField(field string, f cronField) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		rng := part
		if slash := strings.Index(part, "/"); slash >= 0 {
			s, err := strconv.Atoi(part[slash+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("bad step %q", part[slash+1:])
			}
			step = s
			rng = part[:slash]
		}
		lo, hi := f.min, f.max
		if rng != "*" {
			if dash := strings.Index(rng, "-"); dash >= 0 {
				a, err1 := strconv.Atoi(rng[:dash])
				b, err2 := strconv.Atoi(rng[dash+1:])
				if err1 != nil || err2 != nil {
					return nil, fmt.Errorf("bad range %q", rng)
				}
				lo, hi = a, b
			} else {
				n, err := strconv.Atoi(rng)
				if err != nil {
					return nil, fmt.Errorf("bad value %q", rng)
				}
				lo, hi = n, n
			}
		}
		if lo < f.min || hi > f.max || lo > hi {
			return nil, fmt.Errorf("%d-%d out of range %d-%d", lo, hi, f.min, f.max)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, nil
}
