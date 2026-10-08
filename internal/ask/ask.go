// Package ask is the "Ask an LLM" chat: a freeform, streaming chat about
// one document, served by an EXTERNAL model the user configures
// (`ask:` in config.yaml) — an OpenAI-compatible endpoint (OpenAI, or
// anything speaking its wire format), Anthropic, or a local Ollama.
// It is deliberately separate from the pipeline's constrained calls:
// the chat model can be big/cloud/dumb — grammar enforcement for tags
// stays with the pipeline backend either way.
package ask

import "strings"

// Config is the ask.provider configuration (config.yaml `ask:`).
type Config struct {
	Provider string `yaml:"provider"` // none | openai | anthropic | ollama
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"` // override; per-provider default
	// Tools lets the chat model call tools — a fetch_url (WebFetch) tool,
	// and (in the API's chat layer) library search / open / regenerate
	// tools. Supported on OpenAI-compatible providers and Anthropic.
	Tools bool `yaml:"tools"`
}

// Message is one chat turn (both directions).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolDef describes one callable tool the chat model may use.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// ToolCall is a tool invocation requested by the model (Args is the raw JSON
// object string).
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// ToolResult is a tool's output (fed back to the model) plus an optional UI
// action the client should perform (e.g. open a document's Preview page).
type ToolResult struct {
	Content string
	Action  map[string]any
}

// Runner executes one tool call.
type Runner func(ToolCall) ToolResult

// Delta is one streamed chunk; Error (nonzero) terminates the stream. Tool /
// ToolArgs report a tool the model invoked (for a UI hint); Action asks the
// client to do something (e.g. open a page).
type Delta struct {
	Text     string         `json:"text,omitempty"`
	Tool     string         `json:"tool,omitempty"`
	ToolArgs string         `json:"tool_args,omitempty"`
	Action   map[string]any `json:"action,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// ErrDisabled marks a missing provider configuration.
var ErrDisabled = &disabledError{}

type disabledError struct{}

func (*disabledError) Error() string {
	return "no ask provider configured (ask.provider in config.yaml)"
}

// Enabled reports whether an LLM provider is configured.
func (c Config) Enabled() bool {
	switch c.Provider {
	case "openai", "anthropic", "ollama":
		return true
	}
	return false
}

// baseURL resolves the provider's endpoint.
func (c Config) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	switch c.Provider {
	case "openai":
		return "https://api.openai.com"
	case "anthropic":
		return "https://api.anthropic.com"
	case "ollama":
		return "http://127.0.0.1:11434"
	}
	return ""
}

// Stream dispatches on the provider and returns the normalized delta
// channel (closed at completion or on a terminal error). tools/run enable
// the tool-calling loop (ignored by the Ollama-native adapter, which has no
// tools); pass nil, nil for a plain answer.
func (c Config) Stream(sys string, msgs []Message, tools []ToolDef, run Runner) (<-chan Delta, error) {
	if len(tools) == 0 || run == nil {
		tools, run = nil, nil
	}
	switch c.Provider {
	case "openai":
		return c.streamOpenAI(sys, msgs, tools, run)
	case "anthropic":
		return c.streamAnthropic(sys, msgs, tools, run)
	case "ollama":
		return c.streamOllama(sys, msgs)
	}
	return nil, ErrDisabled
}

// Test runs a tiny streaming exchange to verify the configuration end to
// end (used by the UI's connection check).
func (c Config) Test() error {
	if !c.Enabled() {
		return ErrDisabled
	}
	deltas, err := c.Stream("You are a connection test.",
		[]Message{{Role: "user", Content: "Reply with exactly: OK"}}, nil, nil)
	if err != nil {
		return err
	}
	n := 0
	for d := range deltas {
		if d.Error != "" {
			return &testError{d.Error}
		}
		n += len(d.Text)
		if n > 100 {
			break // got output; the connection works
		}
	}
	if n == 0 {
		return &testError{"provider returned no output"}
	}
	return nil
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
