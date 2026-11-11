package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func visibleIDs(rows []VisibleMessage) []int64 {
	var ids []int64
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func labelVisible(t *testing.T, s *Store, id int64, label string) {
	t.Helper()
	if err := s.SaveClassification(context.Background(), Classification{
		MessageID: id, Label: label, Method: "llm", Confidence: 0.9, Rationale: "motivo",
	}, now0); err != nil {
		t.Fatal(err)
	}
}

func TestVisible_ThreadSpanningUnmanagedBox(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	v := Visibility{Mailboxes: []string{"enabled@example.test"}}
	m := msg(v.Mailboxes[0], "<shared@x>", now0)
	m.BodyText = "contratto"
	visible, _ := upsert(t, s, m)
	labelVisible(t, s, visible, "clienti")
	var hidden []int64
	for _, box := range []string{"disabled@example.test", "removed@example.test"} {
		m.Mailbox = box
		id, _ := upsert(t, s, m)
		hidden = append(hidden, id)
		labelVisible(t, s, id, "unsure")
	}
	rows, err := s.VisibleThreadMessages(ctx, v, "tk-<shared@x>")
	if err != nil || !slices.Equal(visibleIDs(rows), []int64{visible}) {
		t.Fatalf("VisibleThreadMessages IDs=%v err=%v, want only %d", visibleIDs(rows), err, visible)
	}
	for _, id := range append(hidden, 99999) {
		if _, err := s.VisibleMessage(ctx, v, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("VisibleMessage(%d) err=%v, want ErrNotFound", id, err)
		}
	}
	for _, scope := range []Visibility{v, {Mailboxes: []string{v.Mailboxes[0], v.Mailboxes[0]}}} {
		rows, err := s.VisibleMessages(ctx, scope, MessageFilter{}, 20, 0)
		if err != nil || !slices.Equal(visibleIDs(rows), []int64{visible}) {
			t.Errorf("VisibleMessages IDs=%v err=%v", visibleIDs(rows), err)
		}
		hits, err := s.SearchVisible(ctx, scope, `"contratto"`, MessageFilter{}, "relevance", 10)
		if err != nil || len(hits) != 1 || hits[0].ID != visible {
			t.Errorf("SearchVisible hits=%v err=%v, want only %d", hits, err, visible)
		}
		counts, unsure, unclassified, err := s.VisibleCounts(ctx, scope, now0, now0.Add(time.Hour))
		if err != nil || !slices.Equal(counts, []BucketCount{{v.Mailboxes[0], "clienti", 1}}) || unsure != 0 || unclassified != 0 {
			t.Errorf("VisibleCounts=%v unsure=%d unclassified=%d err=%v", counts, unsure, unclassified, err)
		}
	}
	if _, err := s.VisibleThreadMessages(ctx, v, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing thread err=%v, want ErrNotFound", err)
	}
	rows, err = s.VisibleMessages(ctx, v, MessageFilter{Mailbox: "disabled@example.test"}, 20, 0)
	if err != nil || len(rows) != 0 {
		t.Errorf("out-of-scope mailbox filter rows=%d err=%v", len(rows), err)
	}
}

func TestVisible_PersonalAndUnclassifiedIncluded(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	box := "personal@example.test"
	v := Visibility{Mailboxes: []string{box}}
	m := msg(box, "<personal@x>", now0)
	m.BodyText = "contratto"
	personal, _ := upsert(t, s, m)
	labelVisible(t, s, personal, "personal")
	m.InternetMessageID, m.ReceivedAt = "<unclassified@x>", now0.Add(time.Hour)
	unclassifiedID, _ := upsert(t, s, m)
	m.InternetMessageID, m.ReceivedAt, m.Folder = "<unsure@x>", now0.Add(2*time.Hour), FolderSentItems
	unsureID, _ := upsert(t, s, m)
	labelVisible(t, s, unsureID, "unsure")
	rows, err := s.VisibleMessages(ctx, v, MessageFilter{}, 20, 0)
	if err != nil || !slices.Equal(visibleIDs(rows), []int64{unsureID, unclassifiedID, personal}) {
		t.Fatalf("personal/unclassified list IDs=%v err=%v", visibleIDs(rows), err)
	}
	u, err := s.VisibleMessage(ctx, v, unclassifiedID)
	if err != nil || u.Label != "" || u.Method != "" || u.Rationale != "" || u.Confidence != 0 || u.BodyPurged {
		t.Fatalf("unclassified metadata not empty, err=%v", err)
	}
	p, err := s.VisibleMessage(ctx, v, personal)
	if err != nil || p.Label != "personal" || p.Method != "llm" || p.Rationale != "motivo" || p.Confidence != 0.9 || p.BodyText != "contratto" ||
		!slices.Equal(p.To, m.To) || !slices.Equal(p.CC, m.CC) || !p.ReceivedAt.Equal(now0) || p.SentAt == nil || !p.SentAt.Equal(m.SentAt) {
		t.Fatalf("classified message metadata mismatch, err=%v", err)
	}
	hits, err := s.SearchVisible(ctx, v, `"contratto"`, MessageFilter{}, "relevance", 10)
	if err != nil || len(hits) != 3 {
		t.Fatalf("personal/unclassified search hits=%d err=%v", len(hits), err)
	}
	counts, unsure, unclassified, err := s.VisibleCounts(ctx, v, now0, now0.Add(2*time.Hour))
	want := []BucketCount{{box, "", 1}, {box, "personal", 1}}
	if err != nil || !slices.Equal(counts, want) || unsure != 0 || unclassified != 1 {
		t.Fatalf("counts=%v unsure=%d unclassified=%d err=%v", counts, unsure, unclassified, err)
	}
	counts, unsure, unclassified, err = s.VisibleCounts(ctx, v, now0.Add(time.Hour), now0.Add(3*time.Hour))
	want = []BucketCount{{box, "", 1}, {box, "unsure", 1}}
	if err != nil || !slices.Equal(counts, want) || unsure != 1 || unclassified != 1 {
		t.Fatalf("counts boundary=%v unsure=%d unclassified=%d err=%v", counts, unsure, unclassified, err)
	}
	if _, err := s.PurgeBodies(ctx, now0.Add(time.Minute), now0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, err = s.VisibleMessage(ctx, v, personal)
	if err != nil || !p.BodyPurged || p.BodyPurgedAt == nil || p.BodyText != "" || p.BodyPreview != "" || p.Label != "personal" {
		t.Fatalf("purged message metadata mismatch, err=%v", err)
	}
}

func TestVisibleMessages_FiltersAndNewestFirst(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	box, other := "enabled@example.test", "other@example.test"
	v := Visibility{Mailboxes: []string{box, other}}
	m := msg(box, "<old@x>", now0.Add(-2*time.Hour))
	m.From, m.BodyText, m.HasAttachments = "Alice@CLIENT.EXAMPLE", "contratto", true
	old, _ := upsert(t, s, m)
	labelVisible(t, s, old, "clienti")
	m = msg(box, "<in@x>", now0)
	m.From, m.BodyText = "news@evilclient.example", "contratto"
	m.To = []string{"ignored@client.example"}
	in, _ := upsert(t, s, m)
	labelVisible(t, s, in, "business.dev")
	m = msg(other, "<to@x>", now0.Add(time.Hour))
	m.Folder, m.From, m.BodyText = FolderSentItems, "me@sender.example", "contratto"
	m.To, m.CC = []string{"bob@Client.Example"}, []string{"eve@partner.example"}
	to, _ := upsert(t, s, m)
	labelVisible(t, s, to, "personal")
	m = msg(box, "<cc@x>", now0.Add(time.Hour))
	m.Folder, m.From, m.BodyText, m.HasAttachments = FolderSentItems, "me@client.example", "contratto", true
	m.To, m.CC = []string{"bob@elsewhere.example"}, []string{"eve@CLIENT.EXAMPLE"}
	cc, _ := upsert(t, s, m)
	labelVisible(t, s, cc, "clienti")
	// An outbound sender on the domain is insufficient; inspect To/Cc instead.
	m.InternetMessageID, m.CC, m.HasAttachments = "<sender-only@x>", nil, false
	senderOnly, _ := upsert(t, s, m)
	labelVisible(t, s, senderOnly, "clienti")
	yes, no := true, false
	since, until := now0, now0.Add(time.Hour)
	for _, tc := range []struct {
		name          string
		f             MessageFilter
		limit, offset int
		want          []int64
	}{
		{"newest and id tie", MessageFilter{}, 20, 0, []int64{senderOnly, cc, to, in, old}},
		{"page", MessageFilter{}, 2, 1, []int64{cc, to}},
		{"mailbox", MessageFilter{Mailbox: other}, 20, 0, []int64{to}},
		{"bucket", MessageFilter{Bucket: "clienti"}, 20, 0, []int64{senderOnly, cc, old}},
		{"from exact case insensitive", MessageFilter{From: "alice@client.example"}, 20, 0, []int64{old}},
		{"domain inbound and outbound to cc", MessageFilter{Domain: "client.example"}, 20, 0, []int64{cc, to, old}},
		{"domain case insensitive", MessageFilter{Domain: "CLIENT.EXAMPLE"}, 20, 0, []int64{cc, to, old}},
		{"domain cc", MessageFilter{Domain: "partner.example"}, 20, 0, []int64{to}},
		{"domain exact suffix", MessageFilter{Domain: "example"}, 20, 0, nil},
		{"domain literal wildcard", MessageFilter{Domain: "%example"}, 20, 0, nil},
		{"direction", MessageFilter{Direction: "out"}, 20, 0, []int64{senderOnly, cc, to}},
		{"attachments", MessageFilter{HasAttachments: &yes}, 20, 0, []int64{cc, old}},
		{"no attachments", MessageFilter{HasAttachments: &no}, 20, 0, []int64{senderOnly, to, in}},
		{"received range", MessageFilter{Since: &since, Until: &until}, 20, 0, []int64{in}},
		{"combined", MessageFilter{Mailbox: box, Bucket: "clienti", Domain: "client.example", Direction: "out", HasAttachments: &yes, Since: &since}, 20, 0, []int64{cc}},
		{"literal mailbox", MessageFilter{Mailbox: box + `' OR 1=1 --`}, 20, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.VisibleMessages(ctx, v, tc.f, tc.limit, tc.offset)
			if err != nil || !slices.Equal(visibleIDs(rows), tc.want) {
				t.Fatalf("IDs=%v err=%v, want %v", visibleIDs(rows), err, tc.want)
			}
			// Search must apply precisely the same predicates before its cap.
			hits, err := s.SearchVisible(ctx, v, `"contratto"`, tc.f, "relevance", 20)
			var got []int64
			for _, hit := range hits {
				got = append(got, hit.ID)
			}
			want := tc.want
			if tc.name == "page" {
				want = []int64{senderOnly, cc, to, in, old}
			}
			got, want = slices.Clone(got), slices.Clone(want)
			slices.Sort(got)
			slices.Sort(want)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("search IDs=%v err=%v, want %v", got, err, want)
			}
		})
	}
	// Thread expansion has the opposite chronology, including id tie breaks.
	if _, err := s.DB().Exec(`UPDATE message SET thread_key = 'combined'`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.VisibleThreadMessages(ctx, v, "combined")
	if err != nil || !slices.Equal(visibleIDs(rows), []int64{old, in, to, cc, senderOnly}) {
		t.Fatalf("thread chronology IDs=%v err=%v", visibleIDs(rows), err)
	}
}

func TestVisibleObligations_ExpiredSnoozeIncludedHandledExcluded(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	box, hidden := "enabled@example.test", "disabled@example.test"
	v := Visibility{Mailboxes: []string{box}}
	due := now0.Add(time.Hour)
	add := func(key, mailbox, priority, bucket, ack string, until *time.Time, age time.Duration, needs bool, muted *time.Time, replyDue *time.Time) {
		t.Helper()
		m := msg(mailbox, "<"+key+"@x>", now0.Add(-age))
		id, _ := upsert(t, s, m)
		if err := s.SaveThreadState(ctx, ThreadRow{ThreadKey: key, Mailbox: mailbox, Subject: m.Subject,
			LastInboundAt: &m.ReceivedAt, LastInboundMessageID: id, LastInboundFrom: m.From,
			NeedsReply: needs, ReplyDueAt: replyDue, Priority: priority, Cassetto: bucket}, now0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`UPDATE thread SET ack_kind = ?, snoozed_until = ?, muted_at = ? WHERE thread_key = ?`, ack, nullTS(until), nullTS(muted), key); err != nil {
			t.Fatal(err)
		}
	}
	past, future := now0.Add(-time.Second), now0.Add(time.Second)
	add("due", box, "P2", "clienti", "", nil, time.Hour, false, nil, &due)
	add("overdue", box, "P1", "clienti", "", nil, 2*time.Hour, true, nil, &past)
	add("pinned", box, "P1+", "clienti", "", nil, 3*time.Hour, true, nil, &past)
	add("expired", box, "P0", "business.dev", "snoozed", &past, 4*time.Hour, false, nil, &past)
	add("at-expiry", box, "P0", "clienti", "snoozed", &now0, 5*time.Hour, false, nil, &past)
	add("handled", box, "P0", "clienti", "handled", nil, 6*time.Hour, false, nil, &past)
	add("snoozed", box, "P0", "clienti", "snoozed", &future, 7*time.Hour, false, nil, &past)
	add("muted", box, "P0", "clienti", "", nil, 8*time.Hour, true, &now0, &past)
	add("none", box, "P0", "clienti", "", nil, 9*time.Hour, true, nil, nil)
	add("hidden", hidden, "P0", "clienti", "", nil, 10*time.Hour, true, nil, &past)
	// A moving/stale thread.mailbox must neither grant nor deny visibility.
	if _, err := s.DB().Exec(`UPDATE thread SET mailbox = CASE WHEN thread_key = 'hidden' THEN ? ELSE ? END`, box, hidden); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mailbox, bucket, priority string
		want                            []string
	}{
		{"all", "", "", "", []string{"pinned", "at-expiry", "expired", "overdue", "due"}},
		{"mailbox from inbound", box, "", "", []string{"pinned", "at-expiry", "expired", "overdue", "due"}},
		{"hidden mailbox", hidden, "", "", nil},
		{"bucket", "", "clienti", "", []string{"pinned", "at-expiry", "overdue", "due"}},
		{"priority", "", "", "P0", []string{"at-expiry", "expired"}},
		{"combined", box, "clienti", "P0", []string{"at-expiry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.VisibleObligations(ctx, v, tc.mailbox, tc.bucket, tc.priority, now0)
			var keys []string
			for _, row := range rows {
				keys = append(keys, row.ThreadKey)
			}
			if err != nil || !slices.Equal(keys, tc.want) {
				t.Fatalf("obligations=%v err=%v, want %v", keys, err, tc.want)
			}
		})
	}
}

