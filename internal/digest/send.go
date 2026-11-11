package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"os"
	"path/filepath"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

// validate checks sender and recipients before any file or network work.
func validate(sender string, recipients []string) error {
	if err := config.CheckSender(sender); err != nil {
		return err
	}
	if len(recipients) == 0 {
		return errors.New("digest: no recipients configured")
	}
	for _, r := range recipients {
		if !config.ValidAddress(r) {
			return fmt.Errorf("digest: invalid recipient %q", r)
		}
	}
	return nil
}

// Send saves the digest under {state}/digests and records it. dryRun stops
// there (no network, sent_at NULL). A live send goes out at most once per
// day from DigestSender with saveToSentItems=false.
func Send(ctx context.Context, s *store.Store, c *graph.Client, cfg *config.Config, d *Draft, dryRun bool, now time.Time) error {
	if err := validate(cfg.DigestSender, cfg.DigestRecipients); err != nil {
		return err
	}
	dir := filepath.Join(cfg.StateDir, "digests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	mdPath := filepath.Join(dir, d.Day+".md")
	if err := os.WriteFile(mdPath, []byte(d.Markdown), 0o600); err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, d.Day+".html"), []byte(d.HTML), 0o600); err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	healthJSON, err := json.Marshal(d.Health)
	if err != nil {
		return err
	}
	if dryRun {
		if err := s.RecordDigest(ctx, d.Day, mdPath, cfg.DigestRecipients, string(healthJSON), nil); err != nil {
			return err
		}
		return s.LogAction(ctx, "digest", "digest_dry_run", "digest:"+d.Day, map[string]any{"path": mdPath}, now)
	}
	// Reserve the day's delivery before the network call: concurrent or
	// repeated runs must never send twice. A send Graph rejected gives it
	// back; one that got no answer keeps it (it may have been delivered).
	claimed, err := s.ClaimDigestSend(ctx, d.Day, mdPath, cfg.DigestRecipients, string(healthJSON), now)
	if err != nil || !claimed {
		return err // already sent (or being sent) today
	}
	if err := c.SendMail(ctx, cfg.DigestSender, cfg.DigestRecipients, d.Subject, d.HTML, false); err != nil {
		if errors.Is(err, graph.ErrDeliveryUnknown) {
			// Graph may have accepted it: keep the claim (at most once).
			_ = s.LogAction(context.WithoutCancel(ctx), "digest", "digest_send_unknown", "digest:"+d.Day,
				map[string]any{"recipients": len(cfg.DigestRecipients)}, now)
		} else {
			_ = s.UnclaimDigestSend(context.WithoutCancel(ctx), d.Day)
		}
		return fmt.Errorf("digest: send: %w", err)
	}
	return s.LogAction(ctx, "digest", "digest_sent", "digest:"+d.Day,
		map[string]any{"recipients": len(cfg.DigestRecipients)}, now)
}

var alertTmpl = htmltemplate.Must(htmltemplate.New("alert").Parse(`<!doctype html>
<html lang="it"><body style="font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;font-size:14px">
<p><b>Avviso P0 — cliente in attesa di risposta.</b></p>
<ul>{{range .}}
<li>{{.LastInboundFrom}} — “{{.Subject}}” — casella {{.Mailbox}}{{if .Waiting}} — in attesa da {{.Waiting}}{{end}}</li>{{end}}
</ul>
<p>Questo avviso parte una sola volta al giorno per conversazione. Se è già stata gestita a voce:
<code>dwt handled &lt;id&gt;</code>. Per smettere di seguire il thread, anche per i messaggi futuri:
<code>dwt mute-thread &lt;id&gt;</code>.</p>
</body></html>`))

type alertRow struct {
	store.ThreadRow
	Waiting string
}

// Alert emails the P0 rows (others are ignored) to AlertRecipients and logs
// one action per thread. dryRun writes the mail under {state}/alerts only.
func Alert(ctx context.Context, s *store.Store, c *graph.Client, cfg *config.Config, rows []store.ThreadRow, dryRun bool, now time.Time) error {
	var p0 []alertRow
	for _, r := range rows {
		if r.Priority == threads.PriorityP0 {
			ar := alertRow{ThreadRow: r}
			if r.LastInboundAt != nil {
				ar.Waiting = age(now.Sub(*r.LastInboundAt))
			}
			p0 = append(p0, ar)
		}
	}
	if len(p0) == 0 {
		return nil
	}
	if err := validate(cfg.DigestSender, cfg.AlertRecipients); err != nil {
		return err
	}
	subject := "[P0] Cliente in attesa: " + p0[0].Subject
	if len(p0) > 1 {
		subject += fmt.Sprintf(" (e altre %d)", len(p0)-1)
	}
	var body bytes.Buffer
	if err := alertTmpl.Execute(&body, p0); err != nil {
		return err
	}
	if dryRun {
		dir := filepath.Join(cfg.StateDir, "alerts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		name := fmt.Sprintf("%s-%d.html", config.Day(now), now.Unix())
		if err := os.WriteFile(filepath.Join(dir, name), body.Bytes(), 0o600); err != nil {
			return err
		}
	} else if err := c.SendMail(ctx, cfg.DigestSender, cfg.AlertRecipients, subject, body.String(), false); err != nil {
		return fmt.Errorf("alert: send: %w", err)
	}
	for _, r := range p0 {
		if err := s.LogAction(ctx, "alerter", "p0_alert", "thread:"+r.ThreadKey, map[string]any{"dry_run": dryRun}, now); err != nil {
			return err
		}
	}
	return nil
}
