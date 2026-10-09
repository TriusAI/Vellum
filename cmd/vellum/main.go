// Command vellum is a small local-model library manager: OCR, summaries,
// controlled-vocabulary tagging, full-text + semantic search. Everything
// runs on local models via Ollama; storage is one SQLite file.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"vellum/internal/classify"
	"vellum/internal/collection"
	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/extract"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
)

const usage = `vellum — small local-model library manager

Usage:
  vellum [--config FILE] [-v] [--json] <command> [args]

Commands:
  ingest PATH...        index files/directories (text extraction + OCR fallback)
  process [--limit N]   summarize + tag pending documents (local LLM)
  search QUERY [--semantic] [--limit N]
                        full-text (default) or semantic search
  show all | ID         list library or show one document
  remove ID...         remove documents from the library (files stay on disk)
  rename-category OLD NEW
                        rename a shelf; its whole subtree moves with it
  collection list|create|delete|add|remove|show
                        user-managed groups of documents (research projects)
  note list|add|show|delete
                        freeform scratchpad notes (ideas while reading)
  chat list|show|new|rename|delete
                        saved chatbot sessions (scoped conversations)
  export [PATH]        write a consistent snapshot of the library (backup)
  import PATH          replace the library with a backup (old kept aside)
  vocab list|add|remove|review|promote
                        manage the controlled tag vocabulary
  embed                 embed chunks lacking embeddings
  watch [add|remove|clear|on|off|interval N|notify on|off|run]
                        filesystem watcher: auto-ingest + enrich new files
  serve [--listen ADDR] [--open]
                        local web UI + JSON API (default 127.0.0.1:8090)
  agent                 print AI-agent instructions (commands, JSON, API)

--json switches command output to machine-readable JSON (anywhere in args).
Config: $VELLUM_CONFIG or ./config.yaml (see README). Storage: one SQLite
file (FTS5). Models served locally by llama.cpp llama-server.
`

// versionString is reported by --version, /api/status and `vellum agent`.
const versionString = "0.27.0"

// documentColumns is the explicit projection used everywhere (never SELECT *,
// so the scan order is fixed even if the schema gains columns).
const documentColumns = "id, path, title, authors, year, summary, status, kind, summary_source, category"

type document struct {
	ID            int64  `json:"id"`
	Path          string `json:"path"`
	Title         string `json:"title"`
	Authors       string `json:"authors"`
	Year          string `json:"year"`
	Summary       string `json:"summary"`
	Status        string `json:"status"`
	Kind          string `json:"kind,omitempty"`
	SummarySource string `json:"summary_source,omitempty"`
	Category      string `json:"category,omitempty"`
}

func (d *document) scan(sc scannable) error {
	var title, authors, year, summary, kind, sumSource, category sql.NullString
	if err := sc.Scan(&d.ID, &d.Path, &title, &authors, &year, &summary,
		&d.Status, &kind, &sumSource, &category); err != nil {
		return err
	}
	d.Title, d.Authors, d.Year, d.Summary =
		title.String, authors.String, year.String, summary.String
	d.Kind, d.SummarySource, d.Category =
		kind.String, sumSource.String, category.String
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	// extract global flags (--config, -v, --json) wherever they appear
	cfgPath := ""
	verbose := false
	jsonOut = false
	var args []string
	rest := os.Args[1:]
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--config":
			if i+1 < len(rest) {
				cfgPath = rest[i+1]
				i++
			}
		case strings.HasPrefix(rest[i], "--config="):
			cfgPath = strings.TrimPrefix(rest[i], "--config=")
		case rest[i] == "-v" || rest[i] == "--verbose":
			verbose = true
		case rest[i] == "--json" || rest[i] == "-json":
			jsonOut = true
		default:
			args = append(args, rest[i])
		}
	}
	if verbose {
		log.SetFlags(log.LstdFlags)
	}

	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %s", err)
	}

	switch args[0] {
	case "ingest":
		cmdIngest(cfg, args[1:])
	case "process":
		cmdProcess(cfg, args[1:])
	case "search":
		cmdSearch(cfg, args[1:])
	case "show":
		cmdShow(cfg, args[1:])
	case "vocab":
		cmdVocab(cfg, args[1:])
	case "embed":
		cmdEmbed(cfg, args[1:])
	case "kind":
		cmdKind(cfg, args[1:])
	case "category":
		cmdCategory(cfg, args[1:])
	case "rename-category":
		cmdRenameCategory(cfg, args[1:])
	case "collection":
		cmdCollection(cfg, args[1:])
	case "note":
		cmdNote(cfg, args[1:])
	case "chat":
		cmdChat(cfg, args[1:])
	case "export":
		cmdExport(cfg, args[1:])
	case "import":
		cmdImport(cfg, args[1:])
	case "reextract":
		cmdReextract(cfg, args[1:])
	case "remove":
		cmdRemove(cfg, args[1:])
	case "backends":
		cmdBackends(cfg, args[1:])
	case "skip":
		cmdSkip(cfg, args[1:])
	case "regenerate":
		cmdRegenerate(cfg, args[1:])
	case "watch":
		cmdWatch(cfg, args[1:])
	case "serve":
		cmdServe(cfg, args[1:])
	case "agent", "agents":
		cmdAgent(cfg, args[1:])
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	case "-V", "--version":
		fmt.Printf("vellum %s (AGPL-3.0)\n", versionString)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// jsonOut is set by the --json global flag: commands print machine-readable
// JSON instead of their human formatting.
var jsonOut bool

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Fatalf("json: %s", err)
	}
}

func cmdIngest(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	reprocess := fs.Bool("reprocess", false, "re-extract even unchanged files")
	fs.Parse(args)
	if fs.NArg() == 0 {
		log.Fatalf("ingest needs at least one path")
	}
	conn := mustOpen(cfg)
	st, err := ingest.Ingest(nil, cfg, conn, fs.Args(), *reprocess, nil)
	if err != nil {
		log.Fatalf("ingest: %s", err)
	}
	if jsonOut {
		printJSON(st)
		return
	}
	fmt.Printf("added=%d updated=%d skipped=%d failed=%d\n",
		st.Added, st.Updated, st.Skipped, st.Failed)
	fmt.Println("next: vellum process   (summarize + tag with the LLM)")
}

