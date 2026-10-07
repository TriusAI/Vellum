package classify

import (
	"strings"
	"testing"
)

func TestDetectPaper(t *testing.T) {
	text := `Attention Is All You Need
Ashish Vaswani et al.

Abstract
The dominant sequence transduction models are based on complex recurrent
or convolutional neural networks. We propose a new architecture, the
Transformer, based solely on attention mechanisms. Experiments show
quality improvements and parallelization gains [1] [2] [3] [4].

1 Introduction
Recurrent neural networks process sequences step by step.

References
[1] Some Author, "A paper", Journal of Things, 2015.
`
	kind, scores := Detect(text, 0, 8)
	if kind != "paper" {
		t.Fatalf("expected paper, got %q (scores: %+v)", kind, scores)
	}
}

func TestDetectBook(t *testing.T) {
	text := `Contents
Chapter One  The Beginning of Everything

Chapter One
In the beginning there were only fields and rain.

Copyright 1987 by the author. All rights reserved. Published by Small Press.
ISBN: 978-0-00-000000-0
`
	kind, _ := Detect(text, 0, 30)
	if kind != "book" {
		t.Fatalf("expected book, got %q", kind)
	}
}

func TestDetectGallery(t *testing.T) {
	text := strings.Repeat("figure 1\n", 12) // thin text, image pages
	kind, _ := Detect(text, 10, 10)
	if kind != "gallery" {
		t.Fatalf("expected gallery, got %q", kind)
	}
}

func TestDetectCourse(t *testing.T) {
	text := `CS 101: Introduction to the Course
Instructor: Prof. X. Office hours Tue 10-12.

Lecture 1: overview of the syllabus. Homework 1 due: Friday.
Lecture 2: problem set review. The midterm covers weeks 1-6.
`
	kind, _ := Detect(text, 0, 6)
	if kind != "course" {
		t.Fatalf("expected course, got %q", kind)
	}
}

func TestDetectUnknown(t *testing.T) {
	text := `random prose without any structural markers at all
just words about nothing in particular
and some more words here`
	kind, _ := Detect(text, 0, 2)
	if kind != "" {
		t.Fatalf("expected no confident kind, got %q", kind)
	}
}

func TestExtractAbstract(t *testing.T) {
	abstract := `The dominant sequence transduction models are based on complex
recurrent or convolutional neural networks that include an encoder and
a decoder. We propose a new simple network architecture, the Transformer,
based solely on attention mechanisms, dispensing with recurrence entirely.
Experiments on two machine translation tasks show quality superior to all
previous work while being more parallelizable.` // > 150 chars
	text := "Title\nAuthors\n\nAbstract\n" + abstract + "\n\n1 Introduction\nRecurrent networks are slow.\n\nReferences\n[1] x\n"
	got := ExtractAbstract(text)
	if !strings.Contains(got, "dominant sequence transduction") {
		t.Fatalf("abstract not extracted: %q", got)
	}
	if strings.Contains(got, "Recurrent networks are slow") {
		t.Fatalf("abstract bled into introduction: %q", got)
	}
}

