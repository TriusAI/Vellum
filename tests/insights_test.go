// Stats + bulk-action API tests (no LLM needed; the chat backend points at a
// dead port so regeneration correctly refuses with 503).
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vellum/internal/api"
	"vellum/internal/config"
	"vellum/internal/db"
)

func TestStatsAndBulk(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.BaseDir = dir
	cfg.DBPath = filepath.Join(dir, "library.db")
	cfg.VocabPath = filepath.Join(dir, "vocab.yaml")
	cfg.Tools.LLMURL = "http://127.0.0.1:9"
	cfg.Tools.EmbedURL = "http://127.0.0.1:9"
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/lib/a.pdf", "/lib/b.pdf"} {
		if _, err := conn.Exec(
			"INSERT INTO documents(path, sha256, title, status) VALUES(?,?,?,'done')",
			p, "sha-"+p, p); err != nil {
			t.Fatal(err)
		}
	}
	srv := api.New(cfg, conn)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	request := func(method, path, body string, out any, wantCode int) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantCode {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: status %d, want %d (%s)", method, path, resp.StatusCode, wantCode, b)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s: decode: %s", method, path, err)
			}
		}
	}

	var st map[string]any
	request("GET", "/api/stats", "", &st, 200)
	if n, _ := st["documents"].(float64); n != 2 {
		t.Fatalf("stats documents = %v, want 2", st["documents"])
	}
	if _, ok := st["kinds"]; !ok {
		t.Fatalf("stats missing kinds: %v", st)
	}

	// bulk remove one document
	var rm map[string]any
	request("POST", "/api/documents/remove", `{"ids":[1]}`, &rm, 200)
	if removed, _ := rm["removed"].([]any); len(removed) != 1 {
		t.Fatalf("bulk remove = %v, want one removed", rm)
	}
	request("GET", "/api/stats", "", &st, 200)
	if n, _ := st["documents"].(float64); n != 1 {
		t.Fatalf("after remove, documents = %v, want 1", st["documents"])
	}

	// bulk regenerate without a backend → 503
	request("POST", "/api/documents/regenerate", `{"ids":[2],"fields":["summary"]}`, nil, 503)

	// clear an (empty) chat session
	var sess map[string]any
	request("POST", "/api/chats", `{"scope_kind":"library"}`, &sess, 201)
	id := int64(sess["id"].(float64))
	var cleared map[string]any
	request("POST", fmt.Sprintf("/api/chats/%d/clear", id), "", &cleared, 200)
	if n, _ := cleared["deleted"].(float64); n != 0 {
		t.Fatalf("clear deleted = %v, want 0", cleared["deleted"])
	}
}
