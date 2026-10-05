// Package extract pulls text out of library files.
//
// Strategy per page of a PDF/EPUB (mirroring the retired Python version):
//  1. take the embedded text layer (mutool draw -F text);
//  2. if a page has fewer than min_chars_per_page extractable characters but
//     contains images, treat it as scanned: rasterize (mutool draw) and run
//     Tesseract on the bitmap.
//
// Plain text files are read directly. EPUB/MOBI/FB2 are first converted to
// PDF by mutool convert.
package extract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/ocrimg"
)

var (
	textExt    = map[string]bool{".txt": true, ".md": true, ".markdown": true, ".rst": true}
	pdfLike    = map[string]bool{".pdf": true, ".epub": true, ".mobi": true, ".azw": true, ".azw3": true, ".fb2": true}
	whitespace = regexp.MustCompile(`[ \t]+\n|\n{3,}`)
)

// Result of extracting one document.
type Result struct {
	Chunks   []db.Chunk
	Title    string
	Authors  string
	OCRPages int
	// NeedsOCR reports whether some pages carry too little usable text
	// (only set by ExtractText — the quick, OCR-less pass ingest uses).
	NeedsOCR bool
}

// Supported reports whether Vellum can ingest this file.
func Supported(path string) bool {
	return textExt[strings.ToLower(filepath.Ext(path))] ||
		pdfLike[strings.ToLower(filepath.Ext(path))]
}

func clean(s string) string {
	return strings.TrimSpace(whitespace.ReplaceAllString(s, "\n\n"))
}

func run(bin string, args ...string) ([]byte, error) {
	// generous per-call timeout: tesseract on dense CJK pages takes
	// minutes legitimately, but a wedged subprocess must not hang an
	// ingest/process run forever
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s: %s", bin, msg)
		}
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out, nil
}

// Extract extracts text from one file, OCR-ing pages that need it
// (low-text pages with image content, plus pages whose embedded text
// layer is junk).
func Extract(path string, cfg *config.Config) (*Result, error) {
	text, err := ExtractText(path, cfg)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] {
		return text, nil
	}
	if !pdfLike[ext] {
		return nil, fmt.Errorf("unsupported file type: %s", ext)
	}
	return ocrize(path, cfg, text, false, nil)
}

// ExtractOCR re-extracts with raster forced over the embedded text
// layer on every page — the repair path for documents with a garbled
// or unusable text layer (auto mode only re-OCRs pages that LOOK
// broken; force distrusts all of them).
func ExtractOCR(path string, cfg *config.Config) (*Result, error) {
	return ExtractOCRPages(path, cfg, true, nil)
}

// ExtractOCRPages is the per-page repair variant: force=true with pages
// set OCRs exactly those pages (1-based, matching chunk.Page), leaving
// every other page's text layer alone; pages=nil with force OCRs all.
func ExtractOCRPages(path string, cfg *config.Config, force bool, pages []int) (*Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] {
		return extractPlainText(path, cfg)
	}
	if !pdfLike[ext] {
		return nil, fmt.Errorf("unsupported file type: %s", ext)
	}
	text, err := ExtractText(path, cfg)
	if err != nil {
		return nil, err
	}
	return ocrize(path, cfg, text, force, pages)
}

func extractPlainText(path string, cfg *config.Config) (*Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	chunkChars := cfg.Summarize.ChunkChars
	var chunks []db.Chunk
	for i, start := 0, 0; start < len(s); i, start = i+1, start+chunkChars {
		end := start + chunkChars
		if end > len(s) {
			end = len(s)
		}
		chunks = append(chunks, db.Chunk{Seq: i, Text: clean(s[start:end])})
	}
	return &Result{Chunks: chunks}, nil
}

