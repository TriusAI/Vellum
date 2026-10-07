package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vellum/internal/config"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/vocab"
)

// cmdWatch manages the filesystem watcher: the folders it watches, whether
// it is enabled, and how often it scans. `vellum serve` runs the watcher in
// the background; `vellum watch run` is a one-shot scan for cron or manual
// use.
//
//	vellum watch                 show the watcher settings
//	vellum watch add PATH...     watch these folders (and enable)
//	vellum watch remove PATH...  stop watching these folders
//	vellum watch clear           watch nothing
//	vellum watch on|off          enable/disable the background watcher
//	vellum watch interval N      seconds between scans (>= 2)
//	vellum watch run             scan once now: ingest + enrich, in order
func cmdWatch(cfg *config.Config, args []string) {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status", "list":
		watchStatus(cfg)
	case "add":
		if len(args) == 0 {
			log.Fatalf("usage: vellum watch add PATH...")
		}
		changed := false
		for _, p := range args {
			abs := watchAbs(p)
			if !containsString(cfg.Watch.Dirs, abs) {
				cfg.Watch.Dirs = append(cfg.Watch.Dirs, abs)
				changed = true
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				fmt.Fprintf(os.Stderr, "note: %s is not an existing directory (still watched)\n", abs)
			}
		}
		sort.Strings(cfg.Watch.Dirs)
		if !cfg.Watch.Enabled {
			cfg.Watch.Enabled = true
			changed = true
		}
		if changed {
			watchSave(cfg)
		}
		watchStatus(cfg)
	case "remove", "rm":
		if len(args) == 0 {
			log.Fatalf("usage: vellum watch remove PATH...")
		}
		for _, p := range args {
			abs := watchAbs(p)
			cfg.Watch.Dirs = removeString(cfg.Watch.Dirs, abs)
		}
		watchSave(cfg)
		watchStatus(cfg)
	case "clear":
		cfg.Watch.Dirs = nil
		watchSave(cfg)
		watchStatus(cfg)
	case "on", "enable":
		cfg.Watch.Enabled = true
		watchSave(cfg)
		watchStatus(cfg)
	case "off", "disable":
		cfg.Watch.Enabled = false
		watchSave(cfg)
		watchStatus(cfg)
	case "interval":
		if len(args) != 1 {
			log.Fatalf("usage: vellum watch interval SECONDS")
		}
		n, err := atoiClamp(args[0], 2, 86400)
		if err != nil {
			log.Fatalf("interval must be a number of seconds (>= 2)")
		}
		cfg.Watch.Interval = n
		watchSave(cfg)
		watchStatus(cfg)
	case "run", "scan":
		watchRun(cfg)
	default:
		log.Fatalf("unknown watch subcommand: %s", sub)
	}
}

func watchStatus(cfg *config.Config) {
	if jsonOut {
		pending := 0
		conn := mustOpen(cfg)
		if ids, err := ingest.PendingUnderDirs(conn, cfg.Watch.Dirs); err == nil {
			pending = len(ids)
		}
		printJSON(map[string]any{
			"enabled":  cfg.Watch.Enabled,
			"dirs":     cfg.Watch.Dirs,
			"interval": cfg.Watch.Interval,
			"pending":  pending,
		})
		return
	}
	state := "off"
	if cfg.Watch.Enabled {
		state = "on"
	}
	fmt.Printf("watcher: %s   interval: %ds\n", state, cfg.Watch.Interval)
	if len(cfg.Watch.Dirs) == 0 {
		fmt.Println("folders: (none — add one with `vellum watch add PATH`)")
	} else {
		fmt.Println("folders:")
		for _, d := range cfg.Watch.Dirs {
			mark := " "
			if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
				mark = "!" // not an existing directory right now
			}
			fmt.Printf("  %s %s\n", mark, d)
		}
	}
	conn := mustOpen(cfg)
	if ids, err := ingest.PendingUnderDirs(conn, cfg.Watch.Dirs); err == nil {
		fmt.Printf("pending under watched folders: %d\n", len(ids))
	}
}

// watchRun is the one-shot scan: ingest every supported file in the watched
// folders (sha256 dedup skips unchanged ones), then enrich the pending
// documents in the requested order (ingest → kind → category → metadata →
// tags → summary).
func watchRun(cfg *config.Config) {
	if len(cfg.Watch.Dirs) == 0 {
		log.Fatalf("no watched folders — add one with `vellum watch add PATH`")
	}
	conn := mustOpen(cfg)
	st, err := ingest.Ingest(nil, cfg, conn, cfg.Watch.Dirs, false,
		func(msg string) { fmt.Println("  …", msg) })
	if err != nil {
		log.Fatalf("watch run: %s", err)
	}
	fmt.Printf("ingest: added=%d updated=%d skipped=%d failed=%d\n",
		st.Added, st.Updated, st.Skipped, st.Failed)

	v, err := vocab.Load(cfg.VocabPath)
	if err != nil {
		log.Fatalf("watch run: %s", err)
	}
	if len(v.Tags) == 0 {
		fmt.Println("vocab.yaml is empty — files are indexed; add tags then re-run to enrich")
		return
	}
	if !llm.AvailableFor(cfg.LLM.Backend, cfg.Tools.LLMURL) {
		fmt.Printf("no chat backend at %s — files are indexed but not enriched\n",
			cfg.Tools.LLMURL)
		return
	}
	ids, err := ingest.PendingUnderDirs(conn, cfg.Watch.Dirs)
	if err != nil {
		log.Fatalf("watch run: %s", err)
	}
	done := 0
	for _, id := range ids {
		if err := ingest.Enrich(nil, cfg, conn, v, id,
			func(msg string) { fmt.Println("  …", msg) }); err != nil {
			fmt.Fprintf(os.Stderr, "enrich #%d failed: %s\n", id, err)
			continue
		}
		done++
	}
	fmt.Printf("enriched %d/%d pending document(s)\n", done, len(ids))
}

func watchSave(cfg *config.Config) {
	if err := cfg.Save(); err != nil {
		log.Fatalf("config save: %s", err)
	}
}

// watchAbs expands a leading ~ and makes the path absolute+clean.
func watchAbs(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

func containsString(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

func removeString(xs []string, x string) []string {
	out := xs[:0]
	for _, e := range xs {
		if e != x {
			out = append(out, e)
		}
	}
	return out
}

// atoiClamp parses a decimal integer and clamps it to [lo, hi].
func atoiClamp(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n, nil
}