func cmdProcess(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("process", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max documents to process (when no ids given)")
	fs.Parse(args)

	v := mustLoadVocab(cfg)
	if len(v.Tags) == 0 {
		log.Fatalf("vocab.yaml is empty — add tags first with `vellum vocab add`. " +
			"Tagging needs a controlled vocabulary to constrain the LLM.")
	}
	if !llm.AvailableFor(cfg.LLM.Backend, cfg.Tools.LLMURL) {
		if cfg.LLM.Backend == "ollama" {
			log.Fatalf("no ollama at %s — start ollama or check llm.backend/llm.model in config.yaml",
				cfg.Tools.LLMURL)
		}
		log.Fatalf("no llama-server at %s — start it with the vellum launcher, your own llama.cpp (llm.external: true), or:\n"+
			"  llama-server -m %s --host 127.0.0.1 --port %s -c %d --jinja",
			cfg.Tools.LLMURL, cfg.Models.LLM, portOf(cfg.Tools.LLMURL), cfg.LLM.NumCtx)
	}

	// positional ids: process specific documents (any status)
	var ids []int64
	for _, a := range fs.Args() {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			log.Fatalf("process: not a document id: %q", a)
		}
		ids = append(ids, id)
	}

	conn := mustOpen(cfg)
	results, err := ingest.ProcessPending(nil, cfg, conn, v, ids, *limit,
		func(msg string) { log.Printf("  %s", msg) })
	if err != nil {
		log.Fatalf("process: %s", err)
	}
	if jsonOut {
		printJSON(results)
		return
	}
	done := 0
	for _, r := range results {
		if r.Status == "done" {
			done++
		}
	}
	if len(ids) == 0 {
		fmt.Printf("processed %d document(s)\n", done)
		return
	}
	for _, r := range results {
		fmt.Printf("#%d %s — %s\n", r.DocID, r.Path, r.Status)
	}
}

func cmdSearch(cfg *config.Config, args []string) {
	// Go's flag package requires flags before positionals, but "search
	// QUERY --semantic" is the natural order (and what the README shows).
	// Pull the known flags from anywhere in the args, then treat the rest as
	// the query.
	semantic := false
	limit := 20
	filterKind, filterCategory := "", ""
	var filterTags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--semantic" || args[i] == "-semantic":
			semantic = true
		case args[i] == "--semantic=false":
			semantic = false
		case args[i] == "--limit" || args[i] == "-limit":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					limit = n
					i++
				}
			}
		case strings.HasPrefix(args[i], "--limit="):
			if n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--limit=")); err == nil {
				limit = n
			}
		case args[i] == "--kind" || args[i] == "-kind":
			if i+1 < len(args) {
				filterKind = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--kind="):
			filterKind = strings.TrimPrefix(args[i], "--kind=")
		case args[i] == "--category" || args[i] == "-category":
			if i+1 < len(args) {
				filterCategory = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--category="):
			filterCategory = strings.TrimPrefix(args[i], "--category=")
		case args[i] == "--tag" || args[i] == "-tag":
			if i+1 < len(args) {
				filterTags = append(filterTags, strings.ToLower(args[i+1]))
				i++
			}
		case strings.HasPrefix(args[i], "--tag="):
			filterTags = append(filterTags, strings.ToLower(strings.TrimPrefix(args[i], "--tag=")))
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 {
		log.Fatalf("search needs a query")
	}
	query := strings.Join(positional, " ")
	conn := mustOpen(cfg)

	allowed := allowedDocs(conn, filterKind, filterCategory, filterTags)
	if semantic {
		hits, err := search.Semantic(cfg, conn, query, limit)
		if err != nil {
			log.Fatalf("search: %s", err)
		}
		if allowed != nil {
			kept := hits[:0]
			for _, h := range hits {
				if allowed[h.DocID] {
					kept = append(kept, h)
				}
			}
			hits = kept
		}
		if jsonOut {
			printJSON(hits)
			return
		}
		if len(hits) == 0 {
			fmt.Println("no matches")
			return
		}
		for _, h := range hits {
			fmt.Printf("[%.3f] %s  (%s)\n", h.Snippets[0].Score, h.Title, h.Path)
			for _, s := range h.Snippets[:min(2, len(h.Snippets))] {
				fmt.Printf("    %s: %s...\n", pageOrChunk(s.Page), clip(s.Text, 160))
			}
			fmt.Println()
		}
		return
	}

	hits, err := search.Keyword(conn, query, limit)
	if err != nil {
		log.Fatalf("search: %s", err)
	}
	if allowed != nil {
		kept := hits[:0]
		for _, h := range hits {
			if allowed[h.DocID] {
				kept = append(kept, h)
			}
		}
		hits = kept
	}
	if jsonOut {
		printJSON(hits)
		return
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return
	}
	current := ""
	for _, h := range hits {
		if h.Path != current {
			current = h.Path
			fmt.Printf("\n%s  (%s)\n", or(h.Title, h.Path), h.Path)
		}
		fmt.Printf("  %s: %s\n", pageOrChunk(h.Page), h.Snippet)
	}
}

func cmdShow(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("show needs 'all' or a document id")
	}
	conn := mustOpen(cfg)

	if args[0] == "all" {
		filterKind, filterCategory := "", ""
		var filterTags []string
		rest2 := args[1:]
		args = args[:1]
		for i := 0; i < len(rest2); i++ {
			switch {
			case rest2[i] == "--kind" || rest2[i] == "-kind":
				if i+1 < len(rest2) {
					filterKind = rest2[i+1]
					i++
				}
			case strings.HasPrefix(rest2[i], "--kind="):
				filterKind = strings.TrimPrefix(rest2[i], "--kind=")
			case rest2[i] == "--category" || rest2[i] == "-category":
				if i+1 < len(rest2) {
					filterCategory = rest2[i+1]
					i++
				}
			case strings.HasPrefix(rest2[i], "--category="):
				filterCategory = strings.TrimPrefix(rest2[i], "--category=")
			case rest2[i] == "--tag" || rest2[i] == "-tag":
				if i+1 < len(rest2) {
					filterTags = append(filterTags, strings.ToLower(rest2[i+1]))
					i++
				}
			case strings.HasPrefix(rest2[i], "--tag="):
				filterTags = append(filterTags, strings.ToLower(strings.TrimPrefix(rest2[i], "--tag=")))
			default:
				log.Fatalf("show all: unexpected argument %q", rest2[i])
			}
		}
		allowed := allowedDocs(conn, filterKind, filterCategory, filterTags)
		rows, err := conn.Query("SELECT " + documentColumns + " FROM documents ORDER BY id")
		if err != nil {
			log.Fatalf("show: %s", err)
		}
		defer rows.Close()
		type docWithTags struct {
			document
			Tags []string `json:"tags"`
		}
		var all []docWithTags
		for rows.Next() {
			var d document
			if err := d.scan(rows); err != nil {
				log.Fatalf("show: %s", err)
			}
			if allowed != nil && !allowed[d.ID] {
				continue
			}
			all = append(all, docWithTags{document: d,
				Tags: strings.Split(docTags(conn, d.ID), ", ")})
		}
		for i := range all {
			if len(all[i].Tags) == 1 && all[i].Tags[0] == "" {
				all[i].Tags = nil
			}
		}
		if jsonOut {
			printJSON(all)
			return
		}
		for _, d := range all {
			head := fmt.Sprintf("#%d %s", d.ID, or(d.Title, d.Path))
			if d.Status != "done" {
				head += fmt.Sprintf("  [%s]", d.Status)
			}
			fmt.Println(head)
			if d.Authors != "" || d.Year != "" {
				fmt.Println("    " + strings.TrimSpace(d.Authors+" "+d.Year))
			}
			if d.Tags != nil {
				fmt.Println("    tags: " + strings.Join(d.Tags, ", "))
			}
			if d.Summary != "" {
				fmt.Println(wrap(d.Summary, "    ", 80))
			}
			fmt.Println()
		}
		return
	}

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		log.Fatalf("show expects 'all' or a numeric id")
	}
	var d document
	row := conn.QueryRow("SELECT "+documentColumns+" FROM documents WHERE id=?", id)
	err = d.scan(row)
	if err == sql.ErrNoRows {
		log.Fatalf("no document #%d", id)
	}
	if err != nil {
		log.Fatalf("show: %s", err)
	}
	tags := strings.Split(docTags(conn, id), ", ")
	if len(tags) == 1 && tags[0] == "" {
		tags = nil
	}
	if jsonOut {
		type docWithTags struct {
			document
			Tags []string `json:"tags"`
		}
		printJSON(docWithTags{document: d, Tags: tags})
		return
	}
	printKV := func(k, v string) {
		if v != "" {
			fmt.Printf("%s: %s\n", k, v)
		}
	}
	printKV("id", strconv.FormatInt(d.ID, 10))
	printKV("path", d.Path)
	printKV("title", d.Title)
	printKV("authors", d.Authors)
	printKV("year", d.Year)
	printKV("status", d.Status)
	printKV("summary", d.Summary)
	if tags := docTags(conn, d.ID); tags != "" {
		printKV("tags", tags)
	}
}

