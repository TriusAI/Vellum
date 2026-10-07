// Package classify detects a document's KIND (paper, book, gallery,
// course, reference) from its structure — a deterministic, instant,
// zero-training first pass. Heuristics score structural signals in the
// extracted text; the result can be overridden by the user (documents.kind
// is a free string) and used to pick a fast processing path.
//
// The initial kinds:
//
//	paper    arXiv/journal style; expects an Abstract section
//	book     monograph/textbook: front matter, chapters, publisher page
//	gallery  mostly images: scans, art, photo collections
//	course   slides, syllabi, lecture notes, problem sets
//	reference handbook/encyclopedia/dictionary/manual style
//
// kind is "" when no signal is confident enough (generic processing).
package classify

import (
	"math"
	"regexp"
	"strings"
)

// Scored is one kind with its heuristic score (for debugging/UI display).
type Scored struct {
	Kind  string
	Score float64
}

var (
	reAbstract   = regexp.MustCompile(`(?im)^[\s]*abstract\b`)
	reArxiv      = regexp.MustCompile(`(?i)arxiv[:\s]`)
	reReferences = regexp.MustCompile(`(?im)^[\s]*(references|bibliography)\b`)
	reCite       = regexp.MustCompile(`\[\d+\]`)
	reDOI        = regexp.MustCompile(`(?i)\bdoi:|\bhttps?://doi\.org/`)
	reKeywords   = regexp.MustCompile(`(?im)^[\s]*keywords?[:.]`)
	reChapter    = regexp.MustCompile(`(?im)^[\s]*chapter\s+(one|two|three|\d+)`)
	reChapterN   = regexp.MustCompile(`(?m)^\s*(chapter\s+(one|two|three|\d+)|[0-9]{1,2}\.\s+\S.{4,80}$)`)
	rePublisher  = regexp.MustCompile(
		`(?i)all rights reserved|published by|isbn[\s:]?\d|` +
			`\blibrary of congress|printed in [a-z]|copyright ©?|©\s*(19|20)\d\d`)
	reContents   = regexp.MustCompile(`(?im)^[\s]*(table of )?contents\b`)
	reTOCLine    = regexp.MustCompile(`(?m)^\s*\S.{2,90}[.·]{2,}\s*\d+\s*$`)
	reFrontSec   = regexp.MustCompile(`(?im)^[\s]*(preface|foreword|acknowledge?ments?|introduction|prologue)\b`)
	reISBN       = regexp.MustCompile(`(?i)isbn[\s:]*97[89][0-9-]{9,}`)
	reGalleryImg = regexp.MustCompile(`(?i)figure\s+\d+`)
	reCourse     = regexp.MustCompile(`(?i)\b(lecture|syllabus|homework|problem set|instructor|office hours|semester|midterm|final exam)\b`)
	reDue        = regexp.MustCompile(`(?i)\bdue[:\s]`)
	reRef        = regexp.MustCompile(`(?i)\b(handbook|encyclopedia|dictionary|manual|companion)\b`)
	reAppendix   = regexp.MustCompile(`(?im)^[\s]*appendix\b`)
)

// Example is a document the user explicitly classified (kind_user): a
// teaching signal. Its Kind votes for structurally/topically similar
// documents, so corrections propagate.
type Example struct {
	Kind  string
	Title string
	Text  string // a short sample (title page / opening text)
}

// Detect scores the kind from the EXTRACTED TEXT (the full text layer;
// large documents are capped internally — the leading ~40k chars carry
// title/front-matter signal, the trailing ~20k carry colophon and
// references) plus page statistics (ocrPages = pages that needed OCR,
// nPages = total page count). Page count is a first-class signal:
// papers are small, monographs are not.
func Detect(text string, ocrPages, nPages int) (kind string, scores []Scored) {
	return DetectWithExamples(text, ocrPages, nPages, nil)
}

// DetectWithExamples is Detect plus the user's explicit kind pins as
// few-shot votes: each strongly-similar example adds weight to its kind,
// so a corrected document teaches the classifier about alike documents
// (deterministic, no model call). A lone example nudges an unconfident
// call; overriding a confident heuristic takes agreement from several.
func DetectWithExamples(text string, ocrPages, nPages int, examples []Example) (kind string, scores []Scored) {
	scores = scoreKinds(text, ocrPages, nPages)
	if len(examples) > 0 {
		docSample := head(text, 6000)
		add := map[string]float64{}
		for _, ex := range examples {
			w := matchWeight(docSample, ex.Title+" "+ex.Text)
			if w > 0 {
				add[ex.Kind] += w
			}
		}
		for i := range scores {
			if v := add[scores[i].Kind]; v > 0 {
				scores[i].Score += v
			}
		}
	}
	best := ""
	bestScore := 0.0
	for _, s := range scores {
		if s.Score > bestScore {
			best, bestScore = s.Kind, s.Score
		}
	}
	if bestScore < 3.5 {
		return "", scores // not confident: generic processing
	}
	return best, scores
}

