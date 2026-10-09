// Chat API tests: session CRUD + one streamed turn against a MOCK
// OpenAI-compatible provider. No real LLM is needed.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vellum/internal/api"
	"vellum/internal/ask"
	"vellum/internal/config"
	"vellum/internal/db"
)

// mockOpenAI emits an open_document tool call on the first round, then a
// text answer once the tool result has been fed back.
func mockOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if !strings.Contains(string(body), `"role":"tool"`) {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Let me look. "}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"open_document","arguments":"{\"doc_id\":1,\"view\":\"preview\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Here it is."}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestChatAPI(t *testing.T) {
	mock := mockOpenAI(t)
	defer mock.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.BaseDir = dir
	cfg.DBPath = filepath.Join(dir, "library.db")
	cfg.VocabPath = filepath.Join(dir, "vocab.yaml")
	cfg.Ask = ask.Config{Provider: "openai", Model: "test", BaseURL: mock.URL, Tools: true}
	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(
		"INSERT INTO documents(path, sha256, title, status) VALUES('/lib/a.pdf','sha','The Paper','done')"); err != nil {
		t.Fatal(err)
	}

	srv := api.New(cfg, conn)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	request := func(method, path, body string, out any, wantCode int) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantCode {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: status %d, want %d (%s)", method, path, resp.StatusCode, wantCode, b)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s %s: decode: %s", method, path, err)
			}
		}
	}

	// create a library-scoped session (reuse=false)
	var sess map[string]any
	request("POST", "/api/chats", `{"scope_kind":"library"}`, &sess, 201)
	id := int64(sess["id"].(float64))
	if id == 0 {
		t.Fatalf("no session id: %v", sess)
	}

	// a tag-scoped session on a missing tag is still valid (empty scope)
	request("POST", "/api/chats", `{"scope_kind":"tag","scope_value":"nope"}`, &sess, 201)
	// an unknown scope kind is rejected
	request("POST", "/api/chats", `{"scope_kind":"bogus"}`, nil, 400)

	// reuse returns the same session for the same scope
	var reused map[string]any
	request("POST", "/api/chats", `{"scope_kind":"library","reuse":true}`, &reused, 200)
	if int64(reused["id"].(float64)) != id {
		t.Fatalf("reuse returned %v, want %d", reused["id"], id)
	}

	// stream one turn (SSE): expect a tool frame, an action frame, and text
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/chats/%d/messages", ts.URL, id),
		strings.NewReader(`{"content":"open the paper"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("message stream status %d: %s", resp.StatusCode, b)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if !strings.Contains(body, `"tool":"open_document"`) {
		t.Fatalf("tool frame missing:\n%s", body)
	}
	if !strings.Contains(body, `"action":`) || !strings.Contains(body, `"kind":"open"`) {
		t.Fatalf("action frame missing:\n%s", body)
	}
	if !strings.Contains(body, "Here it is.") {
		t.Fatalf("answer text missing:\n%s", body)
	}

	// the turn is persisted (user + assistant)
	var got struct {
		Session  map[string]any   `json:"session"`
		Messages []map[string]any `json:"messages"`
	}
	request("GET", fmt.Sprintf("/api/chats/%d", id), "", &got, 200)
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 saved messages, got %d: %v", len(got.Messages), got.Messages)
	}
	if got.Messages[0]["role"] != "user" || got.Messages[1]["role"] != "assistant" {
		t.Fatalf("roles wrong: %v", got.Messages)
	}
	if got.Messages[1]["content"].(string) != "Let me look. Here it is." {
		t.Fatalf("assistant text not saved correctly: %q", got.Messages[1]["content"])
	}
	// tool_log preserves the streaming order: text, tool, text
	var evs []map[string]any
	if err := json.Unmarshal([]byte(got.Messages[1]["tool_log"].(string)), &evs); err != nil {
		t.Fatalf("tool_log not JSON: %v", got.Messages[1]["tool_log"])
	}
	if len(evs) != 3 || evs[0]["t"] != "text" || evs[1]["t"] != "tool" || evs[2]["t"] != "text" {
		t.Fatalf("tool_log not interleaved in order: %v", evs)
	}
	if evs[1]["name"] != "open_document" {
		t.Fatalf("tool name wrong: %v", evs[1])
	}

	// revert the user message: it and the assistant reply go, and the text
	// comes back for the editor
	var rev map[string]any
	request("POST", fmt.Sprintf("/api/chats/%d/revert", id),
		fmt.Sprintf(`{"message_id":%d}`, int64(got.Messages[0]["id"].(float64))), &rev, 200)
	if rev["reverted"] != "open the paper" {
		t.Fatalf("revert text wrong: %v", rev)
	}
	if n, _ := rev["deleted"].(float64); n != 2 {
		t.Fatalf("revert deleted = %v, want 2", rev["deleted"])
	}
	request("GET", fmt.Sprintf("/api/chats/%d", id), "", &got, 200)
	if len(got.Messages) != 0 {
		t.Fatalf("revert did not clear the transcript: %v", got.Messages)
	}

	// list + rename + delete
	var list []map[string]any
	request("GET", "/api/chats", "", &list, 200)
	if len(list) == 0 {
		t.Fatal("session list is empty")
	}
	request("PATCH", fmt.Sprintf("/api/chats/%d", id), `{"title":"renamed"}`, &sess, 200)
	if sess["title"] != "renamed" {
		t.Fatalf("rename failed: %v", sess)
	}
	request("DELETE", fmt.Sprintf("/api/chats/%d", id), "", nil, 200)
	request("GET", fmt.Sprintf("/api/chats/%d", id), "", nil, 404)
}
