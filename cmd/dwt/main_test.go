package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRun_Version(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "dwt ") {
		t.Errorf("version output %q", out.String())
	}
}

func TestCLI_DispatchUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") || !strings.Contains(errb.String(), "usage") {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestCLI_NoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func mcpConfig(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"MS_TENANT_ID", "MS_CLIENT_ID", "MS_CLIENT_SECRET", "LLM_URL", "LLM_MODEL", "LLM_API_KEY",
		"LLM_DAILY_MAX_TOKENS", "LLM_MONTHLY_MAX_COST", "LLM_PRICE_IN_PER_MTOK", "LLM_PRICE_OUT_PER_MTOK", "DWT_STATE_DIR", "DWT_CONFIG"} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{"operators": []string{"founder"}, "state_dir": filepath.Join(dir, "state"),
		"mailboxes":         []map[string]any{{"address": "enabled@example.test", "enabled": true}},
		"digest_recipients": []string{"operator@example.test"}, "digest_sender": "enabled@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tax, err := os.ReadFile("../../taxonomy.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "taxonomy.json"), tax, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPCommand_RequiresKnownOperator(t *testing.T) {
	path := mcpConfig(t)
	for _, operator := range []string{"", "unknown", "cli"} {
		var out, errb bytes.Buffer
		args := []string{"mcp", "--config", path}
		if operator != "" {
			args = append(args, "--operator", operator)
		}
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "operator") || !strings.Contains(errb.String(), "usage") || out.Len() != 0 {
			t.Errorf("operator=%q exit=%d stdout=%q stderr=%q", operator, code, out.String(), errb.String())
		}
	}
}

func TestMCPCommand_NeedsNoCredentials(t *testing.T) {
	path := mcpConfig(t)
	a, err := openApp(paths{config: path}, need{})
	if err != nil {
		t.Fatal(err)
	}
	if a.c != nil || a.llm != nil {
		t.Error("need{} constructed a Graph or LLM client")
	}
	a.close()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = stdinR
	defer func() { os.Stdin = oldStdin }()
	outR, outW := io.Pipe()
	t.Cleanup(func() { _ = stdinR.Close(); _ = stdinW.Close(); _ = outR.Close(); _ = outW.Close() })
	var errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"mcp", "--config", path, "--operator", "founder"}, outW, &errb)
		_ = outW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "command-test", Version: "1"}, nil).Connect(ctx,
		&mcp.IOTransport{Reader: outR, Writer: stdinW}, nil)
	if err != nil {
		_ = stdinW.Close()
		code := <-done
		t.Fatalf("initialize failed: %v; exit=%d stderr=%q", err, code, errb.String())
	}
	if got := cs.InitializeResult().ServerInfo; got.Name != "dwt" || got.Version != version {
		t.Errorf("initialize server info = %+v", got)
	}
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "context"})
	if err != nil || r.IsError {
		t.Errorf("context result=%+v error=%v", r, err)
	}
	_ = cs.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
	case <-ctx.Done():
		t.Fatal("MCP command did not stop after the client disconnected")
	}
}

