package config

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
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

func TestMonth_UsesRome(t *testing.T) {
	for _, tc := range []struct{ instant, want string }{
		{"2026-11-30T22:59:59Z", "2026-11"},
		{"2026-11-30T23:00:00Z", "2026-12"},
		{"2026-06-30T22:00:00Z", "2026-07"},
		{"2026-12-31T23:00:00Z", "2027-01"},
	} {
		t.Run(tc.instant, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.instant)
			if err != nil {
				t.Fatal(err)
			}
			if got := Month(at); got != tc.want {
				t.Fatalf("Month = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLLMCost(t *testing.T) {
	cfg := LLMConfig{PriceInPerMTok: 0.30, PriceOutPerMTok: 1.20}
	for _, tc := range []struct {
		in, out int
		want    float64
	}{
		{0, 0, 0},
		{2_000_000, 0, 0.60},
		{0, 3_000_000, 3.60},
		{2_000_000, 3_000_000, 4.20},
		{100, 20, 0.000054},
	} {
		if got := cfg.Cost(tc.in, tc.out); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("Cost(%d, %d) = %v, want %v", tc.in, tc.out, got, tc.want)
		}
	}
}

func TestConfig_MoneyCapNeedsPrices(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct {
		name, llm string
		wantError string
	}{
		{"missing prices", `{"monthly_max_cost":10}`, "monthly_max_cost needs price_in_per_mtok/price_out_per_mtok"},
		{"zero prices", `{"monthly_max_cost":10,"price_in_per_mtok":0,"price_out_per_mtok":0}`, "monthly_max_cost needs price_in_per_mtok/price_out_per_mtok"},
		{"negative cap", `{"monthly_max_cost":-1}`, "monthly_max_cost must be non-negative"},
		{"both prices", `{"monthly_max_cost":10,"price_in_per_mtok":0.30,"price_out_per_mtok":1.20}`, ""},
		{"input only", `{"monthly_max_cost":10,"price_in_per_mtok":0.30}`, ""},
		{"output only", `{"monthly_max_cost":10,"price_out_per_mtok":1.20}`, ""},
		{"disabled", `{"monthly_max_cost":0}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(minimalJSON, `"digest_sender"`, `"llm":`+tc.llm+`,"digest_sender"`, 1)
			_, err := LoadConfig(writeFile(t, "config.json", body))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want money cap error %q, got %v", tc.wantError, err)
				}
			} else if err != nil {
				t.Fatalf("valid money cap config rejected: %v", err)
			}
		})
	}
}

func TestConfig_MoneyCapEnvOverride(t *testing.T) {
	clearEnv(t)
	body := strings.Replace(minimalJSON, `"digest_sender"`, `"llm":{"monthly_max_cost":5,"price_in_per_mtok":0.30,"price_out_per_mtok":1.20},"digest_sender"`, 1)
	p := writeFile(t, "config.json", body)
	for _, tc := range []struct {
		value string
		want  float64
		bad   bool
	}{
		{"", 5, false}, {"10.25", 10.25, false}, {"0", 0, false},
		{"-1", 0, true}, {"bad", 0, true}, {"NaN", 0, true}, {"+Inf", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("LLM_MONTHLY_MAX_COST", tc.value)
			cfg, err := LoadConfig(p)
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "LLM_MONTHLY_MAX_COST") {
					t.Fatalf("want env error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if cfg.LLM.MonthlyMaxCost != tc.want {
				t.Fatalf("monthly cap = %v, want %v", cfg.LLM.MonthlyMaxCost, tc.want)
			}
		})
	}
}

func TestConfig_PricesJSON(t *testing.T) {
	clearEnv(t)
	for _, field := range []string{"price_in_per_mtok", "price_out_per_mtok"} {
		for _, value := range []string{"-0.30", "0", "0.30"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				body := strings.Replace(minimalJSON, `"digest_sender"`, `"llm":{"`+field+`":`+value+`},"digest_sender"`, 1)
				_, err := LoadConfig(writeFile(t, "config.json", body))
				if value == "-0.30" {
					if err == nil || !strings.Contains(err.Error(), "llm."+field) {
						t.Fatalf("negative JSON price must be rejected naming %s, got %v", field, err)
					}
				} else if err != nil {
					t.Fatalf("valid JSON price rejected: %v", err)
				}
			})
		}
	}
}

func TestConfig_PricesEnv(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "config.json", minimalJSON)
	for _, key := range []string{"LLM_PRICE_IN_PER_MTOK", "LLM_PRICE_OUT_PER_MTOK"} {
		for _, value := range []string{"-0.30", "NaN", "Inf", "+Inf", "-Inf", "bad", "0", "0.30"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Setenv(key, value)
				cfg, err := LoadConfig(p)
				if value != "0" && value != "0.30" {
					if err == nil || !strings.Contains(err.Error(), key) {
						t.Fatalf("invalid env price must be rejected naming %s, got %v", key, err)
					}
				} else if err != nil {
					t.Fatalf("valid env price rejected: %v", err)
				} else {
					got := cfg.LLM.PriceInPerMTok
					if key == "LLM_PRICE_OUT_PER_MTOK" {
						got = cfg.LLM.PriceOutPerMTok
					}
					want := 0.0
					if value == "0.30" {
						want = 0.30
					}
					if got != want {
						t.Fatalf("env price = %v, want %v", got, want)
					}
				}
			})
		}
	}
}

func TestConfig_ValidatePrices(t *testing.T) {
	clearEnv(t)
	for _, field := range []string{"price_in_per_mtok", "price_out_per_mtok"} {
		for _, value := range []float64{-0.30, math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(field+"/"+strconv.FormatFloat(value, 'g', -1, 64), func(t *testing.T) {
				cfg, err := LoadConfig(writeFile(t, "config.json", minimalJSON))
				if err != nil {
					t.Fatal(err)
				}
				if field == "price_in_per_mtok" {
					cfg.LLM.PriceInPerMTok = value
				} else {
					cfg.LLM.PriceOutPerMTok = value
				}
				if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "llm."+field) {
					t.Fatalf("invalid price must fail validation naming %s, got %v", field, err)
				}
			})
		}
	}
}
