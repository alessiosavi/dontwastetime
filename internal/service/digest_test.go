package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/digest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func digestSnapshot(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"digest", "thread", "action_log", "queue"} {
		rows, err := f.db.DB().Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var data [][]any
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			data = append(data, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = string(raw)
	}
	err := filepath.WalkDir(filepath.Join(f.cfg.StateDir, "digests"), func(path string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDigest_TodayHasNoSideEffects(t *testing.T) {
	f := setup(t)
	f.cfg.StateDir = t.TempDir()
	f.cfg.Mailboxes[0].Digest = true
	ctx := context.Background()
	f.mail(t, testBox, "client", "mail", "clienti", f.at.Add(-time.Hour))
	// An expired snooze would be changed by a sweep; the preview must not sweep.
	id := f.obligation(t, "snoozed", "P2", f.at.Add(-25*time.Hour), f.at.Add(-time.Hour), false)
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE thread SET ack_kind='snoozed', snoozed_until=? WHERE thread_key='snoozed'`,
		f.at.Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Enqueue(ctx, store.KindThreadRefresh, id, f.at); err != nil {
		t.Fatal(err)
	}
	before := digestSnapshot(t, f)
	for _, date := range []string{"", "2026-11-11"} {
		r, err := f.svc.Digest(ctx, date)
		if err != nil || !r.Built || !strings.Contains(r.Markdown, "Riepilogo email") {
			t.Fatalf("digest=%+v err=%v", r, err)
		}
		if after := digestSnapshot(t, f); !reflect.DeepEqual(before, after) {
			t.Fatalf("digest changed state: before=%v after=%v", before, after)
		}
		if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "digests")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("digest preview created a directory: %v", err)
		}
		var fields map[string]json.RawMessage
		raw, _ := json.Marshal(r)
		if err := json.Unmarshal(raw, &fields); err != nil || fields["untrusted"] == nil || fields["markdown"] != nil {
			t.Fatalf("mail-derived Markdown must be under untrusted: %s err=%v", raw, err)
		}
	}
}

func TestDigest_PastDateReadsSavedFileOrNotFound(t *testing.T) {
	f := setup(t)
	f.cfg.StateDir = t.TempDir()
	ctx := context.Background()
	if _, err := f.svc.Digest(ctx, "2026-11-10"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing digest=%v", err)
	}
	dir := filepath.Join(f.cfg.StateDir, "digests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const saved = "# Riepilogo salvato\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-11-10.md"), []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	before := digestSnapshot(t, f)
	r, err := f.svc.Digest(ctx, "2026-11-10")
	if err != nil || r.Built || r.Markdown != saved {
		t.Fatalf("saved digest=%+v err=%v", r, err)
	}
	for _, date := range []string{"../2026-11-10", "2026-02-30", "2026-11-12", "2026-11-10T00:00:00Z"} {
		if _, err := f.svc.Digest(ctx, date); !errors.Is(err, ErrInvalid) {
			t.Errorf("date=%q error=%v, want ErrInvalid", date, err)
		}
	}
	if after := digestSnapshot(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("reading a saved digest changed state")
	}
}

func TestDigest_TodayMatchesDeliveryBuilder(t *testing.T) {
	f := setup(t)
	f.cfg.StateDir = t.TempDir()
	f.cfg.Mailboxes[0].Digest = true
	ctx := context.Background()
	for _, seed := range []struct{ box, key, label string }{
		{testBox, "personal", "personal"}, {testBox, "pending", ""},
		{"disabled@example.test", "disabled", "clienti"}, {"removed@example.test", "removed", "clienti"},
		{testBox, "mixed", "clienti"}, {"disabled@example.test", "mixed", "clienti"},
	} {
		id := f.mail(t, seed.box, seed.key, "body", seed.label, f.at.Add(-time.Hour))
		if _, err := f.db.DB().ExecContext(ctx, `UPDATE message SET subject=? WHERE id=?`, seed.box+"/"+seed.key, id); err != nil {
			t.Fatal(err)
		}
	}
	draft, err := digest.Build(ctx, f.db, f.cfg, f.svc.taxonomy, f.at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft.Markdown, testBox+"/mixed") || len([]rune(draft.Markdown)) > pageChars {
		t.Fatal("fixture must produce delivery mail below the Markdown cap")
	}
	for _, date := range []string{"", draft.Day} {
		r, err := f.svc.Digest(ctx, date)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Built || r.Trimmed || r.Markdown != draft.Markdown {
			t.Errorf("date=%q digest=%+v, want delivery Markdown %q", date, r, draft.Markdown)
		}
	}
}

func TestDigest_BoundsMarkdown(t *testing.T) {
	f := setup(t)
	f.cfg.StateDir = t.TempDir()
	f.cfg.Mailboxes[0].Digest = true
	dir := filepath.Join(f.cfg.StateDir, "digests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-11-10.md"), []byte(strings.Repeat("è", pageChars+1)), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Digest(context.Background(), "2026-11-10")
	if err != nil || len([]rune(r.Markdown)) != pageChars || !r.Trimmed {
		t.Fatalf("saved digest characters=%d err=%v, want at most %d", len([]rune(r.Markdown)), err, pageChars)
	}
	// A large subject must not make an on-demand response unbounded either.
	id := f.mail(t, testBox, "large", "body", "clienti", f.at.Add(-time.Hour))
	if _, err := f.db.DB().Exec(`UPDATE message SET subject=? WHERE id=?`, strings.Repeat("è", pageChars+1), id); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.Digest(context.Background(), "")
	if err != nil || len([]rune(r.Markdown)) != pageChars || !r.Trimmed {
		t.Fatalf("today's digest characters=%d err=%v, want at most %d", len([]rune(r.Markdown)), err, pageChars)
	}
}
