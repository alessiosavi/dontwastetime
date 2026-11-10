// Package config loads dwt's configuration: config.json for everything
// non-secret, process env (optionally filled from a gitignored .env) for
// credentials. Env wins over config.json. Loading is fail-closed: any missing
// or invalid required field is an error naming the field.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults.
const (
	DefaultPollSeconds         = 180
	DefaultPollOverlapMinutes  = 30
	DefaultWorkers             = 4
	DefaultRetentionDays       = 365
	DefaultInitialDays         = 90
	DefaultDailyMaxTokens      = 2_000_000
	DefaultConfidenceThreshold = 0.70
	MinPollSeconds             = 60
	MaxPollSeconds             = 600
)

// bannedSenders may never send the digest (house precedent:
// internal-ops-assistant BANNED_SENDERS).
var bannedSenders = map[string]bool{"gm@example.com": true}

// envKeys lists every env var LoadConfig reads.
var envKeys = []string{
	"MS_TENANT_ID", "MS_CLIENT_ID", "MS_CLIENT_SECRET",
	"LLM_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_DAILY_MAX_TOKENS",
	"LLM_PRICE_IN_PER_MTOK", "LLM_PRICE_OUT_PER_MTOK", "DWT_STATE_DIR",
}

// Config is the whole runtime configuration.
type Config struct {
	Mailboxes          []MailboxConfig `json:"mailboxes"`
	DigestRecipients   []string        `json:"digest_recipients"`
	AlertRecipients    []string        `json:"alert_recipients"`
	DigestSender       string          `json:"digest_sender"`
	LLM                LLMConfig       `json:"llm"`
	Graph              GraphConfig     `json:"-"`
	RetentionDays      int             `json:"retention_days"`
	StateDir           string          `json:"state_dir"`
	TopClients         []string        `json:"top_clients"`
	PollSeconds        int             `json:"poll_seconds"`
	PollOverlapMinutes int             `json:"poll_overlap_minutes"`
	Workers            int             `json:"workers"`
	InitialDays        int             `json:"initial_days"`
}

// MailboxConfig is one followed mailbox. Digest=false keeps a mailbox
// classified for coverage but out of the digest (employee personal boxes).
type MailboxConfig struct {
	Address     string `json:"address"`
	Enabled     bool   `json:"enabled"`
	Digest      bool   `json:"digest"`
	PollSeconds int    `json:"poll_seconds,omitempty"`
}

