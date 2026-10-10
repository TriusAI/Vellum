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
