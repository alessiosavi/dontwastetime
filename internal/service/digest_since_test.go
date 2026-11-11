package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
)

func TestDigest_SinceIncludesOlderMailWithoutSideEffects(t *testing.T) {
	f := setup(t)
	f.cfg.StateDir = t.TempDir()
	f.cfg.Mailboxes[0].Digest = true
	ctx := context.Background()
	// Midnight Rome on November 1 is October 31, 23:00 UTC.
	f.mail(t, testBox, "older", "body", "clienti", time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	before := digestSnapshot(t, f)
	for _, date := range []string{"", "2026-11-11"} {
		r, err := f.svc.Digest(ctx, date, "2026-11-01")
		if err != nil {
			t.Fatal(err)
		}
		if !r.Built || r.Trimmed || !strings.Contains(r.Markdown, "sentinel-subject") || !strings.Contains(r.Markdown, "Periodo: dal 1 novembre 2026") {
			t.Errorf("since preview lacks older mail or period: %+v", r)
		}
	}
	if after := digestSnapshot(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("since preview changed stored state or files")
	}
}

func TestDigest_SinceValidation(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		date, since, reason string
	}{
		{"2026-11-10", "2026-11-01", "since non è valido con una data passata"},
		{"", "/private/secret", "since non valido: usare YYYY-MM-DD"},
		{"", "2026-02-30", "since non valido: usare YYYY-MM-DD"},
		{"", "2026-11-12", "since deve precedere l'istante del digest"},
	} {
		_, err := f.svc.Digest(context.Background(), tc.date, tc.since)
		if !errors.Is(err, ErrInvalid) || err.Error() != ErrInvalid.Error()+": "+tc.reason {
			t.Errorf("date=%q since=%q error=%v, want ErrInvalid with %q", tc.date, tc.since, err, tc.reason)
		}
	}
	// Equality must fail too, even when today is only just starting in Rome.
	f.at = time.Date(2026, 11, 11, 0, 0, 0, 0, config.Rome)
	if _, err := f.svc.Digest(context.Background(), "", "2026-11-11"); !errors.Is(err, ErrInvalid) {
		t.Errorf("since equal to digest instant: %v, want ErrInvalid", err)
	}
}
