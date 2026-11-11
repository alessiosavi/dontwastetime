package mailbox

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var now0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// forbidLegal answers 403 for legal@, a one-message page otherwise.
func forbidLegal(w http.ResponseWriter, r *http.Request) {
	if graphtest.Mailbox(r) == "legal@example.test" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	_, _ = w.Write([]byte(`{"value":[{"id":"x"}]}`))
}

func TestVerify_ForbiddenReason(t *testing.T) {
	_, c := graphtest.New(t, forbidLegal)
	err := Verify(context.Background(), c, "legal@example.test")
	if err == nil || !strings.Contains(err.Error(), "access policy") || !strings.Contains(err.Error(), "legal@example.test") {
		t.Fatalf("want an access-policy error naming the mailbox, got %v", err)
	}
	if err := Verify(context.Background(), c, "tech@example.test"); err != nil {
		t.Fatalf("readable mailbox: %v", err)
	}
}

func TestVerify_OneMailboxForbiddenOthersPass(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	cfg := &config.Config{Mailboxes: []config.MailboxConfig{
		{Address: "legal@example.test", Enabled: true, PollSeconds: 180},
		{Address: "tech@example.test", Enabled: true, PollSeconds: 180},
		{Address: "off@example.test", Enabled: false, PollSeconds: 180},
	}}
	if err := SyncFromConfig(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	_, c := graphtest.New(t, forbidLegal)
	states := VerifyAll(ctx, c, s, now0)
	if len(states) != 2 {
		t.Fatalf("disabled mailboxes must not be probed: %+v", states)
	}
	got := map[string]State{}
	for _, st := range states {
		got[st.Address] = st
	}
	if got["legal@example.test"].OK || !strings.Contains(got["legal@example.test"].Reason, "access policy") || !got["tech@example.test"].OK {
		t.Fatalf("states: %+v", states)
	}
	rows, _ := s.ListMailboxes(ctx)
	for _, r := range rows {
		switch r.Address {
		case "legal@example.test":
			if r.State != store.StateDegraded || r.Reason == "" {
				t.Errorf("legal not degraded: %+v", r)
			}
		case "tech@example.test":
			if r.State != store.StateOK || r.LastOKAt == nil {
				t.Errorf("tech not ok: %+v", r)
			}
		}
	}
}

func TestDiscover_ForbiddenIsTypedNotFatal(t *testing.T) {
	_, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	found, missing, err := Discover(context.Background(), c, []string{"admin@example.test"})
	if !errors.Is(err, ErrDiscoveryForbidden) || len(found) != 0 || len(missing) != 0 {
		t.Fatalf("found=%v missing=%v err=%v", found, missing, err)
	}
}

func TestDiscover_ReportsMissingExpected(t *testing.T) {
	var sel string
	f, c := graphtest.New(t, nil)
	f.API = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skiptoken") == "" {
			sel = r.URL.Query().Get("$select")
			_, _ = w.Write([]byte(`{"@odata.nextLink":"` + f.URL + `/v1.0/users?$skiptoken=2","value":[
				{"id":"1","mail":"Admin@Example.test","userPrincipalName":"admin@example.test","accountEnabled":true},
				{"id":"2","mail":null,"userPrincipalName":"room@example.test","accountEnabled":true}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[
			{"id":"3","mail":"tech@example.test","accountEnabled":true},
			{"id":"4","mail":"gone@example.test","accountEnabled":false}]}`))
	}
	found, missing, err := Discover(context.Background(), c, []string{"admin@example.test", "legal@example.test", "gone@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, []string{"admin@example.test", "tech@example.test"}) {
		t.Errorf("found = %v", found)
	}
	if !reflect.DeepEqual(missing, []string{"gone@example.test", "legal@example.test"}) {
		t.Errorf("missing = %v", missing)
	}
	if !strings.Contains(sel, "accountEnabled") || !strings.Contains(sel, "mail") {
		t.Errorf("$select = %q", sel)
	}
}

func TestNextProbeTime_BacksOffAndCaps(t *testing.T) {
	cases := map[int]time.Duration{0: 0, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 7: time.Hour, 50: time.Hour}
	for failures, want := range cases {
		if got := NextProbeTime(now0, failures).Sub(now0); got != want {
			t.Errorf("failures=%d: backoff %s, want %s", failures, got, want)
		}
	}
}

func TestSyncFromConfig_PreservesRuntimeState(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	cfg := &config.Config{Mailboxes: []config.MailboxConfig{
		{Address: "legal@example.test", Enabled: true, Digest: true, PollSeconds: 180},
		{Address: "old@example.test", Enabled: true, PollSeconds: 180},
	}}
	if err := SyncFromConfig(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	cur := now0.Add(-time.Hour)
	_ = s.SetCursor(ctx, "legal@example.test", "inbox", cur)
	_ = s.SetMailboxState(ctx, "legal@example.test", store.StateDegraded, "403", now0)

	cfg.Mailboxes = []config.MailboxConfig{{Address: "legal@example.test", Enabled: true, Digest: false, PollSeconds: 300}}
	if err := SyncFromConfig(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ListMailboxes(ctx)
	byAddr := map[string]store.MailboxRow{}
	for _, r := range rows {
		byAddr[r.Address] = r
	}
	l := byAddr["legal@example.test"]
	if l.State != store.StateDegraded || !l.CursorInbox.Equal(cur) || l.Digest || l.PollSeconds != 300 {
		t.Errorf("legal after re-sync: %+v", l)
	}
	if byAddr["old@example.test"].Enabled {
		t.Error("mailbox removed from config must be disabled (config is authoritative)")
	}
}
