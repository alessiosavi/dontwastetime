package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	previewChars = 300
	threadChars  = 4000
	pageChars    = 40000
	messageChars = 20000
	searchCap    = 500
)

type pageState struct {
	limit, offset int
	asOf          time.Time
}

type cursor struct {
	Offset int   `json:"o"`
	AsOf   int64 `json:"a"`
}

func (p Page) parse(now time.Time) (pageState, error) {
	r := pageState{limit: p.Limit, asOf: time.UnixMilli(now.UnixMilli()).UTC()}
	if p.Limit < 0 {
		return r, invalid("limit deve essere positivo")
	}
	if r.limit == 0 {
		r.limit = defaultLimit
	}
	r.limit = min(r.limit, maxLimit)
	if p.Cursor == "" {
		return r, nil
	}
	if len(p.Cursor) > 1024 {
		return r, invalid("cursore non valido")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return r, invalid("cursore non valido")
	}
	var c struct {
		Offset *int   `json:"o"`
		AsOf   *int64 `json:"a"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return r, invalid("cursore non valido")
	}
	if err := d.Decode(new(any)); err != io.EOF || c.Offset == nil || c.AsOf == nil ||
		*c.Offset < 0 || *c.Offset > int(^uint(0)>>1)-maxLimit-1 || *c.AsOf < 0 || *c.AsOf > now.UnixMilli() {
		return r, invalid("cursore non valido")
	}
	r.offset, r.asOf = *c.Offset, time.UnixMilli(*c.AsOf).UTC()
	return r, nil
}

func (p pageState) next(offset int) string {
	raw, _ := json.Marshal(cursor{Offset: offset, AsOf: p.asOf.UnixMilli()})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (r Range) parse(now time.Time) (since, until *time.Time, err error) {
	if since, err = parseRangeTime(r.Since, now); err != nil {
		return nil, nil, err
	}
	if until, err = parseRangeTime(r.Until, now); err != nil {
		return nil, nil, err
	}
	if since != nil && until != nil && !since.Before(*until) {
		return nil, nil, invalid("since deve precedere until")
	}
	return since, until, nil
}

func parseRangeTime(value string, now time.Time) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, value, config.Rome); err == nil {
		t = t.UTC()
		return &t, nil
	}
	if len(value) >= 2 {
		unit := time.Hour
		switch value[len(value)-1] {
		case 'd':
			unit *= 24
		case 'h':
		default:
			return nil, invalid("data non valida: usare YYYY-MM-DD, nd o nh")
		}
		digits := value[:len(value)-1]
		if strings.Trim(digits, "0123456789") == "" {
			n, err := strconv.ParseInt(digits, 10, 64)
			if err == nil && n > 0 && n <= int64(1<<63-1)/int64(unit) {
				t := now.Add(-time.Duration(n) * unit).UTC()
				return &t, nil
			}
		}
	}
	return nil, invalid("data non valida: usare YYYY-MM-DD, nd o nh")
}

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

// clip counts Unicode characters, without adding an ellipsis to the limit.
func clip(text string, limit int) (string, bool) {
	n := 0
	for i := range text {
		if n == limit {
			return text[:i], true
		}
		n++
	}
	return text, false
}
