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

// fsWatcher signals when something under the watched directories changes.
// newFSWatcher returns nil on platforms without filesystem-notification
// support, and the loop falls back to periodic polling.
type fsWatcher interface {
	Events() <-chan struct{}
	Close()
}

// watchConfig snapshots the watcher settings under the lock.
func (s *Server) watchConfig() (enabled bool, dirs []string, interval int, notify bool, gen int64) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return s.cfg.Watch.Enabled, append([]string(nil), s.cfg.Watch.Dirs...),
		clampInterval(s.cfg.Watch.Interval), s.cfg.Watch.Notify, s.watchGen
}

// clampInterval bounds the min-seconds-between-scans setting (0 = default).
func clampInterval(n int) int {
	switch {
	case n <= 0:
		return 5
	case n < 2:
		return 2
	case n > 86400:
		return 86400
	default:
		return n
	}
}

// watchLoop watches the configured directories. With Notify on it reacts to
// filesystem events (inotify) and runs at most one scan per Interval seconds
// — the interval is a debounce FLOOR, so a burst of changes coalesces into
// one scan. With Notify off, or when the platform has no notification
// support, it polls every Interval seconds. Changing the settings rebuilds
// the watcher and scans once immediately. Every scan runs through the job
// queue, so it is visible in Jobs and cancellable.
func (s *Server) watchLoop() {
	var watcher fsWatcher
	var built bool
	var watched []string
	var gen int64
	var lastScan time.Time
	var dirty, scheduled bool
	tick := make(chan struct{}, 1)

	for {
		enabled, dirs, interval, notify, g := s.watchConfig()
		if !enabled || len(dirs) == 0 {
			if watcher != nil {
				watcher.Close()
				watcher = nil
			}
			s.setWatchEvents(false)
			built, watched, gen = false, nil, g
			select {
			case <-s.watchChanged:
			case <-time.After(2 * time.Second):
			}
			continue
		}

		if !built || g != gen || !equalStrings(dirs, watched) {
			if watcher != nil {
				watcher.Close()
			}
			watcher = nil
			if notify {
				watcher = newFSWatcher(dirs)
			}
			built = true
			watched, gen = append([]string(nil), dirs...), g
			s.setWatchEvents(watcher != nil)
			s.watchMu.Lock()
			s.watchErr = ""
			s.watchMu.Unlock()
			dirty, scheduled = false, false
			// a new configuration takes effect at once
			s.watchScanJob(watcher == nil)
			lastScan = time.Now()
			continue
		}

		intervalDur := time.Duration(interval) * time.Second

		if watcher == nil {
			// polling fallback (Notify off, or no inotify support)
			select {
			case <-s.watchChanged:
			case <-time.After(intervalDur):
				s.watchScanJob(true)
				lastScan = time.Now()
			}
			continue
		}

		// event-driven: a change schedules at most one scan per interval
		select {
		case <-s.watchChanged:
			// settings changed: loop to rebuild
		case <-watcher.Events():
			dirty = true
			if !scheduled {
				delay := intervalDur - time.Since(lastScan)
				if delay < 0 {
					delay = 0
				}
				time.AfterFunc(delay, func() {
					select {
					case tick <- struct{}{}:
					default:
					}
				})
				scheduled = true
			}
		case <-tick:
			scheduled = false
			if dirty {
				dirty = false
				s.watchScanJob(false)
				lastScan = time.Now()
			}
		}
	}
}

func (s *Server) setWatchEvents(on bool) {
	s.watchMu.Lock()
	s.watchEvents = on
	s.watchMu.Unlock()
}

// pokeWatchChanged wakes the loop after a settings change (non-blocking).
func (s *Server) pokeWatchChanged() {
	select {
	case s.watchChanged <- struct{}{}:
	default:
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// watchScanJob runs one scan through the FIFO job queue. It blocks until the
// scan has had its turn, which naturally keeps ticks from piling up.
// settle=true requires a file to hold the same size+mtime across two scans
// (the periodic-poll fallback); event-driven and manual scans pass false.
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
// periodic-poll fallback) a file must additionally have held the same
// size+mtime across two consecutive scans, so a partially written download
// is not indexed; event-driven and manual scans pass settle=false and act at
// once. Seen/done stamps live in memory: a restart re-hashes everything
// once, and ingest's sha256 dedup makes that a no-op for known files.
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
			continue // still changing: wait for a stable tick
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

// watchFolder is one watched folder's settings + live stats for the dialog.
type watchFolder struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Documents int    `json:"documents"`
	Pending   int    `json:"pending"`
}

func (s *Server) watchFolderStats(dirs []string) []watchFolder {
	out := make([]watchFolder, len(dirs))
	for i, d := range dirs {
		out[i].Path = d
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out[i].Exists = true
		}
	}
	rows, err := s.conn.Query("SELECT path, status FROM documents")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p, st string
		if rows.Scan(&p, &st) != nil {
			continue
		}
		for i := range out {
			if pathUnder(out[i].Path, p) {
				out[i].Documents++
				if st == "ingested" {
					out[i].Pending++
				}
			}
		}
	}
	return out
}

func pathUnder(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}

// getWatch reports the watcher's settings and live state for the Watch dialog.
func (s *Server) getWatch(w http.ResponseWriter, r *http.Request) {
	s.watchMu.Lock()
	enabled := s.cfg.Watch.Enabled
	dirs := append([]string(nil), s.cfg.Watch.Dirs...)
	interval := clampInterval(s.cfg.Watch.Interval)
	notify := s.cfg.Watch.Notify
	events := s.watchEvents
	running := s.watchRunning
	last := s.watchLast
	added := s.watchAdded
	lastErr := s.watchErr
	s.watchMu.Unlock()

	mode := "off"
	if enabled && len(dirs) > 0 {
		if events {
			mode = "events"
		} else {
			mode = "poll"
		}
	}
	lastStr := ""
	if !last.IsZero() {
		lastStr = last.Format(time.RFC3339)
	}
	folders := s.watchFolderStats(dirs)
	pending := 0
	for _, f := range folders {
		pending += f.Pending
	}
	pendingAll := 0
	s.conn.QueryRow("SELECT COUNT(*) FROM documents WHERE status='ingested'").Scan(&pendingAll)
	writeJSON(w, 200, map[string]any{
		"enabled":     enabled,
		"dirs":        dirs,
		"folders":     folders,
		"interval":    interval,
		"notify":      notify,
		"mode":        mode,
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
		Notify   *bool     `json:"notify"`
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
	if body.Notify != nil {
		s.cfg.Watch.Notify = *body.Notify
	}
	s.watchGen++
	s.watchMu.Unlock()
	s.pokeWatchChanged()
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
