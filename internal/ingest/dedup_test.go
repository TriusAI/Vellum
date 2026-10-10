package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"vellum/internal/db"
)

// FindDuplicateGroups + MergeDocument: a legacy library (documents inserted
// before the sha3 column, identical bytes at different paths) groups by the
// stored SHA-256, confirms with a fresh hash, and the merge moves tags /
// collection membership / document-scoped chats across before deleting the
// duplicate.
func TestFindAndMergeDuplicates(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()

	body := "hello dedup\n"
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	c := filepath.Join(dir, "c.md")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(c, []byte("different bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a legacy library: same (bogus) stored sha256, no sha3. The
	// third row shares the bogus sha256 but is a different file.
	mustExec(t, conn, "INSERT INTO documents(id, path, sha256, status) VALUES(1,?, 'collide','done')", a)
	mustExec(t, conn, "INSERT INTO documents(id, path, sha256, status) VALUES(2,?, 'collide','done')", b)
	mustExec(t, conn, "INSERT INTO documents(id, path, sha256, status) VALUES(3,?, 'collide','done')", c)
	mustExec(t, conn, "INSERT INTO chunks(doc_id, seq, text) VALUES(2, 0, 'x')")
	mustExec(t, conn, "INSERT INTO doc_tags(doc_id, tag, source) VALUES(2, 'shared', 'manual')")
	mustExec(t, conn, "INSERT INTO collections(id, name) VALUES(1, 'Project')")
	mustExec(t, conn, "INSERT INTO collection_docs(collection_id, doc_id) VALUES(1, 2)")
	mustExec(t, conn, "INSERT INTO chat_sessions(id, scope_kind, scope_value) VALUES(1,'document','2')")

	groups, err := FindDuplicateGroups(conn)
	if err != nil {
		t.Fatalf("FindDuplicateGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly one", groups)
	}
	g := groups[0]
	if g.Canonical.ID != 1 || len(g.Duplicates) != 1 || g.Duplicates[0].ID != 2 {
		t.Fatalf("group = %+v, want canonical #1 with duplicate #2", g)
	}

	if err := MergeDocument(conn, dir, g.Canonical.ID, g.Duplicates[0].ID); err != nil {
		t.Fatalf("MergeDocument: %v", err)
	}

	if n := count1(t, conn, "SELECT COUNT(*) FROM documents"); n != 2 {
		t.Fatalf("documents = %d, want 2 (duplicate removed)", n)
	}
	if n := count1(t, conn, "SELECT COUNT(*) FROM chunks WHERE doc_id=2"); n != 0 {
		t.Fatalf("duplicate chunks = %d, want 0 (cascade)", n)
	}
	if n := count1(t, conn, "SELECT COUNT(*) FROM doc_tags WHERE doc_id=1 AND tag='shared'"); n != 1 {
		t.Fatalf("tag did not move to the canonical")
	}
	if n := count1(t, conn, "SELECT COUNT(*) FROM collection_docs WHERE collection_id=1 AND doc_id=1"); n != 1 {
		t.Fatalf("collection membership did not move to the canonical")
	}
	var scope string
	if err := conn.QueryRow("SELECT scope_value FROM chat_sessions WHERE id=1").Scan(&scope); err != nil || scope != "1" {
		t.Fatalf("chat scope = %q err=%v, want \"1\"", scope, err)
	}
}

func mustExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func count1(t *testing.T, conn *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}
