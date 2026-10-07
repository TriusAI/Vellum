package summarize

import (
	"strings"
	"testing"
)

func TestBestExamples(t *testing.T) {
	examples := []ShelvingExample{
		{Title: "On the Metatheory of Session Types", Category: "programming-languages"},
		{Title: "A Type System for Gradual Guarantees", Category: "programming-languages"},
		{Title: "Attention Is All You Need", Authors: "Vaswani et al.", Category: "machine-learning"},
		{Title: "The Fall of Rome", Category: "history"},
	}
	text := "This paper presents a gradual type system with sound " +
		"guarantees for session types and concurrency."
	got := bestExamples(examples, text, 3)
	if len(got) == 0 {
		t.Fatal("expected similar examples to match")
	}
	for _, g := range got {
		if g.Category != "programming-languages" {
			t.Fatalf("unrelated example matched: %+v", g)
		}
	}
	// weak overlap (< 2 distinctive shared terms) yields nothing
	if got := bestExamples(examples,
		"something entirely different about gardening", 3); len(got) != 0 {
		t.Fatalf("weak overlap should yield nothing, got %v", got)
	}
	// n caps the result
	got = bestExamples(examples,
		"type system guarantees session types concurrency metatheory gradual", 1)
	if len(got) != 1 {
		t.Fatalf("n=1 should yield exactly one example, got %v", got)
	}
	// empty examples list is safe
	if got := bestExamples(nil, text, 3); got != nil {
		t.Fatalf("no examples should yield nil, got %v", got)
	}
}

func TestExamplesBlock(t *testing.T) {
	if s := examplesBlock(nil); s != "" {
		t.Fatalf("no examples must render an empty block, got %q", s)
	}
	s := examplesBlock([]ShelvingExample{
		{Title: "A Type System", Authors: "X", Category: "programming-languages"},
	})
	if !strings.Contains(s, `"A Type System" (X) → programming-languages`) {
		t.Fatalf("unexpected block content: %q", s)
	}
	if !strings.Contains(s, "file an alike document the same way") {
		t.Fatalf("block must carry the instruction: %q", s)
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("The, Metatheory! of Session-Types (2026)")
	want := []string{"the", "metatheory", "of", "session", "types", "2026"}
	if len(got) != len(want) {
		t.Fatalf("tokenize: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize: %v != %v", got, want)
		}
	}
}