func TestExtractAbstractMissing(t *testing.T) {
	if got := ExtractAbstract("no abstract heading here, just text body"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
func TestExtractFrontMatter(t *testing.T) {
	text := `Contents
The Rain  3
The Field  17

Preface
This book grew out of ten years of walking the same muddy lane at dusk.
I wrote it for the people who stopped to ask what I was looking at, and
for the ones who did not stop but wondered anyway. The chapters move
from weather to soil to the small politics of hedgerows, and the preface
continues at some length here in order to pass the minimum-length check
for real front-matter sections, since actual book prefaces run several
pages and we want to reject stray single-line headings but not genuine
overviews written by the author about the book as a whole.

Chapter One
The rain began before the road did.
`
	fm := ExtractFrontMatter(text)
	if !strings.Contains(fm, "ten years of walking") {
		t.Fatalf("front matter not extracted: %q", fm[:200])
	}
	if strings.Contains(fm, "The rain began") {
		t.Fatalf("front matter bled into chapter one: %q", fm)
	}
}

func TestExtractFrontMatterPrefersPreface(t *testing.T) {
	text := `Foreword
A few words from a friend of the author that go on for quite a while
about how we met and what this book means to our shared field, which is
in fact rather a lot to say about a book about mud and lanes and rain.

Preface
The preface is the author's own overview and should win over the
foreword because it describes the book's actual argument, which the
foreword merely gestures at from a friendly distance.
`
	fm := ExtractFrontMatter(text)
	if !strings.Contains(fm, "author's own overview") {
		t.Fatalf("expected preface to win over foreword: %q", fm[:120])
	}
}

func TestExtractTOC(t *testing.T) {
	text := `Contents
1 The Rain ..... 3
2 The Field ..... 17
Appendix A ..... 231

Preface
About this book.
`
	toc := ExtractTOC(text)
	if !strings.Contains(toc, "The Rain") || !strings.Contains(toc, "The Field") {
		t.Fatalf("TOC lines not extracted: %q", toc)
	}
	if strings.Contains(toc, "3") && strings.Contains(toc, ".....") {
		t.Fatalf("TOC dot leaders not stripped: %q", toc)
	}
	if strings.Contains(toc, "About this book") {
		t.Fatalf("TOC bled into preface: %q", toc)
	}
}

// A scanned-looking long book: front pages are cover/acknowledgments/
// preface (no "Chapter" headings in the sample), chapters appear later.
// Page count + ISBN + TOC-dot-leaders + front-matter run must carry it.
func TestDetectBookFrontHeavyLong(t *testing.T) {
	// simulate the sample ingest sees: first pages, no chapter in sight
	text := `The Long Road to Mud
Copyright 1997 by Lorem Author

Acknowledgments
For their patience across seven years, thanks to...
(p) ... and the usual suspects who read the drafts.

Contents
1 The Lane ..... 1
2 The Flood ..... 44
3 The Ditch ..... 90

Preface
This book grew out of ten years of walking the same muddy lane...
ISBN 978-0-13-123456-7
`
	text += strings.Repeat("\nChapter Seven\nThe rain, again, and the field under it.\n", 40)
	kind, scores := Detect(text, 300, 312)
	if kind != "book" {
		t.Fatalf("long front-heavy book not classified as book: %v", scores)
	}
}

// A paper's length must not bleed into book territory.
func TestDetectLongPaperStaysPaper(t *testing.T) {
	text := "Abstract\n" + strings.Repeat("We analyse the structure of attention. ", 30) +
		"\n\n1 Introduction\n" + strings.Repeat("[14] citations and doi:10.1234/x ", 8) +
		"\n\nReferences\n" + strings.Repeat("Author, A. Title of the cited work. Journal of Things, 2019.\n", 30)
	kind, scores := Detect(text, 0, 28)
	if kind != "paper" {
		t.Fatalf("long paper not classified as paper: %v", scores)
	}
}

// User kind pins vote on alike documents: one example nudges an
// unconfident call, several flip a confident heuristic.
func TestDetectWithExamplesLearnsFromUserPins(t *testing.T) {
	// heuristic calls this a paper (an Abstract heading → paper=4)
	text := "Abstract\nWe study renormalization and gauge anomalies in " +
		"quantum field theory and their consequences for scattering.\n"
	if k, _ := Detect(text, 0, 20); k != "paper" {
		t.Fatalf("baseline heuristic should call it paper, got %q", k)
	}

	// one strongly-similar example pinned as course: not enough to flip
	one := []Example{{Kind: "course", Title: "QFT lecture notes",
		Text: "renormalization gauge anomalies quantum field theory lecture"}}
	if k, _ := DetectWithExamples(text, 0, 20, one); k != "paper" {
		t.Fatalf("a single example must not override a confident heuristic, got %q", k)
	}

	// two agreeing examples: the user's pins win
	two := []Example{
		{Kind: "course", Title: "QFT lecture notes",
			Text: "renormalization gauge anomalies quantum field theory lecture"},
		{Kind: "course", Title: "QFT problem sets",
			Text: "renormalization gauge anomalies quantum field theory homework"},
	}
	if k, _ := DetectWithExamples(text, 0, 20, two); k != "course" {
		t.Fatalf("two agreeing user pins should win, got %q", k)
	}

	// an unconfident doc (no signal → "") adopts one strong example
	vague := "renormalization gauge anomalies quantum field theory notes"
	if k, _ := Detect(vague, 0, 4); k != "" {
		t.Fatalf("baseline should be unconfident, got %q", k)
	}
	ex := []Example{{Kind: "reference", Title: "QFT handbook",
		Text: "renormalization gauge anomalies quantum field theory handbook"}}
	if k, _ := DetectWithExamples(vague, 0, 4, ex); k != "reference" {
		t.Fatalf("an unconfident doc should adopt a strong example, got %q", k)
	}

	// unrelated examples never vote
	if k, _ := DetectWithExamples(vague, 0, 4,
		[]Example{{Kind: "book", Title: "Gardening", Text: "soil compost roses"}},
	); k != "" {
		t.Fatalf("unrelated examples must not vote, got %q", k)
	}
}
