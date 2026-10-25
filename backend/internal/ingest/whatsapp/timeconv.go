package whatsapp

import "time"

// dstResult explains how a wall-clock time was mapped to an instant.
type dstResult int

const (
	dstNormal  dstResult = iota
	dstGap               // non-existent local time, shifted forward
	dstOverlap           // ambiguous local time, one of two offsets chosen
)

// toInstant converts a naive wall-clock time (fields of wall, Location
// ignored) in loc to an absolute instant, applying the DST rules of
// docs/parsers/whatsapp.md §10:
//
//   - Gap (e.g. 02:30 on a spring-forward night does not exist): interpret
//     with the offset in force before the transition, which shifts the clock
//     forward by the size of the gap (02:30 EST → 03:30 EDT).
//   - Overlap (e.g. 01:30 on a fall-back night happens twice): choose the
//     earlier instant unless that would put the message before the previous
//     one, in which case choose the later instant.
//
// time.Date does not document which offset it picks in these cases, so the
// candidates are computed explicitly: the offsets in force a day before and a
// day after, each checked for consistency.
func toInstant(wall time.Time, loc *time.Location, prev time.Time) (time.Time, dstResult) {
	naive := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC)
	offsetAt := func(t time.Time) int { _, off := t.In(loc).Zone(); return off }
	before := offsetAt(naive.Add(-26 * time.Hour))
	after := offsetAt(naive.Add(26 * time.Hour))

	var valid []time.Time
	for _, off := range uniq(before, after) {
		t := naive.Add(-time.Duration(off) * time.Second)
		if offsetAt(t) == off {
			valid = append(valid, t)
		}
	}
	switch len(valid) {
	case 1:
		return valid[0], dstNormal
	case 0:
		return naive.Add(-time.Duration(before) * time.Second), dstGap
	}
	early, late := valid[0], valid[1]
	if late.Before(early) {
		early, late = late, early
	}
	if !prev.IsZero() && early.Before(prev) {
		return late, dstOverlap
	}
	return early, dstOverlap
}

func uniq(a, b int) []int {
	if a == b {
		return []int{a}
	}
	return []int{a, b}
}
