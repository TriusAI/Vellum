// API tests: httptest against a throwaway library. The LLM is not required;
// endpoints that need it are exercised only when servers are up.
package tests

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	// a nested subdirectory (recursive ingest) and vault noise that must
	// be SKIPPED: .obsidian/ and .trash/ are not library material.
	if err := os.MkdirAll(filepath.Join(lib, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "subdir", "nested.md"),
		[]byte("# Nested\n\nA note in a subfolder.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{
		filepath.Join(lib, ".obsidian", "plugins", "config.md"),
		filepath.Join(lib, ".trash", "deleted.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(noise), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(noise, []byte("# noise\n"), 0o644); err != nil {
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
	cfgBody := "db: " + filepath.Join(dir, "test.db") +
		"\nlibrary_dir: " + filepath.Join(dir, "library") +
		"\ntools:\n  mutool: " + mutool + "\n"
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
	if st.Added != 3 {
		t.Fatalf("expected added=3 (doc.pdf, notes.md, subdir/nested.md), got %+v", st)
	}
	// recursive ingest found the nested file, and skipped the hidden dirs
	var pathsAfter []string
	if rows, err := conn.Query("SELECT path FROM documents"); err == nil {
		for rows.Next() {
			var p string
			rows.Scan(&p)
			pathsAfter = append(pathsAfter, p)
		}
		rows.Close()
	}
	sawNested := false
	for _, p := range pathsAfter {
		if strings.Contains(p, "/.obsidian/") || strings.Contains(p, "/.trash/") {
			t.Fatalf("hidden vault dir was ingested: %s", p)
		}
		if strings.HasSuffix(p, "subdir/nested.md") {
			sawNested = true
		}
	}
	if !sawNested {
		t.Fatalf("recursive ingest missed subdir/nested.md: %v", pathsAfter)
	}

	// fs listing backs the ingest picker: entries typed, unsupported
	// files flagged, dotfiles hidden
	if err := os.WriteFile(filepath.Join(lib, "junk.bin"),
		[]byte("not a document"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, ".hidden.md"),
		[]byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fsres struct {
		Path    string `json:"path"`
		Entries []struct {
			Name      string `json:"name"`
			Dir       bool   `json:"dir"`
			Supported bool   `json:"supported"`
		} `json:"entries"`
	}
	request("GET", "/api/fs?path="+lib, "", &fsres, 200)
	if fsres.Path != lib {
		t.Fatalf("fs path echo wrong: %q", fsres.Path)
	}
	supportedNames := map[string]bool{}
	for _, e := range fsres.Entries {
		supportedNames[e.Name] = e.Supported
	}
	if !supportedNames["doc.pdf"] || !supportedNames["notes.md"] {
		t.Fatalf("supported files not flagged: %+v", fsres.Entries)
	}
	if supportedNames["junk.bin"] {
		t.Fatal("junk.bin must not be flagged supported")
	}
	if _, ok := supportedNames[".hidden.md"]; ok {
		t.Fatal("dotfiles must be hidden from the picker")
	}
	request("GET", "/api/fs?path=relative/path", "", nil, 400)

	// ---- ask configuration round-trip (api key masked)
	var askCfg map[string]any
	request("PUT", "/api/ask/config",
		`{"provider":"openai","model":"gpt-4o-mini","api_key":"sk-test"}`, &askCfg, 200)
	request("GET", "/api/ask/config", "", &askCfg, 200)
	if askCfg["provider"] != "openai" || askCfg["key_set"] != true {
		t.Fatalf("ask config round-trip wrong: %v", askCfg)
	}
	if _, has := askCfg["api_key"]; has {
		t.Fatal("the api key must not be served back")
	}
	// empty key in a further PUT keeps the stored one
	request("PUT", "/api/ask/config", `{"provider":"anthropic"}`, &askCfg, 200)
	request("GET", "/api/ask/config", "", &askCfg, 200)
	if askCfg["provider"] != "anthropic" || askCfg["key_set"] != true {
		t.Fatalf("empty key must keep the stored key: %v", askCfg)
	}
	// ask with provider unset-but-testable: a test call WITHOUT the key
	// must fail (502) — it would try a network call against the default
	// endpoint with junk; the e2e runs offline-guarded anyway. Test only
	// the shape of the disabled path:
	request("PUT", "/api/ask/config", `{"provider":"none"}`, &askCfg, 200)
	if askCfg["enabled"] != false {
		t.Fatalf("provider none must read as disabled: %v", askCfg)
	}

	// ---- covers: the .pdf renders page 1; text files have none
	var coverResp *http.Response
	req1, _ := http.NewRequest("GET", ts.URL+"/api/documents/1/cover", nil)
	coverResp, err = ts.Client().Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	if coverResp.StatusCode != 200 {
		t.Fatalf("pdf cover should render, got %d", coverResp.StatusCode)
	}
	if ct := coverResp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("cover content type: %s", ct)
	}
	coverResp.Body.Close()
	var mdID int64
	if err := conn.QueryRow("SELECT id FROM documents WHERE path LIKE '%/notes.md'").
		Scan(&mdID); err != nil {
		t.Fatal(err)
	}
	request("GET", fmt.Sprintf("/api/documents/%d/cover", mdID), "", nil, 404)

	// ---- reextract pass-through on a text-format doc (chunks rebuilt)
	var reex map[string]any
	request("POST", fmt.Sprintf("/api/documents/%d/reextract", mdID),
		`{}`, &reex, 200)

	// ---- config: shape + live effect
	var cfgJSON map[string]any
	request("GET", "/api/config", "", &cfgJSON, 200)
	if _, ok := cfgJSON["llm"].(map[string]any)["backend"]; !ok {
		t.Fatal("config missing llm.backend")
	}
	request("PUT", "/api/config", `{"summarize":{"max_tags":6}}`, &cfgJSON, 200)
	if cfgJSON["summarize"].(map[string]any)["max_tags"].(float64) != 6 {
		t.Fatalf("config max_tags not applied: %v", cfgJSON["summarize"])
	}
	// out-of-range values are rejected by clamping to current
	request("PUT", "/api/config", `{"summarize":{"max_tags":99}}`, &cfgJSON, 200)
	if cfgJSON["summarize"].(map[string]any)["max_tags"].(float64) != 6 {
		t.Fatalf("config clamping broken: %v", cfgJSON["summarize"])
	}

	// ---- regenerate: field validation
	request("POST", fmt.Sprintf("/api/documents/%d/regenerate", mdID),
		`{"fields":[]}`, nil, 400)

	// documents list
	var docs []map[string]any
	request("GET", "/api/documents", "", &docs, 200)
	if len(docs) != 3 {
		t.Fatalf("expected 3 documents, got %d", len(docs))
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

	// ---- category rename: subtree move + invalid nesting rejected
	request("PATCH", "/api/documents/"+pdfID,
		`{"category":"cs/ml/transformers"}`, nil, 200)
	var ren map[string]any
	request("POST", "/api/categories/rename",
		`{"from":"cs","to":"computer-science"}`, &ren, 200)
	if ren["updated"].(float64) != 1 {
		t.Fatalf("rename should move 1 document: %v", ren)
	}
	request("GET", "/api/documents/"+pdfID, "", &detail2, 200)
	if detail2.Document["category"] != "computer-science/ml/transformers" {
		t.Fatalf("rename did not move the subtree: %v", detail2.Document["category"])
	}
	request("POST", "/api/categories/rename",
		`{"from":"computer-science","to":"computer-science/x"}`, nil, 400)
	request("POST", "/api/categories/rename",
		`{"from":"","to":"x"}`, nil, 400)
	// rename onto an existing shelf merges (no error, doc still reachable)
	request("PATCH", "/api/documents/"+pdfID,
		`{"category":"shelf-b"}`, nil, 200)
	request("PATCH", fmt.Sprintf("/api/documents/%d", mdID),
		`{"category":"shelf-a"}`, nil, 200)
	request("POST", "/api/categories/rename",
		`{"from":"shelf-a","to":"shelf-b"}`, &ren, 200)
	if ren["updated"].(float64) != 1 {
		t.Fatalf("merge-rename should move 1 document: %v", ren)
	}

	// ---- collections: user-managed groups of documents
	var col map[string]any
	request("POST", "/api/collections",
		`{"name":"applied-ml","description":"ML x medicine"}`, &col, 200)
	if col["name"] != "applied-ml" {
		t.Fatalf("create collection: %v", col)
	}
	colID := strconv.Itoa(int(col["id"].(float64)))
	request("POST", "/api/collections", `{"name":"applied-ml"}`, nil, 409) // dup
	request("POST", "/api/collections", `{"name":"   "}`, nil, 400)        // empty

	var addRes map[string]any
	request("POST", "/api/collections/"+colID+"/documents",
		`{"doc_ids":[`+pdfID+`,`+fmt.Sprint(mdID)+`]}`, &addRes, 200)
	if addRes["added"].(float64) != 2 {
		t.Fatalf("add to collection: %v", addRes)
	}
	// idempotent
	request("POST", "/api/collections/"+colID+"/documents",
		`{"doc_ids":[`+pdfID+`]}`, &addRes, 200)
	if addRes["added"].(float64) != 0 {
		t.Fatalf("re-adding a member must be a no-op: %v", addRes)
	}

	var colDetail struct {
		Collection map[string]any   `json:"collection"`
		Documents  []map[string]any `json:"documents"`
	}
	request("GET", "/api/collections/"+colID, "", &colDetail, 200)
	if len(colDetail.Documents) != 2 {
		t.Fatalf("collection should hold 2 documents, got %d", len(colDetail.Documents))
	}
	// the document detail carries its collections (for the Summary page)
	var detColl struct {
		Document    map[string]any   `json:"document"`
		Collections []map[string]any `json:"collections"`
	}
	request("GET", "/api/documents/"+pdfID, "", &detColl, 200)
	if len(detColl.Collections) != 1 || detColl.Collections[0]["name"] != "applied-ml" {
		t.Fatalf("document.collections: %v", detColl.Collections)
	}
	// rename
	var renCol map[string]any
	request("PATCH", "/api/collections/"+colID,
		`{"name":"applied-ml-2026"}`, &renCol, 200)
	// remove one member
	request("DELETE", "/api/collections/"+colID+"/documents/"+pdfID, "", nil, 200)
	request("GET", "/api/collections/"+colID, "", &colDetail, 200)
	if len(colDetail.Documents) != 1 {
		t.Fatalf("after removal collection should hold 1, got %d", len(colDetail.Documents))
	}
	// list shows the collection with its count
	var cols []map[string]any
	request("GET", "/api/collections", "", &cols, 200)
	foundCol := false
	for _, c := range cols {
		if c["name"] == "applied-ml-2026" {
			foundCol = true
		}
	}
	if !foundCol {
		t.Fatal("renamed collection missing from list")
	}
	// 404s
	request("GET", "/api/collections/99999", "", nil, 404)
	request("DELETE", "/api/collections/99999", "", nil, 404)
	request("POST", "/api/collections/99999/documents", `{"doc_ids":[1]}`, nil, 404)

	// ---- collection export/import: a shareable .zip bundle
	reqExpC, _ := http.NewRequest("GET", ts.URL+"/api/collections/"+colID+"/export", nil)
	respExpC, err := ts.Client().Do(reqExpC)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := io.ReadAll(respExpC.Body)
	respExpC.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if respExpC.StatusCode != 200 || len(bundle) == 0 {
		t.Fatalf("collection export: status=%d len=%d", respExpC.StatusCode, len(bundle))
	}
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("export is not a zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["manifest.json"] {
		t.Fatalf("bundle missing manifest.json: %v", names)
	}
	// import it back: a NEW collection, files ingested under library_dir
	reqImpC, _ := http.NewRequest("POST", ts.URL+"/api/collections/import",
		bytes.NewReader(bundle))
	reqImpC.Header.Set("Content-Type", "application/zip")
	respImpC, err := ts.Client().Do(reqImpC)
	if err != nil {
		t.Fatal(err)
	}
	var impColRes map[string]any
	json.NewDecoder(respImpC.Body).Decode(&impColRes)
	respImpC.Body.Close()
	if respImpC.StatusCode != 200 {
		t.Fatalf("collection import: status=%d res=%v", respImpC.StatusCode, impColRes)
	}
	if impColRes["documents"].(float64) != 1 {
		t.Fatalf("imported collection should have 1 document: %v", impColRes)
	}
	impCol := impColRes["collection"].(map[string]any)
	if !strings.HasPrefix(impCol["name"].(string), "applied-ml-2026") {
		t.Fatalf("imported collection name: %v", impCol["name"])
	}
	// the metadata (the member's category) must have been restored
	impColID := strconv.Itoa(int(impCol["id"].(float64)))
	var impDetail struct {
		Documents []map[string]any `json:"documents"`
	}
	request("GET", "/api/collections/"+impColID, "", &impDetail, 200)
	if len(impDetail.Documents) != 1 {
		t.Fatalf("imported collection members: %d", len(impDetail.Documents))
	}
	if impDetail.Documents[0]["category"] != "shelf-b" {
		t.Fatalf("imported metadata not restored: %v", impDetail.Documents[0]["category"])
	}
	// garbage is rejected
	request("POST", "/api/collections/import", "not a zip", nil, 400)

	// ---- notes: scratchpad entries (body + timestamps only)
	var note map[string]any
	request("POST", "/api/notes", `{"body":"first idea\nsecond line"}`, &note, 200)
	noteID := strconv.Itoa(int(note["id"].(float64)))
	if note["body"] != "first idea\nsecond line" || note["created_at"] == "" {
		t.Fatalf("create note: %v", note)
	}
	request("GET", "/api/notes/"+noteID, "", &note, 200)
	request("PATCH", "/api/notes/"+noteID, `{"body":"edited"}`, &note, 200)
	if note["body"] != "edited" {
		t.Fatalf("patch note: %v", note)
	}
	var notes []map[string]any
	request("GET", "/api/notes", "", &notes, 200)
	if len(notes) != 1 {
		t.Fatalf("notes list: %d", len(notes))
	}
	request("GET", "/api/notes/99999", "", nil, 404)
	request("PATCH", "/api/notes/99999", `{"body":"x"}`, nil, 404)
	request("DELETE", "/api/notes/"+noteID, "", nil, 200)
	request("DELETE", "/api/notes/"+noteID, "", nil, 404)

	// ---- tags: cloud listing + per-tag documents
	var tagList []map[string]any
	request("GET", "/api/tags", "", &tagList, 200)
	if len(tagList) == 0 {
		t.Fatal("tags list empty")
	}
	// the paper carries the manual tags we set earlier
	var tagDetail struct {
		Tag       string           `json:"tag"`
		Documents []map[string]any `json:"documents"`
	}
	request("GET", "/api/tags/epistemology", "", &tagDetail, 200)
	if tagDetail.Tag != "epistemology" || len(tagDetail.Documents) != 1 {
		t.Fatalf("tag detail: tag=%q docs=%d", tagDetail.Tag, len(tagDetail.Documents))
	}

	// ---- theme: persisted in the config surface
	var themeCfg map[string]any
	request("PUT", "/api/config",
		`{"theme":{"preset":"dark","colors":{"accent":"#123456"}}}`, &themeCfg, 200)
	th := themeCfg["theme"].(map[string]any)
	if th["preset"] != "dark" {
		t.Fatalf("theme preset not saved: %v", th)
	}

	// ---- document delete: index-only removal (the file stays on disk)
	var delRes map[string]any
	var docsBefore []map[string]any
	request("GET", "/api/documents", "", &docsBefore, 200)
	request("DELETE", fmt.Sprintf("/api/documents/%d", mdID), "", &delRes, 200)
	if delRes["deleted"] == nil {
		t.Fatal("delete returned no id")
	}
	request("GET", fmt.Sprintf("/api/documents/%d", mdID), "", nil, 404)
	request("DELETE", fmt.Sprintf("/api/documents/%d", mdID), "", nil, 404)
	var docsAfter []map[string]any
	request("GET", "/api/documents", "", &docsAfter, 200)
	if len(docsAfter) != len(docsBefore)-1 {
		t.Fatalf("expected %d documents after delete, got %d",
			len(docsBefore)-1, len(docsAfter))
	}
	// chunks and tags must have cascaded with the row
	var nChunks, nTags int
	conn.QueryRow("SELECT COUNT(*) FROM chunks WHERE doc_id=?", mdID).Scan(&nChunks)
	conn.QueryRow("SELECT COUNT(*) FROM doc_tags WHERE doc_id=?", mdID).Scan(&nTags)
	if nChunks != 0 || nTags != 0 {
		t.Fatalf("orphaned rows after delete: %d chunks, %d tags", nChunks, nTags)
	}
	// FTS must no longer return the deleted doc's text
	var hitsAfter []map[string]any
	request("GET", "/api/search?q=cryptography&mode=keyword", "", &hitsAfter, 200)
	for _, h := range hitsAfter {
		if int64(h["doc_id"].(float64)) == mdID {
			t.Fatal("deleted document still in FTS results")
		}
	}
	// deleting a document also cascades out of every collection
	request("GET", "/api/collections/"+colID, "", &colDetail, 200)
	if len(colDetail.Documents) != 0 {
		t.Fatalf("deleted document still in a collection: %d", len(colDetail.Documents))
	}

	// ---- library export/import (backup + reload)
	reqExp, _ := http.NewRequest("GET", ts.URL+"/api/library/export", nil)
	respExp, err := ts.Client().Do(reqExp)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := io.ReadAll(respExp.Body)
	respExp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if respExp.StatusCode != 200 || !bytes.HasPrefix(snap, []byte("SQLite format 3\x00")) {
		t.Fatalf("export: status=%d, %d bytes", respExp.StatusCode, len(snap))
	}
	snapFile := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(snapFile, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ValidateLibrary(snapFile); err != nil || n != len(docsAfter) {
		t.Fatalf("exported snapshot: n=%d err=%v (want %d documents)", n, err, len(docsAfter))
	}
	// import the snapshot back (replaces the live db; old kept aside)
	reqImp, _ := http.NewRequest("POST", ts.URL+"/api/library/import", bytes.NewReader(snap))
	reqImp.Header.Set("Content-Type", "application/octet-stream")
	respImp, err := ts.Client().Do(reqImp)
	if err != nil {
		t.Fatal(err)
	}
	var impRes map[string]any
	json.NewDecoder(respImp.Body).Decode(&impRes)
	respImp.Body.Close()
	if respImp.StatusCode != 200 || impRes["documents"].(float64) != float64(len(docsAfter)) {
		t.Fatalf("import: status=%d res=%v (want %d docs)", respImp.StatusCode, impRes, len(docsAfter))
	}
	var docsReloaded []map[string]any
	request("GET", "/api/documents", "", &docsReloaded, 200)
	if len(docsReloaded) != len(docsAfter) {
		t.Fatalf("expected %d documents after import, got %d", len(docsAfter), len(docsReloaded))
	}
	// a pre-import copy must exist next to the replaced library
	if _, err := os.Stat(cfg.DBPath + ".pre-import-"); err != nil {
		if matches, _ := filepath.Glob(cfg.DBPath + ".pre-import-*"); len(matches) == 0 {
			t.Fatalf("no pre-import backup found: %v", err)
		}
	}
	// garbage uploads must be rejected
	request("POST", "/api/library/import", "this is not a database", nil, 400)
}
