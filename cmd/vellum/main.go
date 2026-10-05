// Command vellum is a small local-model library manager: OCR, summaries,
// controlled-vocabulary tagging, full-text + semantic search. Everything
// runs on local models via Ollama; storage is one SQLite file.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"vellum/internal/classify"
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
  vocab list|add|remove|review|promote
                        manage the controlled tag vocabulary
  embed                 embed chunks lacking embeddings
  serve [--listen ADDR] [--open]
                        local web UI + JSON API (default 127.0.0.1:8090)
  agent                 print AI-agent instructions (commands, JSON, API)

--json switches command output to machine-readable JSON (anywhere in args).
Config: $VELLUM_CONFIG or ./config.yaml (see README). Storage: one SQLite
file (FTS5). Models served locally by llama.cpp llama-server.
`

// versionString is reported by --version, /api/status and `vellum agent`.
const versionString = "0.8.0"

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
	case "reextract":
		cmdReextract(cfg, args[1:])
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
	st, err := ingest.Ingest(cfg, conn, fs.Args(), *reprocess, nil)
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
	if !llm.Available(cfg.Tools.LLMURL) {
		log.Fatalf("no llama-server at %s — start it with the vellum launcher, or:\n"+
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
	results, err := ingest.ProcessPending(cfg, conn, v, ids, *limit,
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
		log.Fatalf("usage: vellum reextract ID [--force-ocr]")
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		log.Fatalf("reextract expects a numeric document id")
	}
	conn := mustOpen(cfg)
	var path string
	if err := conn.QueryRow("SELECT path FROM documents WHERE id=?", id).
		Scan(&path); err != nil {
		log.Fatalf("reextract: %s", err)
	}
	var res *extract.Result
	if force {
		fmt.Printf("re-extracting #%d with OCR forced on every page (slow)...\n", id)
		res, err = extract.ExtractOCR(path, cfg)
	} else {
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
