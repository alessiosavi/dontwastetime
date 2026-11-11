package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/mailbox"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// app is one command's wiring. c and llm are nil when the command does not
// need them (e.g. a dry-run digest needs neither credential).
type app struct {
	cfg *config.Config
	s   *store.Store
	c   *graph.Client
	llm *classify.LLM
	tx  *classify.Taxonomy
	now func() time.Time
}

// need selects which fail-closed credential checks a command runs.
type need struct {
	graph, llm bool
	model      string // optional per-command model, applied before credential checks
}

// paths are the common flags every command accepts.
type paths struct{ config, stateDir, env, taxonomy string }

// openApp loads env + config + taxonomy, opens the store, and builds the
// clients the command needs. Missing credentials refuse to start.
func openApp(p paths, n need) (*app, error) {
	envPath := p.env
	if envPath == "" {
		envPath = filepath.Join(filepath.Dir(p.config), ".env")
	}
	if err := config.LoadEnvFile(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cfg, err := config.LoadConfig(p.config)
	if err != nil {
		return nil, err
	}
	if p.stateDir != "" {
		cfg.StateDir = p.stateDir
	}
	if n.model != "" {
		cfg.LLM.Model = n.model
	}
	if n.graph {
		if err := cfg.RequireGraph(); err != nil {
			return nil, err
		}
	}
	if n.llm {
		if err := cfg.RequireLLM(); err != nil {
			return nil, err
		}
	}
	taxPath := p.taxonomy
	if taxPath == "" {
		taxPath = filepath.Join(filepath.Dir(p.config), "taxonomy.json")
	}
	tx, err := classify.LoadTaxonomy(taxPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	s, err := store.Open(filepath.Join(cfg.StateDir, "dwt.db"))
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, s: s, tx: tx, now: time.Now}
	if n.graph {
		a.c = graph.New(cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret, graph.WithClock(time.Now))
	}
	if n.llm {
		a.llm = classify.NewLLM(cfg.LLM, s, a.now)
	}
	return a, nil
}

func (a *app) close() { a.s.Close() }

// boot is serve's fail-closed start: materialize the config, verify every
// enabled mailbox (refuse to start when none is readable), recover jobs a
// crash left running, and re-enqueue every unclassified message.
func (a *app) boot(ctx context.Context) error {
	if err := mailbox.SyncFromConfig(ctx, a.s, a.cfg); err != nil {
		return err
	}
	states := mailbox.VerifyAll(ctx, a.c, a.s, a.now())
	ok := 0
	for _, st := range states {
		if st.OK {
			ok++
		} else {
			logger().Warn("mailbox degraded at boot", "mailbox", st.Address, "reason", st.Reason)
		}
	}
	if ok == 0 {
		return fmt.Errorf("no mailbox is readable (%d checked): refusing to start", len(states))
	}
	if n, err := a.s.ResetRunning(ctx, a.now()); err != nil {
		return err
	} else if n > 0 {
		logger().Info("recovered jobs left running by a previous process", "jobs", n)
	}
	if n, err := a.s.ClearClassifyLeases(ctx); err != nil {
		return err
	} else if n > 0 {
		logger().Info("cleared classify leases left by a previous process", "messages", n)
	}
	return a.sweepUnclassified(ctx)
}

// sweepUnclassified enqueues a classify job for every unclassified message
// (duplicates are harmless: the handler no-ops on classified messages).
func (a *app) sweepUnclassified(ctx context.Context) error {
	rows, err := a.s.MessagesToClassify(ctx, 1_000_000)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := a.s.Enqueue(ctx, pipeline.KindClassify, r.ID, a.now()); err != nil {
			return err
		}
	}
	if len(rows) > 0 {
		logger().Info("boot sweep enqueued unclassified messages", "messages", len(rows))
	}
	return nil
}

// nextDigestRun is the next working day (Mon–Fri) at 08:00 Europe/Rome
// strictly after now.
func nextDigestRun(now time.Time) time.Time {
	d := now.In(config.Rome)
	run := time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, config.Rome)
	if !run.After(d) {
		run = run.AddDate(0, 0, 1)
	}
	for run.Weekday() == time.Saturday || run.Weekday() == time.Sunday {
		run = run.AddDate(0, 0, 1)
	}
	return run
}

// mergeMailboxes adds discovered addresses missing from the config file
// (enabled, digest:false — classified for coverage, out of the digest) and
// leaves every existing entry and field untouched. A .bak copy is kept.
func mergeMailboxes(path string, found []string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var boxes []map[string]any
	if len(doc["mailboxes"]) > 0 {
		if err := json.Unmarshal(doc["mailboxes"], &boxes); err != nil {
			return nil, fmt.Errorf("%s: mailboxes: %w", path, err)
		}
	}
	have := map[string]bool{}
	for _, b := range boxes {
		if a, ok := b["address"].(string); ok {
			have[strings.ToLower(a)] = true
		}
	}
	var added []string
	for _, f := range found {
		if f = strings.ToLower(f); !have[f] {
			boxes = append(boxes, map[string]any{"address": f, "enabled": true, "digest": false})
			have[f] = true
			added = append(added, f)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	if doc["mailboxes"], err = json.Marshal(boxes); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		return nil, err
	}
	return added, os.WriteFile(path, append(out, '\n'), 0o600)
}

// muteThread accepts a message id ("57" or "#57", as printed in the digest)
// or a thread key.
func (a *app) muteThread(ctx context.Context, id string, out io.Writer) error {
	key := id
	if n, err := strconv.ParseInt(strings.TrimPrefix(id, "#"), 10, 64); err == nil {
		m, err := a.s.Message(ctx, n)
		if err != nil {
			return err
		}
		key = m.ThreadKey
	}
	if err := a.s.MuteThread(ctx, key, "cli", a.now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "muted thread %s\n", key)
	return nil
}

// correct relabels one message (id as printed in the digest).
func (a *app) correct(ctx context.Context, id, label string, out io.Writer) error {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "#"), 10, 64)
	if err != nil {
		return fmt.Errorf("correct: message id %q is not a number", id)
	}
	if !a.tx.Has(label) {
		return fmt.Errorf("correct: unknown label %q (see taxonomy.json)", label)
	}
	old, err := a.s.AddCorrection(ctx, n, label, "cli", a.now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "message %d: %s → %s (used as a few-shot example from now on)\n", n, orDash(old), label)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
