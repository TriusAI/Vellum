// Package summarize implements map-reduce summarization and constrained
// tagging against the controlled vocabulary. Prompts mirror the retired
// Python version so output behavior stays consistent.
package summarize

import (
	"fmt"
	"log"
	"strings"

	"vellum/internal/config"
	"vellum/internal/llm"
	"vellum/internal/vocab"
)

const directPrompt = `You are writing the index entry for a document in a personal library.
Write ONE flowing paragraph (150-300 words) summarizing the whole document
below: its topic, its main arguments or findings, and what kind of text it
is. Do NOT copy passages from the document — describe it in your own words.
Plain text only, no headings, no lists, no markdown.

Document:
{text}`

const mapPrompt = `You are summarizing one excerpt of a longer document for a personal library index.
Write a concise factual summary (max 150 words) of the key points, arguments,
names, and topics of the excerpt. Do not speculate about missing context.
Plain text only, no headings or lists.

Excerpt:
{text}`

const reducePrompt = `You are writing the index entry for a document in a personal library.
Below are summaries of consecutive excerpts of the document, in order.
Write ONE flowing paragraph (150-300 words) summarizing the whole document:
its topic, its main arguments or findings, and what kind of text it is.
Plain text only, no headings, no lists, no markdown.

Excerpt summaries:
{summaries}`

const tagPrompt = `You are cataloguing a document in a personal library.

Allowed tags (you MUST choose only from this list; a tag applies only if the
document substantively addresses the topic, not if it merely mentions it):
{descriptions}
Rules:
- Choose the most relevant tags, usually 2-6, at most {max_tags}.
- If the document clearly and substantively belongs to a topic that is missing
  from the allowed list, put ONE short lowercase-hyphenated English label for
  it in tags_other (e.g. "marine-biology"). Otherwise leave tags_other empty.
- If you can confidently infer the document's title, authors, or publication
  year from the text, fill them in; otherwise leave them empty ("" / []).

Document text (possibly truncated):
{text}`

var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
	},
	"required": []string{"summary"},
}

func tagSchema(enum []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":      map[string]any{"type": "string"},
			"authors":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"year":       map[string]any{"type": "string"},
			"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": enum}},
			"tags_other": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"title", "authors", "year", "tags", "tags_other"},
	}
}

func chat(cfg *config.Config, prompt string, schema map[string]any) (map[string]any, error) {
	return llm.ChatJSON(cfg.Tools.LLMURL,
		[]llm.Message{{Role: "user", Content: prompt}},
		schema, cfg.LLM.Think, cfg.LLM.Temperature)
}

// chunkText splits on paragraph boundaries close to the target size.
func chunkText(text string, chunkChars int) []string {
	paras := strings.Split(text, "\n\n")
	var chunks []string
	var cur []string
	size := 0
	for _, p := range paras {
		if size+len(p) > chunkChars && len(cur) > 0 {
			chunks = append(chunks, strings.Join(cur, "\n\n"))
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += len(p) + 2
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "\n\n"))
	}
	var out []string
	for _, c := range chunks {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// Summarize produces one paragraph via map-reduce; short documents get a
// single direct pass.
func Summarize(cfg *config.Config, text string) (string, error) {
	chunks := chunkText(text, cfg.Summarize.ChunkChars)

	var prompt string
	if len(chunks) <= 1 {
		limit := cfg.Summarize.ChunkChars * 3
		body := text
		if len(body) > limit {
			body = body[:limit]
		}
		prompt = strings.ReplaceAll(directPrompt, "{text}", body)
	} else {
		log.Printf("map phase: %d chunks", len(chunks))
		summaries := make([]string, 0, len(chunks))
		for _, c := range chunks {
			out, err := chat(cfg, strings.ReplaceAll(mapPrompt, "{text}", c), summarySchema)
			if err != nil {
				return "", err
			}
			summaries = append(summaries, strings.TrimSpace(str(out["summary"])))
		}
		log.Printf("reduce phase")
		prompt = strings.ReplaceAll(reducePrompt, "{summaries}", strings.Join(summaries, "\n---\n"))
	}

	out, err := chat(cfg, prompt, summarySchema)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(str(out["summary"])), nil
}

// TagResult is the validated outcome of a tagging call.
type TagResult struct {
	Title     string
	Authors   []string
	Year      string
	Tags      []string // all in the controlled vocabulary
	TagsOther []string // freeform suggestions, lowercase, max 3
}

// TagDocument runs the constrained tagging call. The schema enum is built
// from the vocabulary, so tags outside it are impossible to emit; anything
// returned is still validated against the vocabulary (defense in depth).
func TagDocument(cfg *config.Config, v *vocab.Vocabulary, text string) (*TagResult, error) {
	limit := cfg.Summarize.ChunkChars * 3
	if len(text) > limit {
		text = text[:limit]
	}

	prompt := strings.ReplaceAll(tagPrompt, "{descriptions}", v.DescriptionsBlock())
	prompt = strings.ReplaceAll(prompt, "{max_tags}", fmt.Sprint(cfg.Summarize.MaxTags))
	prompt = strings.ReplaceAll(prompt, "{text}", text)

	out, err := chat(cfg, prompt, tagSchema(v.SortedKeys()))
	if err != nil {
		return nil, err
	}

	vocabSet := map[string]bool{}
	for k := range v.Tags {
		vocabSet[k] = true
	}
	res := &TagResult{
		Title:   str(out["title"]),
		Year:    str(out["year"]),
		Authors: strSlice(out["authors"]),
	}
	for _, t := range strSlice(out["tags"]) {
		if vocabSet[t] && len(res.Tags) < cfg.Summarize.MaxTags {
			res.Tags = append(res.Tags, t)
		}
	}
	for _, t := range strSlice(out["tags_other"]) {
		t = strings.ToLower(strings.TrimSpace(t))
		switch t {
		case "", "unknown", "untitled", "unspecified", "anonymous", "none",
			"n/a", "na", "null", "other", "misc", "miscellaneous":
			continue // LLM hedging is not a suggestion
		}
		if len(res.TagsOther) < 3 {
			res.TagsOther = append(res.TagsOther, t)
		}
	}
	return res, nil
}

// str coerces JSON any -> string.
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// strSlice coerces JSON any -> []string.
func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
