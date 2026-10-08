package api

import (
	"database/sql"
	"strings"
	"testing"

	"vellum/internal/config"
	"vellum/internal/db"
)

// newTestServer builds a Server over a throwaway library with a few
// documents, tags, a category and a collection.
func newTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.BaseDir = dir
	cfg.DBPath = dir + "/library.db"
	cfg.VocabPath = dir + "/vocab.yaml"
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	ins := func(path, title, cat string) int64 {
		res, err := conn.Exec(
			"INSERT INTO documents(path, sha256, title, category, status) VALUES(?,?,?,?, 'done')",
			path, "sha-"+path, title, cat)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := conn.Exec(
			"INSERT INTO chunks(doc_id, seq, page_no, text) VALUES(?,0,1,?)",
			id, "body of "+title); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := ins("/lib/a.pdf", "Attention Is All You Need", "ai/transformers")
	b := ins("/lib/b.pdf", "The Rust Programming Language", "cs")
	c := ins("/lib/c.pdf", "Cooking With Fire", "cooking")
	conn.Exec("INSERT INTO doc_tags(doc_id, tag, source) VALUES(?, 'transformer','vocab')", a)
	conn.Exec("INSERT INTO doc_tags(doc_id, tag, source) VALUES(?, 'transformer','vocab')", b)
	res, _ := conn.Exec("INSERT INTO collections(name) VALUES('reading')")
	colID, _ := res.LastInsertId()
	conn.Exec("INSERT INTO collection_docs(collection_id, doc_id) VALUES(?,?)", colID, c)

	s := New(cfg, conn)
	return s, conn
}

func TestResolveChatScope(t *testing.T) {
	s, _ := newTestServer(t)

	sc, err := s.resolveChatScope("library", "")
	if err != nil || !sc.All {
		t.Fatalf("library scope: %+v (%v)", sc, err)
	}

	sc, err = s.resolveChatScope("tag", "transformer")
	if err != nil || len(sc.Docs) != 2 || !sc.allows(1) {
		t.Fatalf("tag scope: %+v (%v)", sc, err)
	}
	if sc.allows(3) {
		t.Fatal("tag scope must not allow an untagged document")
	}

	// category subtree: ai/transformers is included in the ai shelf
	sc, err = s.resolveChatScope("category", "ai")
	if err != nil || !sc.allows(1) || sc.allows(2) {
		t.Fatalf("category subtree: %+v (%v)", sc, err)
	}

	sc, err = s.resolveChatScope("collection", "1")
	if err != nil || !sc.allows(3) || sc.allows(1) {
		t.Fatalf("collection scope: %+v (%v)", sc, err)
	}

	if _, err := s.resolveChatScope("document", "999"); err == nil {
		t.Fatal("a missing document scope must error")
	}
	if _, err := s.resolveChatScope("bogus", "x"); err == nil {
		t.Fatal("an unknown scope kind must error")
	}
}

func TestChatToolOpenScoped(t *testing.T) {
	s, _ := newTestServer(t)
	sc, err := s.resolveChatScope("tag", "transformer")
	if err != nil {
		t.Fatal(err)
	}
	// in scope: returns an action the UI can perform
	res := s.toolOpenDocument(sc, `{"doc_id":1,"view":"preview","page":3}`)
	if res.Action == nil || res.Action["kind"] != "open" || res.Action["doc_id"] != int64(1) {
		t.Fatalf("open action wrong: %+v", res)
	}
	// out of scope: refused, no action
	res = s.toolOpenDocument(sc, `{"doc_id":3}`)
	if res.Action != nil || !strings.Contains(res.Content, "outside") {
		t.Fatalf("out-of-scope open must be refused: %+v", res)
	}
	// regenerate is scope-checked before any backend work
	res = s.toolRegenerate(sc, `{"doc_id":3,"fields":["summary"]}`)
	if !strings.Contains(res.Content, "outside") {
		t.Fatalf("out-of-scope regenerate must be refused: %+v", res)
	}
}

func TestChatSystemListsScope(t *testing.T) {
	s, _ := newTestServer(t)
	sc, _ := s.resolveChatScope("library", "")
	sys, err := s.chatSystem(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "Attention Is All You Need") ||
		!strings.Contains(sys, "Cooking With Fire") {
		t.Fatalf("library system prompt missing documents:\n%s", sys)
	}
	// a document scope carries its own text, not the others
	sc, _ = s.resolveChatScope("document", "1")
	sys, _ = s.chatSystem(sc)
	if !strings.Contains(sys, "body of Attention Is All You Need") {
		t.Fatalf("document system prompt missing opening text:\n%s", sys)
	}
	if strings.Contains(sys, "Cooking With Fire") {
		t.Fatalf("document scope leaked another document:\n%s", sys)
	}
}
