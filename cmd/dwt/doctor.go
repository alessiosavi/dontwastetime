package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/mailbox"
)

const (
	redOldestPending = time.Hour
	unsureRerun      = 15 // percent; explore re-run trigger
)

// doctor prints one line per check and reports whether everything is
// non-red. WARN lines (budget reached, high unsure share) do not fail it.
func (a *app) doctor(ctx context.Context, out io.Writer) bool {
	ok := true
	line := func(level, format string, args ...any) {
		if level == "RED" {
			ok = false
		}
		fmt.Fprintf(out, "%-7s"+format+"\n", append([]any{"[" + level + "]"}, args...)...)
	}
	now := a.now()
	cfg := a.cfg

	enabled := 0
	for _, mb := range cfg.Mailboxes {
		if mb.Enabled {
			enabled++
		}
	}
	line("OK", "config: %d mailboxes (%d enabled), digest to %d recipients from %s, state %s",
		len(cfg.Mailboxes), enabled, len(cfg.DigestRecipients), cfg.DigestSender, cfg.StateDir)

	if err := cfg.RequireLLM(); err != nil {
		line("RED", "llm: %v", err)
	} else {
		line("OK", "llm: endpoint configured (model %s)", cfg.LLM.Model)
	}

	if a.c == nil {
		line("RED", "graph: no client (credential missing)")
	} else if err := a.c.CheckAuth(ctx); err != nil {
		line("RED", "graph: token acquisition failed: %v", err)
	} else {
		line("OK", "graph: token acquired")
		for _, mb := range cfg.Mailboxes {
			if !mb.Enabled {
				continue
			}
			if err := mailbox.Verify(ctx, a.c, mb.Address); err != nil {
				line("RED", "mailbox %s: %v", mb.Address, err)
			} else {
				line("OK", "mailbox %s readable (inbox + sentitems)", mb.Address)
			}
		}
	}

	pending, failed, oldest, err := a.s.QueueDepth(ctx, now)
	switch {
	case err != nil:
		line("RED", "queue: %v", err)
	case failed > 0 || time.Duration(oldest)*time.Second > redOldestPending:
		line("RED", "queue: %d pending (oldest pending %s), %d dead-letters", pending, minutes(oldest), failed)
	default:
		line("OK", "queue: %d pending (oldest pending %s), %d dead-letters", pending, minutes(oldest), failed)
	}

	in, outTok, err := a.s.UsageToday(ctx, config.Day(now))
	switch {
	case err != nil:
		line("RED", "llm budget: %v", err)
	case in+outTok >= cfg.LLM.DailyMaxTokens:
		line("WARN", "llm budget reached: %d / %d tokens today — classification paused until tomorrow", in+outTok, cfg.LLM.DailyMaxTokens)
	default:
		line("OK", "llm budget: %d / %d tokens today", in+outTok, cfg.LLM.DailyMaxTokens)
	}

	var total, unsure int
	_ = a.s.DB().QueryRowContext(ctx, `SELECT count(*), coalesce(sum(label = 'unsure'), 0) FROM classification
		WHERE classified_at >= ?`, now.AddDate(0, 0, -30).UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&total, &unsure)
	share := 0
	if total > 0 {
		share = unsure * 100 / total
	}
	level := "OK"
	if share > unsureRerun {
		level = "WARN"
	}
	line(level, "explore: re-run monthly for the first 3 months, or when unsure > %d%% (last 30 days: %d%% of %d)", unsureRerun, share, total)

	var size int64
	_ = filepath.WalkDir(cfg.StateDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	line("OK", "disk: state dir uses %.1f MB", float64(size)/(1<<20))
	return ok
}

func minutes(sec int) string {
	if sec < 120*60 {
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh", sec/3600)
}