// loadDocument reads one document row by id.
func loadDocument(conn *sql.DB, id int64) (document, error) {
	var d document
	err := d.scan(conn.QueryRow("SELECT "+documentColumns+" FROM documents WHERE id=?", id))
	return d, err
}

// firstNonEmpty returns the first non-empty string (display helper).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func cmdVocab(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("vocab needs an action: list|add|remove|review|promote")
	}
	action := args[0]
	v := mustLoadVocab(cfg)
	conn := mustOpen(cfg)

	switch action {
	case "list":
		if jsonOut {
			type item struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			out := []item{}
			for _, k := range v.SortedKeys() {
				out = append(out, item{Name: k, Description: v.Tags[k]})
			}
			printJSON(out)
			return
		}
		for _, k := range v.SortedKeys() {
			fmt.Printf("%s: %s\n", k, v.Tags[k])
		}

	case "add":
		if len(args) < 3 {
			log.Fatalf(`usage: vellum vocab add NAME "DESCRIPTION"`)
		}
		if v.Add(args[1], strings.Join(args[2:], " ")) {
			mustSaveVocab(v)
			fmt.Println("added")
		} else {
			fmt.Printf("'%s' already in vocabulary\n", args[1])
		}

	case "remove":
		if len(args) < 2 {
			log.Fatalf("usage: vellum vocab remove NAME")
		}
		if v.Remove(args[1]) {
			mustSaveVocab(v)
			conn.Exec("DELETE FROM doc_tags WHERE tag=?", strings.ToLower(args[1]))
			fmt.Println("removed")
		} else {
			fmt.Printf("'%s' not in vocabulary\n", args[1])
		}

	case "review":
		rows, err := conn.Query(`
SELECT t.tag, COUNT(*) AS n, MIN(d.title) AS example
FROM doc_tags t JOIN documents d ON d.id = t.doc_id
WHERE t.source='suggested'
GROUP BY t.tag ORDER BY n DESC, t.tag`)
		if err != nil {
			log.Fatalf("vocab review: %s", err)
		}
		defer rows.Close()
		type suggestion struct {
			Tag     string `json:"tag"`
			Count   int    `json:"count"`
			Example string `json:"example"`
		}
		var all []suggestion
		any := false
		for rows.Next() {
			var tag, example sql.NullString
			var n int
			if err := rows.Scan(&tag, &n, &example); err != nil {
				log.Fatalf("vocab review: %s", err)
			}
			any = true
			all = append(all, suggestion{Tag: tag.String, Count: n, Example: example.String})
			if !jsonOut {
				fmt.Printf("  %s  ×%d  (e.g. %s)\n", tag.String, n, example.String)
			}
		}
		if jsonOut {
			printJSON(all)
			return
		}
		if any {
			fmt.Println("\npromote the keepers:  " +
				`vellum vocab promote <tag> "description"`)
		} else {
			fmt.Println("no suggested tags to review")
		}

	case "promote":
		if len(args) < 3 {
			log.Fatalf(`usage: vellum vocab promote NAME "DESCRIPTION"`)
		}
		name := strings.ToLower(strings.TrimSpace(args[1]))
		v.Add(name, strings.Join(args[2:], " "))
		mustSaveVocab(v)
		res, _ := conn.Exec("UPDATE doc_tags SET source='vocab' WHERE tag=?", name)
		if n, err := res.RowsAffected(); err == nil {
			fmt.Printf("promoted '%s'; %d existing document tag(s) now controlled\n",
				name, n)
		} else {
			fmt.Printf("promoted '%s'\n", name)
		}

	default:
		log.Fatalf("unknown vocab action: %s", action)
	}
}

