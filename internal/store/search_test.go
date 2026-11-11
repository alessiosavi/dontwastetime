package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func assertFTSHits(t *testing.T, s *Store, match string, want ...int64) {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT rowid FROM message_fts WHERE message_fts MATCH ? ORDER BY rowid`, match)
	if err != nil {
		t.Fatalf("FTS MATCH: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("FTS hits = %v, want %v", got, want)
	}
}

func TestFTS_InsertIsSearchable(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<fts@x>", now0)
	m.Subject, m.FromName, m.From = "preventivo", "Beatrice", "mittente@example.test"
	m.BodyText = strings.Repeat("testo ", 60) + "contratto"
	m.BodyPreview = "testo"
	id, _ := upsert(t, s, m)
	for _, match := range []string{"preventivo", "Beatrice", "mittente", "contratto"} {
		assertFTSHits(t, s, match, id)
	}
}

func TestFTS_DiacriticsInsensitive(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<accent@x>", now0)
	m.BodyText = "città"
	id, _ := upsert(t, s, m)
	assertFTSHits(t, s, "citta", id)
}

func TestFTS_OverlapUpdateReindexes(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<overlap@x>", now0)
	m.Subject, m.FromName, m.From, m.BodyText = "vecchiosoggetto", "vecchionome", "vecchioindirizzo@example.test", "vecchiocorpo"
	id, _ := upsert(t, s, m)
	for _, match := range []string{"vecchiosoggetto", "vecchionome", "vecchioindirizzo", "vecchiocorpo"} {
		assertFTSHits(t, s, match, id)
	}
	m.Subject, m.FromName, m.From, m.BodyText = "nuovosoggetto", "nuovonome", "nuovoindirizzo@example.test", "nuovocorpo"
	for range 2 {
		if updatedID, inserted := upsert(t, s, m); inserted || updatedID != id {
			t.Fatalf("overlap id=%d inserted=%v, want %d/false", updatedID, inserted, id)
		}
		for _, match := range []string{"vecchiosoggetto", "vecchionome", "vecchioindirizzo", "vecchiocorpo"} {
			assertFTSHits(t, s, match)
		}
		for _, match := range []string{"nuovosoggetto", "nuovonome", "nuovoindirizzo", "nuovocorpo"} {
			assertFTSHits(t, s, match, id)
		}
	}
}

func TestFTS_PurgedBodyStopsMatching(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<purge@x>", now0.Add(-48*time.Hour))
	m.Subject, m.BodyText = "conservato", "scaduto"
	id, _ := upsert(t, s, m)
	assertFTSHits(t, s, "scaduto", id)
	n, err := s.PurgeBodies(context.Background(), now0.Add(-24*time.Hour), now0)
	if err != nil || n != 1 {
		t.Fatalf("PurgeBodies = %d, %v; want 1, nil", n, err)
	}
	assertFTSHits(t, s, "scaduto")
	assertFTSHits(t, s, "conservato", id)
	// An overlap fetch must also leave the purged body out of the index.
	upsert(t, s, m)
	assertFTSHits(t, s, "scaduto")
	assertFTSHits(t, s, "conservato", id)
}

func TestFTS_DeleteStopsMatching(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<delete@x>", now0)
	m.BodyText = "eliminato"
	id, _ := upsert(t, s, m)
	assertFTSHits(t, s, "eliminato", id)
	if _, err := s.DB().ExecContext(context.Background(), `DELETE FROM message WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	assertFTSHits(t, s, "eliminato")
}

func TestOpen_FTSMigrationBackfillsExistingRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for i, ddl := range migrations[:5] {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	old := &Store{db: db}
	m := msg("m@example.test", "<legacy@x>", now0)
	m.Subject, m.FromName, m.From, m.BodyText = "archivio", "Alessandra", "storico@example.test", "documentazione"
	id, _ := upsert(t, old, m)
	m.InternetMessageID, m.Subject, m.BodyText = "<purged@x>", "metadati", "dimenticato"
	purgedID, _ := upsert(t, old, m)
	if _, err := db.Exec(`UPDATE message SET body_text = NULL, body_preview = NULL, body_purged_at = ? WHERE id = ?`, ts(now0), purgedID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		for _, match := range []string{"archivio", "documentazione"} {
			assertFTSHits(t, s, match, id)
		}
		for _, match := range []string{"Alessandra", "storico"} {
			assertFTSHits(t, s, match, id, purgedID)
		}
		assertFTSHits(t, s, "dimenticato")
		assertFTSHits(t, s, "metadati", purgedID)
		var version int
		if err := s.DB().QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != len(migrations) {
			t.Fatalf("schema version=%d, err=%v", version, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFTSQuery_LiteralTokens(t *testing.T) {
	s := openTest(t)
	m := msg("m@example.test", "<literal@x>", now0)
	m.Subject, m.FromName, m.From = "", "", ""
	m.BodyText = `"fattura" OR saldo NEAR(a b) -x a"b prefix`
	literalID, _ := upsert(t, s, m)
	m.InternetMessageID, m.BodyText = "<partial@x>", "fattura saldo prefixsuffix"
	partialID, _ := upsert(t, s, m)
	for _, tc := range []struct {
		name, user, want string
		ids              []int64
	}{
		{"syntax", `"fattura" OR *`, `"""fattura""" "OR" "*"`, []int64{literalID}},
		{"near", `NEAR(a b)`, `"NEAR(a" "b)"`, []int64{literalID}},
		{"minus", `-x`, `"-x"`, []int64{literalID}},
		{"quote", `a"b`, `"a""b"`, []int64{literalID}},
		{"or is a word", `fattura OR saldo`, `"fattura" "OR" "saldo"`, []int64{literalID}},
		{"no prefix expansion", `prefix*`, `"prefix*"`, []int64{literalID}},
		{"parentheses", `(fattura)`, `"(fattura)"`, []int64{literalID, partialID}},
		{"unicode whitespace", "\u2003fattura\tOR\nsaldo\u00a0", `"fattura" "OR" "saldo"`, []int64{literalID}},
		{"only quote", `"`, `""""`, nil},
		{"only star", `*`, `"*"`, nil},
		{"empty", "", "", nil},
		{"whitespace", " \t\n\u2003", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, ok := FTSQuery(tc.user)
			if match != tc.want || ok != (tc.want != "") {
				t.Fatalf("FTSQuery = %q, %v; want %q, %v", match, ok, tc.want, tc.want != "")
			}
			if ok {
				assertFTSHits(t, s, match, tc.ids...)
			}
		})
	}
}
