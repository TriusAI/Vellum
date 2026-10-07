package api

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vellum/internal/extract"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/summarize"
	"vellum/internal/vocab"
)

// ----------------------------- watcher loop --------------------------------

// watchLoop periodically scans the configured directories while the watcher
// is enabled. Each scan runs through the job queue, so it shows up in the
// Jobs dialog and can be cancelled like any other job. The loop sleeps
// between scans; changing the interval takes effect on the next tick.
func (s *Server) watchLoop() {
	for {
		time.Sleep(time.Duration(s.watchIntervalSeconds()) * time.Second)
		s.watchMu.Lock()
		enabled := s.cfg.Watch.Enabled && len(s.cfg.Watch.Dirs) > 0
		s.watchMu.Unlock()
		if !enabled {
			continue
		}
		s.watchScanJob(true)
	}
}

// watchIntervalSeconds clamps the configured scan interval (seconds) to a
// sane range; 0 means "use the default".
func (s *Server) watchIntervalSeconds() int {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	n := s.cfg.Watch.Interval
	switch {
	case n <= 0:
		return 15
	case n < 2:
		return 2
	case n > 86400:
		return 86400
	default:
		return n
	}
}

// watchScanJob runs one scan through the FIFO job queue. It blocks until
// the scan has had its turn, which naturally keeps ticks from piling up.
// settle=false ("Scan now") skips the settle wait so a just-added file is
// picked up immediately.
func (s *Server) watchScanJob(settle bool) *Job {
	return s.runJob("watch", "filesystem watch", func(j *Job) error {
		return s.watchScan(j, settle)
	})
}

// watchScan is the watcher's unit of work: discover new/changed files,
// ingest them, then enrich every pending document under the watched folders
// in the requested order (ingest → kind → category → metadata → tags →
// summary).
func (s *Server) watchScan(j *Job, settle bool) error {
	s.watchMu.Lock()
	s.watchRunning = true
	dirs := append([]string(nil), s.cfg.Watch.Dirs...)
	s.watchMu.Unlock()
	defer func() {
		s.watchMu.Lock()
		s.watchRunning = false
		s.watchLast = time.Now()
		s.watchMu.Unlock()
	}()

	candidates := s.watchCandidates(dirs, settle)
	if j.Stopped() {
		return errJobCancelled
	}
	if len(candidates) > 0 {
		st, err := ingest.Ingest(j.ctx, s.cfg, s.conn, candidates, false,
			func(msg string) { s.jobProgress(j, msg) })
		if err != nil {
			return err
		}
		s.watchMu.Lock()
		s.watchAdded += st.Added + st.Updated
		s.watchMu.Unlock()
		s.watchMarkDone(candidates)
	}
	if j.Stopped() {
		return errJobCancelled
	}

	// Enrichment needs the vocabulary and the chat backend. Without them the
	// files stay indexed (status='ingested') and are retried on a later scan.
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		return fmt.Errorf("vocab: %w", err)
	}
	if len(v.Tags) == 0 ||
		!llm.AvailableFor(s.cfg.LLM.Backend, s.cfg.Tools.LLMURL) {
		return nil
	}
	ids, err := s.watchPendingDocs(dirs)
	if err != nil {
		return err
	}
	const perScan = 8 // bound one scan so jobs stay responsive and cancellable
	if len(ids) > perScan {
		ids = ids[:perScan]
	}
	for _, id := range ids {
		if j.Stopped() {
			return errJobCancelled
		}
		if err := ingest.Enrich(j.ctx, s.cfg, s.conn, v, id,
			func(msg string) { s.jobProgress(j, msg) }); err != nil {
			if errors.Is(err, summarize.ErrCancelled) || j.Stopped() {
				return errJobCancelled
			}
			// one bad document must not stop the watcher: record and move on
			log.Printf("watch: enrich #%d failed: %s", id, err)
			s.watchMu.Lock()
			s.watchErr = fmt.Sprintf("#%d: %s", id, err)
			s.watchMu.Unlock()
			continue
		}
	}
	return nil
}

// watchCandidates walks the watched dirs and returns the supported files
// that are new or changed since the last ingest. With settle set (the
// periodic loop) a file must additionally have held the same size+mtime
// across two consecutive scans, so a partially written download is not
// indexed; a manual scan passes settle=false to act at once. Seen/done
// stamps live in memory: a restart simply re-hashes everything once, and
// ingest's sha256 dedup makes that a no-op for known files.
func (s *Server) watchCandidates(dirs []string, settle bool) []string {
	cur := map[string]fileStamp{}
	for _, d := range dirs {
		collectWatchFiles(d, cur)
	}
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	var out []string
	for p, st := range cur {
		if prev, seen := s.watchSeen[p]; settle && (!seen || prev != st) {
			continue // new or still changing: wait for a stable tick
		}
		if d, ok := s.watchDone[p]; ok && d == st {
			continue // already ingested at this exact version
		}
		out = append(out, p)
	}
	// keep seen current; prune done entries for files that vanished
	s.watchSeen = cur
	for p := range s.watchDone {
		if _, ok := cur[p]; !ok {
			delete(s.watchDone, p)
		}
	}
	sort.Strings(out)
	return out
}

