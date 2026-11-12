package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func sampleNote() Note {
	return Note{To: "founder@example.com", Subject: "Riepilogo quotidiano", Text: "ciao"}
}

func assertNoteAction(t *testing.T, s *store.Store, action string) {
	t.Helper()
	var actor, gotAction, target, details, when string
	if err := s.DB().QueryRow(`SELECT actor, action, target, details_json, at FROM action_log`).
		Scan(&actor, &gotAction, &target, &details, &when); err != nil {
		t.Fatal(err)
	}
	if actor != "note" || gotAction != action || target != "note:b133a0c0e9be" {
		t.Errorf("audit actor=%q action=%q target=%q", actor, gotAction, target)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(details), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["subject"] != "Riepilogo quotidiano" {
		t.Errorf("audit details = %s", details)
	}
	if when != "2026-11-10T07:00:00.000Z" {
		t.Errorf("audit time = %q, want injected time %s", when, at)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM action_log`).Scan(&count); err != nil || count != 1 {
		t.Errorf("audit count=%d error=%v, want exactly one row", count, err)
	}
}

func TestNoteHTMLEscapesAndKeepsLines(t *testing.T) {
	body, err := NoteHTML("a<b>\nriga 2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`lang="it"`, `style="white-space:pre-wrap"`, "a&lt;b&gt;\nriga 2",
		`font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;font-size:14px`} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "a<b>") {
		t.Error("note text became HTML markup")
	}
}

func TestSendNoteRejectsRecipientOutsideDigestRecipients(t *testing.T) {
	f := newFx(t)
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	n := sampleNote()
	n.To = "other@example.com"
	err := SendNote(context.Background(), f.s, c, f.cfg, n, false, at)
	if !errors.Is(err, ErrNoteNotSent) {
		t.Fatalf("outside recipient error=%v, want ErrNoteNotSent", err)
	}
	if got := srv.APICalls.Load(); got != 0 {
		t.Fatalf("outside recipient reached Graph %d times", got)
	}
}

func TestSendNoteSendsOnceFromDigestSender(t *testing.T) {
	f := newFx(t)
	n := sampleNote()
	n.To = "FOUNDER@example.com" // membership is case-insensitive; preserve the address on the wire
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.0/users/admin@example.com/sendMail" {
			t.Errorf("unexpected send request %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Message struct {
				Subject string
				Body    struct{ ContentType, Content string }
				To      []struct{ EmailAddress struct{ Address string } } `json:"toRecipients"`
			}
			SaveToSentItems *bool
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.SaveToSentItems == nil || *payload.SaveToSentItems {
			t.Error("send must explicitly set saveToSentItems=false")
		}
		if len(payload.Message.To) != 1 || payload.Message.To[0].EmailAddress.Address != n.To {
			t.Errorf("recipients=%+v, want only %s", payload.Message.To, n.To)
		}
		if payload.Message.Subject != n.Subject || payload.Message.Body.ContentType != "HTML" ||
			!strings.Contains(payload.Message.Body.Content, `<div style="white-space:pre-wrap">ciao</div>`) {
			t.Errorf("unexpected message %+v", payload.Message)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := SendNote(context.Background(), f.s, c, f.cfg, n, false, at); err != nil {
		t.Fatal(err)
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("sendMail requests=%d, want 1", got)
	}
	assertNoteAction(t, f.s, "note_sent")
}

func TestSendNoteDryRunSendsNothing(t *testing.T) {
	f := newFx(t)
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	if err := SendNote(context.Background(), f.s, c, f.cfg, sampleNote(), true, at); err != nil {
		t.Fatal(err)
	}
	if got := srv.APICalls.Load(); got != 0 {
		t.Fatalf("dry run reached Graph %d times", got)
	}
	assertNoteAction(t, f.s, "note_dry_run")
	// A dry run remains a successful no-send outcome even if auditing fails.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SendNote(context.Background(), f.s, c, f.cfg, sampleNote(), true, at); err != nil {
		t.Fatalf("dry-run audit failure must not change its no-send success: %v", err)
	}
	if got := srv.APICalls.Load(); got != 0 {
		t.Fatalf("dry-run audit failure reached Graph %d times", got)
	}
}

func TestSendNoteUnknownDeliveryLogsAndWraps(t *testing.T) {
	f := newFx(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // Graph has received the mail, but never answers
		<-r.Context().Done()
	})
	err := SendNote(ctx, f.s, c, f.cfg, sampleNote(), false, at)
	if !errors.Is(err, graph.ErrDeliveryUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want delivery unknown wrapping the timeout", err)
	}
	if errors.Is(err, ErrNoteNotSent) {
		t.Fatal("unknown delivery must never be marked safe to retry")
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("unknown delivery sent %d times, want 1", got)
	}
	assertNoteAction(t, f.s, "note_send_unknown") // must work after ctx expired
}

func TestSendNoteServerErrorIsUnknown(t *testing.T) {
	f := newFx(t)
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	err := SendNote(context.Background(), f.s, c, f.cfg, sampleNote(), false, at)
	var cause *graph.HTTPError
	if !errors.Is(err, graph.ErrDeliveryUnknown) || errors.Is(err, ErrNoteNotSent) ||
		!errors.As(err, &cause) || cause.Status != http.StatusBadGateway {
		t.Fatalf("error=%v, want delivery unknown wrapping HTTP 502 and unsafe to retry", err)
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("server error sent %d times, want 1", got)
	}
	assertNoteAction(t, f.s, "note_send_unknown")
}

func TestSendNoteValidatesInput(t *testing.T) {
	f := newFx(t)
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*Note, *config.Config)
	}{
		{"invalid recipient", func(n *Note, cfg *config.Config) { n.To = "bad"; cfg.DigestRecipients = []string{"bad"} }},
		{"no recipients", func(n *Note, cfg *config.Config) { cfg.DigestRecipients = nil }},
		{"empty sender", func(n *Note, cfg *config.Config) { cfg.DigestSender = "" }},
		{"invalid sender", func(n *Note, cfg *config.Config) { cfg.DigestSender = "bad" }},
		{"banned sender", func(n *Note, cfg *config.Config) { cfg.DigestSender = "GM@example.com" }},
		{"empty subject", func(n *Note, cfg *config.Config) { n.Subject = "" }},
		{"newline subject", func(n *Note, cfg *config.Config) { n.Subject = "a\nb" }},
		{"carriage return subject", func(n *Note, cfg *config.Config) { n.Subject = "a\rb" }},
		{"long subject", func(n *Note, cfg *config.Config) { n.Subject = strings.Repeat("è", 201) }},
		{"empty text", func(n *Note, cfg *config.Config) { n.Text = "" }},
		{"long text", func(n *Note, cfg *config.Config) { n.Text = strings.Repeat("x", 64*1024+1) }},
		{"invalid UTF8", func(n *Note, cfg *config.Config) { n.Text = string([]byte{0xff}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				n, cfg := sampleNote(), *f.cfg
				tc.mutate(&n, &cfg)
				err := SendNote(context.Background(), f.s, c, &cfg, n, dryRun, at)
				if !errors.Is(err, ErrNoteNotSent) {
					t.Errorf("dry-run=%v error=%v, want ErrNoteNotSent", dryRun, err)
				}
			}
		})
	}
	if got := srv.APICalls.Load(); got != 0 {
		t.Fatalf("validation failures reached Graph %d times", got)
	}
	var count int
	if err := f.s.DB().QueryRow(`SELECT count(*) FROM action_log`).Scan(&count); err != nil || count != 0 {
		t.Errorf("invalid notes wrote %d audit rows; error=%v", count, err)
	}
	t.Run("inclusive limits", func(t *testing.T) {
		n := sampleNote()
		n.Subject, n.Text = strings.Repeat("è", 200), strings.Repeat("è", 32*1024)
		if err := SendNote(context.Background(), f.s, nil, f.cfg, n, true, at); err != nil {
			t.Fatalf("200-character subject and 64-KiB text should be accepted: %v", err)
		}
	})
}

func TestSendNoteRejected4xxIsNotSent(t *testing.T) {
	f := newFx(t)
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	err := SendNote(context.Background(), f.s, c, f.cfg, sampleNote(), false, at)
	var cause *graph.HTTPError
	if !errors.Is(err, ErrNoteNotSent) || errors.Is(err, graph.ErrDeliveryUnknown) ||
		!errors.As(err, &cause) || cause.Status != http.StatusBadRequest {
		t.Fatalf("rejection error=%v, want sentinel and underlying HTTP 400", err)
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("Graph rejection sent %d times, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = SendNote(ctx, f.s, c, f.cfg, sampleNote(), false, at)
	if !errors.Is(err, ErrNoteNotSent) || !errors.Is(err, context.Canceled) || errors.Is(err, graph.ErrDeliveryUnknown) {
		t.Fatalf("pre-send cancellation error=%v, want safe to retry and context.Canceled", err)
	}
}

func TestSendNoteAuditFailureAfterSendIsSuccess(t *testing.T) {
	f := newFx(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	srv, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		if err := f.s.Close(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := SendNote(context.Background(), f.s, c, f.cfg, sampleNote(), false, at); err != nil {
		t.Fatalf("accepted mail must stay successful after audit failure: %v", err)
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("audit failure sent %d times, want 1", got)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "audit") {
		t.Errorf("missing audit warning: %s", logs.String())
	}
}
