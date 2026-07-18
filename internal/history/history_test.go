package history

import (
	"testing"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

func rec(id, parent string, updated time.Time) Record {
	return Record{
		ID: id, Parent: parent, Started: updated, Updated: updated,
		Workdir: "/w", Title: id,
		Messages: []core.Message{{Role: "user", Text: "hi from " + id}},
	}
}

func TestTreeNestsForksUnderParents(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// rootA (newest) has two forks; rootB is older; orphan's parent is missing.
	for _, r := range []Record{
		rec("rootB", "", t0),
		rec("rootA", "", t0.Add(3*time.Hour)),
		rec("forkA1", "rootA", t0.Add(4*time.Hour)),
		rec("forkA1a", "forkA1", t0.Add(5*time.Hour)),
		rec("orphan", "gone", t0.Add(1*time.Hour)),
	} {
		if err := Save(r); err != nil {
			t.Fatal(err)
		}
	}
	metas, depths, err := Tree()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range metas {
		ids = append(ids, m.ID)
	}
	want := []string{"rootA", "forkA1", "forkA1a", "orphan", "rootB"}
	wantD := []int{0, 1, 2, 0, 0}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] || depths[i] != wantD[i] {
			t.Errorf("row %d: got (%s,%d), want (%s,%d)", i, ids[i], depths[i], want[i], wantD[i])
		}
	}
}

func TestUniqueIDAvoidsCollision(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	id1 := UniqueID(t0)
	if err := Save(rec(id1, "", t0)); err != nil {
		t.Fatal(err)
	}
	if id2 := UniqueID(t0); id2 == id1 {
		t.Errorf("UniqueID returned colliding id %s", id2)
	}
}
