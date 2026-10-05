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
}

// Message is one chat turn (both directions).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Delta is one streamed chunk of assistant text; Error (nonzero)
// terminates the stream.
type Delta struct {
	Text  string
	Error string
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
// channel (closed at completion or on a terminal error).
func (c Config) Stream(sys string, msgs []Message) (<-chan Delta, error) {
	switch c.Provider {
	case "openai":
		return c.streamOpenAI(sys, msgs)
	case "anthropic":
		return c.streamAnthropic(sys, msgs)
	case "ollama":
		return c.streamOllama(sys, msgs)
	}
	return nil, ErrDisabled
}

// Test runs a tiny streaming exchange to verify the configuration end
// to end (used by the UI's connection check).
func (c Config) Test() error {
	if !c.Enabled() {
		return ErrDisabled
	}
	deltas, err := c.Stream("You are a connection test.",
		[]Message{{Role: "user", Content: "Reply with exactly: OK"}})
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
