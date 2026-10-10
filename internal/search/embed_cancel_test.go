package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vellum/internal/config"
	"vellum/internal/db"
)

// EmbedPendingCtx must stop between batches when its context is cancelled,
// keeping the batches already committed — that is what makes the Jobs
// dialog's "cancel" work for an embedding run.
func TestEmbedPendingCtxStopsBetweenBatches(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		"INSERT INTO documents(id, path, sha256, status) VALUES(1,'/x','s','done')"); err != nil {
		t.Fatalf("insert doc: %v", err)
	}
	const n = 6
	for i := 0; i < n; i++ {
		if _, err := conn.Exec(
			"INSERT INTO chunks(doc_id, seq, text) VALUES(1,?,?)",
			i, "hello world"); err != nil {
			t.Fatalf("insert chunk: %v", err)
		}
	}

	var embeds int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/embeddings") {
			http.Error(w, "no tokenizer", http.StatusNotFound) // trimming falls back
			return
		}
		atomic.AddInt32(&embeds, 1)
		time.Sleep(150 * time.Millisecond)
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i := range in.Input {
			out.Data = append(out.Data, item{i, []float32{0.1, 0.2}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.BaseDir = dir
	cfg.Embed.Provider = "llama-server"
	cfg.Tools.EmbedURL = srv.URL
	cfg.Embed.Batch = 1 // one chunk per batch → cancellable after each

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(250 * time.Millisecond); cancel() }()

	done, err := EmbedPendingCtx(ctx, cfg, conn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&embeds); got >= n {
		t.Fatalf("made %d embed calls; cancellation did not stop the loop", got)
	}
	if done >= n {
		t.Fatalf("done = %d, want < %d", done, n)
	}
}