// LLMConfig configures the OpenAI-compatible classifier endpoint. APIKey is
// env-only.
type LLMConfig struct {
	URL                 string  `json:"url"`
	Model               string  `json:"model"`
	APIKey              string  `json:"-"`
	DailyMaxTokens      int     `json:"daily_max_tokens"`
	PriceInPerMTok      float64 `json:"price_in_per_mtok"`
	PriceOutPerMTok     float64 `json:"price_out_per_mtok"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
}

// GraphConfig holds the app-only Microsoft Graph credential. Env-only.
type GraphConfig struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

// LoadConfig reads path, applies env overrides and defaults, and validates.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyEnv() error {
	setStr := func(dst *string, key string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	setStr(&c.Graph.TenantID, "MS_TENANT_ID")
	setStr(&c.Graph.ClientID, "MS_CLIENT_ID")
	setStr(&c.Graph.ClientSecret, "MS_CLIENT_SECRET")
	setStr(&c.LLM.URL, "LLM_URL")
	setStr(&c.LLM.Model, "LLM_MODEL")
	setStr(&c.LLM.APIKey, "LLM_API_KEY")
	setStr(&c.StateDir, "DWT_STATE_DIR")
	if v := os.Getenv("LLM_DAILY_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("config: LLM_DAILY_MAX_TOKENS: invalid %q", v)
		}
		c.LLM.DailyMaxTokens = n
	}
	for key, dst := range map[string]*float64{
		"LLM_PRICE_IN_PER_MTOK":  &c.LLM.PriceInPerMTok,
		"LLM_PRICE_OUT_PER_MTOK": &c.LLM.PriceOutPerMTok,
	} {
		if v := os.Getenv(key); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return fmt.Errorf("config: %s: invalid %q", key, v)
			}
			*dst = f
		}
	}
	return nil
}

func (c *Config) applyDefaults() error {
	if c.PollSeconds == 0 {
		c.PollSeconds = DefaultPollSeconds
	}
	if c.PollOverlapMinutes == 0 {
		c.PollOverlapMinutes = DefaultPollOverlapMinutes
	}
	if c.Workers == 0 {
		c.Workers = DefaultWorkers
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = DefaultRetentionDays
	}
	if c.InitialDays == 0 {
		c.InitialDays = DefaultInitialDays
	}
	if c.LLM.DailyMaxTokens == 0 {
		c.LLM.DailyMaxTokens = DefaultDailyMaxTokens
	}
	if c.LLM.ConfidenceThreshold == 0 {
		c.LLM.ConfidenceThreshold = DefaultConfidenceThreshold
	}
	if len(c.AlertRecipients) == 0 {
		c.AlertRecipients = append([]string(nil), c.DigestRecipients...)
	}
	for i := range c.Mailboxes {
		c.Mailboxes[i].Address = strings.ToLower(strings.TrimSpace(c.Mailboxes[i].Address))
		if c.Mailboxes[i].PollSeconds == 0 {
			c.Mailboxes[i].PollSeconds = c.PollSeconds
		}
	}
	for i, d := range c.TopClients {
		c.TopClients[i] = strings.ToLower(strings.TrimSpace(d))
	}
	if c.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("config: state_dir: no home directory: %w", err)
		}
		c.StateDir = filepath.Join(home, "Library", "Application Support", "dontwastetime")
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Mailboxes) == 0 {
		return errors.New("config: mailboxes: at least one mailbox is required")
	}
	seen := map[string]bool{}
	for _, mb := range c.Mailboxes {
		if !ValidAddress(mb.Address) {
			return fmt.Errorf("config: mailboxes: invalid address %q", mb.Address)
		}
		if seen[mb.Address] {
			return fmt.Errorf("config: mailboxes: duplicate address %q", mb.Address)
		}
		seen[mb.Address] = true
		if mb.PollSeconds < MinPollSeconds || mb.PollSeconds > MaxPollSeconds {
			return fmt.Errorf("config: mailboxes: %s poll_seconds %d outside %d..%d", mb.Address, mb.PollSeconds, MinPollSeconds, MaxPollSeconds)
		}
	}
	if len(c.DigestRecipients) == 0 {
		return errors.New("config: digest_recipients: required")
	}
	if err := validList("digest_recipients", c.DigestRecipients); err != nil {
		return err
	}
	if err := validList("alert_recipients", c.AlertRecipients); err != nil {
		return err
	}
	if c.DigestSender == "" {
		return errors.New("config: digest_sender: required")
	}
	if !ValidAddress(c.DigestSender) {
		return fmt.Errorf("config: digest_sender: invalid address %q", c.DigestSender)
	}
	if bannedSenders[strings.ToLower(c.DigestSender)] {
		return fmt.Errorf("config: digest_sender: %q is a banned sender", c.DigestSender)
	}
	if c.PollSeconds < MinPollSeconds || c.PollSeconds > MaxPollSeconds {
		return fmt.Errorf("config: poll_seconds %d outside %d..%d", c.PollSeconds, MinPollSeconds, MaxPollSeconds)
	}
	if c.PollOverlapMinutes < 0 || c.Workers < 1 || c.RetentionDays < 1 || c.InitialDays < 1 {
		return errors.New("config: poll_overlap_minutes, workers, retention_days and initial_days must be positive")
	}
	if t := c.LLM.ConfidenceThreshold; t <= 0 || t > 1 {
		return fmt.Errorf("config: llm.confidence_threshold %v outside (0,1]", t)
	}
	if c.LLM.DailyMaxTokens < 0 {
		return errors.New("config: llm.daily_max_tokens must be positive")
	}
	return nil
}

func validList(field string, addrs []string) error {
	for _, a := range addrs {
		if !ValidAddress(a) {
			return fmt.Errorf("config: %s: invalid address %q", field, a)
		}
	}
	return nil
}

// ValidAddress reports whether s is a bare email address (no display name).
func ValidAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s && strings.Contains(s, "@")
}

// RequireGraph fails closed when the Graph credential is incomplete. Commands
// that talk to Graph call it at boot.
func (c *Config) RequireGraph() error {
	var missing []string
	if c.Graph.TenantID == "" {
		missing = append(missing, "MS_TENANT_ID")
	}
	if c.Graph.ClientID == "" {
		missing = append(missing, "MS_CLIENT_ID")
	}
	if c.Graph.ClientSecret == "" {
		missing = append(missing, "MS_CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing Graph credential env: %s", strings.Join(missing, ", "))
	}
	return nil
}

// RequireLLM fails closed when the LLM endpoint is incomplete. Commands that
// call the classifier call it at boot.
func (c *Config) RequireLLM() error {
	var missing []string
	if c.LLM.URL == "" {
		missing = append(missing, "LLM_URL")
	}
	if c.LLM.Model == "" {
		missing = append(missing, "LLM_MODEL")
	}
	if c.LLM.APIKey == "" {
		missing = append(missing, "LLM_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing LLM env: %s", strings.Join(missing, ", "))
	}
	return nil
}

// LoadEnvFile fills the process env from a KEY=VALUE file. Variables already
// set are never overwritten. Blank lines, comments and an optional "export "
// prefix are accepted; surrounding quotes are trimmed. A missing file returns
// an error wrapping fs.ErrNotExist so callers can treat it as optional.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("env file %s:%d: expected KEY=VALUE", path, n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("env file %s:%d: %w", path, n, err)
		}
	}
	return sc.Err()
}
