package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/digest"
)

type DigestText struct {
	Markdown string `json:"markdown"`
}

type DigestResult struct {
	// Promote Markdown for Go callers while keeping all mail text under
	// untrusted in the serialized service response.
	DigestText `json:"untrusted"`
	Built      bool `json:"built"`
	Trimmed    bool `json:"trimmed"`
}

// Digest builds today's preview without sweeping, writing or sending anything.
// Earlier dates are read from saved files, never rebuilt from today's state.
// A non-empty since is a received-time lower bound (YYYY-MM-DD, midnight Rome)
// for today's preview only.
func (s *Service) Digest(ctx context.Context, date, since string) (DigestResult, error) {
	at := s.now()
	today := config.Day(at)
	if date != "" {
		if _, err := time.Parse(time.DateOnly, date); err != nil || date > today {
			return DigestResult{}, invalid("data non valida: usare YYYY-MM-DD fino a oggi")
		}
	}
	var opts digest.Options
	if since != "" {
		if date != "" && date != today {
			return DigestResult{}, invalid("since non è valido con una data passata")
		}
		from, err := time.ParseInLocation(time.DateOnly, since, config.Rome)
		if err != nil {
			return DigestResult{}, invalid("since non valido: usare YYYY-MM-DD")
		}
		if !from.Before(at) {
			return DigestResult{}, invalid("since deve precedere l'istante del digest")
		}
		opts.Since = from
	}
	if date == "" || date == today {
		draft, err := digest.BuildWithOptions(ctx, s.store, s.cfg, s.taxonomy, at, opts)
		if err != nil {
			return DigestResult{}, storeError(err)
		}
		return digestResult(draft.Markdown, true), nil
	}
	f, err := os.Open(filepath.Join(s.cfg.StateDir, "digests", date+".md"))
	if errors.Is(err, os.ErrNotExist) {
		return DigestResult{}, ErrNotFound
	}
	if err != nil {
		return DigestResult{}, err
	}
	defer f.Close()
	// At most four UTF-8 bytes per character, plus enough to detect clipping.
	raw, err := io.ReadAll(io.LimitReader(f, 4*(pageChars+1)))
	if err != nil {
		return DigestResult{}, err
	}
	return digestResult(string(raw), false), nil
}

func digestResult(markdown string, built bool) DigestResult {
	markdown, trimmed := clip(markdown, pageChars)
	return DigestResult{DigestText: DigestText{Markdown: markdown}, Built: built, Trimmed: trimmed}
}
