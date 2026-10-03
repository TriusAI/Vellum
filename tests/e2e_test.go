// End-to-end test: generates fixtures (born-digital PDF, image-only "scan",
// markdown), ingests them, and — when Ollama + models are available — runs
// the LLM pipeline and semantic search. Uses a throwaway config + database.
//
// Run:  go test ./tests/ -v
// Env:   MUTOOL (path to mutool binary; skipped if absent)
package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
	"vellum/tests/testutil"
)

const lorem = `On the Origin of Cognitive Machines

This paper argues that contemporary accounts of machine intelligence
systematically underrate the role of embodiment. We review three
schools of thought: symbolic AI, connectionism, and the enactivist
program. Against the tabula rasa assumptions of large-scale
pretraining, we defend the position that learning is always
structured by an agent's sensorimotor loops.

Section 2 surveys the history of the debate, from cybernetics to
deep learning. Section 3 presents our main argument. Section 4
considers objections, in particular the success of language models
that appear to learn without a body. We conclude that the burden of
proof lies with accounts of intelligence that abstract away from
perception and action.
`

func requireMutool(t *testing.T) string {
	t.Helper()
	mutool := os.Getenv("MUTOOL")
	if mutool == "" {
		if p, err := exec.LookPath("mutool"); err == nil {
			mutool = p
		}
	}
	if mutool == "" {
		t.Skip("mutool not found — set MUTOOL=<path> to run this test")
	}
	return mutool
}

func TestE2E(t *testing.T) {
	mutool := requireMutool(t)

	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(lorem), "\n")
	if err := testutil.WriteTextPDF(filepath.Join(lib, "cognitive_machines_digital.pdf"),
		"On the Origin of Cognitive Machines",
		[]testutil.TextPage{{Title: "On the Origin of Cognitive Machines", Lines: lines}}); err != nil {
		t.Fatal(err)
	}
	scanPNG := filepath.Join("..", "testdata", "scan_page.png")
	if _, err := os.Stat(scanPNG); err != nil {
		t.Skip("testdata/scan_page.png missing")
	}
	if err := testutil.WriteImagePDF(filepath.Join(lib, "cognitive_machines_scan.pdf"), scanPNG); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "engineering_notes.md"), []byte(
		"# Field Notes: Epistemic Humility in Engineering\n\n"+
			"A short essay on knowing what you don't know when building systems.\n\n"+
			"Cryptography is the discipline of assuming adversaries. Good key "+
			"management matters more than exotic ciphers. Log analysis is a form "+
			"of applied epistemology: what does the evidence actually support?\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	// throwaway config + vocab in the test dir
	repoRoot, _ := os.Getwd()
	repoRoot = filepath.Dir(repoRoot)
	vocabSrc, err := os.ReadFile(filepath.Join(repoRoot, "vocab.yaml"))
	if err != nil {
		t.Fatalf("vocab.yaml: %s", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.yaml"), vocabSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := fmt.Sprintf("db: %s\ntools:\n  mutool: %s\n  tessdata: %s\nocr:\n  langs: eng\n  workers: 2\n",
		filepath.Join(dir, "test.db"), mutool, filepath.Join(repoRoot, "tessdata"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}

	// ---- ingest
	st, err := ingest.Ingest(cfg, conn, []string{lib}, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 3 || st.Failed != 0 {
		t.Fatalf("expected added=3 failed=0, got +%d/-%d", st.Added, st.Failed)
	}

	// OCR actually extracted the scanned page?
	hits, err := search.Keyword(conn, "sensorimotor", 20)
	if err != nil {
		t.Fatal(err)
	}
	foundScan := false
	for _, h := range hits {
		if strings.Contains(h.Snippet, "[sensorimotor]") {
			foundScan = true
		}
	}
	if !foundScan {
		t.Fatal("OCR text not indexed with a highlighted match")
	}

	// metadata title came through?
	var title string
	if err := conn.QueryRow(
		"SELECT title FROM documents WHERE path LIKE '%cognitive_machines_digital%'").
		Scan(&title); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "Cognitive Machines") {
		t.Fatalf("metadata title not extracted, got %q", title)
	}

	// ---- LLM part (skipped without a live server + model)
	if !llm.Available(cfg.Tools.OllamaURL) ||
		!llm.HasModel(cfg.Tools.OllamaURL, cfg.Models.LLM) {
		t.Log("NOTE: ollama/" + cfg.Models.LLM + " not available — LLM part skipped")
		return
	}
	v, err := vocab.Load(cfg.VocabPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.ProcessPending(cfg, conn, v, 0); err != nil {
		t.Fatal(err)
	}

	rows, err := conn.Query("SELECT id FROM documents WHERE status != 'done'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("not all documents reached status=done")
	}

	// constrained tags only: every emitted tag must be in the vocabulary
	tagsRows, err := conn.Query("SELECT tag, source FROM doc_tags")
	if err != nil {
		t.Fatal(err)
	}
	vocabSet := map[string]bool{}
	for k := range v.Tags {
		vocabSet[k] = true
	}
	for tagsRows.Next() {
		var tag, source string
		if err := tagsRows.Scan(&tag, &source); err != nil {
			t.Fatal(err)
		}
		if source == "vocab" && !vocabSet[tag] {
			t.Fatalf("drifted tag: %q", tag)
		}
	}
	tagsRows.Close()

	// ---- embeddings + semantic search
	if _, err := search.EmbedPending(cfg, conn); err != nil {
		t.Fatal(err)
	}
	semHits, err := search.Semantic(cfg, conn, "does intelligence need a body", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(semHits) == 0 {
		t.Fatal("semantic search found nothing")
	}
	if !strings.Contains(semHits[0].Path, "cognitive_machines") {
		t.Fatalf("unexpected top hit: %s", semHits[0].Path)
	}

	// summary sanity
	var summary string
	if err := conn.QueryRow(
		"SELECT summary FROM documents WHERE path LIKE '%cognitive_machines_digital%'").
		Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) < 200 {
		t.Fatalf("summary suspiciously short (%d chars):\n%s", len(summary), summary)
	}
}
