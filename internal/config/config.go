// Package config loads Vellum's YAML configuration.
//
// Everything (db file, vocab) lives relative to the directory containing the
// config file, so the whole library is portable as one folder.
package config

import (
	"os"
	"path/filepath"

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
		Think       bool    `yaml:"think"`
		Temperature float64 `yaml:"temperature"`
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
		Batch int `yaml:"batch"`
	} `yaml:"embed"`

	Tools struct {
		Mutool    string `yaml:"mutool"`
		Tesseract string `yaml:"tesseract"`
		Tessdata  string `yaml:"tessdata"`  // optional explicit tessdata dir
		LLMURL    string `yaml:"llm_url"`   // llama-server hosting the chat model
		EmbedURL  string `yaml:"embed_url"` // llama-server hosting the embedding model
	} `yaml:"tools"`

	// computed at load time
	BaseDir   string `yaml:"-"`
	DBPath    string `yaml:"-"`
	VocabPath string `yaml:"-"`
}

// Default returns a Config with all defaults filled in.
func Default() *Config {
	c := &Config{}
	c.LibraryDir = "~/VellumLibrary"
	c.DB = "library.db"
	c.Models.LLM = "qwen3-4b.gguf"
	c.Models.Embed = "nomic-embed-text-v1.5.gguf"
	c.LLM.Think = false
	c.LLM.Temperature = 0.3
	c.OCR.Langs = "eng+chi_sim+fin"
	c.OCR.DPI = 300
	c.OCR.MinCharsPerPage = 50
	c.OCR.Workers = 3
	c.Summarize.ChunkChars = 6000
	c.Summarize.MaxTags = 8
	c.Embed.Batch = 32
	c.Tools.Mutool = "mutool"
	c.Tools.Tesseract = "tesseract"
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
	} else {
		cfg.BaseDir = wd
	}

	cfg.DBPath = resolve(cfg.BaseDir, cfg.DB)
	if cfg.VocabPath == "" {
		cfg.VocabPath = filepath.Join(cfg.BaseDir, "vocab.yaml")
	}
	return cfg, nil
}
