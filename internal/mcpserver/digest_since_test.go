package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServer_DigestSinceIncludesOlderMail(t *testing.T) {
	f := setup(t)
	id := f.mail(t, enabledBox, "older-mail", "clienti")
	if _, err := f.db.DB().Exec(`UPDATE message SET received_at=?, first_classified_at=? WHERE id=?`,
		fixedNow.Add(-20*24*time.Hour).Format("2006-01-02T15:04:05.000Z"),
		fixedNow.Add(-time.Minute).Format("2006-01-02T15:04:05.000Z"), id); err != nil {
		t.Fatal(err)
	}
	var r service.DigestResult
	result := f.call(t, "digest", map[string]any{"since": "2026-10-17"})
	if result.IsError {
		t.Fatalf("digest: %s", result.Content[0].(*mcp.TextContent).Text)
	}
	decode(t, result, &r)
	for _, want := range []string{"Contratto older-mail", "Periodo: dal 17 ottobre 2026"} {
		if !strings.Contains(r.Markdown, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	if !r.Built {
		t.Error("since preview was not built")
	}
}

func TestServer_DigestSinceValidationReasons(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		date, since, reason string
	}{
		{"2026-11-10", "2026-11-01", "since non è valido con una data passata"},
		{"", "/private/secret", "since non valido: usare YYYY-MM-DD"},
		{"", "2026-02-30", "since non valido: usare YYYY-MM-DD"},
		{"", "2026-11-12", "since deve precedere l'istante del digest"},
	} {
		r := f.call(t, "digest", map[string]any{"date": tc.date, "since": tc.since})
		want := service.ErrInvalid.Error() + ": " + tc.reason
		if !r.IsError || len(r.Content) != 1 || r.Content[0].(*mcp.TextContent).Text != want {
			t.Errorf("date=%q since=%q: want IsError with %q; got IsError=%v text=%s", tc.date, tc.since, want, r.IsError, r.Content[0].(*mcp.TextContent).Text)
		}
	}
}
