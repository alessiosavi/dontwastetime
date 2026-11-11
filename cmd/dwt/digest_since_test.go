package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func TestCLI_DigestSinceInvalidIsUsageError(t *testing.T) {
	path := mcpConfig(t)
	for _, since := range []string{"2026-11-11", "2026-02-30", "not-a-date"} {
		t.Run(since, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run([]string{"digest", "--config", path, "--date", "2026-11-10", "--dry-run", "--since", since}, &out, &errb)
			if code != 2 || !strings.Contains(errb.String(), "--since must be YYYY-MM-DD before the digest instant") {
				t.Fatalf("exit=%d stderr=%q; want usage error explaining --since", code, errb.String())
			}
		})
	}
}

func TestCLI_DigestSinceDryRunSavesUnsent(t *testing.T) {
	path := mcpConfig(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Mailboxes[0].Digest = true
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := openApp(paths{config: path}, need{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	ctx := context.Background()
	// The lower bound is midnight Rome, still October 31 in UTC.
	received := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	id, _, err := a.s.UpsertMessage(ctx, graph.Message{ID: "old", InternetMessageID: "<old@example.test>", Mailbox: cfg.Mailboxes[0].Address,
		Folder: store.FolderInbox, Subject: "Prima mail del mese", ReceivedAt: received}, "old", received)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.s.SaveClassification(ctx, store.Classification{MessageID: id, Label: "business.dev", Method: "llm"}, received); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"digest", "--config", path, "--date", "2026-11-10", "--dry-run", "--since", "2026-11-01"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, ext := range []string{"md", "html"} {
		raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "digests", "2026-11-10."+ext))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Prima mail del mese", "Periodo: dal 1 novembre 2026"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("saved %s lacks %q", ext, want)
			}
		}
	}
	var sent sql.NullString
	if err := a.s.DB().QueryRowContext(ctx, `SELECT sent_at FROM digest WHERE date='2026-11-10'`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent.Valid {
		t.Fatal("dry-run marked digest as sent")
	}
}
