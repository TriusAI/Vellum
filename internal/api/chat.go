package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"vellum/internal/ask"
	"vellum/internal/db"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
)

// ---------------------------------------------------------------- scopes

// chatScope is a resolved conversation scope: one document, a tag, a
// category subtree, a collection, or the whole library.
type chatScope struct {
	Kind  string // library|document|tag|category|collection
	Value string
	Label string
	All   bool           // library: every document is in scope
	Docs  map[int64]bool // in-scope ids (ignored when All)
}

func (sc chatScope) allows(id int64) bool {
	if sc.All {
		return true
	}
	return sc.Docs[id]
}

// resolveChatScope validates a scope and computes its document set.
func (s *Server) resolveChatScope(kind, value string) (chatScope, error) {
	sc := chatScope{Kind: kind, Value: value, Docs: map[int64]bool{}}
	switch kind {
	case "library":
		sc.All = true
		sc.Label = "the whole library"
	case "document":
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return sc, fmt.Errorf("document scope needs a numeric id")
		}
		d, err := s.chatDoc(id)
		if err != nil {
			return sc, fmt.Errorf("no such document #%s", value)
		}
		sc.Docs[id] = true
		sc.Label = "the document “" + docName(d) + "”"
	case "tag":
		if strings.TrimSpace(value) == "" {
			return sc, fmt.Errorf("tag scope needs a tag name")
		}
		ids, err := s.docIDsWithTag(value)
		if err != nil {
			return sc, err
		}
		for _, id := range ids {
			sc.Docs[id] = true
		}
		sc.Label = "documents tagged #" + value
	case "category":
		if strings.TrimSpace(value) == "" {
			return sc, fmt.Errorf("category scope needs a name")
		}
		ids, err := s.docIDsInCategory(value)
		if err != nil {
			return sc, err
		}
		for _, id := range ids {
			sc.Docs[id] = true
		}
		sc.Label = "the “" + value + "” shelf"
	case "collection":
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return sc, fmt.Errorf("collection scope needs a numeric id")
		}
		var name string
		if err := s.conn.QueryRow(
			"SELECT name FROM collections WHERE id=?", id).Scan(&name); err != nil {
			return sc, fmt.Errorf("no such collection #%s", value)
		}
		ids, err := s.docIDsInCollection(id)
		if err != nil {
			return sc, err
		}
		for _, docID := range ids {
			sc.Docs[docID] = true
		}
		sc.Label = "the “" + name + "” collection"
	default:
		return sc, fmt.Errorf("unknown scope kind %q", kind)
	}
	return sc, nil
}

func (s *Server) docIDsWithTag(tag string) ([]int64, error) {
	rows, err := s.conn.Query(
		"SELECT doc_id FROM doc_tags WHERE tag=? ORDER BY doc_id", tag)
	return scanIDs(rows, err)
}

func (s *Server) docIDsInCategory(cat string) ([]int64, error) {
	rows, err := s.conn.Query(
		`SELECT id FROM documents
		 WHERE category=? OR category LIKE ?||'/%' ORDER BY id`, cat, cat)
	return scanIDs(rows, err)
}

func (s *Server) docIDsInCollection(id int64) ([]int64, error) {
	rows, err := s.conn.Query(
		"SELECT doc_id FROM collection_docs WHERE collection_id=? ORDER BY doc_id", id)
	return scanIDs(rows, err)
}

func scanIDs(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// chatDoc is the compact document view the chat layer uses.
type chatDoc struct {
	ID       int64
	Path     string
	Title    string
	Authors  string
	Year     string
	Summary  string
	Kind     string
	Category string
	Status   string
	Tags     []string
}

func docName(d chatDoc) string {
	if strings.TrimSpace(d.Title) != "" {
		return d.Title
	}
	return baseName(d.Path)
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *Server) chatDoc(id int64) (chatDoc, error) {
	var d chatDoc
	err := s.conn.QueryRow(`
		SELECT id, path, COALESCE(title,''), COALESCE(authors,''), COALESCE(year,''),
		       COALESCE(summary,''), COALESCE(kind,''), COALESCE(category,''), status
		FROM documents WHERE id=?`, id).
		Scan(&d.ID, &d.Path, &d.Title, &d.Authors, &d.Year, &d.Summary,
			&d.Kind, &d.Category, &d.Status)
	if err != nil {
		return d, err
	}
	rows, err := s.conn.Query(
		"SELECT tag FROM doc_tags WHERE doc_id=? ORDER BY tag", id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t string
			if rows.Scan(&t) == nil {
				d.Tags = append(d.Tags, t)
			}
		}
	}
	return d, nil
}