// watchMarkDone records the version of the files just handed to ingest, so
// the next scan does not hash them again.
func (s *Server) watchMarkDone(paths []string) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for _, p := range paths {
		if st, ok := s.watchSeen[p]; ok {
			s.watchDone[p] = st
		}
	}
}

// watchPendingDocs lists the ids of pending documents (status='ingested')
// whose path lies under one of the watched dirs.
func (s *Server) watchPendingDocs(dirs []string) ([]int64, error) {
	return ingest.PendingUnderDirs(s.conn, dirs)
}

// collectWatchFiles adds one directory tree's supported files (with stamps)
// to out. Hidden entries are skipped, matching ingest's walk.
func collectWatchFiles(dir string, out map[string]fileStamp) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return
	}
	root := filepath.Clean(dir)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !extract.Supported(path) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		out[path] = fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
		return nil
	})
}

// ----------------------------- HTTP surface ---------------------------------

// getWatch reports the watcher's settings and live state for the Watch dialog.
func (s *Server) getWatch(w http.ResponseWriter, r *http.Request) {
	s.watchMu.Lock()
	enabled := s.cfg.Watch.Enabled
	dirs := append([]string(nil), s.cfg.Watch.Dirs...)
	interval := s.cfg.Watch.Interval
	running := s.watchRunning
	last := s.watchLast
	added := s.watchAdded
	lastErr := s.watchErr
	s.watchMu.Unlock()
	if interval <= 0 {
		interval = 15
	}
	lastStr := ""
	if !last.IsZero() {
		lastStr = last.Format(time.RFC3339)
	}
	pending := 0
	if len(dirs) > 0 {
		if ids, err := s.watchPendingDocs(dirs); err == nil {
			pending = len(ids)
		}
	}
	pendingAll := 0
	s.conn.QueryRow("SELECT COUNT(*) FROM documents WHERE status='ingested'").Scan(&pendingAll)
	writeJSON(w, 200, map[string]any{
		"enabled":     enabled,
		"dirs":        dirs,
		"interval":    interval,
		"running":     running,
		"last_scan":   lastStr,
		"added":       added,
		"pending":     pending,
		"pending_all": pendingAll,
		"last_error":  lastErr,
	})
}

// putWatch updates + saves the watcher settings. Omitted fields keep their
// current value; dirs are normalized to clean absolute paths.
func (s *Server) putWatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool     `json:"enabled"`
		Dirs     *[]string `json:"dirs"`
		Interval *int      `json:"interval"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	s.watchMu.Lock()
	if body.Enabled != nil {
		s.cfg.Watch.Enabled = *body.Enabled
	}
	if body.Dirs != nil {
		dirs := []string{}
		for _, d := range *body.Dirs {
			d = expandHome(strings.TrimSpace(d))
			if d == "" {
				continue
			}
			if abs, err := filepath.Abs(d); err == nil {
				d = abs
			}
			dirs = append(dirs, filepath.Clean(d))
		}
		s.cfg.Watch.Dirs = dirs
	}
	if body.Interval != nil && *body.Interval >= 2 && *body.Interval <= 86400 {
		s.cfg.Watch.Interval = *body.Interval
	}
	s.watchMu.Unlock()
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, "config save failed: "+err.Error())
		return
	}
	s.getWatch(w, r)
}

// watchScanNow triggers one scan immediately (the Watch dialog's "Scan now").
func (s *Server) watchScanNow(w http.ResponseWriter, r *http.Request) {
	s.watchMu.Lock()
	n := len(s.cfg.Watch.Dirs)
	before := s.watchAdded
	s.watchMu.Unlock()
	if n == 0 {
		writeErr(w, 400, "no watched folders configured")
		return
	}
	job := s.watchScanJob(false)
	if job.Status == "error" {
		writeErr(w, 500, job.Error)
		return
	}
	s.watchMu.Lock()
	added := s.watchAdded - before
	s.watchMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"ok": job.Status == "done", "job": job.ID, "added": added,
	})
}

// expandHome resolves a leading ~ against the user's home directory
// (config paths commonly use ~). A bare "~user" is left untouched.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
