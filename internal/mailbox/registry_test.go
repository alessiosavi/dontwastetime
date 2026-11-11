package mailbox

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
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

func TestDiscover_ReadableMembersAliasesAndMissing(t *testing.T) {
	var sel string
	var mu sync.Mutex
	probed := map[string]bool{}
	inbox := map[string]string{ // address → inbox id; legal@ is an alias of legal.team@
		"admin@example.test": "IN-ADMIN", "tech@example.test": "IN-TECH",
		"legal.team@example.test": "IN-LEGAL", "legal@example.test": "IN-LEGAL",
	}
	f, c := graphtest.New(t, nil)
	f.API = func(w http.ResponseWriter, r *http.Request) {
		mb := strings.ToLower(graphtest.Mailbox(r))
		if mb == "" { // the /users listing
			if r.URL.Query().Get("$skiptoken") == "" {
				sel = r.URL.Query().Get("$select")
				// mail is null for everyone (not visible to the credential); shared
				// mailboxes are disabled accounts; guests carry #EXT#.
				_, _ = w.Write([]byte(`{"@odata.nextLink":"` + f.URL + `/v1.0/users?$skiptoken=2","value":[
					{"id":"1","mail":null,"userPrincipalName":"admin@example.test","accountEnabled":false},
					{"id":"2","mail":null,"userPrincipalName":"room@example.test"},
					{"id":"3","mail":null,"userPrincipalName":"x_gmail.com#EXT#@example.onmicrosoft.com"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"value":[
				{"id":"4","mail":"Tech@Example.test","userPrincipalName":"t.x@example.onmicrosoft.com"},
				{"id":"5","mail":null,"userPrincipalName":"legal.team@example.test"}]}`))
			return
		}
		mu.Lock()
		probed[mb] = true
		mu.Unlock()
		if id, ok := inbox[mb]; ok && strings.HasSuffix(r.URL.Path, "/mailFolders/inbox") {
			_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"MailboxNotEnabledForRESTAPI"}}`))
	}
	found, missing, err := Discover(context.Background(), c, []string{"admin@example.test", "legal@example.test", "gone@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, []string{"admin@example.test", "legal@example.test", "tech@example.test"}) {
		t.Errorf("found = %v (room has no mailbox, legal.team is legal@'s mailbox, guests are skipped)", found)
	}
	if !reflect.DeepEqual(missing, []string{"gone@example.test"}) {
		t.Errorf("missing = %v", missing)
	}
	if probed["x_gmail.com#ext#@example.onmicrosoft.com"] {
		t.Error("guest accounts must not be probed")
	}
	if !strings.Contains(sel, "userPrincipalName") || !strings.Contains(sel, "mail") {
		t.Errorf("$select = %q", sel)
	}
}

func TestDiscover_ThrottledProbeAborts(t *testing.T) {
	_, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		if graphtest.Mailbox(r) == "" {
			_, _ = w.Write([]byte(`{"value":[{"id":"1","userPrincipalName":"a@example.test"}]}`))
			return
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	found, _, err := Discover(context.Background(), c, nil)
	var te *graph.ErrThrottled
	if !errors.As(err, &te) || found != nil {
		t.Fatalf("found=%v err=%v; want no partial list and *ErrThrottled", found, err)
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
