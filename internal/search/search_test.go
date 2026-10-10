package search

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vellum/internal/config"
	"vellum/internal/db"
)

func TestEmbedPrefixes(t *testing.T) {
	cases := []struct {
		model     string
		wantQuery string
		wantDoc   string
	}{
		{"nomic-embed-text-v1.5.gguf", "search_query: ", "search_document: "},
		{"/models/nomic-embed-text-v1.5.Q8_0.gguf", "search_query: ", "search_document: "},
		{"nomic-embed-text", "search_query: ", "search_document: "},
		{"embeddinggemma-2", "task: search result | query: ", "title: none | text: "},
		{"google/embeddinggemma-2-270m", "task: search result | query: ", "title: none | text: "},
		{"some-unknown-embedder", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		q, d := embedPrefixes(c.model)
		if q != c.wantQuery || d != c.wantDoc {
			t.Errorf("embedPrefixes(%q) = (%q,%q), want (%q,%q)",
				c.model, q, d, c.wantQuery, c.wantDoc)
		}
	}
}

// A cancelled context must abort EmbedPendingCtx promptly, even before the
// first batch — that is what lets the Jobs dialog cancel an embedding run.
func TestEmbedPendingCtxCancelled(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()
	cfg := config.Default()
	cfg.BaseDir = dir

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EmbedPendingCtx(ctx, cfg, conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("EmbedPendingCtx(cancelled) err = %v, want context.Canceled", err)
	}
}
