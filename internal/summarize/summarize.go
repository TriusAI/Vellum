// Package summarize implements map-reduce summarization and constrained
// tagging against the controlled vocabulary.
//
// Requests are token-budgeted against the server's context window
// (llm.NumCtx): the client asks the server's tokenizer (/tokenize) how big
// an input is and trims at paragraph boundaries, so "request exceeds
// context" errors cannot happen for arbitrarily large documents. For very
// long documents the reduce phase is hierarchical: chunk summaries are
// reduced in batches until one final summary fits.
package summarize

import (
	"fmt"
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
- The vocabulary grows over time: if the document's central topic is NOT
  well covered by the allowed list, PROPOSE up to two new tags in
  tags_other — short, lowercase, hyphenated English labels (e.g.
  "marine-biology", "austen-studies"). Only propose a new tag when it is
  genuinely better than anything allowed; do not duplicate allowed tags,
  and do not propose vague labels ("misc", "other", "science").
- Also file the document into ONE shelve-like category in the "category"
  field — a broad subject area (not a format, not a tag duplicate), short,
  lowercase, hyphenated ("quantum-mechanics", "moral-theology",
  "medieval-history"). Categories already in use in this library:
{categories}
prefer one of those EXACTLY when it fits; propose a new one otherwise.
If nothing fits, use "".
- If you can confidently infer the document's title, authors, or publication
  year from the text, fill them in; otherwise leave them empty ("" / []).

Document:
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
			"category":   map[string]any{"type": "string", "maxLength": 40},
			"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": enum}},
			"tags_other": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"title", "authors", "year", "category", "tags", "tags_other"},
	}
}

