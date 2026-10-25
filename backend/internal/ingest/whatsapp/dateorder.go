package whatsapp

import (
	"fmt"
	"time"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// resolveDate interprets the raw date fields under order. Two-digit years are
// 2000+yy (WhatsApp launched in 2009, so no earlier data exists). It returns
// ok=false for impossible dates such as 31/02.
func resolveDate(h header, order ingest.DateOrder) (y, m, d int, ok bool) {
	expand := func(v, digits int) int {
		if digits <= 2 {
			return 2000 + v
		}
		return v
	}
	switch order {
	case ingest.DateOrderYMD:
		y, m, d = expand(h.f[0], h.digits[0]), h.f[1], h.f[2]
	case ingest.DateOrderMDY:
		m, d, y = h.f[0], h.f[1], expand(h.f[2], h.digits[2])
	default: // DMY
		d, m, y = h.f[0], h.f[1], expand(h.f[2], h.digits[2])
	}
	if y < 2000 || y > 2100 || m < 1 || m > 12 || d < 1 {
		return 0, 0, 0, false
	}
	// time.Date normalises overflow (Feb 30 → Mar 2); a round trip detects it.
	if time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Day() != d {
		return 0, 0, 0, false
	}
	return y, m, d, true
}

// hypothesis tracks how plausible one date order is while pass 1 streams
// through the headers (§4 steps 4–5). It keeps O(1) state plus a set of
// distinct days, which is bounded by the chat's calendar span.
type hypothesis struct {
	order      ingest.DateOrder
	prev       int64 // minutes since epoch of the previous valid header
	havePrev   bool
	violations int // headers more than a minute earlier than their predecessor
	invalid    int // headers that are not a real date under this order
	days       map[int64]struct{}
	minDay     int64
	maxDay     int64
}

func newHypothesis(o ingest.DateOrder) *hypothesis {
	return &hypothesis{order: o, days: map[int64]struct{}{}}
}

// maxTrackedDays caps the distinct-day set; 30k days is 80+ years.
const maxTrackedDays = 30000

func (hy *hypothesis) add(h header) {
	y, m, d, ok := resolveDate(h, hy.order)
	if !ok {
		hy.invalid++
		return
	}
	hour, min, _, _ := h.clock()
	day := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Unix() / 86400
	t := day*1440 + int64(hour*60+min)
	if hy.havePrev && t < hy.prev-1 {
		hy.violations++
	}
	hy.prev, hy.havePrev = t, true
	if len(hy.days) < maxTrackedDays {
		hy.days[day] = struct{}{}
	}
	if len(hy.days) == 1 || day < hy.minDay {
		hy.minDay = day
	}
	if len(hy.days) == 1 || day > hy.maxDay {
		hy.maxDay = day
	}
}

// density is distinct active days per calendar day spanned. The wrong order
// turns a run of consecutive days into dates a month apart, which lowers
// density sharply.
func (hy *hypothesis) density() float64 {
	if len(hy.days) == 0 {
		return 0
	}
	return float64(len(hy.days)) / float64(hy.maxDay-hy.minDay+1)
}

// orderStats accumulates the evidence for date-order inference during pass 1.
type orderStats struct {
	headers         int
	fourDigitFirst  int // first field has 4 digits (YMD rule)
	dayFirstHard    int // a > 12 and b ≤ 12: only DMY is possible
	monthFirstHard  int // b > 12 and a ≤ 12: only MDY is possible
	asymmetric      int // headers where a ≠ b, i.e. the order matters at all
	upperMer, lower int // meridiem case counts (locale hint)
	dmy, mdy        *hypothesis
}

func newOrderStats() *orderStats {
	return &orderStats{dmy: newHypothesis(ingest.DateOrderDMY), mdy: newHypothesis(ingest.DateOrderMDY)}
}

func (s *orderStats) add(h header) {
	s.headers++
	a, b := h.f[0], h.f[1]
	if h.digits[0] == 4 {
		s.fourDigitFirst++
	}
	if a > 12 && b <= 12 {
		s.dayFirstHard++
	}
	if b > 12 && a <= 12 {
		s.monthFirstHard++
	}
	if a != b {
		s.asymmetric++
	}
	if h.mer != 0 {
		if h.merLower {
			s.lower++
		} else {
			s.upperMer++
		}
	}
	s.dmy.add(h)
	s.mdy.add(h)
}

// decision is the outcome of inference, written to the import report.
type decision struct {
	order     ingest.DateOrder
	evidence  string
	ambiguous bool
	conflict  bool
}

// decide applies docs/parsers/whatsapp.md §4 steps 2–6 in order and explains
// which step settled it.
func (s *orderStats) decide() decision {
	// Step 2: ISO-like dates (YYYY-MM-DD) in ≥ 95% of headers.
	if s.headers > 0 && float64(s.fourDigitFirst) >= 0.95*float64(s.headers) {
		return decision{order: ingest.DateOrderYMD,
			evidence: fmt.Sprintf("step 2: year-first dates in %d of %d headers", s.fourDigitFirst, s.headers)}
	}
	// Day and month equal everywhere: every order yields the same dates.
	if s.asymmetric == 0 {
		return decision{order: ingest.DateOrderDMY,
			evidence: "day and month are equal in every header, so the order does not matter"}
	}
	// Step 3: hard evidence from a field that can only be a day.
	conflict := s.dayFirstHard > 0 && s.monthFirstHard > 0
	switch {
	case s.dayFirstHard > 0 && !conflict:
		return decision{order: ingest.DateOrderDMY,
			evidence: fmt.Sprintf("step 3: first field exceeded 12 in %d headers", s.dayFirstHard)}
	case s.monthFirstHard > 0 && !conflict:
		return decision{order: ingest.DateOrderMDY,
			evidence: fmt.Sprintf("step 3: second field exceeded 12 in %d headers", s.monthFirstHard)}
	}
	// Step 4: chats are chronological; the wrong order produces jumps back in
	// time whenever the month changes. Impossible dates count as violations.
	vd := s.dmy.violations + s.dmy.invalid
	vm := s.mdy.violations + s.mdy.invalid
	if better, ok := clearlyFewer(vd, vm); ok {
		o := ingest.DateOrderDMY
		if better == 1 {
			o = ingest.DateOrderMDY
		}
		return decision{order: o, conflict: conflict,
			evidence: fmt.Sprintf("step 4: order violations DMY=%d MDY=%d", vd, vm)}
	}
	// Step 5: prefer the denser hypothesis (see hypothesis.density). The spec
	// phrases this as "fewer distinct days relative to the range"; density is
	// the formulation that actually separates a right from a wrong order.
	dd, dm := s.dmy.density(), s.mdy.density()
	if dd >= 1.5*dm && dd > 0 {
		return decision{order: ingest.DateOrderDMY, conflict: conflict,
			evidence: fmt.Sprintf("step 5: day density DMY=%.2f MDY=%.2f", dd, dm)}
	}
	if dm >= 1.5*dd && dm > 0 {
		return decision{order: ingest.DateOrderMDY, conflict: conflict,
			evidence: fmt.Sprintf("step 5: day density DMY=%.2f MDY=%.2f", dd, dm)}
	}
	// Step 6: locale hint. Upper-case "AM/PM" is the en-US Android/iOS style
	// (MDY); lower-case "am/pm" and 24-hour clocks are en-GB/en-IN and most
	// other locales (DMY). The user must confirm (§4 step 7).
	o := ingest.DateOrderDMY
	if s.upperMer > s.lower {
		o = ingest.DateOrderMDY
	}
	return decision{order: o, ambiguous: true, conflict: conflict,
		evidence: "step 6: dates are ambiguous; order guessed from the clock style, please confirm"}
}

// clearlyFewer reports which of two violation counts is clearly smaller:
// better by ≥ 3 absolute or ≥ 10% relative (§4 step 4). It returns 0 for a,
// 1 for b.
func clearlyFewer(a, b int) (int, bool) {
	lo, hi, idx := a, b, 0
	if b < a {
		lo, hi, idx = b, a, 1
	}
	diff := hi - lo
	if diff == 0 {
		return 0, false
	}
	if diff >= 3 || float64(diff)/float64(hi) >= 0.10 {
		return idx, true
	}
	return 0, false
}
