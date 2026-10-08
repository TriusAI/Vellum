package ask

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestWebFetch checks the fetch + HTML→text reduction.
func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><style>b{}</style><script>evil()</script></head>`+
			`<body><h1>Hello</h1><p>World &amp; friends</p></body></html>`)
	}))
	defer srv.Close()

	text, err := WebFetch(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Hello") || !strings.Contains(text, "World & friends") {
		t.Fatalf("fetch text wrong: %q", text)
	}
	if strings.Contains(text, "evil") || strings.Contains(text, "b{}") {
		t.Fatalf("script/style leaked into text: %q", text)
	}
	// scheme guard
	if _, err := WebFetch("file:///etc/passwd"); err == nil {
		t.Fatal("non-http scheme must be refused")
	}
}

// TestAnthropicToolLoop drives a mock Anthropic /v1/messages server through
// a tool_use round (the URL arrives split across input_json_delta events)
// then a text answer — exercising the Anthropic tool loop.
func TestAnthropicToolLoop(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			io.WriteString(w, "<html><body><p>Fetched page content here.</p></body></html>")
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			sse := func(v any) {
				b, _ := json.Marshal(v)
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			if !strings.Contains(string(body), `"tool_result"`) {
				// round 1: a tool_use block whose input streams in pieces
				sse(map[string]any{"type": "content_block_start", "index": 0,
					"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "fetch_url"}})
				sse(map[string]any{"type": "content_block_delta", "index": 0,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"url":"`}})
				sse(map[string]any{"type": "content_block_delta", "index": 0,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": srv.URL + `/page"}`}})
				sse(map[string]any{"type": "content_block_stop", "index": 0})
				sse(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}})
				sse(map[string]any{"type": "message_stop"})
				return
			}
			// round 2: the tool result must be present; give a text answer
			if !strings.Contains(string(body), "Fetched page content here.") {
				sse(map[string]any{"type": "error", "error": map[string]any{"message": "tool result missing"}})
				return
			}
			sse(map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""}})
			sse(map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "The page says hi."}})
			sse(map[string]any{"type": "content_block_stop", "index": 0})
			sse(map[string]any{"type": "message_stop"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Config{Provider: "anthropic", Model: "test", BaseURL: srv.URL, Tools: true}
	deltas, err := cfg.Stream("sys",
		[]Message{{Role: "user", Content: "what does the link say?"}},
		[]ToolDef{FetchToolDef()}, RunFetch)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	toolReported := false
	for d := range deltas {
		if d.Error != "" {
			t.Fatalf("stream error: %s", d.Error)
		}
		if d.Tool != "" {
			toolReported = true
		}
		text.WriteString(d.Text)
	}
	if !toolReported {
		t.Fatal("the fetch tool call was not reported")
	}
	if got := text.String(); got != "The page says hi." {
		t.Fatalf("final answer wrong: %q", got)
	}
}

// asks for a fetch_url tool call, then answers using the fetched text —
// exercising the whole loop (request → tool call → fetch → tool result →
// second request → streamed answer).
func TestOpenAIToolLoop(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			io.WriteString(w, "<html><body><p>Fetched page content here.</p></body></html>")
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			if !strings.Contains(string(body), `"role":"tool"`) {
				// first round: emit a tool call to fetch our page
				args := `{"url":"` + srv.URL + `/page"}`
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":["+
					"{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":"+
					"{\"name\":\"fetch_url\",\"arguments\":%s}}]}}]}\n\n", jsonStr(args))
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			// second round: the tool result must be present; answer
			if !strings.Contains(string(body), "Fetched page content here.") {
				fmt.Fprint(w, "data: {\"error\":{\"message\":\"tool result missing\"}}\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"The page says hi.\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Config{Provider: "openai", Model: "test", BaseURL: srv.URL, Tools: true}
	deltas, err := cfg.Stream("sys",
		[]Message{{Role: "user", Content: "what does the link say?"}},
		[]ToolDef{FetchToolDef()}, RunFetch)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	toolReported := false
	for d := range deltas {
		if d.Error != "" {
			t.Fatalf("stream error: %s", d.Error)
		}
		if d.Tool != "" {
			toolReported = true
		}
		text.WriteString(d.Text)
	}
	if !toolReported {
		t.Fatal("the fetch tool call was not reported")
	}
	if got := text.String(); got != "The page says hi." {
		t.Fatalf("final answer wrong: %q", got)
	}
}
