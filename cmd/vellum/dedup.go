package main

import (
	"flag"
	"fmt"
	"log"

	"vellum/internal/config"
	"vellum/internal/ingest"
)

// cmdDedup reports documents that are byte-identical (matching SHA-256 and
// SHA3-256) and, with --apply, folds the extras into one canonical entry
// (moving tags, collection membership and document-scoped chats across, then
// deleting the duplicates). Without --apply nothing is changed.
func cmdDedup(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("dedup", flag.ExitOnError)
	apply := fs.Bool("apply", false, "merge duplicates (default: report only)")
	fs.Parse(args)

	conn := mustOpen(cfg)
	groups, err := ingest.FindDuplicateGroups(conn)
	if err != nil {
		log.Fatalf("dedup: %s", err)
	}
	if jsonOut && !*apply {
		printJSON(groups)
		return
	}
	if len(groups) == 0 {
		fmt.Println("no duplicate documents found")
		return
	}
	total := 0
	for _, g := range groups {
		fmt.Printf("identical content — keep #%d %s\n", g.Canonical.ID, g.Canonical.Path)
		for _, d := range g.Duplicates {
			total++
			fmt.Printf("    duplicate #%d %s\n", d.ID, d.Path)
			if *apply {
				if err := ingest.MergeDocument(conn, cfg.BaseDir, g.Canonical.ID, d.ID); err != nil {
					log.Fatalf("dedup: merge #%d into #%d: %s", d.ID, g.Canonical.ID, err)
				}
			}
		}
	}
	if *apply {
		fmt.Printf("merged %d duplicate document(s) into %d group(s)\n", total, len(groups))
	} else {
		fmt.Printf("%d duplicate document(s) in %d group(s) — re-run with --apply to merge\n",
			total, len(groups))
	}
}
