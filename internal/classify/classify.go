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
	rePublisher  = regexp.MustCompile(`(?i)all rights reserved|published by|isbn[\s:]`)
	reContents   = regexp.MustCompile(`(?im)^[\s]*(table of )?contents\b`)
	reGalleryImg = regexp.MustCompile(`(?i)figure\s+\d+`)
	reCourse     = regexp.MustCompile(`(?i)\b(lecture|syllabus|homework|problem set|instructor|office hours|semester|midterm|final exam)\b`)
	reDue        = regexp.MustCompile(`(?i)\bdue[:\s]`)
	reRef        = regexp.MustCompile(`(?i)\b(handbook|encyclopedia|dictionary|manual|companion)\b`)
	reAppendix   = regexp.MustCompile(`(?im)^[\s]*appendix\b`)
)

// Detect scores the kind from full extracted text plus page statistics
// (ocrPages = pages that needed OCR, nPages = total). Text may be
// truncated by the caller for very large documents (the leading ~30k
// chars carry most structural signal).
func Detect(text string, ocrPages, nPages int) (kind string, scores []Scored) {
	t := text
	if len(t) > 40000 {
		t = t[:40000]
	}
	// also sample the tail: references/colophon live at the end
	if len(text) > 60000 {
		t += "\n" + text[len(text)-20000:]
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
	if reChapter.MatchString(t) {
		book += 3
	}
	if rePublisher.MatchString(t) {
		book += 2
	}
	if reContents.MatchString(t) {
		book += 1
		course += 0.2
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
	scores = []Scored{
		{"paper", paper}, {"book", book}, {"gallery", gallery},
		{"course", course}, {"reference", reference},
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