func TestVisible_EmptyScopeDeniesAllReads(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	id, _ := upsert(t, s, msg("enabled@example.test", "<scope@x>", now0))
	for _, v := range []Visibility{{}, {Mailboxes: []string{"missing@example.test"}}} {
		rows, err := s.VisibleMessages(ctx, v, MessageFilter{}, 20, 0)
		if err != nil || len(rows) != 0 {
			t.Fatalf("empty scope list=%d err=%v", len(rows), err)
		}
		if _, err := s.VisibleMessage(ctx, v, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty scope message err=%v", err)
		}
		if _, err := s.VisibleThreadMessages(ctx, v, "tk-<scope@x>"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty scope thread err=%v", err)
		}
		hits, err := s.SearchVisible(ctx, v, `"corpo"`, MessageFilter{}, "relevance", 20)
		if err != nil || len(hits) != 0 {
			t.Fatalf("empty scope search=%v err=%v", hits, err)
		}
		counts, unsure, unclassified, err := s.VisibleCounts(ctx, v, now0, now0.Add(time.Hour))
		if err != nil || len(counts) != 0 || unsure != 0 || unclassified != 0 {
			t.Fatalf("empty scope counts=%v/%d/%d err=%v", counts, unsure, unclassified, err)
		}
		obligations, err := s.VisibleObligations(ctx, v, "", "", "", now0)
		if err != nil || len(obligations) != 0 {
			t.Fatalf("empty scope obligations=%d err=%v", len(obligations), err)
		}
	}
}

func TestSearchVisible_RankAndCapAfterFiltering(t *testing.T) {
	ctx, s := context.Background(), openTest(t)
	v := Visibility{Mailboxes: []string{"enabled@example.test"}}
	m := msg("disabled@example.test", "<hidden-best@x>", now0)
	m.Subject, m.From, m.FromName, m.BodyText = "", "", "", "contratto"
	upsert(t, s, m)
	m.Mailbox, m.InternetMessageID, m.BodyText = v.Mailboxes[0], "<weak@x>", "contratto testo testo testo testo testo testo testo testo testo"
	weak, _ := upsert(t, s, m)
	m.InternetMessageID, m.BodyText = "<best@x>", "contratto"
	best, _ := upsert(t, s, m)
	for _, cap := range []int{1, 2} {
		hits, err := s.SearchVisible(ctx, v, `"contratto"`, MessageFilter{}, "relevance", cap)
		if err != nil || len(hits) != cap || hits[0].ID != best {
			t.Fatalf("rank/cap hits=%v err=%v", hits, err)
		}
		if cap == 2 && (hits[1].ID != weak || hits[0].BM25 >= hits[1].BM25 || hits[0].BM25 >= 0) {
			t.Fatalf("raw bm25 order=%v", hits)
		}
	}
}
