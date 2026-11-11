package eval

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadGold_ValidatesWithoutEchoingContent(t *testing.T) {
	valid := `{"n":1,"mailbox":"a@x","internet_message_id":"<PRIVATE>","accept":["clienti","business.dev"],"needs_reply":false}`
	for name, input := range map[string]string{
		"empty": "", "malformed": "PRIVATE", "empty accept": strings.Replace(valid, `["clienti","business.dev"]`, `[]`, 1),
		"missing reply":   strings.Replace(valid, `,"needs_reply":false`, "", 1),
		"null reply":      strings.Replace(valid, `:false`, `:null`, 1),
		"unknown field":   strings.Replace(valid, `"needs_reply"`, `"PRIVATE"`, 1),
		"missing mailbox": strings.Replace(valid, `"a@x"`, `""`, 1),
		"duplicate":       valid + "\n" + valid, "trailing JSON": valid + " {}",
		"label content": strings.Replace(valid, `"clienti"`, `"PRIVATE BODY"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "gold.jsonl")
			if err := os.WriteFile(p, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadGold(p); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unsafe/missing error: %v", err)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "gold.jsonl")
	if err := os.WriteFile(p, []byte("\n"+valid+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadGold(p)
	if err != nil || len(rows) != 1 || rows[0].NeedsReply || rows[0].N != 1 || len(rows[0].Accept) != 2 {
		t.Fatalf("rows=%+v, err=%v", rows, err)
	}
}

func TestScore_ReplyErrorsAndTopConfusions(t *testing.T) {
	r := Report{Rows: []Row{
		{Mailbox: "a@x", Predicted: "clienti", Method: "llm", Status: "classified", ReplyDecision: true, Correct: true},
		{Mailbox: "a@x", Predicted: "clienti", Method: "llm", Status: "classified", ReplyDecision: true},
		{Mailbox: "b@x", Predicted: "unsure", Method: "llm", Status: "classified"},
		{Mailbox: "b@x", Predicted: "notifications", Method: "rule", Status: "classified", Correct: true},
		{Status: "missing"}, {Status: "budget_skipped"},
	}}
	g := []Gold{{Accept: []string{"clienti"}, NeedsReply: true}, {Accept: []string{"admin.docs"}}, {Accept: []string{"clienti"}, NeedsReply: true}, {Accept: []string{"notifications"}}, {}, {}}
	r.score(g)
	if r.Accuracy != (Count{2, 4}) || r.Coverage != (Count{3, 4}) || r.ReplyTP != 1 || r.ReplyFP != 1 || r.ReplyFN != 1 {
		t.Fatalf("metrics=%+v", r)
	}
	if r.ByMailbox["a@x"] != (Count{1, 2}) || r.ByMethod["llm"] != (Count{1, 3}) || r.ByBucket["clienti"] != (Count{1, 2}) {
		t.Fatalf("groups=%+v", r)
	}
	if !reflect.DeepEqual(r.Confusions, []Confusion{{"admin.docs", "clienti", 1}, {"clienti", "unsure", 1}}) {
		t.Fatalf("confusions=%+v", r.Confusions)
	}
	text := string(r.Markdown("model"))
	if !strings.Contains(text, "Reply precision: 1/2 (50.00%)") || !strings.Contains(text, "Reply recall: 1/2 (50.00%)") {
		t.Fatal(text)
	}
	var many Report
	var manyGold []Gold
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		many.Rows = append(many.Rows, Row{Predicted: "unsure", Status: "classified"})
		manyGold = append(manyGold, Gold{Accept: []string{label}})
	}
	// The most frequent confusion must sort ahead of alphabetic ties.
	many.Rows = append(many.Rows, Row{Predicted: "unsure", Status: "classified"})
	manyGold = append(manyGold, Gold{Accept: []string{"l"}})
	many.score(manyGold)
	if len(many.Confusions) != 10 || many.Confusions[0] != (Confusion{"l", "unsure", 2}) || many.Confusions[1].Expected != "a" {
		t.Fatalf("top ten=%+v", many.Confusions)
	}
}

func TestReportMarkdown_BudgetStoppedNamesBothCaps(t *testing.T) {
	r := Report{BudgetStopped: true}
	text := string(r.Markdown("model"))
	if !strings.Contains(text, "daily tokens or monthly cost") {
		t.Fatalf("budget stop must name both caps:\n%s", text)
	}
}

func TestReportWrite_PrivateAndNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := Report{Rows: []Row{{N: 1, Status: "missing"}}}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	paths, err := r.Write(dir, "../../model", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || filepath.Dir(paths[0]) != dir {
		t.Fatalf("paths=%v", paths)
	}
	before, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(dir, "../../model", now); err == nil {
		t.Fatal("overwrote previous report")
	}
	after, err := os.ReadFile(paths[0])
	if err != nil || string(after) != string(before) {
		t.Fatal("previous report changed")
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("mode=%v, err=%v", st, err)
	}
}
