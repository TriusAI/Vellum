// Watch API test: configures the filesystem watcher, scans a throwaway
// inbox, and checks that a new file is indexed. No LLM is required — with
// the chat backend unreachable the file lands pending, which is exactly the
// state the watcher exists to create.
package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vellum/internal/api"
	"vellum/internal/config"
	"vellum/internal/db"
)

func TestWatchAPI(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	inbox := filepath.Join(dir, "inbox")
	for _, d := range []string{lib, inbox} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vocabSrc, err := os.ReadFile(filepath.Join(repoRoot(t), "vocab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.yaml"), vocabSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	// llm_url on a dead port: the scan indexes the file and stops before
	// enrichment (no server to talk to).
	cfgBody := "db: " + filepath.Join(dir, "test.db") +
		"\nlibrary_dir: " + lib +
		"\ntools:\n  llm_url: http://127.0.0.1:9\n"
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

	// initial state: disabled, no folders
	var w map[string]any
	request("GET", "/api/watch", "", &w, 200)
	if w["enabled"] != false {
		t.Fatalf("watcher should start disabled, got %v", w["enabled"])
	}
	if dirs, _ := w["dirs"].([]any); len(dirs) != 0 {
		t.Fatalf("watcher should start with no dirs, got %v", dirs)
	}

	// configure + persist. The interval is large so the background loop
	// cannot fire mid-test; manual scans are exercised below.
	request("PUT", "/api/watch",
		`{"enabled":true,"dirs":[`+jsonPath(inbox)+`],"interval":3600}`, &w, 200)
	if w["enabled"] != true {
		t.Fatalf("watcher not enabled: %v", w["enabled"])
	}
	if dirs, _ := w["dirs"].([]any); len(dirs) != 1 || dirs[0] != inbox {
		t.Fatalf("watcher dirs = %v, want [%s]", w["dirs"], inbox)
	}
	if w["notify"] != true {
		t.Fatalf("watcher notify = %v, want true (inotify default)", w["notify"])
	}
	// per-folder stats: one existing folder, no documents yet
	folders, _ := w["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v, want one entry", w["folders"])
	}
	if f, _ := folders[0].(map[string]any); f["path"] != inbox || f["exists"] != true {
		t.Fatalf("folder stats = %v", folders[0])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), inbox) || !strings.Contains(string(raw), "enabled: true") {
		t.Fatalf("watch settings not persisted to config.yaml:\n%s", raw)
	}

	// a second server loading the same config sees the persisted settings
	cfg2, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.Watch.Enabled || len(cfg2.Watch.Dirs) != 1 || cfg2.Watch.Dirs[0] != inbox {
		t.Fatalf("reloaded watch config = %+v", cfg2.Watch)
	}

	// stop the background loop before the manual scans
	request("PUT", "/api/watch", `{"enabled":false}`, &w, 200)

	// drop a new file and trigger a manual scan
	if err := os.WriteFile(filepath.Join(inbox, "fresh.md"),
		[]byte("# Fresh\n\nA newly arrived note about cryptography.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var scan map[string]any
	request("POST", "/api/watch/scan", "", &scan, 200)
	if n, _ := scan["added"].(float64); n < 1 {
		t.Fatalf("scan added = %v, want >= 1", scan["added"])
	}

	var n int
	if err := conn.QueryRow(
		"SELECT COUNT(*) FROM documents WHERE path=?",
		filepath.Join(inbox, "fresh.md")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fresh.md indexed %d times, want 1", n)
	}
	var status string
	if err := conn.QueryRow("SELECT status FROM documents WHERE path=?",
		filepath.Join(inbox, "fresh.md")).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ingested" {
		t.Fatalf("fresh.md status = %q, want ingested (pending enrichment)", status)
	}

	// re-scanning does not re-add the same file
	request("POST", "/api/watch/scan", "", &scan, 200)
	if n, _ := scan["added"].(float64); n != 0 {
		t.Fatalf("second scan added = %v, want 0", scan["added"])
	}
	if err := conn.QueryRow("SELECT COUNT(*) FROM documents").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("document count = %d after rescans, want 1", n)
	}

	// status surfaces the pending file under the watched folder
	request("GET", "/api/watch", "", &w, 200)
	if p, _ := w["pending"].(float64); p != 1 {
		t.Fatalf("watch pending = %v, want 1", w["pending"])
	}

	// scan with no folders configured is a 400
	request("PUT", "/api/watch", `{"enabled":false,"dirs":[]}`, &w, 200)
	request("POST", "/api/watch/scan", "", nil, 400)
}