// ------------------------------------------------------------- system prompt

func (s *Server) chatSystem(sc chatScope) (string, error) {
	var b strings.Builder
	b.WriteString("You are Vellum's library assistant, embedded in a personal " +
		"library of books, papers and notes. Answer concisely; use the material " +
		"you are given, and say plainly when something is outside it.\n\n")

	if sc.Kind == "document" && sc.Value != "" {
		id, _ := strconv.ParseInt(sc.Value, 10, 64)
		d, err := s.chatDoc(id)
		if err != nil {
			return "", err
		}
		b.WriteString("Scope: one document.\n")
		b.WriteString(fmt.Sprintf("Document #%d: title=%q; authors=%q; year=%q; kind=%q",
			d.ID, d.Title, d.Authors, d.Year, d.Kind))
		if d.Category != "" {
			b.WriteString("; category=" + d.Category)
		}
		if len(d.Tags) > 0 {
			b.WriteString("; tags=" + strings.Join(d.Tags, ", "))
		}
		b.WriteString("\n")
		if d.Summary != "" {
			b.WriteString("\nSummary:\n" + d.Summary + "\n")
		}
		if text, err := db.DocumentText(s.conn, id); err == nil && text != "" {
			if len(text) > 6000 {
				text = text[:6000]
			}
			b.WriteString("\nOpening text (truncated):\n" + text + "\n")
		}
	} else {
		docs, err := s.allDocuments()
		if err != nil {
			return "", err
		}
		inScope := make([]documentJSON, 0, len(docs))
		for _, d := range docs {
			if sc.allows(d.ID) {
				inScope = append(inScope, d)
			}
		}
		b.WriteString("Scope: " + sc.Label)
		b.WriteString(fmt.Sprintf(" (%d document(s)).\n", len(inScope)))
		if len(inScope) == 0 {
			b.WriteString("There are no documents in this scope.\n")
		} else {
			const listed = 80
			b.WriteString("Documents" + map[bool]string{true: "", false: " (first 80)"}[len(inScope) <= listed] + ":\n")
			for i, d := range inScope {
				if i >= listed {
					break
				}
				b.WriteString(formatDocLine(d, 240) + "\n")
			}
		}
	}
	b.WriteString("\n" + chatToolGuidance)
	return b.String(), nil
}

// chatToolGuidance tells the model how (and how not) to use the tools.
const chatToolGuidance = `Tools:
- search_library: find documents by keyword or semantic similarity. Use it
  whenever the user asks you to locate something.
- get_document: read one document's metadata, summary and opening text.
- open_document: open a document's page in the user's UI (a real side
  effect the user sees). After you have CONFIRMED the right document with
  search_library, call open_document with view="preview" to show it. Never
  say a document was opened unless you actually called this tool.
- regenerate_metadata: rebuild a document's summary / tags / category /
  metadata / kind (runs in the background). Use it when the user asks to
  refresh or re-do a document's metadata.
Only act on documents inside your scope.`

func formatDocLine(d documentJSON, snip int) string {
	name := d.Title
	if strings.TrimSpace(name) == "" {
		name = baseName(d.Path)
	}
	bits := []string{fmt.Sprintf("#%d %s", d.ID, name)}
	if d.Authors != "" {
		bits = append(bits, d.Authors)
	}
	if d.Year != "" {
		bits = append(bits, d.Year)
	}
	if d.Kind != "" {
		bits = append(bits, "["+d.Kind+"]")
	}
	if d.Category != "" {
		bits = append(bits, "{"+d.Category+"}")
	}
	if len(d.Tags) > 0 {
		bits = append(bits, "#"+strings.Join(d.Tags, " #"))
	}
	line := strings.Join(bits, " — ")
	if d.Summary != "" && snip > 0 {
		s := strings.Join(strings.Fields(d.Summary), " ")
		if len(s) > snip {
			s = s[:snip] + "…"
		}
		line += "\n    " + s
	}
	return line
}

// ---------------------------------------------------------------- tools