// ExtractText is the QUICK path: text layer only — no rasterizing, no
// OCR, no geometry analysis. Ingest runs this so indexing stays fast;
// pages that would need OCR are reported via NeedsOCR and the work is
// deferred to the processing phase (processOne), where progress can
// be reported while tesseract grinds.
func ExtractText(path string, cfg *config.Config) (*Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] {
		return extractPlainText(path, cfg)
	}
	if !pdfLike[ext] {
		return nil, fmt.Errorf("unsupported file type: %s", ext)
	}
	tmp, err := os.MkdirTemp("", "vellum-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	pdfPath := path
	if ext != ".pdf" {
		pdfPath = filepath.Join(tmp, "converted.pdf")
		if _, err := run(cfg.Tools.Mutool, "convert", "-o", pdfPath, path); err != nil {
			return nil, fmt.Errorf("conversion to PDF failed: %w", err)
		}
	}

	pagesText, err := mutoolPagesText(cfg.Tools.Mutool, pdfPath)
	if err != nil {
		return nil, err
	}
	title, authors := mutoolMeta(cfg.Tools.Mutool, pdfPath)

	chunks := make([]db.Chunk, len(pagesText))
	needsOCR := false
	for i, text := range pagesText {
		trimmed := strings.TrimSpace(text)
		insufficient := len(trimmed) < cfg.OCR.MinCharsPerPage
		// An embedded text layer can be BROKEN — scans processed by
		// some other tool's bad OCR pass. Garbage in the layer is
		// worse than no layer: it poisons summaries, tags, and search.
		insane := !insufficient && !textLayerSane(trimmed)
		if insufficient || insane {
			needsOCR = true
		}
		chunks[i] = db.Chunk{Seq: i, Page: i + 1, Text: clean(text)}
	}
	return &Result{
		Chunks:   chunks,
		Title:    title,
		Authors:  authors,
		NeedsOCR: needsOCR,
	}, nil
}

// ocrize runs the full OCR pass (with scan geometry) over the pages of
// a quick-extracted document, replacing thin/garbled chunks in place.
// force/pages: all pages, a page subset (per-page repair), or the
// auto-selected broken ones.
func ocrize(path string, cfg *config.Config, quick *Result, force bool, pages []int) (*Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] || !pdfLike[ext] {
		return quick, nil
	}
	tmp, err := os.MkdirTemp("", "vellum-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	pdfPath := path
	if ext != ".pdf" {
		pdfPath = filepath.Join(tmp, "converted.pdf")
		if _, err := run(cfg.Tools.Mutool, "convert", "-o", pdfPath, path); err != nil {
			return quick, fmt.Errorf("conversion to PDF failed: %w", err)
		}
	}

	chunks := quick.Chunks
	needsOCR := []int{}
	if force {
		if pages == nil {
			for i := range chunks {
				needsOCR = append(needsOCR, i)
			}
		} else {
			// per-page repair (1-based page numbers as stored on
			// chunks as Page; the user reads them in the Text tab)
			for _, pg := range pages {
				if pg >= 1 && pg <= len(chunks) {
					needsOCR = append(needsOCR, pg-1)
				}
			}
			sort.Ints(needsOCR)
		}
	} else {
		for i, c := range chunks {
			trimmed := strings.TrimSpace(c.Text)
			insufficient := len(trimmed) < cfg.OCR.MinCharsPerPage
			insane := !insufficient && !textLayerSane(trimmed)
			if insufficient || insane {
				// OCR only if the page actually contains image XObjects
				// (verified: `mutool show file 'pages.N.Resources.XObject.*'`
				// lists XObjects, or prints null when there are none).
				if pageHasImage(cfg.Tools.Mutool, pdfPath, i+1) || insufficient {
					needsOCR = append(needsOCR, i)
				}
			}
		}
	}

	ocrPages := 0
	if len(needsOCR) > 0 {
		ocrPages = ocrPagesIn(cfg, pdfPath, tmp, needsOCR, chunks)
	}
	if quick.OCRPages == 0 {
		quick.OCRPages = ocrPages
	}
	return quick, nil
}

// mutoolPagesText returns the text of each page. Each page block ends with
// "\f\n" (verified against mutool 1.28.5), so the split always yields a
// trailing empty element — it's dropped here.
func mutoolPagesText(mutool, pdfPath string) ([]string, error) {
	out, err := run(mutool, "draw", "-F", "text", "-o", "-", pdfPath)
	if err != nil {
		return nil, fmt.Errorf("text extraction failed: %w", err)
	}
	parts := strings.Split(string(out), "\f")
	if n := len(parts); n > 0 && strings.TrimSpace(parts[n-1]) == "" {
		parts = parts[:n-1]
	}
	return parts, nil
}

