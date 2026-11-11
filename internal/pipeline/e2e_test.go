package pipeline_test

import (
	"context"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/classify/llmtest"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/digest"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/mailbox"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

var update = flag.Bool("update", false, "rewrite golden files")

// digestAt is Tuesday 10 Nov 2026, 08:00 Europe/Rome.
var digestAt = time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)

type fixtureMsg struct {
	Mailbox, Folder, ID, InternetMessageID, ConversationID string
	Subject, From, FromName, Body                          string
	To                                                     []string
	ReceivedAt                                             time.Time
	Headers                                                []graph.Header
	Attachments                                            []struct {
		Name, ContentType string
		Size              int64
	}
	LLM llmtest.Answer
}

func loadFixture(t *testing.T) []fixtureMsg {
	t.Helper()
	raw, err := os.ReadFile("testdata/mailbox.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixtureMsg
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPipeline_FixtureToDigest(t *testing.T) {
	ctx := context.Background()
	fixture := loadFixture(t)
	if len(fixture) != 50 {
		t.Fatalf("fixture has %d messages, want 50", len(fixture))
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := classify.LoadTaxonomy("../../taxonomy.json")
	if err != nil {
		t.Fatal(err)
	}

	// Fake Graph holds the mailbox fixture; fake LLM answers by subject.
	mail, _, client := graphtest.NewMail(t)
	bySubject := map[string]llmtest.Answer{}
	for _, f := range fixture {
		m := graph.Message{ID: f.ID, InternetMessageID: f.InternetMessageID, ConversationID: f.ConversationID,
			Mailbox: f.Mailbox, Folder: f.Folder, Subject: f.Subject, From: f.From, FromName: f.FromName,
			To: f.To, BodyText: f.Body, ReceivedAt: f.ReceivedAt, SentAt: f.ReceivedAt, HasAttachments: len(f.Attachments) > 0}
		mail.Add(m)
		mail.Headers[f.ID] = f.Headers
		for i, a := range f.Attachments {
			mail.Attachments[f.ID] = append(mail.Attachments[f.ID], graph.Attachment{ID: f.ID + "-a" + string(rune('0'+i)),
				Name: a.Name, ContentType: a.ContentType, Size: a.Size})
		}
		bySubject[f.Subject] = f.LLM
	}
	llm := llmtest.New(t, func(r llmtest.Request) llmtest.Reply {
		var as []llmtest.Answer
		for _, it := range r.Items {
			a := bySubject[it.Subject]
			a.ID = it.ID
			as = append(as, a)
		}
		return llmtest.Reply{Content: llmtest.Results(as...), In: 50 * len(as), Out: 10 * len(as)}
	})

	cfg := &config.Config{
		Mailboxes: []config.MailboxConfig{
			{Address: "business@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "tech@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "employee.example@example.com", Enabled: true, Digest: false, PollSeconds: 180},
		},
		DigestRecipients: []string{"founder@example.com"}, AlertRecipients: []string{"founder@example.com"},
		DigestSender: "admin@example.com", TopClients: []string{"bigbrand.example"}, StateDir: t.TempDir(),
		PollSeconds: 180, PollOverlapMinutes: 30, InitialDays: 90, Workers: 1,
		LLM: config.LLMConfig{URL: llm.URL, Model: "fake", APIKey: "k", DailyMaxTokens: 1_000_000, ConfidenceThreshold: 0.7},
	}
	// Mail is processed a minute before the 08:00 digest, as in a real run.
	processAt := digestAt.Add(-time.Minute)
	now := func() time.Time { return processAt }

	// sync → classify → thread_refresh, all at the pinned instant.
	if err := mailbox.SyncFromConfig(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	var pollers []*pipeline.Poller
	for _, mb := range cfg.Mailboxes {
		pollers = append(pollers, pipeline.NewPoller(client, s, mb, cfg, nil))
	}
	if err := pipeline.SyncAll(ctx, pollers, processAt); err != nil {
		t.Fatal(err)
	}
	var alerted []string
	alert := func(_ context.Context, rows []store.ThreadRow) error {
		for _, r := range rows {
			alerted = append(alerted, r.ThreadKey)
		}
		return nil
	}
	handlers := map[string]pipeline.Handler{
		pipeline.KindClassify:      pipeline.ClassifyHandler(s, client, classify.NewLLM(cfg.LLM, s, now), tx, cfg, now),
		pipeline.KindThreadRefresh: threads.RefreshHandler(s, cfg, alert, now),
	}
	for {
		j, err := s.Claim(ctx, nil, processAt)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		if err := handlers[j.Kind](ctx, j); err != nil {
			t.Fatalf("job %d (%s): %v", j.ID, j.Kind, err)
		}
		if err := s.Complete(ctx, j.ID, processAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := threads.Sweep(ctx, s, cfg, alert, digestAt); err != nil {
		t.Fatal(err)
	}

	if n, _ := s.UnclassifiedCount(ctx); n != 0 {
		t.Fatalf("%d messages left unclassified", n)
	}
	slices.Sort(alerted)
	if want := []string{"c:conv-c1", "c:conv-c7"}; !slices.Equal(alerted, want) {
		t.Errorf("P0 alerts = %v, want %v (employee box and answered threads never alert)", alerted, want)
	}

	d, err := digest.Build(ctx, s, cfg, tx, digestAt)
	if err != nil {
		t.Fatal(err)
	}
	const goldenPath = "testdata/golden-digest.md"
	if *update {
		if err := os.WriteFile(goldenPath, []byte(d.Markdown), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (run with -update and review)", err)
	}
	if d.Markdown != string(want) {
		t.Fatalf("digest differs from %s (run with -update and review the diff):\n%s", goldenPath, d.Markdown)
	}
}

// TestNoWallClockOutsideCmd enforces the time discipline: only cmd/dwt reads
// the wall clock; everything under internal/ takes `now` from its caller.
func TestNoWallClockOutsideCmd(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, call := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(string(src), call) {
				t.Errorf("%s calls %s — take now from the caller", path, call)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
