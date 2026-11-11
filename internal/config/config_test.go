package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalJSON = `{
  "mailboxes": [{"address": "marketing@example.com", "enabled": true, "digest": true}],
  "digest_recipients": ["founder@example.com"],
  "digest_sender": "admin@example.com"
}`

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearEnv blanks every env var LoadConfig reads, so the developer's shell
// cannot leak into a test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadConfig(writeFile(t, "config.json", minimalJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PollSeconds != 180 || cfg.PollOverlapMinutes != 30 || cfg.Workers != 4 || cfg.RetentionDays != 365 || cfg.InitialDays != 90 {
		t.Errorf("defaults: poll=%d overlap=%d workers=%d retention=%d initial=%d", cfg.PollSeconds, cfg.PollOverlapMinutes, cfg.Workers, cfg.RetentionDays, cfg.InitialDays)
	}
	if cfg.LLM.ConfidenceThreshold != 0.70 || cfg.LLM.DailyMaxTokens != 2000000 {
		t.Errorf("llm defaults: threshold=%v max=%d", cfg.LLM.ConfidenceThreshold, cfg.LLM.DailyMaxTokens)
	}
	if got := strings.Join(cfg.AlertRecipients, ","); got != "founder@example.com" {
		t.Errorf("alert recipients default to digest recipients, got %q", got)
	}
	if cfg.Mailboxes[0].PollSeconds != 180 {
		t.Errorf("mailbox poll inherits global, got %d", cfg.Mailboxes[0].PollSeconds)
	}
	if cfg.StateDir == "" {
		t.Error("state dir default empty")
	}
}

func TestLoadConfig_EnvOverridesConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("LLM_URL", "https://env.example/v1")
	t.Setenv("LLM_API_KEY", "k")
	t.Setenv("MS_TENANT_ID", "tenant")
	body := strings.Replace(minimalJSON, `"digest_sender"`, `"llm": {"url": "https://config.example/v1", "model": "m"}, "digest_sender"`, 1)
	cfg, err := LoadConfig(writeFile(t, "config.json", body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LLM.URL != "https://env.example/v1" {
		t.Errorf("env must win: got %q", cfg.LLM.URL)
	}
	if cfg.LLM.Model != "m" {
		t.Errorf("config value kept when env unset: got %q", cfg.LLM.Model)
	}
	if cfg.LLM.APIKey != "k" || cfg.Graph.TenantID != "tenant" {
		t.Errorf("secrets from env: key=%q tenant=%q", cfg.LLM.APIKey, cfg.Graph.TenantID)
	}
}

func TestLoadConfig_MissingRequired(t *testing.T) {
	clearEnv(t)
	cases := map[string]string{
		"digest_recipients": `{"mailboxes":[{"address":"a@example.com","enabled":true}],"digest_sender":"admin@example.com"}`,
		"digest_sender":     `{"mailboxes":[{"address":"a@example.com","enabled":true}],"digest_recipients":["f@example.com"]}`,
		"mailboxes":         `{"digest_recipients":["f@example.com"],"digest_sender":"admin@example.com"}`,
	}
	for field, body := range cases {
		_, err := LoadConfig(writeFile(t, "config.json", body))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s: want error naming it, got %v", field, err)
		}
	}
}

func TestLoadConfig_BannedSender(t *testing.T) {
	clearEnv(t)
	body := strings.Replace(minimalJSON, "admin@example.com", "GM@example.com", 1)
	_, err := LoadConfig(writeFile(t, "config.json", body))
	if err == nil || !strings.Contains(err.Error(), "digest_sender") {
		t.Fatalf("banned sender must be refused, got %v", err)
	}
}

func TestLoadConfig_InvalidMailbox(t *testing.T) {
	clearEnv(t)
	body := strings.Replace(minimalJSON, "marketing@example.com", "not-an-address", 1)
	_, err := LoadConfig(writeFile(t, "config.json", body))
	if err == nil || !strings.Contains(err.Error(), "not-an-address") {
		t.Fatalf("invalid mailbox must be refused naming it, got %v", err)
	}
}

func TestLoadConfig_MissingFileFailsClosed(t *testing.T) {
	clearEnv(t)
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing config file must be an error")
	}
}

func TestRequireGraph(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadConfig(writeFile(t, "config.json", minimalJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireGraph(); err == nil || !strings.Contains(err.Error(), "MS_TENANT_ID") {
		t.Fatalf("RequireGraph without creds: got %v", err)
	}
	cfg.Graph = GraphConfig{TenantID: "t", ClientID: "c", ClientSecret: "s"}
	if err := cfg.RequireGraph(); err != nil {
		t.Fatalf("RequireGraph with creds: %v", err)
	}
}

func TestLoadEnvFile_NoOverwrite(t *testing.T) {
	t.Setenv("DWT_TEST_KEEP", "shell")
	t.Setenv("DWT_TEST_FILL", "")
	os.Unsetenv("DWT_TEST_FILL")
	p := writeFile(t, ".env", "# comment\n\nDWT_TEST_KEEP=file\nexport DWT_TEST_FILL=\"from file\"\n")
	if err := LoadEnvFile(p); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("DWT_TEST_KEEP"); got != "shell" {
		t.Errorf("existing var overwritten: %q", got)
	}
	if got := os.Getenv("DWT_TEST_FILL"); got != "from file" {
		t.Errorf("unset var not filled (quotes trimmed): %q", got)
	}
}

func TestDay_UsesRome(t *testing.T) {
	// 23:30 UTC on Nov 10 is already Nov 11 in Rome (UTC+1).
	if got := Day(time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)); got != "2026-11-11" {
		t.Fatalf("Day = %s", got)
	}
}
