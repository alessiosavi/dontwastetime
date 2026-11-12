package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
