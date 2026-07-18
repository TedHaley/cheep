package hivemind

import "testing"

func TestIdentityPersists(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	a, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if a.AuthorID() != b.AuthorID() {
		t.Fatalf("identity should persist: %s != %s", a.AuthorID(), b.AuthorID())
	}
	if a.AuthorID() == "" {
		t.Fatal("author id should not be empty")
	}
}

func TestFindingSignAndVerify(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	id, _ := LoadIdentity()
	f := Finding{Title: "use rm before go build", Body: "avoids the codesign kill", Tags: []string{"macOS", "  Build "}}
	f.Sign(id)

	if f.Author != id.AuthorID() {
		t.Fatal("Sign should stamp the author")
	}
	if f.ID == "" || f.Sig == "" {
		t.Fatal("Sign should set id and sig")
	}
	if err := f.Verify(); err != nil {
		t.Fatalf("valid finding should verify: %v", err)
	}
	// Tags normalized (lowercased, trimmed, sorted).
	if len(f.Tags) != 2 || f.Tags[0] != "build" || f.Tags[1] != "macos" {
		t.Fatalf("tags not normalized: %v", f.Tags)
	}
	// Tamper: mutating the body must invalidate the signature.
	bad := f
	bad.Body = "something else"
	if bad.Verify() == nil {
		t.Fatal("tampered finding must not verify")
	}
}

func TestPoolDedupeFilterMute(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	id, _ := LoadIdentity()
	mk := func(title string, tags ...string) Finding {
		f := Finding{Title: title, Body: "b:" + title, Tags: tags}
		f.Sign(id)
		return f
	}
	p := LoadPool()
	f1 := mk("one", "go")
	f2 := mk("two", "rust")

	if !p.Add(f1) || !p.Add(f2) {
		t.Fatal("first adds should succeed")
	}
	if p.Add(f1) {
		t.Fatal("duplicate id should not re-add")
	}
	if got := len(p.List()); got != 2 {
		t.Fatalf("List() = %d, want 2", got)
	}
	if got := p.List("go"); len(got) != 1 || got[0].Title != "one" {
		t.Fatalf("tag filter failed: %v", got)
	}

	// Persist + reload round-trip.
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	if got := len(LoadPool().List()); got != 2 {
		t.Fatalf("reloaded pool has %d, want 2", got)
	}

	// Mute drops the author's findings and blocks re-adds.
	p.Mute(id.AuthorID())
	if len(p.List()) != 0 {
		t.Fatal("mute should drop the author's findings")
	}
	if p.Add(mk("three")) {
		t.Fatal("muted author's new finding should be rejected")
	}
}
