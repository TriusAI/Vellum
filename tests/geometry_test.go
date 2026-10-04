// Scan-geometry tests: rendered pages (testutil text PDFs rasterized by
// mutool, i.e. REAL glyph shapes) must be recognized as upright, and
// rotated/split variants handled by internal/ocrimg + tesseract.
package tests

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vellum/internal/config"
	"vellum/internal/extract"
	"vellum/internal/ocrimg"
	"vellum/tests/testutil"
)

// geometryText is the body text every geometry fixture shares.
var geomTextLines = []string{
	"On the Classification of Cognitive Machines",
	"",
	"Abstract. This short page exists to be rasterized. The glyphs of",
	"Helvetica carry enough asymmetry for the orientation heuristics to",
	"be calibrated against real shapes rather than drawn rectangles.",
	"Line after line the same rhythm repeats: bands of ink, bands of",
	"white, a settled baseline hanging slightly below the middle of",
	"every line box, and sparse descenders dipping underneath.",
	"The quick brown fox jumps over the lazy dog while counting all",
	"the punctuation marks. Parentheses (and brackets [too]) appear.",
	"Colons; semicolons; question marks? All in a row!",
}

func rasterize(t *testing.T, mutool, pdf, prefix string, dpi int) []string {
	t.Helper()
	tmp := t.TempDir()
	out := filepath.Join(tmp, prefix+"-%d.pgm")
	cmd := exec.Command(mutool, "draw", "-F", "pgm", "-r", fmt.Sprint(dpi),
		"-o", out, pdf)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	if _, err := cmd.Output(); err != nil {
		t.Fatalf("mutool draw: %v %s", err, buf.String())
	}
	matches, _ := filepath.Glob(filepath.Join(tmp, prefix+"-*.pgm"))
	if len(matches) == 0 {
		t.Fatal("no pages rendered")
	}
	return matches
}

func TestOrientationUpright(t *testing.T) {
	mutool := os.Getenv("MUTOOL")
	if mutool == "" {
		t.Skip("MUTOOL not set")
	}
	pdf := filepath.Join(t.TempDir(), "upright.pdf")
	if err := testutil.WriteTextPDF(pdf, "Orientation Fixture",
		[]testutil.TextPage{{Title: "", Lines: geomTextLines}}); err != nil {
		t.Fatal(err)
	}
	pages := rasterize(t, mutool, pdf, "up", 200)
	img := mustPGM(t, pages[0])

	// report the raw signals once so thresholds stay calibrated
	rows := ocrimg.RhythmScore(img.RowFractions(110))
	cols := ocrimg.RhythmScore(img.ColFractions(110))
	a0 := ocrimg.MassAsym(img, 110)
	t.Logf("signals: rows=%v cols=%v asym=%.4f", rows, cols, a0)

	deg, conf := ocrimg.Orientation(img)
	t.Logf("upright -> deg=%d confident=%v", deg, conf)
	if deg != 0 || !conf {
		t.Fatalf("upright page misoriented: %d %v", deg, conf)
	}

	// rotated copies flip the verdict accordingly
	for _, want := range []struct {
		rot  int
		want int
	}{{90, 270}, {180, 180}, {270, 90}} {
		rotated := img.Rotate(want.rot)
		deg, conf := ocrimg.Orientation(rotated)
		t.Logf("rot%d -> deg=%d confident=%v", want.rot, deg, conf)
		if !conf || deg != want.want {
			t.Errorf("rotate-%d page: got %d (conf %v), want %d",
				want.rot, deg, conf, want.want)
		}
	}
}