// cmdCategory shows or sets the user-curated category ("shelving").
func cmdCategory(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum category ID [VALUE]   (no value = show; 'none' clears)")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		log.Fatalf("category expects a numeric document id")
	}
	conn := mustOpen(cfg)
	if len(args) < 2 {
		var cat sql.NullString
		if err := conn.QueryRow("SELECT category FROM documents WHERE id=?", id).
			Scan(&cat); err != nil {
			log.Fatalf("category: %s", err)
		}
		if cat.String == "" {
			fmt.Printf("#%d: uncategorized\n", id)
		} else {
			fmt.Printf("#%d: %s\n", id, cat.String)
		}
		return
	}
	value := strings.ToLower(strings.TrimSpace(args[1]))
	if value == "none" || value == "-" {
		value = ""
	}
	if _, err := conn.Exec("UPDATE documents SET category=?, category_user=1 WHERE id=?", value, id); err != nil {
		log.Fatalf("category: %s", err)
	}
	fmt.Printf("#%d: category set to %q (user-pinned; the model will not re-file it)\n", id, value)
}

// cmdRenameCategory renames a category everywhere, moving its whole
// subtree ("machine-learning" -> "computer-science/machine-learning"
// turns "machine-learning/transformers" into
// "computer-science/machine-learning/transformers"). Renaming onto an
// existing shelf merges them.
//
//	vellum rename-category OLD NEW
func cmdRenameCategory(cfg *config.Config, args []string) {
	if len(args) != 2 {
		log.Fatalf("usage: vellum rename-category OLD NEW   (the whole subtree moves)")
	}
	from := strings.Trim(args[0], "/")
	to := strings.Trim(args[1], "/")
	if from == "" || to == "" {
		log.Fatalf("rename-category: from and to are required")
	}
	if to == from || strings.HasPrefix(to, from+"/") {
		log.Fatalf("rename-category: cannot nest a category inside itself")
	}
	conn := mustOpen(cfg)
	tx, err := conn.Begin()
	if err != nil {
		log.Fatalf("rename-category: %s", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE documents SET category=? WHERE category=?", to, from)
	if err != nil {
		log.Fatalf("rename-category: %s", err)
	}
	n, _ := res.RowsAffected()
	res, err = tx.Exec(
		"UPDATE documents SET category=?||substr(category,?) WHERE substr(category,1,?)=?",
		to, len(from)+1, len(from)+1, from+"/")
	if err != nil {
		log.Fatalf("rename-category: %s", err)
	}
	n2, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		log.Fatalf("rename-category: %s", err)
	}
	if jsonOut {
		printJSON(map[string]any{"from": from, "to": to, "updated": n + n2})
		return
	}
	fmt.Printf("renamed %q -> %q (%d document(s) moved)\n", from, to, n+n2)
}

// cmdCollection manages collections: user-defined groups of documents
// (research projects). A paper can be in several collections; membership
// is entirely manual.
//
//	vellum collection list
//	vellum collection create NAME ["description"]
//	vellum collection delete NAME|ID
//	vellum collection add NAME|ID DOCID...
//	vellum collection remove NAME|ID DOCID...
//	vellum collection show NAME|ID
func cmdCollection(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum collection list|create|delete|add|remove|show ...")
	}
	conn := mustOpen(cfg)
	sub := args[0]
	rest := args[1:]

	// collectionRef resolves a NAME or numeric id to a collection.
	collectionRef := func(ref string) db.Collection {
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			var c db.Collection
			err := conn.QueryRow(
				"SELECT id, name, description FROM collections WHERE id=?", id).
				Scan(&c.ID, &c.Name, &c.Description)
			if err == sql.ErrNoRows {
				log.Fatalf("collection: no collection with id %d", id)
			}
			if err != nil {
				log.Fatalf("collection: %s", err)
			}
			return c
		}
		var c db.Collection
		err := conn.QueryRow(
			"SELECT id, name, description FROM collections WHERE name=?", ref).
			Scan(&c.ID, &c.Name, &c.Description)
		if err == sql.ErrNoRows {
			log.Fatalf("collection: no collection named %q", ref)
		}
		if err != nil {
			log.Fatalf("collection: %s", err)
		}
		return c
	}
	// docIDs parses remaining args as document ids, validating they exist.
	docIDs := func(vals []string) []int64 {
		out := []int64{}
		for _, v := range vals {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				log.Fatalf("collection: %q is not a document id", v)
			}
			var one int
			if err := conn.QueryRow("SELECT COUNT(*) FROM documents WHERE id=?", id).
				Scan(&one); err != nil || one == 0 {
				log.Fatalf("collection: no document with id %d", id)
			}
			out = append(out, id)
		}
		return out
	}

	switch sub {
	case "list":
		cols, err := db.ListCollections(conn)
		if err != nil {
			log.Fatalf("collection: %s", err)
		}
		if jsonOut {
			printJSON(cols)
			return
		}
		if len(cols) == 0 {
			fmt.Println("no collections yet (vellum collection create NAME \"description\")")
			return
		}
		for _, c := range cols {
			desc := ""
			if c.Description != "" {
				desc = " — " + c.Description
			}
			fmt.Printf("#%d  %s  (%d document(s))%s\n", c.ID, c.Name, c.Documents, desc)
		}

	case "create":
		if len(rest) < 1 {
			log.Fatalf("usage: vellum collection create NAME [\"description\"]")
		}
		name := rest[0]
		desc := ""
		if len(rest) > 1 {
			desc = strings.Join(rest[1:], " ")
		}
		c, err := db.CreateCollection(conn, name, desc)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				log.Fatalf("collection: a collection named %q already exists", name)
			}
			log.Fatalf("collection: %s", err)
		}
		if jsonOut {
			printJSON(c)
			return
		}
		fmt.Printf("created collection #%d %q\n", c.ID, c.Name)

	case "delete":
		if len(rest) < 1 {
			log.Fatalf("usage: vellum collection delete NAME|ID")
		}
		c := collectionRef(rest[0])
		if err := db.DeleteCollection(conn, c.ID); err != nil {
			log.Fatalf("collection: %s", err)
		}
		fmt.Printf("deleted collection #%d %q (documents are untouched)\n", c.ID, c.Name)

	case "add":
		if len(rest) < 2 {
			log.Fatalf("usage: vellum collection add NAME|ID DOCID...")
		}
		c := collectionRef(rest[0])
		ids := docIDs(rest[1:])
		added, err := db.AddDocsToCollection(conn, c.ID, ids)
		if err != nil {
			log.Fatalf("collection: %s", err)
		}
		fmt.Printf("added %d document(s) to %q\n", added, c.Name)

	case "remove":
		if len(rest) < 2 {
			log.Fatalf("usage: vellum collection remove NAME|ID DOCID...")
		}
		c := collectionRef(rest[0])
		ids := docIDs(rest[1:])
		for _, id := range ids {
			if err := db.RemoveDocFromCollection(conn, c.ID, id); err != nil {
				log.Fatalf("collection: %s", err)
			}
		}
		fmt.Printf("removed %d document(s) from %q\n", len(ids), c.Name)

	case "show":
		if len(rest) < 1 {
			log.Fatalf("usage: vellum collection show NAME|ID")
		}
		c := collectionRef(rest[0])
		ids, err := db.CollectionDocIDs(conn, c.ID)
		if err != nil {
			log.Fatalf("collection: %s", err)
		}
		// reuse the show query surface: load the docs the CLI way
		docs := []document{}
		for _, id := range ids {
			d, err := loadDocument(conn, id)
			if err == nil {
				docs = append(docs, d)
			}
		}
		if jsonOut {
			printJSON(map[string]any{"collection": c, "documents": docs})
			return
		}
		fmt.Printf("%s (%d document(s))", c.Name, len(docs))
		if c.Description != "" {
			fmt.Printf(" — %s", c.Description)
		}
		fmt.Println()
		for _, d := range docs {
			fmt.Printf("  #%d  %s  (%s)\n", d.ID, firstNonEmpty(d.Title, filepath.Base(d.Path)), d.Status)
		}

	case "export":
		if len(rest) < 1 {
			log.Fatalf("usage: vellum collection export NAME|ID [PATH.zip]")
		}
		c := collectionRef(rest[0])
		out := "collection-" + collection.Slug(c.Name) + ".zip"
		if len(rest) > 1 {
			out = rest[1]
		}
		if _, err := os.Stat(out); err == nil {
			log.Fatalf("collection export: %s already exists", out)
		}
		f, err := os.Create(out)
		if err != nil {
			log.Fatalf("collection export: %s", err)
		}
		name, err := collection.Export(conn, c.ID, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out)
			log.Fatalf("collection export: %s", err)
		}
		if jsonOut {
			printJSON(map[string]any{"path": out, "collection": name})
			return
		}
		fmt.Printf("exported %q -> %s   (share it; import with: vellum collection import %s)\n",
			name, out, out)

	case "import":
		if len(rest) < 1 {
			log.Fatalf("usage: vellum collection import PATH.zip [--name NAME]")
		}
		zipPath := rest[0]
		nameOverride := ""
		for i := 1; i < len(rest); i++ {
			if rest[i] == "--name" && i+1 < len(rest) {
				nameOverride = rest[i+1]
				i++
			}
		}
		f, err := os.Open(zipPath)
		if err != nil {
			log.Fatalf("collection import: %s", err)
		}
		defer f.Close()
		res, err := collection.Import(context.Background(), cfg, conn, f, nameOverride)
		if err != nil {
			log.Fatalf("collection import: %s", err)
		}
		if jsonOut {
			printJSON(map[string]any{
				"collection": res.Collection, "documents": res.Documents, "dir": res.Dir})
			return
		}
		fmt.Printf("imported collection #%d %q (%d document(s); files in %s)\n",
			res.Collection.ID, res.Collection.Name, res.Documents, res.Dir)

	default:
		log.Fatalf("collection: unknown subcommand %q (list|create|delete|add|remove|show|export|import)", sub)
	}
}

