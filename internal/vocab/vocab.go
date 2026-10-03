// Package vocab manages the controlled tag vocabulary (vocab.yaml).
package vocab

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Vocabulary is an ordered map of tag name -> description.
type Vocabulary struct {
	Path string
	Tags map[string]string
}

type fileFormat struct {
	Tags map[string]string `yaml:"tags"`
}

// Load reads vocab.yaml (an empty vocabulary is valid — process will refuse
// to tag without one).
func Load(path string) (*Vocabulary, error) {
	v := &Vocabulary{Path: path, Tags: map[string]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for k, val := range f.Tags {
		v.Tags[k] = val
	}
	return v, nil
}

// Save writes the vocabulary back, with a short explanatory header.
func (v *Vocabulary) Save() error {
	var b strings.Builder
	b.WriteString("# Vellum controlled vocabulary.\n")
	b.WriteString("# tag name -> description shown to the LLM when choosing tags.\n")
	data, err := yaml.Marshal(fileFormat{Tags: v.Tags})
	if err != nil {
		return err
	}
	b.Write(data)
	return os.WriteFile(v.Path, []byte(b.String()), 0o644)
}

// Add inserts a tag (lowercased); returns false if it already exists.
func (v *Vocabulary) Add(name, description string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := v.Tags[key]; ok {
		return false
	}
	v.Tags[key] = description
	return true
}

// Remove deletes a tag; returns false if it wasn't there.
func (v *Vocabulary) Remove(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := v.Tags[key]; !ok {
		return false
	}
	delete(v.Tags, key)
	return true
}

// SortedKeys returns the vocabulary keys, sorted (for the schema enum).
func (v *Vocabulary) SortedKeys() []string {
	keys := make([]string, 0, len(v.Tags))
	for k := range v.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DescriptionsBlock renders "- name: description" lines for the LLM prompt.
func (v *Vocabulary) DescriptionsBlock() string {
	var b strings.Builder
	for _, k := range v.SortedKeys() {
		fmt.Fprintf(&b, "- %s: %s\n", k, v.Tags[k])
	}
	return b.String()
}