// pageHasImage reports whether a page has image XObjects.
func pageHasImage(mutool, pdfPath string, page int) bool {
	out, err := run(mutool, "show", pdfPath,
		fmt.Sprintf("pages.%d.Resources.XObject.*", page))
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, "/Subtype /Image") ||
		strings.Contains(s, "/Subtype/Image")
}

// mutoolMeta reads the Info dictionary via the trailer.Info path
// ("null" when the PDF has no metadata — that's fine, the LLM pass fills
// gaps).
func mutoolMeta(mutool, pdfPath string) (title, authors string) {
	out, err := run(mutool, "show", pdfPath, "trailer.Info")
	if err != nil || strings.TrimSpace(string(out)) == "null" {
		return "", ""
	}
	s := string(out)
	return pdfString(s, "/Title"), pdfString(s, "/Author")
}

// pdfString pulls a /Key (...) value from a mutool show dump (loose parse;
// metadata is best-effort — the LLM pass fills gaps).
func pdfString(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			rest := strings.TrimSpace(line[i+len(key):])
			if strings.HasPrefix(rest, "(") {
				if j := strings.Index(rest[1:], ")"); j >= 0 {
					return cleanMetaValue(rest[1 : 1+j])
				}
			}
		}
	}
	return ""
}

// cleanMetaValue drops junk placeholder values that PDF producers commonly
// emit ("unknown", "untitled", ...) — they are worse than nothing.
func cleanMetaValue(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "unknown", "untitled", "unspecified", "anonymous", "none",
		"n/a", "na", "null":
		return ""
	}
	return strings.TrimSpace(s)
}

// ocrPagesIn OCRs the given pages in parallel, filling chunks in place.
// Returns how many OCR passes produced text. Scan geometry is handled
// per page: two-up spreads (open-book scans) are detected and split at
// the gutter, and rotated scans are turned upright (tesseract OSD when
// available, ink-profile heuristics otherwise) before recognition.
func ocrPagesIn(cfg *config.Config, pdfPath, tmpdir string, pages []int, chunks []db.Chunk) int {
	tessdataPrefix(cfg)

	langs := cfg.OCR.Langs
	if missing := missingLangs(cfg.Tools.Tesseract, langs); len(missing) > 0 {
		fmt.Fprintf(os.Stderr,
			"vellum: tesseract traineddata missing for %v — see README (tessdata/)\n",
			missing)
		return 0
	}
	osdOK := osdAvailable(cfg.Tools.Tesseract)

	sem := make(chan struct{}, cfg.OCR.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for _, pageIdx := range pages {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pgmPath := filepath.Join(tmpdir, fmt.Sprintf("p-%d.pgm", idx+1))
			if _, err := run(cfg.Tools.Mutool, "draw", "-F", "pgm", "-r",
				strconv.Itoa(cfg.OCR.DPI), "-o", pgmPath, pdfPath,
				strconv.Itoa(idx+1)); err != nil {
				return
			}
			data, err := os.ReadFile(pgmPath)
			if err != nil {
				return
			}
			im, err := ocrimg.DecodePGM(data)
			if err != nil {
				return
			}
			text, passes := ocrBitmap(cfg, langs, osdOK, im, tmpdir,
				fmt.Sprintf("p-%d", idx+1))
			if text == "" {
				return
			}
			chunks[idx].Text = text
			mu.Lock()
			done += passes
			mu.Unlock()
		}(pageIdx)
	}
	wg.Wait()
	return done
}