// head returns the first n bytes of s (UTF-8 safe enough for matching).
func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func scoreKinds(text string, ocrPages, nPages int) (scores []Scored) {
	t := text
	if len(t) > 40000 {
		t = t[:40000]
	}
	// also sample the tail: references/colophon live at the end
	if len(text) > 60000 {
		t += "\n" + text[len(text)-20000:]
	}
	// full text for counting structural repeats (chapters, ISBN-like
	// strings spread across the book) — capped to keep the scan cheap
	full := text
	if len(full) > 2_000_000 {
		full = full[:2_000_000]
	}
	tl := strings.ToLower(t)

	var paper, book, gallery, course, reference float64

	// --- paper signals
	if reAbstract.MatchString(t) {
		paper += 4
	}
	if reArxiv.MatchString(t) {
		paper += 3
	}
	if reReferences.MatchString(t) {
		paper += 2
		reference += 0.5 // reference lists exist in handbooks too
	}
	if n := len(reCite.FindAllString(t, -1)); n >= 4 {
		paper += math.Min(3, float64(n)/4)
	}
	if reDOI.MatchString(t) {
		paper += 1.5
	}
	if reKeywords.MatchString(t) {
		paper += 1
	}

	// --- book signals
	if n := len(reChapterN.FindAllString(full, -1)); n >= 2 {
		book += math.Min(4, 1.5+float64(n)/3)
	} else if reChapter.MatchString(t) {
		book += 3
	}
	if n := len(reTOCLine.FindAllString(full, -1)); n >= 6 {
		// a real table of contents: many "title ..... 12" lines
		book += 2
	}
	if n := len(reFrontSec.FindAllString(full, -1)); n >= 2 {
		// preface/foreword/acknowledgments/introduction/prologue —
		// the classic front-matter run of a monograph
		book += 1.5
	}
	if reISBN.MatchString(full) {
		book += 2
	}
	if rePublisher.MatchString(t) {
		book += 2
	}
	if reContents.MatchString(t) {
		book += 1
		course += 0.2
	}
	switch {
	case nPages >= 120:
		book += 3 // papers don't run this long
	case nPages >= 60:
		book += 1.5
	}

	// --- gallery signals: mostly images, thin text
	if nPages > 0 {
		imgRatio := float64(ocrPages) / float64(nPages)
		charsPerPage := float64(len(text)) / float64(nPages)
		if imgRatio > 0.6 && charsPerPage < 600 {
			gallery += 5
		} else if imgRatio > 0.6 {
			book += 1 // OCR'd scan, dense text: likely a scanned book
		}
		if n := len(reGalleryImg.FindAllString(t, -1)); n >= 6 && charsPerPage < 1200 {
			gallery += math.Min(3, float64(n)/6)
		}
	}

	// --- course signals
	if n := len(reCourse.FindAllString(t, -1)); n >= 3 {
		course += math.Min(4, float64(n))
	}
	if reDue.MatchString(t) {
		course += 1
	}

	// --- reference signals
	if n := len(reRef.FindAllString(t, -1)); n >= 1 {
		reference += math.Min(3, float64(n))
	}
	if reAppendix.MatchString(t) {
		reference += 1
		book += 0.5
	}

	_ = tl
	return []Scored{
		{"paper", paper}, {"book", book}, {"gallery", gallery},
		{"course", course}, {"reference", reference},
	}
}

// kindStopwords: words too generic to indicate a document's kind.
var kindStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "are": true, "was": true, "were": true,
	"into": true, "about": true, "their": true, "which": true,
	"how": true, "what": true, "why": true, "can": true, "its": true,
	"one": true, "two": true, "new": true, "study": true, "using": true,
	"paper": true, "document": true, "introduction": true, "abstract": true,
	"chapter": true, "section": true, "figure": true, "table": true,
	"page": true, "pages": true, "volume": true, "journal": true,
}

// matchWeight scores how strongly an example matches the document under
// classification: distinctive shared terms, saturating. 0 means no
// usable overlap.
func matchWeight(doc, example string) float64 {
	docTerms := map[string]bool{}
	for _, t := range kindTokens(doc) {
		docTerms[t] = true
	}
	seen := map[string]bool{}
	shared := 0
	for _, t := range kindTokens(example) {
		if docTerms[t] && !seen[t] {
			seen[t] = true
			shared++
		}
	}
	if shared < 2 {
		return 0
	}
	// A strong match is worth enough to make an otherwise-unconfident
	// document confident (3.0 + growth), but one alone ties a confident
	// heuristic (4.0) and loses to it — overriding a confident call
	// takes agreement from two or more examples.
	w := 3.0 + 0.25*float64(shared-2)
	if w > 4.0 {
		w = 4.0
	}
	return w
}

