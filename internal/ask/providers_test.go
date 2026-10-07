package ask

import "testing"

func TestEndpoint(t *testing.T) {
	cases := []struct {
		base, version, path, want string
	}{
		// plain host: version + path appended
		{"https://api.openai.com", "v1", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "v1", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		// OpenAI-compatible base that already ends in /v1 (very common):
		// must NOT become /v1/v1
		{"http://localhost:8080/v1", "v1", "/chat/completions", "http://localhost:8080/v1/chat/completions"},
		{"http://localhost:8080/v1/", "v1", "/chat/completions", "http://localhost:8080/v1/chat/completions"},
		// the full endpoint pasted back is left alone
		{"http://localhost:8080/v1/chat/completions", "v1", "/chat/completions", "http://localhost:8080/v1/chat/completions"},
		// ollama's native /api prefix, same tolerance
		{"http://127.0.0.1:11434", "api", "/chat", "http://127.0.0.1:11434/api/chat"},
		{"http://127.0.0.1:11434/api", "api", "/chat", "http://127.0.0.1:11434/api/chat"},
	}
	for _, c := range cases {
		if got := endpoint(c.base, c.version, c.path); got != c.want {
			t.Errorf("endpoint(%q,%q,%q) = %q, want %q",
				c.base, c.version, c.path, got, c.want)
		}
	}
}
