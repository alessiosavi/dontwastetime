package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func ratio(c Count) string {
	if c.Total == 0 {
		return fmt.Sprintf("%d/0 (n/a)", c.Correct)
	}
	return fmt.Sprintf("%d/%d (%.2f%%)", c.Correct, c.Total, 100*float64(c.Correct)/float64(c.Total))
}

func cell(s string) string {
	s = html.EscapeString(s)
	return strings.NewReplacer("|", "&#124;", "\n", " ", "\r", " ", "`", "&#96;").Replace(s)
}

// Markdown renders the summary without any email text or message identifiers.
func (r Report) Markdown(model string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Classification evaluation\n\nModel: %s\n\n", cell(model))
	fmt.Fprintf(&b, "Accuracy: %s\n\nCoverage: %s\n\n", ratio(r.Accuracy), ratio(r.Coverage))
	fmt.Fprintf(&b, "Rows: %d; scored: %d; Missing: %d; Budget skipped: %d\n\n", len(r.Rows), r.Accuracy.Total, r.Missing, r.BudgetSkipped)
	if r.BudgetStopped {
		fmt.Fprintf(&b, "Stopped: %v; metrics cover completed rows only.\n\n", store.ErrBudgetReached)
	}
	fmt.Fprintf(&b, "Reply precision: %s\n\nReply recall: %s\n\n", ratio(Count{r.ReplyTP, r.ReplyTP + r.ReplyFP}), ratio(Count{r.ReplyTP, r.ReplyTP + r.ReplyFN}))
	fmt.Fprintf(&b, "LLM tokens: %d (input %d, output %d)\n\n", r.Usage.In+r.Usage.Out, r.Usage.In, r.Usage.Out)
	b.WriteString("All metrics exclude missing and budget-skipped rows; coverage means predicted != unsure.\n\n")
	for _, group := range []struct {
		name string
		m    map[string]Count
	}{{"Mailbox", r.ByMailbox}, {"Method", r.ByMethod}, {"Bucket (accept[0])", r.ByBucket}} {
		fmt.Fprintf(&b, "## %s\n\n| %s | Correct/total (accuracy) |\n| --- | --- |\n", group.name, group.name)
		for _, key := range slices.Sorted(maps.Keys(group.m)) {
			fmt.Fprintf(&b, "| %s | %s |\n", cell(key), ratio(group.m[key]))
		}
		b.WriteByte('\n')
	}
	b.WriteString("## Top 10 confusions\n\n| accept[0] → predicted | Count |\n| --- | --- |\n")
	for _, confusion := range r.Confusions {
		fmt.Fprintf(&b, "| %s → %s | %d |\n", cell(confusion.Expected), cell(confusion.Predicted), confusion.Total)
	}
	b.WriteString("\n## Skipped rows\n\n| n | Mailbox | Status |\n| --- | --- | --- |\n")
	for _, row := range r.Rows {
		if row.Status != "classified" {
			fmt.Fprintf(&b, "| %d | %s | %s |\n", row.N, cell(row.Mailbox), row.Status)
		}
	}
	return b.Bytes()
}

// Write creates private report artifacts without overwriting previous runs.
// Model slugs cannot escape the output directory. Existing target directories
// are tightened to 0700; any newly created parents are also private.
func (r Report) Write(dir, model string, now time.Time) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	slug := strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c) {
			return c
		}
		return '_'
	}, model)
	slug = strings.Trim(slug, ".")
	if slug == "" {
		slug = "model"
	}
	if len(slug) > 128 {
		slug = slug[:128]
	}
	base := filepath.Join(dir, now.Format("20060102-150405")+"-"+slug)
	var rows bytes.Buffer
	enc := json.NewEncoder(&rows)
	for _, row := range r.Rows {
		if err := enc.Encode(row); err != nil {
			return nil, err
		}
	}
	var paths []string
	for _, file := range []struct {
		ext  string
		data []byte
	}{{".md", r.Markdown(model)}, {".jsonl", rows.Bytes()}} {
		path := base + file.ext
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			for _, created := range paths {
				_ = os.Remove(created)
			}
			return nil, err
		}
		_, writeErr := io.Copy(f, bytes.NewReader(file.data))
		closeErr := f.Close()
		paths = append(paths, path)
		if writeErr != nil || closeErr != nil {
			for _, created := range paths {
				_ = os.Remove(created)
			}
			if writeErr != nil {
				return nil, writeErr
			}
			return nil, closeErr
		}
	}
	return paths, nil
}