// cmdNote manages scratchpad notes (body + timestamps only).
//
//	vellum note list
//	vellum note add "text"      # or - to read stdin
//	vellum note show ID
//	vellum note delete ID
func cmdNote(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum note list|add|show|delete ...")
	}
	conn := mustOpen(cfg)
	switch args[0] {
	case "list":
		notes, err := db.ListNotes(conn)
		if err != nil {
			log.Fatalf("note: %s", err)
		}
		if jsonOut {
			printJSON(notes)
			return
		}
		if len(notes) == 0 {
			fmt.Println("no notes yet (vellum note add \"...\")")
			return
		}
		for _, n := range notes {
			fmt.Printf("#%d  %s  (updated %s)\n", n.ID, noteTitle(n), n.UpdatedAt)
		}

	case "add":
		if len(args) < 2 {
			log.Fatalf("usage: vellum note add \"text\"   (use - to read stdin)")
		}
		body := args[1]
		if body == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				log.Fatalf("note add: %s", err)
			}
			body = string(data)
		}
		n, err := db.CreateNote(conn, body)
		if err != nil {
			log.Fatalf("note add: %s", err)
		}
		if jsonOut {
			printJSON(n)
			return
		}
		fmt.Printf("created note #%d\n", n.ID)

	case "show":
		if len(args) < 2 {
			log.Fatalf("usage: vellum note show ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			log.Fatalf("note show expects a numeric id")
		}
		n, err := db.GetNote(conn, id)
		if err != nil {
			log.Fatalf("note show: %s", err)
		}
		if jsonOut {
			printJSON(n)
			return
		}
		fmt.Printf("#%d (created %s, updated %s)\n\n%s\n", n.ID, n.CreatedAt, n.UpdatedAt, n.Body)

	case "delete":
		if len(args) < 2 {
			log.Fatalf("usage: vellum note delete ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			log.Fatalf("note delete expects a numeric id")
		}
		if err := db.DeleteNote(conn, id); err != nil {
			log.Fatalf("note delete: %s", err)
		}
		fmt.Printf("deleted note #%d\n", id)

	default:
		log.Fatalf("note: unknown subcommand %q (list|add|show|delete)", args[0])
	}
}

