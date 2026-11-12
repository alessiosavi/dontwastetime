package digest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// ErrNoteNotSent marks failures that are definitely safe for the caller to retry.
var ErrNoteNotSent = errors.New("note: definitely not sent")

// Note is a plain-text recap addressed to one configured digest recipient.
type Note struct{ To, Subject, Text string }

var noteTmpl = template.Must(template.New("note").Parse(`<!doctype html>
<html lang="it"><head><meta charset="utf-8"></head><body style="font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;font-size:14px">
<div style="white-space:pre-wrap">{{.}}</div>
</body></html>`))

// NoteHTML renders escaped text while preserving its line breaks and spacing.
func NoteHTML(text string) (string, error) {
	var body bytes.Buffer
	if err := noteTmpl.Execute(&body, text); err != nil {
		return "", err
	}
	return body.String(), nil
}

func validateNote(cfg *config.Config, n Note) error {
	if !config.ValidAddress(n.To) {
		return fmt.Errorf("note: invalid recipient %q", n.To)
	}
	allowed := false
	for _, recipient := range cfg.DigestRecipients {
		if strings.EqualFold(n.To, recipient) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("note: recipient %q is outside digest_recipients", n.To)
	}
	if err := config.CheckSender(cfg.DigestSender); err != nil {
		return err
	}
	if n.Subject == "" || strings.ContainsAny(n.Subject, "\r\n") || utf8.RuneCountInString(n.Subject) > 200 {
		return errors.New("note: subject must be non-empty, at most 200 characters, and contain no CR/LF")
	}
	if n.Text == "" || len(n.Text) > 64*1024 || !utf8.ValidString(n.Text) {
		return errors.New("note: text must be non-empty, at most 64 KiB, and valid UTF-8")
	}
	return nil
}

// SendNote makes one SendMail call without retrying an uncertain delivery.
// Missing replies, unexpected 2xx responses, and HTTP 5xx wrap
// graph.ErrDeliveryUnknown; all other failures wrap ErrNoteNotSent.
// The caller owns deduplication across invocations. Once Graph accepts a note,
// an audit failure must not turn success into a retryable failure.
func SendNote(ctx context.Context, s *store.Store, c *graph.Client, cfg *config.Config, n Note, dryRun bool, now time.Time) error {
	if err := validateNote(cfg, n); err != nil {
		return fmt.Errorf("%w: %w", ErrNoteNotSent, err)
	}
	body, err := NoteHTML(n.Text)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoteNotSent, err)
	}
	hash := sha256.Sum256([]byte(n.Text))
	target := fmt.Sprintf("note:%x", hash[:6])
	details := map[string]any{"subject": n.Subject}
	if dryRun {
		if err := s.LogAction(ctx, "note", "note_dry_run", target, details, now); err != nil {
			slog.Warn("note dry run; audit write failed", "target", target, "error", err)
		}
		return nil
	}
	if err := c.SendMail(ctx, cfg.DigestSender, []string{n.To}, n.Subject, body, false); err != nil {
		var httpErr *graph.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status >= 500 {
			err = fmt.Errorf("%w: %w", graph.ErrDeliveryUnknown, err)
		}
		if errors.Is(err, graph.ErrDeliveryUnknown) {
			if auditErr := s.LogAction(context.WithoutCancel(ctx), "note", "note_send_unknown", target, details, now); auditErr != nil {
				slog.Warn("note delivery unknown; audit write failed", "target", target, "error", auditErr)
			}
			return fmt.Errorf("note: send: %w", err)
		}
		return fmt.Errorf("%w: %w", ErrNoteNotSent, err)
	}
	if err := s.LogAction(context.WithoutCancel(ctx), "note", "note_sent", target, details, now); err != nil {
		slog.Warn("note accepted by Graph; audit write failed", "target", target, "error", err)
	}
	return nil
}