// ocrBitmap runs the scan-geometry pipeline on one page bitmap and
// returns the recognized text plus how many raster passes produced
// text (a detected spread counts its two halves separately).
//
// Pipeline: (1) open-book spreads are detected geometrically (a
// near-white gutter column at page center flanked by ink) and split
// into halves; (2) otherwise the whole page goes through orientation
// arbitration — candidates ORDERED by cheap signals (tesseract OSD
// when its traineddata is present, else ink-profile hints), each
// candidate OCR'd and scored against a common-word dictionary, first
// pass scoring >= orientAcceptThreshold wins, best-of kept otherwise;
// (3) an un-rotated sideways spread is re-checked and split there;
// (4) halves go through the same orientation arbitration (0/180).
//
// The dictionary arbitration is what makes orientation CORRECTNESS
// content-independent: the cheap hints proved reliable for ordering
// but flip their absolute sign across fonts/DPI/glyph mixes, so they
// are hints — a wrong hint costs one extra OCR pass, never accuracy.
func ocrBitmap(cfg *config.Config, langs string, osdOK bool,
	im *ocrimg.Image, tmpdir, id string) (string, int) {
	parts := []ocrPart{{im: im, path: filepath.Join(tmpdir, id+".pgm")}}

	// open-book layout: two pages in one image, gutter near the center
	if at, ok := ocrimg.DetectSpine(im); ok {
		l, r := im.SplitTwoUp(at)
		parts = []ocrPart{
			{im: l, path: filepath.Join(tmpdir, id+"-l.pgm")},
			{im: r, path: filepath.Join(tmpdir, id+"-r.pgm")},
		}
		text, n := ocrParts(cfg, langs, osdOK, parts)
		return text, n
	}

	// no spread signature: whole-page orientation arbitration first
	// (sideways scans must be un-rotated before the gutter shows)
	oriented, text := ocrPartOriented(cfg, langs, osdOK, parts[0])
	if at, ok := ocrimg.DetectSpine(oriented); ok {
		// sideways spread: now upright, the gutter is visible — split
		// and refine the halves (their text beats the fused-column one)
		l, r := oriented.SplitTwoUp(at)
		halves, n := ocrParts(cfg, langs, osdOK, []ocrPart{
			{im: l, path: filepath.Join(tmpdir, id+"-l.pgm")},
			{im: r, path: filepath.Join(tmpdir, id+"-r.pgm")},
		})
		if n > 0 {
			return halves, n
		}
	}
	if text == "" {
		return "", 0
	}
	return text, 1
}

// ocrPart is one image going through OCR with its temp-file path.
type ocrPart struct {
	im   *ocrimg.Image
	path string
}

// ocrParts runs orientation arbitration per image and joins the text.
func ocrParts(cfg *config.Config, langs string, osdOK bool, parts []ocrPart) (string, int) {
	texts := []string{}
	n := 0
	for _, p := range parts {
		_, text := ocrPartOriented(cfg, langs, osdOK, p)
		if text != "" {
			texts = append(texts, text)
			n++
		}
	}
	return strings.Join(texts, "\n\n"), n
}

// orientAcceptThreshold: an OCR pass scoring at least this on word
// quality is trusted outright; lower passes only win by comparison.
const orientAcceptThreshold = 0.40

// ocrPartOriented runs the orientation arbitration on one image: each
// candidate rotation is written out and OCR'd; the winning image and
// its text come back.
func ocrPartOriented(cfg *config.Config, langs string, osdOK bool, p ocrPart) (*ocrimg.Image, string) {
	if !writePGM(p.im, p.path) {
		return p.im, ""
	}
	cands := orientCandidates(cfg, p.path, p.im, osdOK)
	if len(cands) == 0 {
		cands = []int{0}
	}
	best, bestText := p.im, ""
	bestScore := -1.0
	for _, deg := range cands {
		img := p.im
		if deg != 0 {
			img = p.im.Rotate(deg)
		}
		if !writePGM(img, p.path) { // always write: file may hold a prior pass
			break
		}
		out, err := run(cfg.Tools.Tesseract, p.path, "stdout",
			"-l", langs, "--dpi", strconv.Itoa(cfg.OCR.DPI))
		if err != nil {
			continue
		}
		text := clean(string(out))
		if text == "" {
			continue
		}
		score := ocrQuality(text)
		if score > bestScore {
			best, bestText, bestScore = img, text, score
		}
		if score >= orientAcceptThreshold {
			break // confident; no arbitration pass needed
		}
	}
	return best, bestText
}

