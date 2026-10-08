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

// ---- tool plumbing --------------------------------------------------------

// oaiTool is the OpenAI wire shape for a tool definition.
type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func toOAITools(tools []ToolDef) []oaiTool {
	out := make([]oaiTool, 0, len(tools))
	for _, t := range tools {
		var o oaiTool
		o.Type = "function"
		o.Function.Name = t.Name
		o.Function.Description = t.Description
		o.Function.Parameters = t.Parameters
		out = append(out, o)
	}
	return out
}

// execTool announces the call to the UI, runs it, and emits any UI action.
func execTool(out chan<- Delta, run Runner, id, name, args string) string {
	out <- Delta{Tool: name, ToolArgs: args}
	res := run(ToolCall{ID: id, Name: name, Args: args})
	if res.Action != nil {
		out <- Delta{Action: res.Action}
	}
	if res.Content == "" {
		return "(no result)"
	}
	return res.Content
}

func (c Config) client() *http.Client {
	return &http.Client{Timeout: 0} // stream duration is controlled by the request body
}

// endpoint joins an API base with its version + path, tolerating a base the
// user pasted WITH the version already on it (OpenAI-compatible servers are
// commonly addressed as "…/v1", which must not become "…/v1/v1/…"), or the
// full endpoint path.
func endpoint(base, version, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, path) {
		return base // the user pasted the full endpoint
	}
	if strings.HasSuffix(base, "/"+version) {
		return base + path
	}
	return base + "/" + version + path
}

// ---- openai-compatible -------------------------------------------------

type openaiStreamReq struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Stream      bool         `json:"stream"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	ToolChoice  string       `json:"tool_choice,omitempty"`
}

// oaiMessage is a request turn: it can carry tool calls (assistant) or a
// tool result (role "tool").
type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// maxToolRounds bounds the tool-calling loop (each round is one provider
// request); it stops a model that keeps fetching forever.
const maxToolRounds = 4

func (c Config) streamOpenAI(sys string, msgs []Message, tools []ToolDef, run Runner) (<-chan Delta, error) {
	conv := []oaiMessage{{Role: "system", Content: sys}}
	for _, m := range msgs {
		conv = append(conv, oaiMessage{Role: m.Role, Content: m.Content})
	}
	out := make(chan Delta)
	go func() {
		defer close(out)
		for round := 0; round < maxToolRounds; round++ {
			text, calls, err := c.openaiRound(out, conv, tools)
			if err != nil {
				out <- Delta{Error: err.Error()}
				return
			}
			if len(calls) == 0 {
				return // a normal answer
			}
			// feed the assistant's tool calls + our results back in
			conv = append(conv, oaiMessage{Role: "assistant", Content: text, ToolCalls: calls})
			for _, tc := range calls {
				result := execTool(out, run, tc.ID, tc.Function.Name, tc.Function.Arguments)
				conv = append(conv, oaiMessage{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: result})
			}
		}
		out <- Delta{Error: "stopped: too many tool rounds"}
	}()
	return out, nil
}

// openaiRound runs one streaming completion, forwarding text deltas to out
// and returning the assistant text plus any tool calls it requested.
func (c Config) openaiRound(out chan<- Delta, conv []oaiMessage, tools []ToolDef) (string, []oaiToolCall, error) {
	reqBody := openaiStreamReq{
		Model: c.Model, Messages: conv, Stream: true, MaxTokens: 4096,
	}
	if len(tools) > 0 {
		reqBody.Tools = toOAITools(tools)
		reqBody.ToolChoice = "auto"
	}
	payload, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", endpoint(c.baseURL(), "v1", "/chat/completions"),
		bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var text strings.Builder
	calls := map[int]*oaiToolCall{}
	var order []int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // keep-alive comments and the like
		}
		if chunk.Error != nil {
			return text.String(), nil, fmt.Errorf("%s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			text.WriteString(d.Content)
			out <- Delta{Text: d.Content}
		}
		for _, tc := range d.ToolCalls {
			cur := calls[tc.Index]
			if cur == nil {
				cur = &oaiToolCall{}
				calls[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return text.String(), nil, err
	}
	ordered := make([]oaiToolCall, 0, len(order))
	for _, i := range order {
		ordered = append(ordered, *calls[i])
	}
	return text.String(), ordered, nil
}

// ---- anthropic ----------------------------------------------------------

type anthropicReq struct {
	Model     string          `json:"model"`
	System    string          `json:"system,omitempty"`
	Messages  []anthropicMsg  `json:"messages"` // system roles go in System
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	Tools     []anthropicTool `json:"tools,omitempty"`
}

// anthropicMsg.Content is a string (plain turns) or []anthropicBlock (turns
// that carry tool_use / tool_result blocks).
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"` // text | tool_use | tool_result
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

