// Package analytics is the deterministic analytics engine (docs/04-metrics-catalog.md).
// It has no LLM, network or database dependency: callers feed it messages and
// rollups, and it returns derived rows and metric results.
//
// Layout:
//
//	analytics        shared model (this contract), window resolution, metric computation
//	analytics/text   per-message text features: words, emoji, sentiment, lexicons
//
// The pipeline that reads from and writes to the store lives in internal/analysis.
package analytics

import (
	"fmt"
	"time"
)

// Date is a local calendar date, stored as days since 1970-01-01. Using an
// integer makes window arithmetic, map keys and comparisons trivial and keeps
// DST out of date maths (a day is always one step, never 23 or 25 hours).
type Date int32

// DateOf returns the calendar date of t in loc.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return DateFromYMD(y, m, d)
}

// DateFromYMD builds a Date from its fields; out-of-range days normalise like
// time.Date (Feb 30 → Mar 1/2).
func DateFromYMD(y int, m time.Month, d int) Date {
	return Date(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// ParseDate parses "YYYY-MM-DD".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return 0, fmt.Errorf("invalid date %q (want YYYY-MM-DD)", s)
	}
	return Date(t.Unix() / 86400), nil
}

// Time returns the date at 00:00 UTC; use only for its Y/M/D fields.
func (d Date) Time() time.Time { return time.Unix(int64(d)*86400, 0).UTC() }

// String formats as "YYYY-MM-DD".
func (d Date) String() string { return d.Time().Format(time.DateOnly) }

// AddDays returns d shifted by n days.
func (d Date) AddDays(n int) Date { return d + Date(n) }

// Weekday returns the day of the week.
func (d Date) Weekday() time.Weekday { return d.Time().Weekday() }

// Start returns the instant this date begins in loc (local midnight, or the
// first existing instant after it on a DST-gap day).
func (d Date) Start(loc *time.Location) time.Time {
	y, m, day := d.Time().Date()
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

// MarshalText implements encoding.TextMarshaler so Dates serialise as strings.
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Date) UnmarshalText(b []byte) error {
	v, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