// noteTitle derives a display title from a note body (its first non-empty
// line, capped), falling back to "Note #id".
func noteTitle(n db.Note) string {
	for _, line := range strings.Split(n.Body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#*-> "))
		if line != "" {
			if len(line) > 60 {
				line = line[:60] + "…"
			}
			return line
		}
	}
	return fmt.Sprintf("Note #%d", n.ID)
}

// cmdExport writes a consistent snapshot of the library to PATH (a
// plain SQLite file — restore it with `vellum import` or the UI's
// Import/Export dialog). Safe to run any time, even while `vellum serve`
// is processing.
//
//	vellum export [PATH]
func cmdExport(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	fs.Parse(args)
	out := "vellum-library-" + time.Now().Format("20060102-150405") + ".db"
	if fs.NArg() > 0 {
		out = fs.Arg(0)
	}
	if _, err := os.Stat(out); err == nil {
		log.Fatalf("export: %s already exists", out)
	}
	conn := mustOpen(cfg)
	defer conn.Close()
	if _, err := conn.Exec(
		"VACUUM INTO '" + strings.ReplaceAll(out, "'", "''") + "'"); err != nil {
		log.Fatalf("export: %s", err)
	}
	if jsonOut {
		printJSON(map[string]any{"path": out})
		return
	}
	fmt.Printf("library snapshot -> %s   (restore with: vellum import %s)\n", out, out)
}

// cmdImport replaces the library database with a backup file (validated
// first: it must be a vellum library). The current library is moved aside
// to <db>.pre-import-<timestamp>, so nothing is lost. STOP a running
// `vellum serve` before importing from the CLI — a live serve holds the
// old file open and keeps using it (the UI's import does the swap
// live-safely via the API).
//
//	vellum import PATH
func cmdImport(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatalf("usage: vellum import PATH   (a backup .db from vellum export)")
	}
	src := fs.Arg(0)
	n, err := db.ValidateLibrary(src)
	if err != nil {
		log.Fatalf("import: %s is not a vellum library database: %s", src, err)
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		backup := cfg.DBPath + ".pre-import-" + time.Now().Format("20060102-150405")
		if err := os.Rename(cfg.DBPath, backup); err != nil {
			log.Fatalf("import: %s", err)
		}
		os.Remove(cfg.DBPath + "-wal") // stale WAL of the old library
		os.Remove(cfg.DBPath + "-shm")
		fmt.Printf("current library kept at %s\n", backup)
	}
	in, err := os.Open(src)
	if err != nil {
		log.Fatalf("import: %s", err)
	}
	defer in.Close()
	out, err := os.Create(cfg.DBPath)
	if err != nil {
		log.Fatalf("import: %s", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		log.Fatalf("import: %s", err)
	}
	if err := out.Close(); err != nil {
		log.Fatalf("import: %s", err)
	}
	conn, err := db.Open(cfg.DBPath) // applies any pending migrations
	if err != nil {
		log.Fatalf("import: the file failed to open as a library: %s", err)
	}
	conn.Close()
	if jsonOut {
		printJSON(map[string]any{"imported": n, "path": src})
		return
	}
	fmt.Printf("imported %d document(s) from %s\n", n, src)
	fmt.Fprintln(os.Stderr, "note: stop vellum serve before importing from the CLI — a running serve keeps using the old file")
}

// allowedDocs returns the set of document ids matching the filters, or nil
// when no filter is set (all allowed).
func allowedDocs(conn *sql.DB, kind, category string, tags []string) map[int64]bool {
	if kind == "" && category == "" && len(tags) == 0 {
		return nil
	}
	q := "SELECT DISTINCT d.id, d.kind, d.category FROM documents d"
	args := []any{}
	if len(tags) > 0 {
		q += " JOIN doc_tags t ON t.doc_id = d.id AND t.tag IN ("
		for i, t := range tags {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, t)
		}
		q += ") GROUP BY d.id HAVING COUNT(DISTINCT t.tag) = ?"
		args = append(args, len(tags))
	}
	rows, err := conn.Query(q, args...)
	if err != nil {
		log.Fatalf("filter: %s", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var k, c sql.NullString
		rows.Scan(&id, &k, &c)
		if kind != "" && !strings.EqualFold(k.String, kind) {
			continue
		}
		if category != "" && !strings.EqualFold(c.String, category) {
			continue
		}
		out[id] = true
	}
	return out
}

// cmdKind shows or overrides the detected document kind ("paper", "book",
// "gallery", "course", "reference", or any custom string).
func cmdKind(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum kind ID [VALUE]   (no value = show)")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		log.Fatalf("kind expects a numeric document id")
	}
	conn := mustOpen(cfg)
	var kind sql.NullString
	err = conn.QueryRow("SELECT kind FROM documents WHERE id=?", id).Scan(&kind)
	if err == sql.ErrNoRows {
		log.Fatalf("no document #%d", id)
	}
	if err != nil {
		log.Fatalf("kind: %s", err)
	}
	if len(args) < 2 {
		if kind.String == "" {
			fmt.Printf("#%d: kind not detected (generic processing)\n", id)
		} else {
			fmt.Printf("#%d: %s\n", id, kind.String)
		}
		return
	}
	value := strings.ToLower(strings.TrimSpace(args[1]))
	if value == "none" || value == "-" {
		value = ""
	}
	if _, err := conn.Exec("UPDATE documents SET kind=?, kind_user=1 WHERE id=?", value, id); err != nil {
		log.Fatalf("kind: %s", err)
	}
	fmt.Printf("#%d: kind set to %q (user-set kinds are kept; 'none' clears and marks generic)\n", id, value)
	fmt.Println("note: re-run `vellum process " + args[0] + "` to process with the new kind")
}