// kindTokens lowercases and splits into word tokens (length > 2,
// non-stopword).
func kindTokens(s string) []string {
	out := []string{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r < 128
	}) {
		if len(t) > 2 && !kindStopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// stopHeadings are the section headings that terminate an Abstract.
var abstractStoppers = regexp.MustCompile(
	`(?im)^[\s]*(introduction|1[\s.]+introduction|i\.?\s+introduction|` +
		`keywords?|index terms|acm reference format|ccs concepts|` +
		`references|related work|background|1[\s.]+(related|preliminaries)|` +
		`acknowledge?ments|resumen|résumé)\b`)

// ExtractAbstract pulls the Abstract section out of paper text: from the
// "Abstract" heading to the next section heading, capped at ~3000 chars.
// Returns "" when no abstract heading is found.
func ExtractAbstract(text string) string {
	loc := reAbstract.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	rest := text[loc[1]:]
	// skip the heading line remainder (e.g. "Abstract—We ...", "ABSTRACT\n")
	if i := strings.Index(rest, "\n"); i >= 0 && i < 400 {
		line := rest[:i]
		// "Abstract—..." style puts content on the same line
		if trimmed := strings.TrimLeft(line, "—-– \t"); len(trimmed) > 80 {
			rest = trimmed + rest[i:]
		}
	}
	if end := abstractStoppers.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	} else if len(rest) > 3000 {
		rest = rest[:3000]
	}
	out := strings.TrimSpace(rest)
	// trim leading dash style
	out = strings.TrimLeft(out, "—-– \n\t")
	if len(out) < 150 {
		// too short to be a real abstract (probably a stray heading)
		return ""
	}
	return out
}

// front-matter headings terminate at chapter-like structure.
var (
	rePreface      = regexp.MustCompile(`(?im)^[\s]*preface\b`)
	reForeword     = regexp.MustCompile(`(?im)^[\s]*(foreword|acknowledge?ments?)\b`)
	reIntro        = regexp.MustCompile(`(?im)^[\s]*(introduction|prologue|about this (book|work))\b`)
	reChapterStart = regexp.MustCompile(
		`(?im)^[\s]*(chapter|part|appendix)\b|^[\s]*1\s+[\w(]`)
)

// ExtractFrontMatter pulls a book's overview from its front matter, in
// preference order: Preface, then Foreword, then Introduction. This is the
// author's own description of the book — better than a generated summary,
// and free. Returns "" when nothing is found (the caller then falls back
// to full map-reduce summarization).
func ExtractFrontMatter(text string) string {
	for _, re := range []*regexp.Regexp{rePreface, reForeword, reIntro} {
		loc := re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		rest := text[loc[1]:]
		if end := reChapterStart.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		if len(rest) > 7000 {
			rest = rest[:7000]
		}
		out := strings.TrimSpace(rest)
		if len(out) < 400 {
			continue // stray heading, not a real section
		}
		return out
	}
	return ""
}

var reTOCPage = regexp.MustCompile(`(?im)^[\s]*(table of )?contents\b`)

var (
	reDotLeader = regexp.MustCompile(`[.·—-]{2,}\s*\d+\s*$`)
	reTrailingN = regexp.MustCompile(`\s+\d+$`)
)

// ExtractTOC pulls the contents listing (chapter/section titles) out of a
// book — used as a topic hint for tagging, not as a summary.
func ExtractTOC(text string) string {
	loc := reTOCPage.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	rest := text[loc[1]:]
	// TOC entries are often numbered lines, so the chapter-start pattern
	// must NOT be a stop here — stop at the next prose section instead
	// (a TOC is typically followed by the preface/foreword/introduction).
	for _, end := range []*regexp.Regexp{rePreface, reForeword, reIntro} {
		if m := end.FindStringIndex(rest); m != nil {
			rest = rest[:m[0]]
		}
	}
	if len(rest) > 4000 {
		rest = rest[:4000]
	}
	lines := []string{}
	for _, line := range strings.Split(rest, "\n") {
		cleaned := reDotLeader.ReplaceAllString(line, "")
		cleaned = strings.TrimSpace(reTrailingN.ReplaceAllString(cleaned, ""))
		if cleaned == "" || len(cleaned) < 3 {
			continue
		}
		lines = append(lines, cleaned)
		if len(lines) >= 60 {
			break
		}
	}
	return strings.Join(lines, "\n")
}
