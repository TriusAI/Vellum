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

	text, err := webFetch(srv.URL)
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
	if _, err := webFetch("file:///etc/passwd"); err == nil {
		t.Fatal("non-http scheme must be refused")
	}
}

// TestOpenAIToolLoop drives a mock OpenAI-compatible server that first
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
	deltas, err := cfg.Stream("sys", []Message{{Role: "user", Content: "what does the link say?"}})
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