// cmdReextract re-runs text extraction on a document's source file.
// Without --force-ocr only broken-looking pages are re-OCR'd; with it
// every page is rasterized + OCR'd (the repair path for a garbled
// embedded text layer). The text is replaced in place and the document
// becomes pending again (re-summarize + re-tag to refresh derived data).
// parseStoredPages decodes the stored ocr_done_pages csv (or "all").
func parseStoredPages(s string, nPages int) []int {
	if s == "all" {
		if nPages <= 0 {
			return nil
		}
		out := make([]int, 0, nPages)
		for i := 1; i <= nPages; i++ {
			out = append(out, i)
		}
		return out
	}
	pages, err := parsePageList(s)
	if err != nil {
		return nil
	}
	return pages
}

// mergeDone accumulates per-page repair history on the document.
// pages==nil && all=true marks "all".
func mergeDone(conn *sql.DB, id int64, pages []int, all int) {
	if pages != nil {
		var current string
		conn.QueryRow("SELECT ocr_done_pages FROM documents WHERE id=?", id).Scan(&current)
		allPrev := parseStoredPages(current, 1<<20)
		full := sortedSetUnion(allPrev, pages)
		var parts []string
		for _, p := range full {
			parts = append(parts, strconv.Itoa(p))
		}
		conn.Exec("UPDATE documents SET ocr_done_pages=? WHERE id=?",
			strings.Join(parts, ","), id)
		return
	}
	if all == -1 {
		conn.Exec("UPDATE documents SET ocr_done_pages='all' WHERE id=?", id)
	}
}

