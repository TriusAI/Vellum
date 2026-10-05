package ask

// Provider-specific streaming: all three adapters normalize their native
// wire format into a Delta channel.
//
//   openai  (and any compatible endpoint: local llama-server, vLLM,
//           LM Studio, OpenRouter, ...): POST /v1/chat/completions,
//           stream:true → SSE lines "data: {...}" → delta in
//           choices[0].delta.content, terminated by "data: [DONE]".
//   anthropic: POST /v1/messages, stream:true → SSE with header
//           x-api-key + anthropic-version; text arrives as
//           content_block_delta events (delta.text).
//   ollama: POST /api/chat → NDJSON lines {"message":{"content":..}}.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c Config) client() *http.Client {
	return &http.Client{Timeout: 0} // stream duration is controlled by the request body
}

// ---- openai-compatible -------------------------------------------------

type openaiStreamReq struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c Config) streamOpenAI(sys string, msgs []Message) (<-chan Delta, error) {
	all := []wireMessage{{Role: "system", Content: sys}}
	for _, m := range msgs {
		all = append(all, wireMessage{Role: m.Role, Content: m.Content})
	}
	maxTokens := 4096
	payload, _ := json.Marshal(openaiStreamReq{
		Model: c.Model, Messages: all, Stream: true, MaxTokens: maxTokens,
	})
	req, err := http.NewRequest("POST", c.baseURL()+"/v1/chat/completions",
		bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	out := make(chan Delta)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				return
			}
			var chunk openaiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue // keep-alive comments and the like
			}
			if chunk.Error != nil {
				out <- Delta{Error: chunk.Error.Message}
				return
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				out <- Delta{Text: chunk.Choices[0].Delta.Content}
			}
		}
	}()
	return out, nil
}

// ---- anthropic ----------------------------------------------------------

type anthropicStreamReq struct {
	Model     string        `json:"model"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"` // system roles not allowed here
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

func (c Config) streamAnthropic(sys string, msgs []Message) (<-chan Delta, error) {
	rest := []wireMessage{}
	for _, m := range msgs {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		rest = append(rest, wireMessage{Role: role, Content: m.Content})
	}
	payload, _ := json.Marshal(anthropicStreamReq{
		Model: c.Model, System: sys, Messages: rest,
		MaxTokens: 4096, Stream: true,
	})
	req, err := http.NewRequest("POST", c.baseURL()+"/v1/messages",
		bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	out := make(chan Delta)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // "event: ..." lines precede each data line
			}
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "content_block_delta":
				if ev.Delta.Text != "" {
					out <- Delta{Text: ev.Delta.Text}
				}
			case "message_stop":
				return
			case "error":
				if ev.Error != nil {
					out <- Delta{Error: ev.Error.Message}
					return
				}
			}
		}
	}()
	return out, nil
}

// ---- ollama --------------------------------------------------------------

type ollamaChatReq struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

func (c Config) streamOllama(sys string, msgs []Message) (<-chan Delta, error) {
	all := []wireMessage{{Role: "system", Content: sys}}
	for _, m := range msgs {
		all = append(all, wireMessage{Role: m.Role, Content: m.Content})
	}
	payload, _ := json.Marshal(ollamaChatReq{
		Model: c.Model, Messages: all, Stream: true,
	})
	req, err := http.NewRequest("POST", c.baseURL()+"/api/chat",
		bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	out := make(chan Delta)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1024), 1024*1024)
		for sc.Scan() {
			var chunk struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Error string `json:"error,omitempty"`
				Done  bool   `json:"done"`
			}
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}
			if chunk.Error != "" {
				out <- Delta{Error: chunk.Error}
				return
			}
			if chunk.Message.Content != "" {
				out <- Delta{Text: chunk.Message.Content}
			}
			if chunk.Done {
				return
			}
		}
	}()
	return out, nil
}
