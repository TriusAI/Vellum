package search

import "testing"

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
