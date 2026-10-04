// API tests: httptest against a throwaway library. The LLM is not required;
// endpoints that need it are exercised only when servers are up.
package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"vellum/internal/api"
	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/tests/testutil"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

func TestAPI(t *testing.T) {
	mutool := requireMutool(t)
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// one digital pdf (exercises mutool extraction) + one md
	if err := testutil.WriteTextPDF(filepath.Join(lib, "doc.pdf"),
		"Epistemic Probes", []testutil.TextPage{
			{Title: "Epistemic Probes", Lines: []string{
				"Log analysis is applied epistemology.",
				"Cryptography assumes adversaries exist."}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "notes.md"), []byte(
		"# Notes\n\nCryptography is the discipline of assuming adversaries.\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	vocabSrc, err := os.ReadFile(filepath.Join(repoRoot(t), "vocab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.yaml"), vocabSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := "db: " + filepath.Join(dir, "test.db") + "\ntools:\n  mutool: " + mutool + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := api.New(cfg, conn)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	request := func(method, path, body string, out any, wantCode int) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantCode {
			t.Fatalf("%s %s: status %d, want %d", method, path,
				resp.StatusCode, wantCode)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s: decode: %s", method, path, err)
			}
		}
	}

	// web UI is served at /
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "vellum") {
		t.Fatal("web UI not served at /")
	}

	// status on an empty library
	var status map[string]any
	request("GET", "/api/status", "", &status, 200)
	if status["documents"].(float64) != 0 {
		t.Fatalf("expected empty library, got %v", status["documents"])
	}
	if _, ok := status["llm_up"]; !ok {
		t.Fatal("status must report llm_up")
	}

	// ingest via API
	var st struct {
		Added int `json:"Added"`
	}
	request("POST", "/api/ingest", `{"paths":["`+lib+`"]}`, &st, 200)
	if st.Added != 2 {
		t.Fatalf("expected added=2, got %+v", st)
	}

	// documents list
	var docs []map[string]any
	request("GET", "/api/documents", "", &docs, 200)
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(docs))
	}
	pdfID := ""
	for _, d := range docs {
		if strings.HasSuffix(d["path"].(string), "doc.pdf") {
			pdfID = strconv.Itoa(int(d["id"].(float64)))
			if d["title"] != "Epistemic Probes" {
				t.Fatalf("metadata title not extracted: %v", d["title"])
			}
		}
	}
	if pdfID == "" {
		t.Fatal("digital pdf not ingested")
	}

	// document detail
	var detail struct {
		Document map[string]any   `json:"document"`
		Chunks   []map[string]any `json:"chunks"`
	}
	request("GET", "/api/documents/"+pdfID, "", &detail, 200)
	if len(detail.Chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(detail.Chunks))
	}

	// PATCH metadata (manual editing)
	request("PATCH", "/api/documents/"+pdfID,
		`{"year":"2026"}`, nil, 200)
	var detail2 struct {
		Document map[string]any `json:"document"`
	}
	request("GET", "/api/documents/"+pdfID, "", &detail2, 200)
	if detail2.Document["year"] != "2026" {
		t.Fatalf("PATCH did not persist: %v", detail2.Document["year"])
	}

	// PUT tags (manual tagging)
	request("PUT", "/api/documents/"+pdfID+"/tags",
		`{"tags":["epistemology","my-manual-tag"]}`, nil, 200)
	request("GET", "/api/documents/"+pdfID, "", &detail2, 200)
	tags := detail2.Document["tags"].([]any)
	if len(tags) != 2 || tags[0] != "epistemology" {
		t.Fatalf("manual tags not stored: %v", tags)
	}

	// 404s
	request("GET", "/api/documents/999", "", nil, 404)
	request("GET", "/api/search?q=", "", nil, 400)

	// keyword search
	var hits []map[string]any
	request("GET", "/api/search?q=cryptography&mode=keyword", "", &hits, 200)
	if len(hits) == 0 {
		t.Fatal("keyword search found nothing")
	}

	// vocab API
	var vocab []map[string]string
	request("GET", "/api/vocab", "", &vocab, 200)
	if len(vocab) == 0 {
		t.Fatal("vocab empty")
	}
	request("POST", "/api/vocab",
		`{"name":"test-tag","description":"for tests"}`, nil, 200)
	request("GET", "/api/vocab", "", &vocab, 200)
	found := false
	for _, v := range vocab {
		if v["name"] == "test-tag" {
			found = true
		}
	}
	if !found {
		t.Fatal("vocab add via API failed")
	}
	request("DELETE", "/api/vocab/test-tag", "", nil, 200)
	request("DELETE", "/api/vocab/test-tag", "", nil, 404)
}
