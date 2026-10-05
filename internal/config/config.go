// Package config loads Vellum's YAML configuration.
//
// Everything (db file, vocab) lives relative to the directory containing the
// config file, so the whole library is portable as one folder.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"vellum/internal/ask"

	"gopkg.in/yaml.v3"
)

// Config mirrors config.yaml. Same file format as the (retired) Python
// version; a library built by it keeps working.
type Config struct {
	LibraryDir string `yaml:"library_dir"`
	DB         string `yaml:"db"`

	Models struct {
		// informational only: which GGUF each server should host (the
		// launcher reads these as file names in models/).
		LLM   string `yaml:"llm"`
		Embed string `yaml:"embed"`
	} `yaml:"models"`

	LLM struct {
		// Backend is how Vellum talks to the chat model:
		//   llama-server (default): bundled llama.cpp server via
		//     /v1/chat/completions (schema-constrained decoding)
		//   ollama: any Ollama instance via its native /api/chat with
		//     the same JSON schema as "format" (tags stay grammar-bound;
		//     tools.llm_url becomes the Ollama base URL, llm.model the
		//     Ollama model name; the launcher skips its llama-servers)
		// With external=true the launcher starts NO bundled server and
		// tools.llm_url is used as-is — e.g. your own llama.cpp on
		// another machine or GPU host (num_ctx must match its -c).
		Backend     string  `yaml:"backend"`
		External    bool    `yaml:"external"`
		Model       string  `yaml:"model"`
		Think       bool    `yaml:"think"`
		Temperature float64 `yaml:"temperature"`
		// NumCtx is the chat server's context window (its -c flag). The
		// client uses it to budget requests; it must match how the server
		// is actually started (the pack launcher and docker entrypoint
		// use the same value).
		NumCtx int `yaml:"num_ctx"`
	} `yaml:"llm"`

	OCR struct {
		Langs           string `yaml:"langs"`
		DPI             int    `yaml:"dpi"`
		MinCharsPerPage int    `yaml:"min_chars_per_page"`
		Workers         int    `yaml:"workers"`
	} `yaml:"ocr"`

	Summarize struct {
		ChunkChars int `yaml:"chunk_chars"`
		MaxTags    int `yaml:"max_tags"`
	} `yaml:"summarize"`

	Embed struct {
		Provider  string `yaml:"provider"` // llama-server (default) | ollama
		External  bool   `yaml:"external"` // launcher skips its servers
		Model     string `yaml:"model"`    // ollama: embed model name
		Batch     int    `yaml:"batch"`
		MaxTokens int    `yaml:"max_tokens"` // per-chunk embedding input cap
	} `yaml:"embed"`

	// Ask configures the "Ask an LLM" chat (item-context Q&A in the
	// UI): none (default) | openai (any OpenAI-compatible endpoint, see
	// base_url) | anthropic | ollama. The pipeline's constrained
	// tagging is UNAFFECTED — this is for the freeform chat only.
	Ask ask.Config `yaml:"ask"`

	Tools struct {
		Mutool    string `yaml:"mutool"`
		Tesseract string `yaml:"tesseract"`
		Tessdata  string `yaml:"tessdata"`  // optional explicit tessdata dir
		LLMURL    string `yaml:"llm_url"`   // llama-server hosting the chat model
		EmbedURL  string `yaml:"embed_url"` // llama-server hosting the embedding model
	} `yaml:"tools"`

	// computed at load time
	configPath string `yaml:"-"`
	BaseDir    string `yaml:"-"`
	DBPath     string `yaml:"-"`
	VocabPath  string `yaml:"-"`
}

// Default returns a Config with all defaults filled in.
func Default() *Config {
	c := &Config{}
	c.LibraryDir = "~/VellumLibrary"
	c.DB = "library.db"
	c.Models.LLM = "qwen3-4b.gguf"
	c.Models.Embed = "nomic-embed-text-v1.5.gguf"
	c.LLM.Backend = "llama-server"
	c.LLM.Model = ""
	c.LLM.Think = false
	c.LLM.Temperature = 0.3
	// chat server: context sized to fit GPU VRAM; the client tokenizes and
	// trims its inputs to llm.num_ctx, so long documents never error out.
	// raise num_ctx (and -c) if you have the memory for less trimming.
	c.LLM.NumCtx = 8192
	c.OCR.Langs = "eng+chi_sim+fin"
	c.OCR.DPI = 300
	c.OCR.MinCharsPerPage = 50
	c.OCR.Workers = 3
	c.Summarize.ChunkChars = 6000
	c.Summarize.MaxTags = 8
	c.Embed.Provider = "llama-server"
	c.Embed.Model = ""
	c.Embed.Batch = 32
	// nomic-embed-text-v1.5 has a 2048-token context: chunk embedding
	// inputs are trimmed to this cap (the head of each chunk)
	c.Embed.MaxTokens = 1500
	c.Tools.Mutool = "mutool"
	c.Tools.Tesseract = "tesseract"
	c.Ask.Provider = "none"
	c.Tools.LLMURL = "http://127.0.0.1:8081"
	c.Tools.EmbedURL = "http://127.0.0.1:8082"
	return c
}

func resolve(baseDir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

// Load resolves the config path (--config flag > $VELLUM_CONFIG > ./config.yaml),
// reads user settings over the defaults, and computes derived paths.
func Load(path string) (*Config, error) {
	cfg := Default()

	candidates := []string{}
	if path != "" {
		candidates = append(candidates, path)
	}
	if env := os.Getenv("VELLUM_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	wd, _ := os.Getwd()
	candidates = append(candidates, filepath.Join(wd, "config.yaml"))

	cfgPath := ""
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			cfgPath = c
			break
		}
	}

	if cfgPath != "" {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
		cfg.BaseDir = filepath.Dir(cfgPath)
		cfg.configPath = cfgPath
	} else {
		cfg.BaseDir = wd
	}

	cfg.DBPath = resolve(cfg.BaseDir, cfg.DB)
	if cfg.VocabPath == "" {
		cfg.VocabPath = filepath.Join(cfg.BaseDir, "vocab.yaml")
	}
	return cfg, nil
}

// Save writes the config back to the YAML file it was loaded from,
// preserving whatever comments/format yaml.Marshal produces. Only used
// by the "ask" configuration UI.
func (c *Config) Save() error {
	if c.configPath == "" {
		return fmt.Errorf("config was not loaded from a file (defaults)")
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(c.configPath, data, 0o600) // contains the ask api key
}

// ConfigPath reports the file the config was loaded from.
func (c *Config) ConfigPath() string { return c.configPath }