// TestWatchEvents exercises the automatic (non-manual) path: a file dropped
// into a watched folder is picked up by the background loop, with no API
// call triggering it. On Linux this is inotify; elsewhere the polling
// fallback covers it. Both must stay within a few intervals.
func TestWatchEvents(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	vocabSrc, err := os.ReadFile(filepath.Join(repoRoot(t), "vocab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.yaml"), vocabSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := "db: " + filepath.Join(dir, "test.db") +
		"\nlibrary_dir: " + dir +
		"\ntools:\n  llm_url: http://127.0.0.1:9\n" +
		"watch:\n  enabled: true\n  dirs:\n    - " + inbox + "\n  interval: 2\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Watch.Notify {
		t.Fatal("Notify should default to true")
	}
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(cfg, conn)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	// let the startup scan (empty folder) finish
	time.Sleep(700 * time.Millisecond)

	arrival := filepath.Join(inbox, "arrived.md")
	if err := os.WriteFile(arrival, []byte(
		"# Arrived\n\nA note that appeared while the watcher was running.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(25 * time.Second)
	waitIndexed := func(path string) {
		t.Helper()
		for {
			var n int
			if err := conn.QueryRow(
				"SELECT COUNT(*) FROM documents WHERE path=?", path).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("watcher did not pick up %s automatically", path)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	waitIndexed(arrival)

	// a subdirectory created AFTER the watcher started must be watched too
	// (the recursive add on IN_CREATE|IN_ISDIR)
	sub := filepath.Join(inbox, "later", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(sub, "nested.md")
	if err := os.WriteFile(nested, []byte(
		"# Nested\n\nA note in a subfolder created after the watcher started.\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(25 * time.Second)
	waitIndexed(nested)

	// stop the loop before the temp dir is removed
	req, _ := http.NewRequest("PUT", ts.URL+"/api/watch",
		strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := ts.Client().Do(req); err == nil {
		resp.Body.Close()
	}
}

// TestWatchPoll covers the polling fallback (notify:false): unlike the
// event-driven path, the loop must SLEEP between scans rather than rebuild
// in a tight loop. It also checks a new file is still picked up.
func TestWatchPoll(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	vocabSrc, err := os.ReadFile(filepath.Join(repoRoot(t), "vocab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.yaml"), vocabSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := "db: " + filepath.Join(dir, "test.db") +
		"\nlibrary_dir: " + dir +
		"\ntools:\n  llm_url: http://127.0.0.1:9\n" +
		"watch:\n  enabled: true\n  notify: false\n  dirs:\n    - " + inbox + "\n  interval: 2\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Watch.Notify {
		t.Fatal("notify:false should disable event-driven watching")
	}
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(cfg, conn)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	// give the loop time to run a few polls; a rebuild/spin bug would pile
	// up far more than the initial scan + one poll per 2s
	time.Sleep(5 * time.Second)
	if n := countWatchJobs(t, ts.URL); n > 6 {
		t.Fatalf("polling loop ran %d scans in 5s — it is not sleeping between scans", n)
	}

	arrival := filepath.Join(inbox, "polled.md")
	if err := os.WriteFile(arrival, []byte(
		"# Polled\n\nA note the polling watcher should find within a couple of intervals.\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err := conn.QueryRow(
			"SELECT COUNT(*) FROM documents WHERE path=?", arrival).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("polling watcher did not pick up the new file")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func countWatchJobs(t *testing.T, base string) int {
	t.Helper()
	resp, err := http.Get(base + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Jobs []struct {
			Kind string `json:"kind"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range body.Jobs {
		if j.Kind == "watch" {
			n++
		}
	}
	return n
}

// jsonPath renders a filesystem path as a JSON string.
func jsonPath(p string) string {
	b, _ := json.Marshal(p)
	return string(b)
}
