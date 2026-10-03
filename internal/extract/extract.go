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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"vellum/internal/config"
	"vellum/internal/db"
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
	cmd := exec.Command(bin, args...)
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

// Extract extracts text from one file.
func Extract(path string, cfg *config.Config) (*Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if textExt[ext] {
		return extractPlainText(path, cfg)
	}
	if pdfLike[ext] {
		return extractPDFLike(path, cfg)
	}
	return nil, fmt.Errorf("unsupported file type: %s", ext)
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

func extractPDFLike(path string, cfg *config.Config) (*Result, error) {
	tmp, err := os.MkdirTemp("", "vellum-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	pdfPath := path
	if strings.ToLower(filepath.Ext(path)) != ".pdf" {
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
	needsOCR := []int{}
	for i, text := range pagesText {
		trimmed := strings.TrimSpace(text)
		if len(trimmed) < cfg.OCR.MinCharsPerPage {
			// Low text: OCR only if the page actually contains image XObjects
			// (verified: `mutool show file 'pages.N.Resources.XObject.*'`
			// lists XObjects, or prints null when there are none).
			if pageHasImage(cfg.Tools.Mutool, pdfPath, i+1) || trimmed == "" {
				needsOCR = append(needsOCR, i)
			}
		}
		chunks[i] = db.Chunk{Seq: i, Page: i + 1, Text: clean(text)}
	}

	ocrPages := 0
	if len(needsOCR) > 0 {
		ocrPages = ocrPagesIn(cfg, pdfPath, tmp, needsOCR, chunks)
	}

	return &Result{
		Chunks:   chunks,
		Title:    title,
		Authors:  authors,
		OCRPages: ocrPages,
	}, nil
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
					return rest[1 : 1+j]
				}
			}
		}
	}
	return ""
}

// ocrPagesIn OCRs the given pages in parallel, filling chunks in place.
// Returns how many OCR passes produced text.
func ocrPagesIn(cfg *config.Config, pdfPath, tmpdir string, pages []int, chunks []db.Chunk) int {
	tessdataPrefix(cfg)

	langs := cfg.OCR.Langs
	if missing := missingLangs(cfg.Tools.Tesseract, langs); len(missing) > 0 {
		fmt.Fprintf(os.Stderr,
			"vellum: tesseract traineddata missing for %v — see README (tessdata/)\n",
			missing)
		return 0
	}

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

			png := filepath.Join(tmpdir, fmt.Sprintf("p-%d.png", idx+1))
			if _, err := run(cfg.Tools.Mutool, "draw", "-r",
				strconv.Itoa(cfg.OCR.DPI), "-o", png, pdfPath,
				strconv.Itoa(idx+1)); err != nil {
				return
			}
			out, err := run(cfg.Tools.Tesseract, png, "stdout", "-l", langs)
			if err != nil {
				return
			}
			text := clean(string(out))
			if text == "" {
				return
			}
			chunks[idx].Text = text
			mu.Lock()
			done++
			mu.Unlock()
		}(pageIdx)
	}
	wg.Wait()
	return done
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