func TestMCPNowFixesContextDate(t *testing.T) {
	path := mcpConfig(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = stdinR
	defer func() { os.Stdin = oldStdin }()
	outR, outW := io.Pipe()
	t.Cleanup(func() { _ = stdinR.Close(); _ = stdinW.Close(); _ = outR.Close(); _ = outW.Close() })
	var errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"mcp", "--config", path, "--operator", "founder", "--now", "2026-03-02T10:00:00+01:00"}, outW, &errb)
		_ = outW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "clock-test", Version: "1"}, nil).Connect(ctx,
		&mcp.IOTransport{Reader: outR, Writer: stdinW}, nil)
	if err != nil {
		_ = stdinW.Close()
		code := <-done
		t.Fatalf("initialize failed: %v; exit=%d stderr=%q", err, code, errb.String())
	}
	defer func() { _ = cs.Close() }()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "context"})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("context tool error: %+v", r.Content)
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Today string `json:"today"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Today != "2026-03-02" {
		t.Errorf("context today = %q, want 2026-03-02", got.Today)
	}
	_ = cs.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
	case <-ctx.Done():
		t.Fatal("MCP command did not stop after the client disconnected")
	}
}

func TestNowFlagRejectedOutsideMCP(t *testing.T) {
	path := mcpConfig(t)
	for _, cmd := range []string{"serve", "sync", "classify", "digest", "explore", "eval", "mailboxes", "correct",
		"handled", "snooze", "mute-thread", "reopen", "doctor", "version", "help", "-h", "--help"} {
		t.Run(cmd, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run([]string{cmd, "--now", "2026-03-02T10:00:00+01:00", "--config", path}, &out, &errb)
			if code != 2 || out.Len() != 0 {
				t.Errorf("exit=%d stdout=%q stderr=%q, want exit 2 and empty stdout", code, out.String(), errb.String())
			}
			message := strings.TrimSpace(errb.String())
			if !strings.Contains(message, "--now") || !strings.Contains(message, "mcp") || strings.Contains(message, "\n") {
				t.Errorf("want one-line explanation that --now requires mcp, got stderr %q", errb.String())
			}
		})
	}
}

func TestNowFlagRejectsBadValue(t *testing.T) {
	path := mcpConfig(t)
	for _, value := range []string{"yesterday", ""} {
		t.Run(value, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run([]string{"mcp", "--config", path, "--operator", "founder", "--now", value}, &out, &errb)
			if code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "RFC3339") {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want exit 2 and RFC3339 explanation", code, out.String(), errb.String())
			}
		})
	}
}

// noteCommandFixture routes every request to a local fake; unexpected hosts
// fail closed so these CLI tests cannot contact real Graph or send real mail.
func noteCommandFixture(t *testing.T, api http.HandlerFunc) (string, string, *graphtest.Server) {
	t.Helper()
	path := mcpConfig(t)
	for key, value := range map[string]string{"MS_TENANT_ID": "tenant", "MS_CLIENT_ID": "client", "MS_CLIENT_SECRET": "secret"} {
		t.Setenv(key, value)
	}
	srv, _ := graphtest.New(t, api)
	fakeURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = evalTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.microsoft.com" && r.URL.Host != "login.microsoftonline.com" {
			return nil, fmt.Errorf("unexpected test host %q", r.URL.Host)
		}
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = fakeURL.Scheme, fakeURL.Host, ""
		return previous.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	file := filepath.Join(filepath.Dir(path), "note.txt")
	if err := os.WriteFile(file, []byte("Fatto\nRiepilogo di oggi."), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, file, srv
}

func TestRunSendNoteExitCodeUnknownIs3(t *testing.T) {
	path, file, srv := noteCommandFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // received the note; lose the reply to the caller
	})
	var out, errb bytes.Buffer
	code := run([]string{"send-note", "--config", path, "--to", "operator@example.test", "--subject", "Riepilogo", "--file", file}, &out, &errb)
	if code != 3 || !strings.Contains(errb.String(), "may have been delivered") {
		t.Fatalf("exit=%d stderr=%q, want unknown delivery exit 3", code, errb.String())
	}
	if got := srv.APICalls.Load(); got != 1 {
		t.Fatalf("send attempts=%d, want 1", got)
	}
}

func TestRunSendNoteAmbiguousResponseIs3(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			path, file, srv := noteCommandFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			})
			var out, errb bytes.Buffer
			code := run([]string{"send-note", "--config", path, "--to", "operator@example.test", "--subject", "Riepilogo", "--file", file}, &out, &errb)
			if code != 3 {
				t.Fatalf("HTTP %d: exit=%d stderr=%q, want unknown delivery exit 3", status, code, errb.String())
			}
			if got := srv.APICalls.Load(); got != 1 {
				t.Fatalf("send attempts=%d, want 1", got)
			}
		})
	}
}

func TestRunSendNoteRejectedIs4(t *testing.T) {
	for _, tc := range []struct {
		name    string
		to      string
		subject string
		want    int32
	}{
		{"Graph 400", "operator@example.test", "Riepilogo", 1},
		{"outside digest recipients", "other@example.test", "Riepilogo", 0},
		{"empty recipient", "", "Riepilogo", 0},
		{"empty subject", "operator@example.test", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, file, srv := noteCommandFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			})
			var out, errb bytes.Buffer
			code := run([]string{"send-note", "--config", path, "--to", tc.to, "--subject", tc.subject, "--file", file}, &out, &errb)
			if code != 4 {
				t.Fatalf("exit=%d stderr=%q, want definitely not sent exit 4", code, errb.String())
			}
			if got := srv.APICalls.Load(); got != tc.want {
				t.Fatalf("Graph requests=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunSendNoteAcceptedIs0(t *testing.T) {
	path, file, srv := noteCommandFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	var out, errb bytes.Buffer
	code := run([]string{"send-note", "--config", path, "--to", "operator@example.test", "--subject", "Riepilogo", "--file", file}, &out, &errb)
	if code != 0 || srv.APICalls.Load() != 1 {
		t.Fatalf("exit=%d sends=%d stderr=%q", code, srv.APICalls.Load(), errb.String())
	}
}

func TestRunSendNoteDryRunNeedsNoCredentials(t *testing.T) {
	path := mcpConfig(t)
	file := filepath.Join(filepath.Dir(path), "note.txt")
	if err := os.WriteFile(file, []byte("Fatto\nRiepilogo di oggi."), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = evalTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("dry run reached network: %s", r.URL.Host)
		return nil, fmt.Errorf("network forbidden in dry run")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	var out, errb bytes.Buffer
	code := run([]string{"send-note", "--config", path, "--to", "operator@example.test", "--subject", "Riepilogo", "--file", file, "--dry-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q, want credential-free dry run", code, errb.String())
	}
}

func TestRunSendNoteUsageAndFileErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, bytes.Repeat([]byte{'x'}, 64*1024+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"all missing", nil, 2},
		{"missing to", []string{"--subject", "Riepilogo", "--file", file}, 2},
		{"missing subject", []string{"--to", "operator@example.test", "--file", file}, 2},
		{"missing file", []string{"--to", "operator@example.test", "--subject", "Riepilogo"}, 2},
		{"positional argument", []string{"--to", "operator@example.test", "--subject", "Riepilogo", "--file", file, "extra"}, 2},
		{"unreadable file", []string{"--to", "operator@example.test", "--subject", "Riepilogo", "--file", filepath.Join(dir, "absent")}, 4},
		{"oversized file", []string{"--to", "operator@example.test", "--subject", "Riepilogo", "--file", file}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			args := append([]string{"send-note", "--config", filepath.Join(dir, "no-config.json")}, tc.args...)
			if code := run(args, &out, &errb); code != tc.want {
				t.Fatalf("exit=%d stderr=%q, want %d", code, errb.String(), tc.want)
			}
		})
	}
}

func TestRunSendNoteFlagsStayCommandSpecific(t *testing.T) {
	for _, name := range []string{"to", "subject", "file"} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{"version", "--" + name, "value"}, &out, &errb); code != 2 {
				t.Fatalf("exit=%d stderr=%q, want existing unknown-flag exit 2", code, errb.String())
			}
		})
	}
}
