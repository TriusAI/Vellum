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