// Package classify labels messages with the cassetti taxonomy: deterministic
// rules first (no tokens), then an OpenAI-compatible LLM for the rest.
package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Unsure is the explicit low-confidence lane.
const Unsure = "unsure"

// Method values stored with a classification.
const (
	MethodRule = "rule"
	MethodLLM  = "llm"
)

// Label is one cassetto. Title is the Italian name shown in the digest.
type Label struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Signals     []string `json:"signals"`
}

// Taxonomy is the versioned label set (taxonomy.json). Order keeps the
// file's label order for prompts and digest sections.
type Taxonomy struct {
	Version string
	Labels  map[string]Label
	Order   []string
}

// Has reports whether key is a known label.
func (t *Taxonomy) Has(key string) bool { _, ok := t.Labels[key]; return ok }

// Title returns the label's display name, or the key when untitled.
func (t *Taxonomy) Title(key string) string {
	if l, ok := t.Labels[key]; ok && l.Title != "" {
		return l.Title
	}
	return key
}

// LoadTaxonomy reads and validates a taxonomy file: unique non-empty keys,
// and the `unsure` lane must exist.
func LoadTaxonomy(path string) (*Taxonomy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: %w", err)
	}
	var file struct {
		Version string  `json:"version"`
		Labels  []Label `json:"labels"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("taxonomy: parse %s: %w", path, err)
	}
	t := &Taxonomy{Version: file.Version, Labels: map[string]Label{}}
	for _, l := range file.Labels {
		if l.Key == "" {
			return nil, errors.New("taxonomy: label with empty key")
		}
		if _, dup := t.Labels[l.Key]; dup {
			return nil, fmt.Errorf("taxonomy: duplicate label %q", l.Key)
		}
		t.Labels[l.Key] = l
		t.Order = append(t.Order, l.Key)
	}
	if !t.Has(Unsure) {
		return nil, fmt.Errorf("taxonomy: the %q label is required", Unsure)
	}
	if t.Version == "" {
		return nil, errors.New("taxonomy: version is required")
	}
	return t, nil
}
