// Package llm is a minimal client for llama.cpp's llama-server — the only
// backend Vellum needs.
//
// Structured output is the load-bearing feature here: the JSON schema sent
// as response_format is enforced via GBNF grammar-constrained decoding. An
// enum in the schema makes it *impossible* for the model to emit a tag
// outside the vocabulary — that's how Vellum avoids tag drift.
//
// The server hosts exactly one GGUF per process, so the pack runs two small
// servers: the chat model (llm_url) and the embedding model (embed_url).
package llm

import (
	"bytes"
	"context"
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

type responseFormat struct {
	Type       string       `json:"type"`
	JSONSchema *jsonSchema_ `json:"json_schema,omitempty"`
}

type jsonSchema_ struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
}

type chatRequest struct {
	Messages       []Message       `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	Stream         bool            `json:"stream"`
	// llama.cpp (--jinja) passes these to the chat template; Qwen3 uses
	// enable_thinking to toggle its reasoning pass.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type embeddingsRequest struct {
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Available reports whether a llama-server is reachable and healthy.
func Available(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(baseURL, "/") + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// CountTokens returns how many tokens the server's tokenizer sees in text.
// Used to budget requests against the context window.
func CountTokens(baseURL, text string) (int, error) {
	body, err := post(baseURL+"/tokenize", map[string]any{"content": text})
	if err != nil {
		return 0, err
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("bad tokenize response: %w", err)
	}
	return len(out.Tokens), nil
}

// TrimToTokenBudget trims text so prompt (text + overhead) fits in ctx.
// It estimates rather than tokenize-checks every candidate string: first a
// characters-per-token heuristic, then verifies with the tokenizer.
func TrimToTokenBudget(baseURL, text string, budgetTokens, overheadTokens int) (string, error) {
	if budgetTokens <= 0 {
		budgetTokens = 4096
	}
	effective := budgetTokens - overheadTokens
	if effective < 512 {
		effective = 512
	}
	n, err := CountTokens(baseURL, text)
	if err == nil {
		if n <= effective {
			return text, nil
		}
	} else {
		// no tokenizer reachable (e.g. the ollama backend): fall back to
		// the character heuristic directly instead of returning the
		// text uncut
		est := len(text)
		if est <= effective*3 {
			return text, nil
		}
		cut := strings.LastIndex(text[:effective*3], "\n\n")
		if cut <= 0 {
			cut = effective * 3
		}
		return text[:cut], nil
	}
	// estimate a cut point, then verify
	for {
		// ~3.5 chars/token is a safe lower bound for English prose
		cut := int(float64(effective) * 3.5)
		if cut >= len(text) {
			cut = len(text) - 1
		}
		cut = strings.LastIndex(text[:cut], "\n\n")
		if cut <= 0 {
			cut = int(float64(effective) * 3.5)
		}
		candidate := text[:cut]
		n, err := CountTokens(baseURL, candidate)
		if err != nil {
			return candidate, err
		}
		if n <= effective {
			return candidate, nil
		}
		// still too long: scale down by the measured ratio and retry
		ratio := float64(n) / float64(len(candidate))
		newLen := int(float64(effective) / ratio)
		if newLen >= len(candidate) {
			newLen = len(candidate) / 2
		}
		text = text[:newLen]
	}
}

// ChatJSON runs one chat call with a JSON-schema-constrained response and
// decodes the JSON object; ctx (nil = background) aborts the HTTP call
// mid-flight (job cancellation).
func ChatJSON(ctx context.Context, baseURL string, messages []Message, schema map[string]any,
	think bool, temperature float64) (map[string]any, error) {
	ctx = orCtx(ctx)
	req := chatRequest{
		Messages: messages,
		ResponseFormat: &responseFormat{Type: "json_schema",
			JSONSchema: &jsonSchema_{Name: "vellum", Schema: schema}},
		Temperature: temperature,
		MaxTokens:   2048,
		Stream:      false,
	}
	if !think {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	body, err := postCtx(ctx, baseURL+"/v1/chat/completions", req)
	if err != nil {
		return nil, err
	}
	var resp chatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad chat response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("chat error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("chat response has no choices: %s",
			strings.TrimSpace(string(body)))
	}
	content := resp.Choices[0].Message.Content
	if content == "" && resp.Choices[0].Message.ReasoningContent != "" {
		// reasoning produced, answer empty (shouldn't happen with a
		// grammar attached, but never trust a model fully)
		content = resp.Choices[0].Message.ReasoningContent
	}
	return decodeJSONObject(stripThink(content))
}

// ModelID asks an OpenAI-compatible server (/v1/models) which model it is
// serving, so callers can pick model-specific embedding task prefixes.
// Handles both the OpenAI shape (data[].id) and llama.cpp's (models[].name).
func ModelID(baseURL string) (string, error) {
	body, err := get(baseURL + "/v1/models")
	if err != nil {
		return "", err
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("bad /v1/models response: %w", err)
	}
	for _, d := range resp.Data {
		if d.ID != "" {
			return d.ID, nil
		}
	}
	for _, m := range resp.Models {
		if m.Name != "" {
			return m.Name, nil
		}
		if m.Model != "" {
			return m.Model, nil
		}
	}
	return "", fmt.Errorf("server reported no model")
}

// Embed calls /v1/embeddings for a batch of texts.
func Embed(baseURL string, texts []string) ([][]float32, error) {
	body, err := post(baseURL+"/v1/embeddings", embeddingsRequest{Input: texts})
	if err != nil {
		return nil, err
	}
	var resp embeddingsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bad embeddings response: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d",
			len(texts), len(resp.Data))
	}
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embedding index out of range: %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

func post(url string, payload any) ([]byte, error) {
	return postCtx(nil, url, payload)
}

// orCtx tolerates nil contexts (callers that hold none).
func orCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// get fetches a URL and returns the body (200 only).
func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return body, nil
}

// postCtx is post with cancellation: an aborted job's HTTP call dies
// mid-flight instead of waiting for the LLM to finish.
func postCtx(ctx context.Context, url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(orCtx(ctx), "POST", url,
		bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("llama-server %s: %s", resp.Status,
			strings.TrimSpace(string(body)))
	}
	return body, nil
}

// stripThink removes a <think>...</think> block in case the model's
// reasoning leaked into content (llama-server normally separates it).
func stripThink(s string) string {
	if i := strings.Index(s, "<think>"); i >= 0 {
		if j := strings.Index(s, "</think>"); j >= i {
			return strings.TrimSpace(s[:i] + s[j+len("</think>"):])
		}
	}
	return s
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