func TestSpineSplitOCR(t *testing.T) {
	mutool := os.Getenv("MUTOOL")
	if mutool == "" {
		t.Skip("MUTOOL not set")
	}
	// A fake open-book scan: ONE wide page, two full-height text
	// blocks (a scanned spread). Real pages fill their height with
	// lines — a handful of lines would leave the gutter ambiguous.
	pool := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dogs",
		"while", "counting", "punctuation", "marks", "quietly", "under",
		"clouds", "that", "gathered", "earlier", "than", "expected",
		"morning", "light", "spilled", "across", "the", "desk", "and",
		"settled", "on", "pages", "waiting", "for", "a", "reader",
	}
	filler := func(seed, label string) []string {
		lines := []string{label, ""}
		for n := 0; len(lines) < 46; n++ {
			line := ""
			for w := 0; ; w++ {
				word := pool[(n*7+w*3+13+len(label))%len(pool)]
				if len(line)+len(word)+1 > 98 {
					break
				}
				if w > 0 {
					line += " "
				}
				line += word
			}
			lines = append(lines, line)
		}
		return lines
	}
	pdf := filepath.Join(t.TempDir(), "spread.pdf")
	if err := testutil.WriteLayoutPDF(pdf, "Fake Spread", 1190, 842,
		[][]testutil.LayoutBlock{
			{{X: 45, YTop: 780, Lines: filler("l", "Chapter Seven")},
				{X: 640, YTop: 780, Lines: filler("r", "Chapter Eight")}},
		}); err != nil {
		t.Fatal(err)
	}
	pages := rasterize(t, mutool, pdf, "sp", 300)
	img := mustPGM(t, pages[0])
	at, ok := ocrimg.DetectSpine(img)
	if !ok {
		t.Fatal("gutter not detected on rendered spread")
	}
	t.Logf("spine at %d / %d (%.1f%%)", at, img.W, 100*float64(at)/float64(img.W))
	l, r := img.SplitTwoUp(at)
	lText, rText := ocrHalf(t, l), ocrHalf(t, r)
	t.Logf("left half:  %q", firstOCRLine(lText))
	t.Logf("right half: %q", firstOCRLine(rText))
	if !containsFuzzy(lText, "Chapter Seven") {
		t.Errorf("left half did not OCR its page: %q", firstOCRLine(lText))
	}
	if !containsFuzzy(rText, "Chapter Eight") {
		t.Errorf("right half did not OCR its page: %q", firstOCRLine(rText))
	}
	if containsFuzzy(lText, "Chapter Eight") {
		t.Errorf("left half contains right-page text: %q", firstOCRLine(lText))
	}
	if containsFuzzy(rText, "Chapter Seven") {
		t.Errorf("right half contains left-page text: %q", firstOCRLine(rText))
	}
}

// helpers -------------------------------------------------------------

func mustPGM(t *testing.T, path string) *ocrimg.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ocrimg.DecodePGM(data)
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// ocrHalf runs tesseract on one half-image (written out as a PGM).
func ocrHalf(t *testing.T, im *ocrimg.Image) string {
	t.Helper()
	pgmPath := filepath.Join(t.TempDir(), "half.pgm")
	var pb bytes.Buffer
	fmt.Fprintf(&pb, "P5\n%d %d\n255\n", im.W, im.H)
	pb.Write(im.Pix)
	if err := os.WriteFile(pgmPath, pb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("tesseract", pgmPath, "stdout", "-l", "eng",
		"--dpi", "300").Output()
	if err != nil {
		t.Fatalf("tesseract: %v", err)
	}
	return string(out)
}

func firstOCRLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// containsFuzzy matches lowercase substrings, tolerating OCR noise by
// testing successive slices of the needle (starts/ends degrade first).
func containsFuzzy(hay, needle string) bool {
	h, n := strings.ToLower(hay), strings.ToLower(needle)
	k := len(n) * 3 / 4
	if k < 4 {
		return false
	}
	for i := 0; i+k <= len(n); i += k / 3 {
		if strings.Contains(h, n[i:i+k]) {
			return true
		}
	}
	return false
}

// End-to-end through the REAL extraction pipeline: an image-only PDF of a
// two-page spread comes out with both pages' text in place.
func TestSpreadExtractionPipeline(t *testing.T) {
	mutool := os.Getenv("MUTOOL")
	if mutool == "" {
		t.Skip("MUTOOL not set")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not in PATH")
	}
	root := repoRoot(t)

	pool := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dogs",
		"while", "counting", "punctuation", "marks", "quietly", "under",
		"clouds", "that", "gathered", "earlier", "than", "expected",
	}
	filler := func(label string) []string {
		lines := []string{label, ""}
		for n := 0; len(lines) < 46; n++ {
			line := ""
			for w := 0; ; w++ {
				word := pool[(n*7+w*3+13)%len(pool)]
				if len(line)+len(word)+1 > 98 {
					break
				}
				if w > 0 {
					line += " "
				}
				line += word
			}
			lines = append(lines, line)
		}
		return lines
	}
	spread := filepath.Join(t.TempDir(), "spread.pdf")
	if err := testutil.WriteLayoutPDF(spread, "Pipeline Spread", 1190, 842,
		[][]testutil.LayoutBlock{
			{{X: 45, YTop: 780, Lines: filler("Chapter Fifteen")},
				{X: 640, YTop: 780, Lines: filler("Chapter Sixteen")}},
		}); err != nil {
		t.Fatal(err)
	}
	pages := rasterize(t, mutool, spread, "pl", 300)
	raw, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	im, err := ocrimg.DecodePGM(raw)
	if err != nil {
		t.Fatal(err)
	}
	// PGM -> PNG -> image-only PDF (the scan fixture path)
	gray := image.NewGray(image.Rect(0, 0, im.W, im.H))
	copy(gray.Pix, im.Pix)
	pngPath := filepath.Join(t.TempDir(), "spread.png")
	f, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, gray); err != nil {
		t.Fatal(err)
	}
	f.Close()
	scan := filepath.Join(t.TempDir(), "scan_spread.pdf")
	if err := testutil.WriteImagePDF(scan, pngPath, 300); err != nil {
		t.Fatal(err)
	}

	cfgBody := "db: " + filepath.Join(t.TempDir(), "x.db") + "\ntools:\n" +
		"  mutool: " + mutool + "\n  tessdata: " +
		filepath.Join(root, "tessdata") + "\nocr:\n  langs: eng\n  workers: 2\n" +
		"  dpi: 300\n  min_chars_per_page: 50\n"
	cfgPath := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := extract.Extract(scan, cfg)
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, c := range res.Chunks {
		all += c.Text + "\n"
	}
	t.Logf("chunks=%d ocr_pages=%d first=%q", len(res.Chunks), res.OCRPages,
		firstOCRLine(all))
	if !containsFuzzy(all, "Chapter Fifteen") || !containsFuzzy(all, "Chapter Sixteen") {
		t.Errorf("pipeline did not extract both spread halves: %d chunks, first %q",
			len(res.Chunks), firstOCRLine(all))
	}
	// a full spread half is ~45 lines x ~15 words; garbage OCR (flipped
	// or squashed) would not produce this much structured text
	t.Logf("spread chunk bytes: %d", len(all))
	if len(all) < 6000 {
		t.Errorf("spread body text suspiciously small: %d bytes (want >= 6000 — a full spread is ~8k)", len(all))
	}
}

