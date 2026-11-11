package config

import (
	"time"
	_ "time/tzdata" // embedded zone data: the digest schedule must not depend on the host
)

// Rome is the business time zone: budget days/months, digest schedule and dates.
var Rome = mustLoad("Europe/Rome")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("config: time zone " + name + ": " + err.Error())
	}
	return loc
}

// Day is the Europe/Rome calendar day of t as YYYY-MM-DD.
func Day(t time.Time) string { return t.In(Rome).Format(time.DateOnly) }

// Month is the Europe/Rome calendar month of t as YYYY-MM.
func Month(t time.Time) string { return t.In(Rome).Format("2006-01") }