// sortedSetUnion merges and dedupes page lists.
func sortedSetUnion(a, b []int) []int {
	set := map[int]bool{}
	for _, p := range a {
		set[p] = true
	}
	for _, p := range b {
		set[p] = true
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// parsePageList parses "3,7-12" into 1-based page numbers.
func parsePageList(s string) ([]int, error) {
	out := []int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, b, found := strings.Cut(part, "-"); found {
			lo, err1 := strconv.Atoi(strings.TrimSpace(a))
			hi, err2 := strconv.Atoi(strings.TrimSpace(b))
			if err1 != nil || err2 != nil || lo < 1 || hi < lo {
				return nil, fmt.Errorf("bad page range %q", part)
			}
			for p := lo; p <= hi && p-lo < 5000; p++ {
				out = append(out, p)
			}
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 {
			return nil, fmt.Errorf("bad page number %q", part)
		}
		out = append(out, p)
	}
	return out, nil
}

func cmdReextract(cfg *config.Config, args []string) {
	force := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--force-ocr", "-force-ocr":
			force = true
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 1 {
		log.Fatalf("usage: vellum reextract ID [--force-ocr] [--pages 3,7-12]")
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		log.Fatalf("reextract expects a numeric document id")
	}
	var pages []int
	for i, a := range args {
		if (a == "--pages" || a == "-pages") && i+1 < len(args) {
			pages, err = parsePageList(args[i+1])
			if err != nil {
				log.Fatalf("reextract: %s", err)
			}
			force = true // --pages implies force on those pages
		}
	}
	conn := mustOpen(cfg)
	var path, donePages string
	if err := conn.QueryRow("SELECT path, ocr_done_pages FROM documents WHERE id=?", id).
		Scan(&path, &donePages); err != nil {
		log.Fatalf("reextract: %s", err)
	}
	var res *extract.Result
	switch {
	case force && len(pages) > 0:
		fmt.Printf("re-extracting #%d: OCR forced on %d page(s)...\n", id, len(pages))
		// previously repaired pages stay repaired even though the text
		// pass rebuilds every chunk
		previous := parseStoredPages(donePages, len(res.Chunks))
		res, err = extract.ExtractOCRPages(path, cfg, true, append(append([]int{}, pages...), previous...))
		mergeDone(conn, id, pages, len(res.Chunks))
	case force:
		fmt.Printf("re-extracting #%d with OCR forced on every page (slow)...\n", id)
		res, err = extract.ExtractOCR(path, cfg)
		mergeDone(conn, id, nil, -1) // all
	default:
		fmt.Printf("re-extracting #%d...\n", id)
		res, err = extract.Extract(path, cfg)
	}
	if err != nil {
		log.Fatalf("reextract: %s", err)
	}
	if err := db.ReplaceDocumentText(conn, id, res.Chunks); err != nil {
		log.Fatalf("reextract: %s", err)
	}
	var kindUser int64
	conn.QueryRow("SELECT kind_user FROM documents WHERE id=?", id).Scan(&kindUser)
	kind := ""
	if kindUser == 0 && len(res.Chunks) > 0 {
		var sb strings.Builder
		for _, c := range res.Chunks {
			sb.WriteString(c.Text)
			sb.WriteString("\n\n")
			if sb.Len() > 200000 {
				break
			}
		}
		kind, _ = classify.Detect(sb.String(), res.OCRPages, len(res.Chunks))
	}
	if _, err := conn.Exec(
		"UPDATE documents SET kind=?, ocr_pages=?, n_pages=?, ocr_pending=0, status='ingested', error=NULL, processed_at=NULL WHERE id=?",
		kind, res.OCRPages, len(res.Chunks), id); err != nil {
		log.Fatalf("reextract: %s", err)
	}
	fmt.Printf("#%d: re-extracted (%d chunks, %d OCR pages) — status back to pending;\n"+
		"process it again to refresh summary/tags: vellum process %d\n",
		id, len(res.Chunks), res.OCRPages, id)
}

// cmdRegenerate rebuilds individual metadata fields in place, without a
// full reprocess: meta (title/authors/year), summary, tags, category,
// kind. Default (no fields) regenerates the LLM-derived set.
func cmdRegenerate(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum regenerate ID [meta|summary|tags|category|kind ...]")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		log.Fatalf("regenerate expects a numeric document id")
	}
	fields := args[1:]
	if len(fields) == 0 {
		fields = []string{"meta", "summary", "tags", "category"}
	}
	conn := mustOpen(cfg)
	v, err := vocab.Load(cfg.VocabPath)
	if err != nil {
		log.Fatalf("regenerate: %s", err)
	}
	if len(v.Tags) == 0 {
		log.Fatalf("vocab.yaml is empty — add tags first")
	}
	if !llm.AvailableFor(cfg.LLM.Backend, cfg.Tools.LLMURL) {
		log.Fatalf("no chat backend at %s (llm.backend=%s) — start ollama/the vellum launcher or your own llama-server",
			cfg.Tools.LLMURL, cfg.LLM.Backend)
	}
	applied, err := ingest.Regenerate(nil, cfg, conn, v, id, fields,
		func(msg string) { fmt.Println("  …", msg) })
	if err != nil {
		log.Fatalf("regenerate: %s", err)
	}
	fmt.Printf("#%d: regenerated %v\n", id, applied)
}

// cmdBackends prints the per-channel backend decision for the shell
// launcher (which must decide whether to start bundled llama-servers
// without parsing YAML):
//
//	chat=<bundled|external> embed=<bundled|external>
//
// external means: ollama backend/provider, or llm/embed external: true
// (the user's own llama.cpp, often on a GPU).
func cmdBackends(cfg *config.Config, args []string) {
	chat := "bundled"
	if cfg.LLM.Backend == "ollama" || cfg.LLM.External {
		chat = "external"
	}
	embed := "bundled"
	if cfg.Embed.Provider == "ollama" || cfg.Embed.External {
		embed = "external"
	}
	fmt.Printf("chat=%s embed=%s\n", chat, embed)
}

func cmdEmbed(cfg *config.Config, args []string) {
	conn := mustOpen(cfg)
	n, err := search.EmbedPending(cfg, conn)
	if err != nil {
		log.Fatalf("embed: %s", err)
	}
	fmt.Printf("embedded %d chunk(s)\n", n)
}

// ---------------------------------------------------------------- helpers

func docTags(conn *sql.DB, docID int64) string {
	rows, err := conn.Query("SELECT tag FROM doc_tags WHERE doc_id=? ORDER BY tag", docID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		tags = append(tags, t)
	}
	return strings.Join(tags, ", ")
}

func wrap(s, indent string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		if i > 0 && lineLen+1+len(w) > width-len(indent) {
			b.WriteString("\n" + indent)
			lineLen = 0
		} else if i > 0 {
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(w)
		lineLen += len(w)
	}
	return indent + b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func pageOrChunk(page int) string {
	if page > 0 {
		return fmt.Sprintf("p.%d", page)
	}
	return "chunk"
}

// portOf extracts the port from a URL for the startup hint message.
func portOf(url string) string {
	if i := strings.LastIndex(url, ":"); i >= 0 {
		if p := url[i+1:]; p != "" {
			return p
		}
	}
	return "8081"
}

func mustOpen(cfg *config.Config) *sql.DB {
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %s", err)
	}
	return conn
}

func mustLoadVocab(cfg *config.Config) *vocab.Vocabulary {
	v, err := vocab.Load(cfg.VocabPath)
	if err != nil {
		log.Fatalf("vocab: %s", err)
	}
	return v
}

func mustSaveVocab(v *vocab.Vocabulary) {
	if err := v.Save(); err != nil {
		log.Fatalf("vocab save: %s", err)
	}
}

// cmdSkip hides pages in the Text view (and from summarization):
//
//	vellum skip ID 3,7-12    # skip those pages
//	vellum skip ID -         # clear the skip list
//	vellum skip ID           # show the current list
func cmdSkip(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum skip ID [PAGES] (e.g. 3,7-12; '' clears)")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		log.Fatalf("skip expects a numeric document id")
	}
	conn := mustOpen(cfg)
	if len(args) < 2 {
		var skip sql.NullString
		if err := conn.QueryRow("SELECT skip_pages FROM documents WHERE id=?", id).
			Scan(&skip); err != nil {
			log.Fatalf("skip: %s", err)
		}
		fmt.Printf("#%d: skip_pages=%q\n", id, skip.String)
		return
	}
	value := strings.TrimSpace(args[1])
	if value == "-" || value == "" || value == "none" {
		value = ""
	} else if _, err := parsePageList(value); err != nil {
		log.Fatalf("skip: %s", err)
	}
	if _, err := conn.Exec("UPDATE documents SET skip_pages=? WHERE id=?",
		value, id); err != nil {
		log.Fatalf("skip: %s", err)
	}
	fmt.Printf("#%d: skip_pages set to %q\n", id, value)
}

// cmdRemove removes documents from the library index. The files on disk
// are never touched — "indexed in place" means the index can also be
// un-indexed. Chunks/tags cascade; the cached cover PNG is dropped.
//
//	vellum remove ID [ID...]
func cmdRemove(cfg *config.Config, args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: vellum remove ID [ID...]   (files stay on disk)")
	}
	conn := mustOpen(cfg)
	removed := []map[string]any{}
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			log.Fatalf("remove expects numeric document ids (got %q)", a)
		}
		var path string
		err = conn.QueryRow("SELECT path FROM documents WHERE id=?", id).Scan(&path)
		if err == sql.ErrNoRows {
			fmt.Printf("#%d: not found (skipped)\n", id)
			continue
		}
		if err != nil {
			log.Fatalf("remove: %s", err)
		}
		if _, err := conn.Exec("DELETE FROM documents WHERE id=?", id); err != nil {
			log.Fatalf("remove: %s", err)
		}
		os.Remove(filepath.Join(cfg.BaseDir, "covers", fmt.Sprintf("cover-%d.png", id)))
		fmt.Printf("#%d: removed from the library — %s stays on disk\n", id, path)
		removed = append(removed, map[string]any{"removed": id, "path": path})
	}
	if jsonOut && len(removed) > 0 {
		printJSON(removed)
	}
}