// orientCandidates returns the rotations (clockwise fix degrees) to
// try, best guess first. OSD reads actual glyph shapes and is trusted
// alone when confident; the ink-profile hints only ORDER the
// candidates (their absolute sign proved unreliable across content).
func orientCandidates(cfg *config.Config, pgmPath string, im *ocrimg.Image, osdOK bool) []int {
	if osdOK {
		if deg, ok := osdRotate(cfg.Tools.Tesseract, pgmPath, cfg.OCR.DPI); ok {
			return []int{deg}
		}
	}
	// axis: which profile carries the line rhythm (text-line pitch)
	rows := ocrimg.RhythmScore(im.RowFractions(110))
	cols := ocrimg.RhythmScore(im.ColFractions(110))
	if cols > rows*1.3 && cols >= 0.15 {
		// text lines run vertically: a quarter turn fixes them; the
		// cw candidate's MassAsym sign hints which side
		a := ocrimg.MassAsym(im.Rotate(90), 110)
		if a > 0 {
			return []int{90, 270}
		}
		return []int{270, 90}
	}
	if rows >= 0.15 {
		// horizontal text: 0 vs 180; positive hint = upright first
		a := ocrimg.MassAsym(im, 110)
		if a < 0 {
			return []int{180, 0}
		}
		return []int{0, 180}
	}
	return nil // no structure: single try at 0°
}

var (
	reOOrientation = regexp.MustCompile(`Orientation in degrees:\s*(\d+)`)
	reOConfidence  = regexp.MustCompile(`Orientation confidence:\s*([\d.]+)`)
)

// osdRotate parses tesseract's OSD output (--psm 0): the "Rotate:"
// value is the clockwise correction to apply; low confidence (short
// pages, decorative scans) means abstain.
func osdRotate(tesseract, pgmPath string, dpi int) (int, bool) {
	out, err := run(tesseract, pgmPath, "stdout", "-l", "osd",
		"--psm", "0", "--dpi", strconv.Itoa(dpi))
	if err != nil {
		return 0, false // "too few characters" and friends
	}
	s := string(out)
	c := reOConfidence.FindStringSubmatch(s)
	if c == nil {
		return 0, false
	}
	conf, _ := strconv.ParseFloat(c[1], 64)
	if conf < 3.0 {
		return 0, false
	}
	m := reOOrientation.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	deg, _ := strconv.Atoi(m[1])
	if deg != 0 && deg != 90 && deg != 180 && deg != 270 {
		return 0, false
	}
	return deg, true
}

// osdAvailable reports whether the osd traineddata can be loaded
// (checked once per document; OSD refuses to run without it).
func osdAvailable(tesseract string) bool {
	if v, ok := osdAvailableCache.Load(tesseract + "|" + os.Getenv("TESSDATA_PREFIX")); ok {
		return v.(bool)
	}
	ok := false
	if out, err := run(tesseract, "--list-langs"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == "osd" {
				ok = true
				break
			}
		}
	}
	osdAvailableCache.Store(tesseract+"|"+os.Getenv("TESSDATA_PREFIX"), ok)
	return ok
}

var osdAvailableCache sync.Map

// writePGM serializes im as a binary PGM (tesseract and OSD both read
// plain PGM files directly — no PNG dependency).
func writePGM(im *ocrimg.Image, path string) bool {
	var b bytes.Buffer
	fmt.Fprintf(&b, "P5\n%d %d\n255\n", im.W, im.H)
	b.Write(im.Pix)
	return os.WriteFile(path, b.Bytes(), 0o644) == nil
}

// tessdataPrefix points Tesseract at a project-local tessdata dir if one
// exists (no root needed to add languages: drop .traineddata files there).
func tessdataPrefix(cfg *config.Config) {
	if cfg.Tools.Tessdata != "" {
		os.Setenv("TESSDATA_PREFIX", cfg.Tools.Tessdata)
		return
	}
	for _, cand := range []string{
		filepath.Join(cfg.BaseDir, "tessdata"),
	} {
		if entries, err := os.ReadDir(cand); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".traineddata") {
					os.Setenv("TESSDATA_PREFIX", cand)
					return
				}
			}
		}
	}
}

