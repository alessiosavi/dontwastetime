package digest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func TestBuild_SinceIncludesMailOlderThanLateCap(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	const subject = "Mail ricevuta venti giorni fa"
	id := f.add(t, "business@example.com", "vendor@example.test", subject, "", 20*24*time.Hour)
	if err := f.s.SaveClassification(ctx, store.Classification{MessageID: id, Label: "business.dev", Method: "llm"}, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, err := BuildWithOptions(ctx, f.s, f.cfg, f.tx, at, Options{Since: at.Add(-25 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for format, body := range map[string]string{"markdown": d.Markdown, "html": d.HTML} {
		if !strings.Contains(body, subject) || !strings.Contains(body, "Periodo: dal 16 ottobre 2026") {
			t.Errorf("%s lacks older mail or period", format)
		}
	}
	d, err = Build(ctx, f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Markdown, subject) || strings.Contains(d.HTML, subject) {
		t.Fatal("default digest bypassed lateCap")
	}
}

func TestBuild_ZeroSinceUnchanged(t *testing.T) {
	f := seed(t)
	d, err := BuildWithOptions(context.Background(), f.s, f.cfg, f.tx, at, Options{})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "testdata/golden.md", d.Markdown)
}

func TestBuild_SinceBoundariesAndVisibility(t *testing.T) {
	since := at.Add(-25 * 24 * time.Hour)
	for _, tc := range []struct {
		name                 string
		received, classified time.Time
		label, mailbox       string
		outbound, want       bool
	}{
		{name: "received at since", received: since, classified: at.Add(-time.Hour), want: true},
		{name: "classified before since", received: since, classified: since.Add(-time.Hour), want: true},
		{name: "received before since", received: since.Add(-time.Millisecond), classified: at.Add(-time.Hour)},
		{name: "received at digest", received: at, classified: at.Add(-time.Hour)},
		{name: "received after digest", received: at.Add(time.Hour), classified: at.Add(-time.Hour)},
		{name: "classified at digest", received: since, classified: at},
		{name: "classified after digest", received: since, classified: at.Add(time.Hour)},
		{name: "unclassified", received: since},
		{name: "outbound", received: since, classified: since, outbound: true},
		{name: "personal", received: since, classified: since, label: "personal"},
		{name: "digest off", received: since, classified: since, mailbox: "employee@example.com"},
		{name: "disabled", received: since, classified: since, mailbox: "disabled@example.com"},
		{name: "unsure", received: since, classified: since, label: "unsure", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			ctx := context.Background()
			box := tc.mailbox
			if box == "" {
				box = "business@example.com"
			}
			const subject = "Messaggio di prova nel periodo"
			id := f.add(t, box, "vendor@example.test", subject, "", at.Sub(tc.received))
			if !tc.classified.IsZero() {
				label := tc.label
				if label == "" {
					label = "business.dev"
				}
				if err := f.s.SaveClassification(ctx, store.Classification{MessageID: id, Label: label, Method: "llm"}, tc.classified); err != nil {
					t.Fatal(err)
				}
			}
			if tc.outbound {
				if _, err := f.s.DB().Exec(`UPDATE message SET direction='out' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			d, err := BuildWithOptions(ctx, f.s, f.cfg, f.tx, at, Options{Since: since})
			if err != nil {
				t.Fatal(err)
			}
			for format, body := range map[string]string{"markdown": d.Markdown, "html": d.HTML} {
				if got := strings.Contains(body, subject); got != tc.want {
					t.Errorf("%s mail present=%v, want %v", format, got, tc.want)
				}
			}
		})
	}
}

func TestBuild_SinceLiveSendKeepsDailyReservation(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	var sends int
	_, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		sends++
		w.WriteHeader(http.StatusAccepted)
	})
	d, err := BuildWithOptions(ctx, f.s, f.cfg, f.tx, at, Options{Since: at.Add(-25 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(ctx, f.s, c, f.cfg, d, false, at); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.s.DigestSent(ctx, "2026-11-10"); err != nil || !sent {
		t.Fatalf("since live digest was not recorded sent: sent=%v err=%v", sent, err)
	}
	d, err = Build(ctx, f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(ctx, f.s, c, f.cfg, d, false, at); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("same-day default digest sent again: sends=%d", sends)
	}
}