// A sideways two-up spread must be un-rotated BEFORE the gutter becomes
// visible, then split — the full combined path through the pipeline.
func TestSidewaysExtractionPipeline(t *testing.T) {
	mutool := os.Getenv("MUTOOL")
	if mutool == "" {
		t.Skip("MUTOOL not set")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not in PATH")
	}
	root := repoRoot(t)
	pool := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dogs",
		"while", "counting", "punctuation", "marks", "quietly", "under",
		"clouds", "that", "gathered", "earlier", "than", "expected",
	}
	filler := func(label string) []string {
		lines := []string{label, ""}
		for n := 0; len(lines) < 46; n++ {
			line := ""
			for w := 0; ; w++ {
				word := pool[(n*7+w*3+13)%len(pool)]
				if len(line)+len(word)+1 > 98 {
					break
				}
				if w > 0 {
					line += " "
				}
				line += word
			}
			lines = append(lines, line)
		}
		return lines
	}
	spread := filepath.Join(t.TempDir(), "spread.pdf")
	if err := testutil.WriteLayoutPDF(spread, "Side Spread", 1190, 842,
		[][]testutil.LayoutBlock{
			{{X: 45, YTop: 780, Lines: filler("Chapter Thirty")},
				{X: 640, YTop: 780, Lines: filler("Chapter Thirty-One")}},
		}); err != nil {
		t.Fatal(err)
	}
	pages := rasterize(t, mutool, spread, "sd", 300)
	raw, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	im, err := ocrimg.DecodePGM(raw)
	if err != nil {
		t.Fatal(err)
	}
	im = im.Rotate(90) // turned on its side
	gray := image.NewGray(image.Rect(0, 0, im.W, im.H))
	copy(gray.Pix, im.Pix)
	pngPath := filepath.Join(t.TempDir(), "sideways.png")
	f, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, gray); err != nil {
		t.Fatal(err)
	}
	f.Close()
	scan := filepath.Join(t.TempDir(), "sideways.pdf")
	if err := testutil.WriteImagePDF(scan, pngPath, 300); err != nil {
		t.Fatal(err)
	}
	cfgBody := "db: " + filepath.Join(t.TempDir(), "x.db") + "\ntools:\n" +
		"  mutool: " + mutool + "\n  tessdata: " + filepath.Join(root, "tessdata") +
		"\nocr:\n  langs: eng\n  workers: 2\n  dpi: 300\n  min_chars_per_page: 50\n"
	cfgPath := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := extract.Extract(scan, cfg)
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, c := range res.Chunks {
		all += c.Text + "\n"
	}
	t.Logf("chunks=%d bytes=%d first=%q", len(res.Chunks), len(all), firstOCRLine(all))
	if !containsFuzzy(all, "Chapter Thirty") || !containsFuzzy(all, "Thirty-One") {
		t.Errorf("sideways spread not recovered: first %q", firstOCRLine(all))
	}
	if len(all) < 6000 {
		t.Errorf("sideways spread body suspiciously small: %d bytes", len(all))
	}
}
