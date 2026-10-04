// End-to-end test: generates fixtures (born-digital PDF, image-only "scan",
// markdown), ingests them, and — when Ollama + models are available — runs
// the LLM pipeline and semantic search. Uses a throwaway config + database.
//
// Run:  go test ./tests/ -v
// Env:   MUTOOL (path to mutool binary; skipped if absent)
package tests

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// startLLaMAServers starts llama-server instances for the chat and embedding
// models (llama.cpp serves one model per process). Uses env vars:
//
//	LLAMA_SERVER_BIN  path to the llama-server binary
//	QWEN_GGUF         chat model GGUF
//	NOMIC_GGUF        embedding model GGUF
//
// Returns the URLs (and they are killed at test cleanup). If llama-servers
// are already up at the default ports, those are used instead.
func startLLaMAServers(t *testing.T) (llmURL, embedURL string) {
	t.Helper()

	// already running? (e.g. the portable pack or docker entrypoint did it)
	for _, cand := range [][2]string{
		{"http://127.0.0.1:8081", "http://127.0.0.1:8082"},
	} {
		if llm.Available(cand[0]) && llm.Available(cand[1]) {
			return cand[0], cand[1]
		}
	}

	bin := os.Getenv("LLAMA_SERVER_BIN")
	qwen := os.Getenv("QWEN_GGUF")
	nomic := os.Getenv("NOMIC_GGUF")
	if bin == "" || qwen == "" || nomic == "" {
		t.Log("NOTE: no llama-servers up and LLAMA_SERVER_BIN/QWEN_GGUF/NOMIC_GGUF not set — LLM part skipped")
		return "", ""
	}

	// ephemeral ports: fixed ports silently reuse whatever stale server
	// happens to be listening — a leftover from a previous run then
	// masquerades as ours (wrong flags, wrong model). Pick free ports and
	// fail loudly if our process dies instead of answering.
	freePort := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		_, port, _ := net.SplitHostPort(l.Addr().String())
		return port
	}
	llmPort, embedPort := freePort(), freePort()

	logs, _ := os.MkdirTemp("", "vellum-llama-*")

	// prefill template that disables Qwen3's think pass (the embedded
	// template in the ollama-provenance GGUF ignores enable_thinking)
	wd, _ := os.Getwd()
	tmpl := filepath.Join(filepath.Dir(wd), "templates", "qwen3-nothink.jinja")

	start := func(serverBin, port, model, logName string, extra ...string) *exec.Cmd {
		f, err := os.Create(filepath.Join(logs, logName))
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"-m", model, "--host", "127.0.0.1", "--port", port, "-np", "1"}
		if strings.Contains(logName, "embed") {
			// embeddings mode; nomic has a 2048-token context: no -c override
			args = append(args, "--embeddings", "--ubatch-size", "2048")
		} else {
			args = append(args, "-c", "8192", "--jinja", "--chat-template-file", tmpl)
		}
		args = append(args, extra...)
		cmd := exec.Command(serverBin, args...)
		// the official prebuilts keep their shared libs next to the binary
		// (RUNPATH $ORIGIN), and the vulkan build dlopens the loader from
		// the usual places — running with cwd = bin dir keeps it simple
		cmd.Dir = filepath.Dir(serverBin)
		cmd.Stdout = f
		cmd.Stderr = f
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	llmBin := bin
	// chat: as configured (GPU-capable build if given); embed: CPU build —
	// on small GPUs both models on the device exhaust VRAM, and the tiny
	// embed model is fast on CPU anyway.
	embedBin := os.Getenv("EMBED_SERVER_BIN")
	if embedBin == "" {
		embedBin = strings.Replace(llmBin, "llama-server-vulkan", "llama-server-cpu", 1)
	}

	llmCmd := start(llmBin, llmPort, qwen, "llm.log")
	embedCmd := start(embedBin, embedPort, nomic, "embed.log")
	alive := func(cmd *exec.Cmd) bool {
		return cmd.Process == nil ||
			cmd.Process.Signal(syscall.Signal(0)) == nil
	}
	t.Cleanup(func() {
		llmCmd.Process.Kill()
		embedCmd.Process.Kill()
		llmCmd.Wait()
		embedCmd.Wait()
	})

	waitUp := func(url string, what string, cmd *exec.Cmd) {
		for i := 0; i < 300; i++ {
			if llm.Available(url) {
				return
			}
			if !alive(cmd) {
				t.Fatalf("%s server process exited early (see %s/ logs)",
					what, logs)
			}
			time.Sleep(1 * time.Second)
		}
		t.Fatalf("%s server did not come up (see %s)", what, logs)
	}
	waitUp("http://127.0.0.1:"+llmPort, "chat", llmCmd)
	waitUp("http://127.0.0.1:"+embedPort, "embed", embedCmd)

	return "http://127.0.0.1:" + llmPort, "http://127.0.0.1:" + embedPort
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

	// ---- LLM part (skipped without servers or launch paths)
	llmURL, embedURL := startLLaMAServers(t)
	if llmURL == "" {
		return
	}
	cfg.Tools.LLMURL = llmURL
	cfg.Tools.EmbedURL = embedURL
	v, err := vocab.Load(cfg.VocabPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.ProcessPending(cfg, conn, v, nil, 0, nil); err != nil {
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

	// ---- long-document regression: a ~35k-char doc must not blow the
	// context window (token-budgeted requests + hierarchical reduce)
	longText := strings.Repeat(strings.Repeat(lorem, 3), 6)
	longPath := filepath.Join(lib, "long_document.md")
	if err := os.WriteFile(longPath, []byte(longText), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := ingest.Ingest(cfg, conn, []string{longPath}, false)
	if err != nil || st2.Added != 1 {
		t.Fatalf("long doc not ingested: %+v (%v)", st2, err)
	}
	longResults, err := ingest.ProcessPending(cfg, conn, v, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range longResults {
		if strings.Contains(r.Path, "long_document") && r.Status != "done" {
			t.Fatalf("long document processing failed: %s", r.Error)
		}
	}
	var longSummary string
	if err := conn.QueryRow(
		"SELECT summary FROM documents WHERE path LIKE '%long_document%'").
		Scan(&longSummary); err != nil {
		t.Fatal(err)
	}
	if len(longSummary) < 200 {
		t.Fatalf("long-document summary suspiciously short: %d chars", len(longSummary))
	}

	// ---- per-id processing: re-run one document by id
	one, err := ingest.ProcessPending(cfg, conn, v, []int64{1}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].DocID != 1 || one[0].Status != "done" {
		t.Fatalf("process-by-id failed: %+v", one)
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