func toAnthropicTools(tools []ToolDef) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	return out
}

// anthropicCall is a tool_use accumulated from the stream.
type anthropicCall struct {
	id, name, args string
}

func (c Config) streamAnthropic(sys string, msgs []Message, tools []ToolDef, run Runner) (<-chan Delta, error) {
	conv := []anthropicMsg{}
	for _, m := range msgs {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		conv = append(conv, anthropicMsg{Role: role, Content: m.Content})
	}
	out := make(chan Delta)
	go func() {
		defer close(out)
		for round := 0; round < maxToolRounds; round++ {
			text, calls, err := c.anthropicRound(out, sys, conv, tools)
			if err != nil {
				out <- Delta{Error: err.Error()}
				return
			}
			if len(calls) == 0 {
				return // a normal answer
			}
			// assistant turn: any text, then the tool_use blocks
			blocks := []anthropicBlock{}
			if strings.TrimSpace(text) != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
			}
			for _, tc := range calls {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.id, Name: tc.name,
					Input: json.RawMessage(orEmptyObject(tc.args))})
			}
			conv = append(conv, anthropicMsg{Role: "assistant", Content: blocks})
			// user turn: the tool results
			results := []anthropicBlock{}
			for _, tc := range calls {
				result := execTool(out, run, tc.id, tc.name, tc.args)
				results = append(results, anthropicBlock{
					Type: "tool_result", ToolUseID: tc.id, Content: result})
			}
			conv = append(conv, anthropicMsg{Role: "user", Content: results})
		}
		out <- Delta{Error: "stopped: too many tool rounds"}
	}()
	return out, nil
}

func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// anthropicRound runs one streaming /v1/messages call, forwarding text
// deltas to out and returning the assistant text + any tool_use blocks.
func (c Config) anthropicRound(out chan<- Delta, sys string, conv []anthropicMsg, tools []ToolDef) (string, []anthropicCall, error) {
	reqBody := anthropicReq{
		Model: c.Model, System: sys, Messages: conv,
		MaxTokens: 4096, Stream: true,
	}
	if len(tools) > 0 {
		reqBody.Tools = toAnthropicTools(tools)
	}
	payload, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", endpoint(c.baseURL(), "v1", "/messages"),
		bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.client().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var text strings.Builder
	calls := map[int]*anthropicCall{}
	var order []int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024), 1024*1024)
scan:
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // "event: ..." lines precede each data line
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				calls[ev.Index] = &anthropicCall{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					text.WriteString(ev.Delta.Text)
					out <- Delta{Text: ev.Delta.Text}
				}
			case "input_json_delta":
				if tc := calls[ev.Index]; tc != nil {
					tc.args += ev.Delta.PartialJSON
				}
			}
		case "message_stop":
			break scan
		case "error":
			if ev.Error != nil {
				return text.String(), nil, fmt.Errorf("%s", ev.Error.Message)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return text.String(), nil, err
	}
	ordered := make([]anthropicCall, 0, len(order))
	for _, i := range order {
		ordered = append(ordered, *calls[i])
	}
	return text.String(), ordered, nil
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
	req, err := http.NewRequest("POST", endpoint(c.baseURL(), "api", "/chat"),
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
