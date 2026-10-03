// Command vellum is a small local-model library manager: OCR, summaries,
// controlled-vocabulary tagging, full-text + semantic search. Everything
// runs on local models via Ollama; storage is one SQLite file.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
)

const usage = `vellum — small local-model library manager

Usage:
  vellum [--config FILE] [-v] <command> [args]

Commands:
  ingest PATH...        index files/directories (text extraction + OCR fallback)
  process [--limit N]   summarize + tag pending documents (local LLM)
  search QUERY [--semantic] [--limit N]
                        full-text (default) or semantic search
  show all | ID         list library or show one document
  vocab list|add|remove|review|promote
                        manage the controlled tag vocabulary
  embed                 embed chunks lacking embeddings

Config: $VELLUM_CONFIG or ./config.yaml (see README). Storage: one SQLite
file (FTS5). Models served locally by Ollama.
`

// documentColumns is the explicit projection used everywhere (never SELECT *,
// so the scan order is fixed even if the schema gains columns).
const documentColumns = "id, path, title, authors, year, summary, status"

type document struct {
	ID                    int64
	Path, Title, Authors  string
	Year, Summary, Status string
}

func (d *document) scan(sc scannable) error {
	return sc.Scan(&d.ID, &d.Path, &d.Title, &d.Authors, &d.Year,
		&d.Summary, &d.Status)
}

type scannable interface {
	Scan(dest ...any) error
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	// split global flags (--config, -v) from the rest, wherever they appear
	var cfgPath string
	verbose := false
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
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
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
	st, err := ingest.Ingest(cfg, conn, fs.Args(), *reprocess)
	if err != nil {
		log.Fatalf("ingest: %s", err)
	}
	fmt.Printf("added=%d updated=%d skipped=%d failed=%d\n",
		st.Added, st.Updated, st.Skipped, st.Failed)
	fmt.Println("next: vellum process   (summarize + tag with the LLM)")
}

func cmdProcess(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("process", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max documents to process")
	fs.Parse(args)

	v := mustLoadVocab(cfg)
	if len(v.Tags) == 0 {
		log.Fatalf("vocab.yaml is empty — add tags first with `vellum vocab add`. " +
			"Tagging needs a controlled vocabulary to constrain the LLM.")
	}
	if !llm.Available(cfg.Tools.OllamaURL) {
		log.Fatalf("ollama is not running at %s — start it with `ollama serve`",
			cfg.Tools.OllamaURL)
	}
	if !llm.HasModel(cfg.Tools.OllamaURL, cfg.Models.LLM) {
		log.Fatalf("model %s is not present — run: ollama pull %s",
			cfg.Models.LLM, cfg.Models.LLM)
	}
	conn := mustOpen(cfg)
	n, err := ingest.ProcessPending(cfg, conn, v, *limit)
	if err != nil {
		log.Fatalf("process: %s", err)
	}
	fmt.Printf("processed %d document(s)\n", n)
}

func cmdSearch(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	semantic := fs.Bool("semantic", false, "cosine similarity over chunk embeddings")
	limit := fs.Int("limit", 20, "max results")
	fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatalf("search needs a query")
	}
	query := strings.Join(fs.Args(), " ")
	conn := mustOpen(cfg)

	if *semantic {
		hits, err := search.Semantic(cfg, conn, query, *limit)
		if err != nil {
			log.Fatalf("search: %s", err)
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

	hits, err := search.Keyword(conn, query, *limit)
	if err != nil {
		log.Fatalf("search: %s", err)
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
		rows, err := conn.Query("SELECT " + documentColumns + " FROM documents ORDER BY id")
		if err != nil {
			log.Fatalf("show: %s", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d document
			if err := d.scan(rows); err != nil {
				log.Fatalf("show: %s", err)
			}
			head := fmt.Sprintf("#%d %s", d.ID, or(d.Title, d.Path))
			if d.Status != "done" {
				head += fmt.Sprintf("  [%s]", d.Status)
			}
			fmt.Println(head)
			if d.Authors != "" || d.Year != "" {
				fmt.Println("    " + strings.TrimSpace(d.Authors+" "+d.Year))
			}
			if tags := docTags(conn, d.ID); tags != "" {
				fmt.Println("    tags: " + tags)
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
		any := false
		for rows.Next() {
			var tag, example string
			var n int
			if err := rows.Scan(&tag, &n, &example); err != nil {
				log.Fatalf("vocab review: %s", err)
			}
			any = true
			fmt.Printf("  %s  ×%d  (e.g. %s)\n", tag, n, example)
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