// missingLangs verifies the requested tesseract languages are installed.
func missingLangs(tesseract, langs string) []string {
	out, err := run(tesseract, "--list-langs")
	if err != nil {
		return []string{"(cannot query tesseract)"}
	}
	available := map[string]bool{}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			continue // header line
		}
		available[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, l := range strings.Split(langs, "+") {
		if l != "" && l != "osd" && !available[l] {
			missing = append(missing, l)
		}
	}
	return missing
}

// textLayerSane is a cheap plausibility check for an embedded text
// layer: recognizable characters (no glyph-code garbage) and word-like
// structure (broken word spacing is the classic bad-OCR signature).
// Used to decide whether a page's embedded text can be trusted or the
// page should be re-OCR'd from its raster.
func textLayerSane(s string) bool {
	if len(s) < cfgMinSaneChars {
		return false
	}
	bad := 0
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			!unicode.IsSpace(r) && !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			bad++
		}
	}
	if float64(bad)/float64(len(s)) > garbleMaxRatio {
		return false
	}
	// word structure: most alphabetic characters belong to words of
	// 3+ letters ("thequickbrown" = 0 would fail)
	letters, wordish := 0, 0
	for _, match := range reWord.FindAllString(s, -1) {
		wordish += len(match)
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	if letters == 0 {
		return false // pure numbers/punct: not prose
	}
	if float64(wordish)/float64(letters) < wordishMinRatio {
		return false
	}
	// broken spacing is the classic bad-OCR signature: prose has
	// short tokens; concatenated words make one endless "token".
	// (CJK text has no Word spaces at all and fails here — accepted
	// trade-off: CJK pages with images get re-OCR'd, text-only CJK
	// pages keep their layer.)
	tokens := strings.Fields(s)
	total := 0
	for _, tok := range tokens {
		total += len(tok)
	}
	if len(tokens) == 0 || total/len(tokens) > tokenMaxLen {
		return false
	}
	return true
}

const (
	cfgMinSaneChars = 40
	garbleMaxRatio  = 0.04
	wordishMinRatio = 0.55
	tokenMaxLen     = 14
)

var reWord = regexp.MustCompile(`[A-Za-zÀ-ž]{3,}`)

// KindOf classifies a path's family for feature gating ("pdf" =
// rasterizable PDF-like formats, "text" = plain text, "" = unknown).
func KindOf(ext string) string {
	switch strings.ToLower(ext) {
	case ".pdf", ".epub", ".mobi", ".azw", ".azw3", ".fb2":
		return "pdf"
	case ".txt", ".md", ".markdown", ".rst":
		return "text"
	}
	return ""
}

// RenderPagePNG draws one page (1-based) of a PDF-like file to dst at
// the given DPI — used for cover thumbnails. EPUB/MOBI/FB2 convert
// first, so the first call for a book costs one conversion.
func RenderPagePNG(cfg *config.Config, path string, page, dpi int, dst string) error {
	if KindOf(filepath.Ext(path)) != "pdf" {
		return fmt.Errorf("unsupported for raster rendering: %s", filepath.Ext(path))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "vellum-render-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	pdfPath := path
	if strings.ToLower(filepath.Ext(path)) != ".pdf" {
		pdfPath = filepath.Join(tmp, "converted.pdf")
		if _, err := run(cfg.Tools.Mutool, "convert", "-o", pdfPath, path); err != nil {
			return fmt.Errorf("convert: %w", err)
		}
	}
	outPath := filepath.Join(tmp, fmt.Sprintf("p%d.png", page))
	if _, err := run(cfg.Tools.Mutool, "draw", "-F", "png", "-r",
		strconv.Itoa(dpi), "-o", outPath, pdfPath, strconv.Itoa(page)); err != nil {
		return err
	}
	data, err := os.ReadFile(outPath)
	if err != nil || len(data) < 8 {
		return fmt.Errorf("empty render")
	}
	tmpDst := dst + ".tmp"
	if err := os.WriteFile(tmpDst, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpDst, dst)
}
