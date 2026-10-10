package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"vellum/internal/config"
	"vellum/internal/db"
)

// TestIngestMissingPathReportsFailure: a requested path that does not exist
// must surface as a failed FileResult, not a silent zero-file "success" (the
// bug: collectFiles swallowed Stat/WalkDir errors).
func TestIngestMissingPathReportsFailure(t *testing.T) {
	conn, err := db.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()
	cfg := config.Default()
	cfg.BaseDir = t.TempDir()

	st, err := Ingest(nil, cfg, conn, []string{filepath.Join(cfg.BaseDir, "nope")}, false, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if st.Failed != 1 {
		t.Fatalf("Failed = %d, want 1 (stats %+v)", st.Failed, st)
	}
	if len(st.Files) != 1 || st.Files[0].Action != "failed" || st.Files[0].Error == "" {
		t.Fatalf("files = %+v, want one failed entry carrying the OS error", st.Files)
	}
}

// TestIngestUnreadableDirReportsFailure: a directory the process cannot read
// must also surface as a failure. Skipped as root (which bypasses perms).
func TestIngestUnreadableDirReportsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	conn, err := db.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()
	cfg := config.Default()
	cfg.BaseDir = t.TempDir()

	locked := filepath.Join(cfg.BaseDir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // let TempDir cleanup remove it

	st, err := Ingest(nil, cfg, conn, []string{locked}, false, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if st.Failed < 1 || len(st.Files) < 1 || st.Files[0].Action != "failed" {
		t.Fatalf("want a failed entry for the unreadable dir; got Failed=%d files=%+v",
			st.Failed, st.Files)
	}
}

// Ingest must store identical content (a file and its copies at different
// paths) once: later paths come back as "duplicate".
func TestIngestDeduplicatesIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()
	cfg := config.Default()
	cfg.BaseDir = dir

	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(filepath.Join(lib, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lib, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Same\n\nidentical bytes\n"
	os.WriteFile(filepath.Join(lib, "a", "one.md"), []byte(body), 0o644)
	os.WriteFile(filepath.Join(lib, "b", "copy.md"), []byte(body), 0o644) // duplicate
	os.WriteFile(filepath.Join(lib, "a", "other.md"), []byte("different\n"), 0o644)

	st, err := Ingest(nil, cfg, conn, []string{lib}, false, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if st.Added != 2 || st.Duplicates != 1 {
		t.Fatalf("added=%d duplicates=%d, want 2/1 (%+v)", st.Added, st.Duplicates, st)
	}
	var docs int
	conn.QueryRow("SELECT COUNT(*) FROM documents").Scan(&docs)
	if docs != 2 {
		t.Fatalf("documents = %d, want 2", docs)
	}
	var dup FileResult
	for _, f := range st.Files {
		if f.Action == "duplicate" {
			dup = f
		}
	}
	if dup.DuplicateOf == 0 {
		t.Fatalf("no duplicate result with DuplicateOf: %+v", st.Files)
	}
	var sha3 string
	if err := conn.QueryRow("SELECT sha3 FROM documents WHERE id=?", dup.DuplicateOf).
		Scan(&sha3); err != nil || sha3 == "" {
		t.Fatalf("canonical sha3 = %q err=%v, want a stored hash", sha3, err)
	}

	// a second run is a no-op for the known files plus the same duplicate
	st2, err := Ingest(nil, cfg, conn, []string{lib}, false, nil)
	if err != nil {
		t.Fatalf("Ingest#2: %v", err)
	}
	if st2.Added != 0 || st2.Skipped != 2 || st2.Duplicates != 1 {
		t.Fatalf("second ingest added=%d skipped=%d dup=%d, want 0/2/1",
			st2.Added, st2.Skipped, st2.Duplicates)
	}
}

// A copy must not be deduped against a canonical whose file has gone missing
// (that would hide the only surviving bytes behind a stale entry).
func TestIngestDuplicateNotHiddenByMissingCanonical(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()
	cfg := config.Default()
	cfg.BaseDir = dir

	canon := filepath.Join(dir, "gone.md")
	body := "# x\n\nthe only copy of these bytes\n"
	if err := os.WriteFile(canon, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(nil, cfg, conn, []string{canon}, false, nil); err != nil {
		t.Fatalf("Ingest canon: %v", err)
	}
	if err := os.Remove(canon); err != nil { // the canonical file is now gone
		t.Fatal(err)
	}
	survivor := filepath.Join(dir, "survivor.md")
	if err := os.WriteFile(survivor, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Ingest(nil, cfg, conn, []string{survivor}, false, nil)
	if err != nil {
		t.Fatalf("Ingest survivor: %v", err)
	}
	if st.Added != 1 || st.Duplicates != 0 {
		t.Fatalf("added=%d duplicates=%d, want 1/0 (survivor must be kept)",
			st.Added, st.Duplicates)
	}
}
