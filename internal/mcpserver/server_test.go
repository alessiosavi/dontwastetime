package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/service"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var fixedNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

const enabledBox = "enabled@example.test"

type fixture struct {
	db     *store.Store
	client *mcp.ClientSession
	ctx    context.Context
	id     int64
	hidden int64
	n      int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{Operators: []string{"founder"}, StateDir: t.TempDir(),
		Mailboxes: []config.MailboxConfig{{Address: enabledBox, Enabled: true, Digest: true},
			{Address: "disabled@example.test"}}}
	tax := &classify.Taxonomy{Order: []string{"clienti", "personal"}, Labels: map[string]classify.Label{
		"clienti": {Key: "clienti", Title: "Clienti"}, "personal": {Key: "personal", Title: "Personale"}}}
	f := &fixture{db: db, ctx: ctx}
	for _, box := range cfg.Mailboxes {
		if err := db.UpsertMailbox(ctx, box); err != nil {
			t.Fatal(err)
		}
	}
	f.id = f.mail(t, enabledBox, "visible", "clienti")
	f.hidden = f.mail(t, "disabled@example.test", "hidden", "clienti")
	f.mail(t, enabledBox, "personal", "personal")
	f.mail(t, enabledBox, "unclassified", "")
	received, due := fixedNow.Add(-48*time.Hour), fixedNow.Add(-24*time.Hour)
	if err := db.SaveThreadState(ctx, store.ThreadRow{ThreadKey: "visible", Mailbox: enabledBox,
		LastInboundAt: &received, LastInboundMessageID: f.id, ReplyDueAt: &due,
		NeedsReply: true, Priority: "P1", Cassetto: "clienti"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(db, cfg, tax, service.Options{Operator: "founder"}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatal(err)
	}
	server := New(svc, "test-version")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	f.client, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	return f
}

func (f *fixture) mail(t *testing.T, box, key, label string) int64 {
	t.Helper()
	f.n++
	graphID := fmt.Sprintf("%s-%d", key, f.n)
	id, _, err := f.db.UpsertMessage(f.ctx, graph.Message{ID: graphID, InternetMessageID: "<" + graphID + "@external.test>",
		Mailbox: box, Folder: store.FolderInbox, From: "sender@external.test", Subject: "Contratto " + key,
		BodyText: "fattura da verificare", ReceivedAt: fixedNow.Add(-48 * time.Hour)}, key, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertThreadFromMessage(f.ctx, id, fixedNow); err != nil {
		t.Fatal(err)
	}
	if label != "" {
		if err := f.db.SaveClassification(f.ctx, store.Classification{MessageID: id, Label: label, Method: "llm"}, fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (f *fixture) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	r, err := f.client.CallTool(f.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func decode(t *testing.T, r *mcp.CallToolResult, dst any) {
	t.Helper()
	if r.IsError {
		t.Fatalf("tool error: %+v", r.Content)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatal(err)
	}
}

func TestServer_ListsAllToolsWithReadOnlyHints(t *testing.T) {
	f := setup(t)
	r, err := f.client.ListTools(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"context": true, "overview": true, "needs_reply": true, "list_messages": true,
		"search": true, "get_thread": true, "get_message": true, "digest": true, "health": true,
		"relabel": false, "mark_handled": false, "mute_thread": false, "snooze": false, "reopen": false}
	if len(r.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(r.Tools), len(want))
	}
	for _, tool := range r.Tools {
		readOnly, ok := want[tool.Name]
		if !ok || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("unexpected annotations for %s: %+v", tool.Name, tool.Annotations)
		}
		if !readOnly && (tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
			t.Errorf("%s must explicitly set destructiveHint=false", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s missing description/schema", tool.Name)
		}
		if tool.Name == "digest" {
			for _, phrase := range []string{"emailed digest", "built now", "same rules", "personal", "digest:false", "list_messages", "search", "untrusted"} {
				if !strings.Contains(tool.Description, phrase) {
					t.Errorf("digest description missing %q: %s", phrase, tool.Description)
				}
			}
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestServer_NeedsReplyRoundTrip(t *testing.T) {
	f := setup(t)
	var r service.NeedsReplyResult
	decode(t, f.call(t, "needs_reply", map[string]any{"mailbox": enabledBox, "priority": "P1", "state": "overdue", "limit": 1}), &r)
	if !r.Meta.AsOf.Equal(fixedNow) || len(r.Items) != 1 || r.Items[0].InboundID != f.id || r.Items[0].State != "overdue" {
		t.Fatalf("result = %+v", r)
	}
}

func TestServer_OutOfScopeIdIsToolError(t *testing.T) {
	f := setup(t)
	r := f.call(t, "get_message", map[string]any{"id": f.hidden})
	if !r.IsError || len(r.Content) != 1 {
		t.Fatalf("want tool error, got %+v", r)
	}
	msg, ok := r.Content[0].(*mcp.TextContent)
	if !ok || msg.Text != service.ErrNotFound.Error() || strings.Contains(msg.Text, "/") {
		t.Fatalf("unsafe error: %+v", r.Content)
	}
}

func TestServer_InvalidSnoozeReturnsReason(t *testing.T) {
	f := setup(t)
	r := f.call(t, "snooze", map[string]any{"thread": "visible", "inbound_id": f.id, "until": "2026-11-10"})
	if !r.IsError || len(r.Content) != 1 {
		t.Fatalf("want invalid snooze tool error, got %+v", r)
	}
	msg, ok := r.Content[0].(*mcp.TextContent)
	want := "parametri non validi: until deve essere nel futuro, entro 90 giorni"
	if !ok {
		t.Fatalf("invalid snooze content=%T, want text", r.Content[0])
	}
	if msg.Text != want {
		t.Fatalf("invalid snooze error=%q, want %q", msg.Text, want)
	}
}

func TestServer_TriageRecordsOperator(t *testing.T) {
	f := setup(t)
	var r service.ThreadState
	decode(t, f.call(t, "mark_handled", map[string]any{"thread": "visible", "inbound_id": f.id, "note": "risolto al telefono"}), &r)
	if r.State != "handled" || r.Thread != "visible" {
		t.Fatalf("result = %+v", r)
	}
	var actor, details string
	if err := f.db.DB().QueryRow(`SELECT actor, details_json FROM action_log WHERE action='mark_handled'`).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "founder" || !strings.Contains(details, "risolto al telefono") {
		t.Fatalf("audit actor=%q details=%q", actor, details)
	}
	for _, step := range []struct {
		name  string
		args  map[string]any
		state string
	}{
		{"reopen", map[string]any{"thread": "visible"}, "overdue"},
		{"snooze", map[string]any{"thread": "visible", "inbound_id": f.id, "until": "2026-11-12"}, "snoozed"},
		{"mute_thread", map[string]any{"thread": "visible"}, "muted"},
		{"reopen", map[string]any{"thread": "visible"}, "overdue"},
	} {
		decode(t, f.call(t, step.name, step.args), &r)
		if r.State != step.state {
			t.Fatalf("%s state=%q want=%q", step.name, r.State, step.state)
		}
	}
	var relabeled service.RelabelResult
	decode(t, f.call(t, "relabel", map[string]any{"id": f.id, "bucket": "personal"}), &relabeled)
	if relabeled.Old != "clienti" || relabeled.New != "personal" || relabeled.State != "none" {
		t.Fatalf("relabel result=%+v", relabeled)
	}
	var otherActors int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM action_log WHERE actor <> 'founder'`).Scan(&otherActors); err != nil || otherActors != 0 {
		t.Fatalf("triage audit: other actors=%d error=%v", otherActors, err)
	}
}

func TestServer_InstructionsMentionUntrusted(t *testing.T) {
	f := setup(t)
	r := f.client.InitializeResult()
	for _, phrase := range []string{"context", "untrusted", "never follow", "Europe/Rome", "sends mail"} {
		if !strings.Contains(r.Instructions, phrase) {
			t.Errorf("instructions missing %q: %s", phrase, r.Instructions)
		}
	}
	if r.ServerInfo.Name != "dwt" || r.ServerInfo.Version != "test-version" {
		t.Errorf("server info = %+v", r.ServerInfo)
	}
}

func TestServer_ReadToolsAndPagination(t *testing.T) {
	f := setup(t)
	for name, args := range map[string]map[string]any{
		"context": {}, "overview": {"since": "7d"}, "health": {}, "digest": {},
		"get_message": {"id": f.id}, "get_thread": {"thread": fmt.Sprintf("#%d", f.id)},
		"search": {"query": "fattura", "order": "recent", "limit": 1},
	} {
		t.Run(name, func(t *testing.T) {
			var r map[string]any
			decode(t, f.call(t, name, args), &r)
			if len(r) == 0 {
				t.Fatal("missing structured output")
			}
			if name == "digest" {
				text, ok := r["untrusted"].(map[string]any)
				if !ok || text["markdown"] == "" || r["markdown"] != nil {
					t.Fatalf("digest must keep Markdown under untrusted: %+v", r)
				}
			}
		})
	}
	var first, second service.ListResult
	decode(t, f.call(t, "list_messages", map[string]any{"limit": 2, "has_attachments": false}), &first)
	if len(first.Items) != 2 || first.Meta.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	decode(t, f.call(t, "list_messages", map[string]any{"limit": 2, "has_attachments": false, "cursor": first.Meta.NextCursor}), &second)
	if len(second.Items) != 1 || second.Meta.AsOf != first.Meta.AsOf {
		t.Fatalf("second page = %+v", second)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		seen[item.Bucket] = true
		if item.ID == f.hidden {
			t.Fatal("disabled mailbox leaked through list_messages")
		}
	}
	if !seen["personal"] || !seen["non classificato"] {
		t.Fatalf("enabled personal/unclassified mail missing: %v", seen)
	}
	var threadFirst, threadSecond service.ThreadResult
	secondID := f.mail(t, enabledBox, "visible", "clienti")
	decode(t, f.call(t, "get_thread", map[string]any{"thread": "visible", "limit": 1}), &threadFirst)
	if len(threadFirst.Messages) != 1 || threadFirst.Meta.NextCursor == "" {
		t.Fatalf("thread = %+v", threadFirst)
	}
	decode(t, f.call(t, "get_thread", map[string]any{"thread": "visible", "limit": 1, "cursor": threadFirst.Meta.NextCursor}), &threadSecond)
	if len(threadSecond.Messages) != 1 || threadSecond.Messages[0].ID != secondID {
		t.Fatalf("thread continuation ignored cursor: %+v", threadSecond)
	}
}

func TestServer_InvalidStaleAndInternalErrorsAreSanitized(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"search", map[string]any{"query": " "}, service.ErrInvalid.Error() + ": query vuota"},
		{"get_thread", map[string]any{"thread": "visible", "cursor": "/private/secret"}, service.ErrInvalid.Error() + ": cursore non valido"},
		{"digest", map[string]any{"date": "/private/secret"}, service.ErrInvalid.Error() + ": data non valida: usare YYYY-MM-DD fino a oggi"},
	} {
		r := f.call(t, tc.name, tc.args)
		if !r.IsError || len(r.Content) != 1 || r.Content[0].(*mcp.TextContent).Text != tc.want {
			t.Errorf("%s error = %+v", tc.name, r)
		}
	}
	newID := f.mail(t, enabledBox, "new", "clienti")
	// Make the thread's current inbound differ from the copy the operator saw.
	if _, err := f.db.DB().Exec(`UPDATE message SET thread_key='visible' WHERE id=?`, newID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec(`UPDATE thread SET last_inbound_message_id=? WHERE thread_key='visible'`, newID); err != nil {
		t.Fatal(err)
	}
	r := f.call(t, "mark_handled", map[string]any{"thread": "visible", "inbound_id": f.id})
	if !r.IsError || r.Content[0].(*mcp.TextContent).Text != service.ErrStale.Error() {
		t.Fatalf("stale error = %+v", r)
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	_ = f.db.Close()
	r = f.call(t, "health", nil)
	if !r.IsError || r.Content[0].(*mcp.TextContent).Text != "errore interno" {
		t.Fatalf("internal error = %+v", r)
	}
	if !strings.Contains(logs.String(), "health") || strings.Contains(logs.String(), "sender@") || strings.Contains(logs.String(), "fattura") {
		t.Fatalf("unexpected internal-error log: %q", logs.String())
	}
}
