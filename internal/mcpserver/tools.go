package mcpserver

import (
	"context"

	"github.com/alessiosavi/go-dontwastetime/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type emptyInput struct{}

type pageInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum items: default 20, capped at 100."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque meta.next_cursor from the previous page; keep all filters unchanged."`
}

func (p pageInput) page() service.Page { return service.Page{Limit: p.Limit, Cursor: p.Cursor} }

type rangeInput struct {
	Since string `json:"since,omitempty" jsonschema:"Inclusive received date: YYYY-MM-DD at midnight Europe/Rome, or relative time such as 7d or 24h."`
	Until string `json:"until,omitempty" jsonschema:"Exclusive received date: YYYY-MM-DD at midnight Europe/Rome, or relative time such as 7d or 24h."`
}

func (r rangeInput) dateRange() service.Range { return service.Range{Since: r.Since, Until: r.Until} }

type overviewInput struct {
	Mailbox string `json:"mailbox,omitempty" jsonschema:"Enabled mailbox address; omitted selects all enabled mailboxes."`
	rangeInput
}

type needsReplyInput struct {
	Mailbox  string `json:"mailbox,omitempty" jsonschema:"Enabled mailbox address; omitted selects all enabled mailboxes."`
	Priority string `json:"priority,omitempty" jsonschema:"Priority filter: P0, P1 or P2."`
	Bucket   string `json:"bucket,omitempty" jsonschema:"Bucket key from context."`
	State    string `json:"state,omitempty" jsonschema:"Reply state filter: due or overdue; omitted selects both."`
	pageInput
}

type listInput struct {
	Mailbox        string `json:"mailbox,omitempty" jsonschema:"Enabled mailbox address; omitted selects all enabled mailboxes."`
	Bucket         string `json:"bucket,omitempty" jsonschema:"Bucket key from context, or non classificato for unclassified mail."`
	From           string `json:"from,omitempty" jsonschema:"Sender address filter."`
	Domain         string `json:"domain,omitempty" jsonschema:"Sender domain filter."`
	Direction      string `json:"direction,omitempty" jsonschema:"Message direction filter: in or out."`
	HasAttachments *bool  `json:"has_attachments,omitempty" jsonschema:"Filter by attachment presence; false selects messages without attachments."`
	rangeInput
	pageInput
}

type searchInput struct {
	Query   string `json:"query" jsonschema:"Nonempty literal words to find in subject, sender or body; words are combined with AND."`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"Enabled mailbox address; omitted selects all enabled mailboxes."`
	Bucket  string `json:"bucket,omitempty" jsonschema:"Bucket key from context, or non classificato for unclassified mail."`
	Order   string `json:"order,omitempty" jsonschema:"relevance (default, 30-day recency half-life) or recent (newest first)."`
	rangeInput
	pageInput
}

type threadInput struct {
	Thread string `json:"thread" jsonschema:"Thread key or #id of a visible message."`
}

type threadPageInput struct {
	threadInput
	pageInput
}

type messageInput struct {
	ID int64 `json:"id" jsonschema:"Numeric id of a visible message."`
}

type digestInput struct {
	Date  string `json:"date,omitempty" jsonschema:"YYYY-MM-DD: today is built on demand; past dates read a saved digest or return not found. Omit for today."`
	Since string `json:"since,omitempty" jsonschema:"YYYY-MM-DD (midnight Europe/Rome): build today's digest covering mail received since this date, e.g. the first digest of a month. Only valid for today."`
}

// The SDK's schema inference flattens tagged anonymous structs. Use a named
// field so the schema matches the service's untrusted JSON envelope.
type digestOutput struct {
	Untrusted service.DigestText `json:"untrusted"`
	Built     bool               `json:"built"`
	Trimmed   bool               `json:"trimmed"`
}

type relabelInput struct {
	ID     int64  `json:"id" jsonschema:"Numeric id of a visible message to relabel."`
	Bucket string `json:"bucket" jsonschema:"New bucket key from context."`
}

type handledInput struct {
	threadInput
	InboundID int64  `json:"inbound_id" jsonschema:"Last inbound message id you saw; refused as stale if a different inbound has arrived."`
	Note      string `json:"note,omitempty" jsonschema:"Optional audit note, for example resolved by phone."`
}

type snoozeInput struct {
	threadInput
	InboundID int64  `json:"inbound_id" jsonschema:"Last inbound message id you saw; refused as stale if a different inbound has arrived."`
	Until     string `json:"until" jsonschema:"Future YYYY-MM-DD (08:00 Europe/Rome) or RFC3339 timestamp, at most 90 days ahead."`
}