func chat(cfg *config.Config, prompt string, schema map[string]any) (map[string]any, error) {
	// ollama backend: same grammar-structured call through its native API
	if cfg.LLM.Backend == "ollama" {
		return llm.OllamaChatJSON(cfg.Tools.LLMURL, cfg.LLM.Model,
			[]llm.Message{{Role: "user", Content: prompt}},
			schema, cfg.LLM.Think, cfg.LLM.Temperature, cfg.LLM.NumCtx)
	}
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

// promptOverhead is the token head-room reserved for instructions + the
// assistant's reply when budgeting inputs (deliberately generous).
const promptOverhead = 1400

// budgeted returns text trimmed (at a paragraph boundary) to fit the
// server's context window, minus overhead. Falls back to a conservative
// character cap if the tokenizer is unreachable.
func budgeted(cfg *config.Config, text string) string {
	out, err := llm.TrimToTokenBudget(cfg.Tools.LLMURL, text,
		cfg.LLM.NumCtx, promptOverhead)
	if err != nil {
		// tokenizer unavailable: conservative chars-based cap (~3.5 chars/token)
		limit := int(float64(cfg.LLM.NumCtx-promptOverhead) * 3.5)
		if limit > 0 && len(text) > limit {
			return text[:limit]
		}
		return text
	}
	return out
}

// MapSummaries runs the map phase: one summary per chunk. Returns
// (summaries, chunks); for single-chunk documents the map phase is skipped
// and summaries is nil.
// ProgressFunc receives live human-readable progress updates; nil is fine.
type ProgressFunc func(message string)

func report(cb ProgressFunc, format string, args ...any) {
	if cb != nil {
		cb(fmt.Sprintf(format, args...))
	}
}

func MapSummaries(cfg *config.Config, text string, cb ProgressFunc) ([]string, []string, error) {
	chunks := chunkText(text, cfg.Summarize.ChunkChars)
	if len(chunks) <= 1 {
		return nil, chunks, nil
	}
	summaries := make([]string, 0, len(chunks))
	for i, c := range chunks {
		report(cb, "summarizing section %d/%d", i+1, len(chunks))
		out, err := chat(cfg, strings.ReplaceAll(mapPrompt, "{text}", c), summarySchema)
		if err != nil {
			return nil, nil, err
		}
		summaries = append(summaries, strings.TrimSpace(str(out["summary"])))
	}
	return summaries, chunks, nil
}

// Summarize produces one paragraph via map-reduce; short documents get a
// single direct pass.
func Summarize(cfg *config.Config, text string) (string, error) {
	summaries, chunks, err := MapSummaries(cfg, text, nil)
	if err != nil {
		return "", err
	}
	return SummarizeFrom(cfg, chunks, summaries, nil)
}

// SummarizeFrom finishes the summarization: direct pass for short documents,
// hierarchical reduce for long ones (chunk summaries are merged in batches
// until one final call fits the context window).
func SummarizeFrom(cfg *config.Config, chunks, summaries []string, cb ProgressFunc) (string, error) {
	if summaries == nil {
		// short document: single direct pass over the (budgeted) text
		report(cb, "summarizing document")
		body := budgeted(cfg, strings.Join(chunks, "\n\n"))
		out, err := chat(cfg, strings.ReplaceAll(directPrompt, "{text}", body), summarySchema)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(str(out["summary"])), nil
	}

	// ~3.5 chars/token: batch summaries so each reduce call fits
	budgetChars := int(float64(cfg.LLM.NumCtx-promptOverhead) * 3.5)
	level := 0
	for {
		total := 0
		for i, s := range summaries {
			total += len(s)
			if i > 0 {
				total += 5
			}
		}
		report(cb, "reducing %d section summaries", len(summaries))
		if len(summaries) <= 1 || total <= budgetChars {
			return reduceCall(cfg, summaries)
		}

		// pack into batches that each fit the budget
		var batches [][]string
		var cur []string
		size := 0
		for _, s := range summaries {
			if size+len(s)+5 > budgetChars && len(cur) > 1 {
				batches = append(batches, cur)
				cur, size = nil, 0
			}
			cur = append(cur, s)
			size += len(s) + 5
		}
		if len(cur) > 0 {
			batches = append(batches, cur)
		}
		if len(batches) <= 1 {
			return reduceCall(cfg, summaries)
		}
		level++
		report(cb, "reduce level %d: %d batches", level, len(batches))
		next := make([]string, 0, len(batches))
		for _, b := range batches {
			out, err := reduceCall(cfg, b)
			if err != nil {
				return "", err
			}
			next = append(next, out)
		}
		summaries = next
	}
}

func reduceCall(cfg *config.Config, summaries []string) (string, error) {
	prompt := strings.ReplaceAll(reducePrompt, "{summaries}",
		strings.Join(summaries, "\n---\n"))
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
	Category  string   // shelving suggestion ("" none); junk-filtered
	Tags      []string // all in the controlled vocabulary
	TagsOther []string // freeform suggestions, lowercase, max 3
}

// TagDocument runs the constrained tagging call. The schema enum is built
// from the vocabulary, so tags outside it are impossible to emit; anything
// returned is still validated against the vocabulary (defense in depth).
//
// Long documents are tagged from their summary + section summaries + the
// opening text (title pages carry the bibliographic metadata) instead of a
// raw-text prefix — cheaper and enough signal for topic tagging.
func TagDocument(cfg *config.Config, v *vocab.Vocabulary,
	chunks, summaries []string, finalSummary string, cb ProgressFunc) (*TagResult, error) {
	return TagDocumentWithCategories(cfg, v, nil, chunks, summaries, finalSummary, cb)
}

// TagDocumentWithCategories is TagDocument plus the library's existing
// categories, which the model is told to reuse when one fits (so
// auto-categorization consolidates shelves instead of inventing one per
// document).
func TagDocumentWithCategories(cfg *config.Config, v *vocab.Vocabulary,
	categories []string, chunks, summaries []string, finalSummary string,
	cb ProgressFunc) (*TagResult, error) {

	var input string
	if len(summaries) > 0 {
		var b strings.Builder
		b.WriteString("Summary of the document:\n" + finalSummary + "\n\n")
		opening := chunks[0]
		if len(chunks) > 1 {
			opening = opening + "\n\n[...]\n\n" + chunks[1]
		}
		if len(opening) > 6000 {
			opening = opening[:6000]
		}
		b.WriteString("Opening text:\n" + opening + "\n\n")
		b.WriteString("Section summaries:\n" + strings.Join(summaries, "\n---\n"))
		input = b.String()
	} else {
		input = strings.Join(chunks, "\n\n")
	}
	input = budgeted(cfg, input)

	report(cb, "choosing tags")
	prompt := strings.ReplaceAll(tagPrompt, "{descriptions}", v.DescriptionsBlock())
	prompt = strings.ReplaceAll(prompt, "{max_tags}", fmt.Sprint(cfg.Summarize.MaxTags))
	prompt = strings.ReplaceAll(prompt, "{categories}", func() string {
		if len(categories) == 0 {
			return "(none yet — propose freely)"
		}
		return strings.Join(categories, ", ")
	}())
	prompt = strings.ReplaceAll(prompt, "{text}", input)

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
	{
		cat := strings.ToLower(strings.TrimSpace(str(out["category"])))
		cat = strings.Join(strings.Fields(cat), "-") // spaces -> hyphens
		switch cat {
		case "", "unknown", "untitled", "unspecified", "none", "n/a",
			"na", "null", "other", "misc", "miscellaneous", "general",
			"documents", "library", "uncategorized":
			cat = ""
		}
		// reuse an existing shelf's exact spelling when it matches
		for _, existing := range categories {
			if strings.EqualFold(existing, cat) && existing != "" {
				cat = existing
				break
			}
		}
		if len(cat) > 40 {
			cat = cat[:40]
		}
		res.Category = cat
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
		if vocabSet[t] {
			continue // suggestions must extend the vocabulary, not duplicate it
		}
		if len(res.TagsOther) < 2 {
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
