package store

import "strings"

// SearchHit identifies a message and its raw FTS5 relevance score.
type SearchHit struct {
	ID   int64
	BM25 float64
}

// FTSQuery quotes whitespace-separated tokens as literal FTS5 phrases joined
// by implicit AND. Empty input returns ok=false and must not be used with MATCH.
func FTSQuery(user string) (match string, ok bool) {
	tokens := strings.Fields(user)
	if len(tokens) == 0 {
		return "", false
	}
	for i, token := range tokens {
		tokens[i] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	}
	return strings.Join(tokens, " "), true
}
