package llm

// Ollama backend support for the pipeline. Structured output stays
// grammar-enforced: Ollama's native /api/chat accepts the same JSON
// schema in its "format" field, so the tag vocabulary enum keeps its
// decode-time guarantee when the user switches to Ollama.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ollamaChatRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Format   any        `json:"format"` // schema object = structured output
	Stream   bool       `json:"stream"`
	Think    bool       `json:"think"`
	Options  ollamaOpts `json:"options"`
}
type ollamaOpts struct {
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Error string `json:"error,omitempty"`
}

// OllamaChatJSON is ChatJSON for an Ollama server: same decode of the
// constrained JSON object.
func OllamaChatJSON(ctx context.Context, baseURL, model string, messages []Message,
	schema map[string]any, think bool, temperature float64, numCtx int,
) (map[string]any, error) {
	ctx = orCtx(ctx)
	req := ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Format:   schema,
		Stream:   false,
		Think:    think,
		Options:  ollamaOpts{Temperature: temperature, NumCtx: numCtx},
	}
	body, err := postCtx(ctx, baseURL+"/api/chat", req)
	if err != nil {
		return nil, err
	}
	var resp ollamaChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad ollama chat response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("ollama error: %s", resp.Error)
	}
	if resp.Message.Content == "" {
		return nil, fmt.Errorf("ollama returned empty content")
	}
	return decodeJSONObject(resp.Message.Content)
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// OllamaEmbed batches texts through Ollama's /api/embed.
func OllamaEmbed(baseURL, model string, texts []string) ([][]float32, error) {
	body, err := post(baseURL+"/api/embed", ollamaEmbedRequest{Model: model, Input: texts})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad ollama embed response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("ollama error: %s", resp.Error)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d",
			len(texts), len(resp.Embeddings))
	}
	return resp.Embeddings, nil
}

// OllamaAvailable probes the Ollama server (GET /api/tags).
func OllamaAvailable(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(trimBase(baseURL) + "/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// trimBase normalizes a base URL (drops a trailing slash).
func trimBase(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

// AvailableFor is the backend-aware health probe: the chat model can
// run on either a llama-server (GET /health) or Ollama (GET /api/tags).
func AvailableFor(backend, baseURL string) bool {
	if backend == "ollama" {
		return OllamaAvailable(baseURL)
	}
	return Available(baseURL)
}