func (s *Server) chatTools(sc chatScope) ([]ask.ToolDef, ask.Runner) {
	defs := []ask.ToolDef{
		{
			Name: "search_library",
			Description: "Search the library for documents matching a query. " +
				"Returns matching documents (id, title, path, snippet). Use " +
				"keyword mode for names/topics, semantic mode for meaning-based " +
				"questions.",
			Parameters: objSchema(map[string]any{
				"query": map[string]any{"type": "string", "description": "what to look for"},
				"mode": map[string]any{"type": "string", "enum": []string{"keyword", "semantic"},
					"description": "keyword (default) or semantic"},
				"limit": map[string]any{"type": "integer", "description": "max results (default 8)"},
			}, "query"),
		},
		{
			Name:        "get_document",
			Description: "Read one document's metadata, summary and opening text by id.",
			Parameters: objSchema(map[string]any{
				"doc_id": map[string]any{"type": "integer", "description": "document id"},
			}, "doc_id"),
		},
		{
			Name: "open_document",
			Description: "Open a document's page in the user's UI. view defaults " +
				"to preview (the PDF/reader); page is optional. Call it once you " +
				"have confirmed the document with search_library.",
			Parameters: objSchema(map[string]any{
				"doc_id": map[string]any{"type": "integer", "description": "document id"},
				"view": map[string]any{"type": "string", "enum": []string{"preview", "summary", "text"},
					"description": "which page to open (default preview)"},
				"page": map[string]any{"type": "integer", "description": "page number for preview"},
			}, "doc_id"),
		},
		{
			Name: "regenerate_metadata",
			Description: "Rebuild a document's metadata as a background job. " +
				"fields is a subset of [\"meta\",\"summary\",\"tags\",\"category\",\"kind\"].",
			Parameters: objSchema(map[string]any{
				"doc_id": map[string]any{"type": "integer", "description": "document id"},
				"fields": map[string]any{
					"type": "array", "items": map[string]any{"type": "string",
						"enum": []string{"meta", "summary", "tags", "category", "kind"}},
					"description": "which fields to rebuild"},
			}, "doc_id", "fields"),
		},
	}
	if s.cfg.Ask.Tools {
		defs = append(defs, ask.FetchToolDef())
	}
	run := func(call ask.ToolCall) ask.ToolResult {
		switch call.Name {
		case "search_library":
			return s.toolSearchLibrary(sc, call.Args)
		case "get_document":
			return s.toolGetDocument(sc, call.Args)
		case "open_document":
			return s.toolOpenDocument(sc, call.Args)
		case "regenerate_metadata":
			return s.toolRegenerate(sc, call.Args)
		case "fetch_url":
			return ask.RunFetch(call)
		default:
			return ask.ToolResult{Content: "error: unknown tool " + call.Name}
		}
	}
	return defs, run
}

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (s *Server) toolSearchLibrary(sc chatScope, args string) ask.ToolResult {
	var a struct {
		Query string `json:"query"`
		Mode  string `json:"mode"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return ask.ToolResult{Content: "error: search_library requires a query"}
	}
	limit := a.Limit
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	lines := []string{}
	if a.Mode == "semantic" {
		hits, err := search.Semantic(s.cfg, s.conn, a.Query, limit*3)
		if err != nil {
			return ask.ToolResult{Content: "error: semantic search failed: " + err.Error()}
		}
		for _, h := range hits {
			if len(lines) >= limit {
				break
			}
			if !sc.allows(h.DocID) {
				continue
			}
			snip := ""
			if len(h.Snippets) > 0 {
				snip = strings.Join(strings.Fields(h.Snippets[0].Text), " ")
			}
			lines = append(lines, hitLine(h.DocID, h.Title, h.Path, snip))
		}
	} else {
		hits, err := search.Keyword(s.conn, a.Query, limit*3)
		if err != nil {
			return ask.ToolResult{Content: "error: search failed: " + err.Error()}
		}
		for _, h := range hits {
			if len(lines) >= limit {
				break
			}
			if !sc.allows(h.DocID) {
				continue
			}
			lines = append(lines, hitLine(h.DocID, h.Title, h.Path, h.Snippet))
		}
	}
	if len(lines) == 0 {
		return ask.ToolResult{Content: "no matching documents in scope"}
	}
	return ask.ToolResult{Content: strings.Join(lines, "\n")}
}

func hitLine(id int64, title, path, snip string) string {
	name := title
	if strings.TrimSpace(name) == "" {
		name = baseName(path)
	}
	line := fmt.Sprintf("#%d %s — %s", id, name, path)
	if s := strings.Join(strings.Fields(snip), " "); s != "" {
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		line += "\n    " + s
	}
	return line
}

func (s *Server) toolGetDocument(sc chatScope, args string) ask.ToolResult {
	var a struct {
		DocID int64 `json:"doc_id"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil || a.DocID <= 0 {
		return ask.ToolResult{Content: "error: get_document requires doc_id"}
	}
	if !sc.allows(a.DocID) {
		return ask.ToolResult{Content: fmt.Sprintf(
			"error: document #%d is outside this chat's scope (%s)", a.DocID, sc.Label)}
	}
	d, err := s.chatDoc(a.DocID)
	if err != nil {
		return ask.ToolResult{Content: fmt.Sprintf("error: no document #%d", a.DocID)}
	}
	var b strings.Builder
	b.WriteString(formatDocLine(docToJSON(d), 0) + "\n")
	if d.Summary != "" {
		b.WriteString("Summary:\n" + d.Summary + "\n")
	}
	if text, err := db.DocumentText(s.conn, a.DocID); err == nil && text != "" {
		if len(text) > 4000 {
			text = text[:4000]
		}
		b.WriteString("Opening text (truncated):\n" + text + "\n")
	}
	return ask.ToolResult{Content: b.String()}
}

func docToJSON(d chatDoc) documentJSON {
	return documentJSON{ID: d.ID, Path: d.Path, Title: d.Title, Authors: d.Authors,
		Year: d.Year, Summary: d.Summary, Kind: d.Kind, Category: d.Category,
		Status: d.Status, Tags: d.Tags}
}

func (s *Server) toolOpenDocument(sc chatScope, args string) ask.ToolResult {
	var a struct {
		DocID int64  `json:"doc_id"`
		View  string `json:"view"`
		Page  int    `json:"page"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil || a.DocID <= 0 {
		return ask.ToolResult{Content: "error: open_document requires doc_id"}
	}
	if !sc.allows(a.DocID) {
		return ask.ToolResult{Content: fmt.Sprintf(
			"error: document #%d is outside this chat's scope (%s)", a.DocID, sc.Label)}
	}
	d, err := s.chatDoc(a.DocID)
	if err != nil {
		return ask.ToolResult{Content: fmt.Sprintf("error: no document #%d", a.DocID)}
	}
	view := a.View
	switch view {
	case "preview", "summary", "text":
	default:
		view = "preview"
	}
	action := map[string]any{
		"kind": "open", "doc_id": a.DocID, "view": view, "page": a.Page,
		"title": docName(d),
	}
	return ask.ToolResult{
		Content: fmt.Sprintf("opened #%d (%s) in the %s view", a.DocID, docName(d), view),
		Action:  action,
	}
}

func (s *Server) toolRegenerate(sc chatScope, args string) ask.ToolResult {
	var a struct {
		DocID  int64    `json:"doc_id"`
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil || a.DocID <= 0 {
		return ask.ToolResult{Content: "error: regenerate_metadata requires doc_id"}
	}
	if !sc.allows(a.DocID) {
		return ask.ToolResult{Content: fmt.Sprintf(
			"error: document #%d is outside this chat's scope (%s)", a.DocID, sc.Label)}
	}
	allowed := map[string]bool{"meta": true, "summary": true, "tags": true,
		"category": true, "kind": true}
	fields := []string{}
	for _, f := range a.Fields {
		if allowed[f] {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		fields = []string{"summary", "tags", "category", "meta"}
	}
	if !llm.AvailableFor(s.cfg.LLM.Backend, s.cfg.Tools.LLMURL) {
		return ask.ToolResult{Content: "error: the pipeline chat backend is not " +
			"running, so metadata cannot be regenerated right now"}
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		return ask.ToolResult{Content: "error: " + err.Error()}
	}
	if len(v.Tags) == 0 && (contains(fields, "tags") || contains(fields, "category")) {
		return ask.ToolResult{Content: "error: the tag vocabulary is empty"}
	}
	docID := a.DocID
	label := fmt.Sprintf("chat: regenerate #%d (%s)", docID, strings.Join(fields, ", "))
	go s.runJob("regenerate", label, func(j *Job) error {
		_, err := ingest.Regenerate(j.ctx, s.cfg, s.conn, v, docID, fields,
			func(m string) { s.jobProgress(j, m) })
		return err
	})
	return ask.ToolResult{Content: fmt.Sprintf(
		"started a background job to regenerate %s for #%d; it will show in the Jobs list",
		strings.Join(fields, ", "), docID)}
}

func contains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- handlers

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	rows, err := db.ListChatSessions(s.conn)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

// createChat creates a scoped session. With {"reuse":true} it returns the
// most recent existing session for that scope instead of creating one.
func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ScopeKind  string `json:"scope_kind"`
		ScopeValue string `json:"scope_value"`
		Title      string `json:"title"`
		Reuse      bool   `json:"reuse"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	switch body.ScopeKind {
	case "library", "document", "tag", "category", "collection":
	default:
		writeErr(w, 400, "scope_kind must be library|document|tag|category|collection")
		return
	}
	sc, err := s.resolveChatScope(body.ScopeKind, body.ScopeValue)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.Reuse {
		if sess, err := db.FindChatSession(s.conn, body.ScopeKind, body.ScopeValue); err == nil {
			writeJSON(w, 200, sess)
			return
		}
	}
	title := body.Title
	if title == "" {
		title = "Chat about " + sc.Label
	}
	sess, err := db.CreateChatSession(s.conn, title, body.ScopeKind, body.ScopeValue)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, sess)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	sess, err := db.GetChatSession(s.conn, id)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such chat session")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	msgs, err := db.ListChatMessages(s.conn, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sc, _ := s.resolveChatScope(sess.ScopeKind, sess.ScopeValue)
	label := sc.Label
	if label == "" {
		label = sess.ScopeKind
	}
	writeJSON(w, 200, map[string]any{
		"session": sess, "messages": msgs, "scope_label": label,
	})
}

func (s *Server) patchChat(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Title *string `json:"title"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if body.Title == nil {
		writeErr(w, 400, "title required")
		return
	}
	sess, err := db.RenameChatSession(s.conn, id, strings.TrimSpace(*body.Title))
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such chat session")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, sess)
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := db.DeleteChatSession(s.conn, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// postChatMessage streams one assistant turn (SSE). The user's message is
// persisted immediately; the assistant's reply (plus a compact tool log) is
// persisted when the stream completes.
func (s *Server) postChatMessage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	sess, err := db.GetChatSession(s.conn, id)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such chat session")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		writeErr(w, 400, "content required")
		return
	}
	cfg := s.cfg.Ask
	if !cfg.Enabled() {
		writeErr(w, 502, "no ask provider configured (Settings → Ask an LLM)")
		return
	}
	sc, err := s.resolveChatScope(sess.ScopeKind, sess.ScopeValue)
	if err != nil {
		writeErr(w, 400, "session scope is no longer valid: "+err.Error())
		return
	}
	sys, err := s.chatSystem(sc)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tools, runner := s.chatTools(sc)

	hist, err := db.ListChatMessages(s.conn, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	msgs := make([]ask.Message, 0, len(hist)+1)
	for _, m := range hist {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, ask.Message{Role: m.Role, Content: m.Content})
		}
	}
	msgs = append(msgs, ask.Message{Role: "user", Content: content})

	// persist the user's turn before streaming (it is part of the transcript
	// even if the provider then fails), and auto-title a fresh session
	if _, err := db.AddChatMessage(s.conn, id, "user", content, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if strings.TrimSpace(sess.Title) == "" || sess.Title == "Chat about "+sc.Label {
		title := content
		if len([]rune(title)) > 60 {
			title = string([]rune(title)[:60]) + "…"
		}
		db.RenameChatSession(s.conn, id, title)
	}

	deltas, err := cfg.Stream(sys, msgs, tools, runner)
	if err != nil {
		writeErr(w, 502, "ask provider: "+err.Error())
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	var text strings.Builder
	events := []map[string]any{}
	for d := range deltas {
		if d.Error != "" {
			sseSend(w, map[string]any{"e": d.Error})
			break
		}
		if d.Action != nil {
			sseSend(w, map[string]any{"action": d.Action})
			continue
		}
		if d.Tool != "" {
			events = append(events, map[string]any{"t": "tool", "name": d.Tool, "args": d.ToolArgs})
			sseSend(w, map[string]any{"tool": d.Tool, "args": d.ToolArgs})
			continue
		}
		if d.Text != "" {
			text.WriteString(d.Text)
			// merge consecutive text deltas into the running text segment so
			// the stored event list interleaves text/tool in stream order
			if n := len(events); n > 0 && events[n-1]["t"] == "text" {
				events[n-1]["text"] = events[n-1]["text"].(string) + d.Text
			} else {
				events = append(events, map[string]any{"t": "text", "text": d.Text})
			}
			sseSend(w, map[string]any{"d": d.Text})
		}
	}
	logJSON := ""
	if len(events) > 0 {
		if b, err := json.Marshal(events); err == nil {
			logJSON = string(b)
		}
	}
	db.AddChatMessage(s.conn, id, "assistant", text.String(), logJSON)
	sseSend(w, map[string]any{"done": "1"})
	flush.Flush()
}

// revertChat drops one message and everything after it (no forking) and
// returns the reverted message so the client can put it back in the editor.
func (s *Server) revertChat(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		MessageID int64 `json:"message_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if body.MessageID <= 0 {
		writeErr(w, 400, "message_id required")
		return
	}
	var role, content string
	err = s.conn.QueryRow(
		"SELECT role, content FROM chat_messages WHERE id=? AND session_id=?",
		body.MessageID, id).Scan(&role, &content)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such message in this chat")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n, err := db.DeleteChatMessagesFrom(s.conn, id, body.MessageID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"reverted": content, "role": role, "deleted": n,
	})
}
