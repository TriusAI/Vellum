// Package llm is a minimal client for the local Ollama server.
//
// Structured output is the load-bearing feature here: the JSON schema sent
// with /api/chat is enforced via grammar-constrained decoding. An enum in
// the schema makes it *impossible* for the model to emit a tag outside the
// vocabulary — that's how Vellum avoids tag drift.
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Format   any       `json:"format"`
	Think    bool      `json:"think"`
	Options  options   `json:"options"`
	Stream   bool      `json:"stream"`
}

type options struct {
	NumCtx      int     `json:"num_ctx"`
	Temperature float64 `json:"temperature"`
}

type chatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Available reports whether the Ollama server is reachable.
func Available(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(baseURL, "/") + "/api/tags")
	return err == nil && resp.StatusCode == 200
}

// HasModel reports whether a model is present on the server.
func HasModel(baseURL, model string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(baseURL, "/") + "/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	for _, m := range out.Models {
		if m.Name == model || strings.SplitN(m.Name, ":", 2)[0] == model {
			return true
		}
	}
	return false
}

// ChatJSON runs one chat call with a JSON-schema-constrained response and
// decodes the JSON object.
func ChatJSON(baseURL, model string, messages []Message, schema map[string]any,
	think bool, numCtx int, temperature float64) (map[string]any, error) {
	req := chatRequest{
		Model:    model,
		Messages: messages,
		Format:   schema,
		Think:    think,
		Options:  options{NumCtx: numCtx, Temperature: temperature},
		Stream:   false,
	}
	body, err := post(baseURL+"/api/chat", req)
	if err != nil {
		return nil, err
	}
	var resp chatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad chat response: %w", err)
	}
	return decodeJSONObject(resp.Message.Content)
}

// Embed calls /api/embed for a batch of texts (server-side batching handled
// by the caller).
func Embed(baseURL, model string, texts []string) ([][]float32, error) {
	req := embedRequest{Model: model, Input: texts}
	body, err := post(baseURL+"/api/embed", req)
	if err != nil {
		return nil, err
	}
	var resp embedResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad embed response: %w", err)
	}
	return resp.Embeddings, nil
}

func post(url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama %s: %s", resp.Status,
			strings.TrimSpace(string(body)))
	}
	return body, nil
}

// decodeJSONObject parses the model's JSON output; with a schema attached
// this should always be a clean object, but never trust an LLM fully.
func decodeJSONObject(content string) (map[string]any, error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") {
		if i := strings.Index(content, "{"); i >= 0 {
			content = content[i:]
		}
	}
	if j := strings.LastIndex(content, "}"); j >= 0 {
		content = content[:j+1]
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("model did not return a JSON object: %w", err)
	}
	return out, nil
}
