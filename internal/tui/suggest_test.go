package tui

import (
	"reflect"
	"testing"
)

func TestSplitSuggestions(t *testing.T) {
	in := "Added the endpoint and a test; all green.\n\n[[NEXT]] run the full test suite | ship as a PR | schedule nightly"
	clean, sug := splitSuggestions(in)
	if clean != "Added the endpoint and a test; all green." {
		t.Errorf("clean text wrong: %q", clean)
	}
	want := []string{"run the full test suite", "ship as a PR", "schedule nightly"}
	if !reflect.DeepEqual(sug, want) {
		t.Errorf("suggestions = %v, want %v", sug, want)
	}
}

func TestSplitSuggestionsCapsAtThree(t *testing.T) {
	_, sug := splitSuggestions("done\n[[NEXT]] a | b | c | d | e")
	if len(sug) != 3 {
		t.Errorf("want 3, got %d: %v", len(sug), sug)
	}
}

func TestSplitSuggestionsNone(t *testing.T) {
	in := "Just a normal reply with no sentinel."
	clean, sug := splitSuggestions(in)
	if clean != in || sug != nil {
		t.Errorf("expected passthrough, got clean=%q sug=%v", clean, sug)
	}
	// A [[NEXT]] not on the last non-empty line is ignored.
	in2 := "[[NEXT]] a | b\nmore text after"
	clean2, sug2 := splitSuggestions(in2)
	if clean2 != in2 || sug2 != nil {
		t.Errorf("mid-text sentinel should be ignored, got clean=%q sug=%v", clean2, sug2)
	}
}