func registerTools(s *mcp.Server, svc *service.Service) {
	addTool(s, "context", "Call first: current date in Europe/Rome, operator, enabled mailboxes with freshness, and bucket keys and Italian titles.", true,
		func(ctx context.Context, _ emptyInput) (service.ContextResult, error) { return svc.Context(ctx) })
	addTool(s, "overview", "Counts by mailbox and bucket, reply obligations by priority and state, P0 items, unsure and unclassified counts.", true,
		func(ctx context.Context, in overviewInput) (service.OverviewResult, error) {
			return svc.Overview(ctx, service.OverviewReq{Mailbox: in.Mailbox, Range: in.dateRange()})
		})
	addTool(s, "needs_reply", "Open reply obligations, including due and overdue states, deadline, priority and the last inbound_id needed for triage.", true,
		func(ctx context.Context, in needsReplyInput) (service.NeedsReplyResult, error) {
			return svc.NeedsReply(ctx, service.NeedsReplyReq{Mailbox: in.Mailbox, Priority: in.Priority,
				Bucket: in.Bucket, State: in.State, Page: in.page()})
		})
	addTool(s, "list_messages", "Filtered messages newest first, including personal and unclassified mail in enabled mailboxes; previews up to 300 characters.", true,
		func(ctx context.Context, in listInput) (service.ListResult, error) {
			return svc.ListMessages(ctx, service.ListReq{Mailbox: in.Mailbox, Bucket: in.Bucket, From: in.From,
				Domain: in.Domain, Direction: in.Direction, HasAttachments: in.HasAttachments, Range: in.dateRange(), Page: in.page()})
		})
	addTool(s, "search", "Full-text search of visible mail, including personal and unclassified mail; literal words, recent or relevance order, 300-character previews.", true,
		func(ctx context.Context, in searchInput) (service.ListResult, error) {
			return svc.Search(ctx, service.SearchReq{Query: in.Query, Mailbox: in.Mailbox, Bucket: in.Bucket,
				Order: in.Order, Range: in.dateRange(), Page: in.page()})
		})
	addTool(s, "get_thread", "Visible thread messages oldest first, reply state and sending mailbox; bodies up to 4,000 characters each and 40,000 per page. Continue with meta.next_cursor.", true,
		func(ctx context.Context, in threadPageInput) (service.ThreadResult, error) {
			return svc.GetThreadPage(ctx, in.Thread, in.page())
		})
	addTool(s, "get_message", "One visible message with recipients, bucket and classifier rationale; body up to 20,000 characters, with purged and trimmed flags.", true,
		func(ctx context.Context, in messageInput) (service.MessageDetail, error) {
			return svc.GetMessage(ctx, in.ID)
		})
	addTool(s, "digest", "The digest as it would be built now, with the same rules as the emailed digest: personal mail and digest:false mailboxes never appear here; use list_messages or search for them. Today's preview is built without sweeping, saving or sending; past dates read saved digests. Markdown is bounded and reports trimmed.", true,
		func(ctx context.Context, in digestInput) (digestOutput, error) {
			r, err := svc.Digest(ctx, in.Date, in.Since)
			return digestOutput{Untrusted: r.DigestText, Built: r.Built, Trimmed: r.Trimmed}, err
		})
	addTool(s, "health", "Local mailbox freshness, queue, unclassified counts, daily/monthly tokens, estimated USD cost and budget state; no remote probes.", true,
		func(ctx context.Context, _ emptyInput) (service.HealthResult, error) { return svc.Health(ctx) })
	addTool(s, "relabel", "Correct a visible message's bucket, record the operator and refresh its thread; corrections take precedence over classifier output.", false,
		func(ctx context.Context, in relabelInput) (service.RelabelResult, error) {
			return svc.Relabel(ctx, in.ID, in.Bucket)
		})
	addTool(s, "mark_handled", "Close the current reply obligation, for example after a phone call; later inbound mail reopens it. Reversible with reopen.", false,
		func(ctx context.Context, in handledInput) (service.ThreadState, error) {
			return svc.MarkHandled(ctx, in.Thread, in.InboundID, in.Note)
		})
	addTool(s, "mute_thread", "Stop tracking this thread, including future inbound mail, until reopen. Use mark_handled to close only the current obligation.", false,
		func(ctx context.Context, in threadInput) (service.ThreadState, error) {
			return svc.Mute(ctx, in.Thread)
		})
	addTool(s, "snooze", "Hide the current obligation until a future date, then restore its original deadline; later inbound mail ends the snooze. Reversible with reopen.", false,
		func(ctx context.Context, in snoozeInput) (service.ThreadState, error) {
			return svc.Snooze(ctx, in.Thread, in.InboundID, in.Until)
		})
	addTool(s, "reopen", "Clear mute, handled and snooze state, then recompute the thread's reply obligation.", false,
		func(ctx context.Context, in threadInput) (service.ThreadState, error) {
			return svc.Reopen(ctx, in.Thread)
		})
}
