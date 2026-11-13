package analytics

import (
	"errors"
	"fmt"
	"time"
)

// WindowKind names a window type (docs/04-metrics-catalog.md §2.1).
type WindowKind string

// Window kinds.
const (
	WindowDay    WindowKind = "day"
	WindowWeek   WindowKind = "week"
	WindowMonth  WindowKind = "month"
	WindowYear   WindowKind = "year"
	WindowAll    WindowKind = "all"
	WindowCustom WindowKind = "custom"
)

// ErrEmptyChat is returned when an "all" window is requested for a chat with
// no human messages.
var ErrEmptyChat = errors.New("analytics: chat has no messages")

// ErrInvalidWindow wraps every user-correctable query error (bad kind,
// reversed custom range) so the API can answer 400 instead of 500.
var ErrInvalidWindow = errors.New("invalid window")

// Query is a metrics request (FR-ANA-001).
type Query struct {
	Kind WindowKind
	// Anchor is any date inside the wanted day/week/month/year. The zero
	// value (1970-01-01, long before any chat app existed) means "unset" and
	// resolves to the chat's last active day, so the default view is the most
	// recent period with messages.
	Anchor Date
	// From and To bound a custom window; To is exclusive (the API converts
	// the UI's inclusive end date).
	From, To Date
	// Compare asks for the previous equivalent window (FR-ANA-002).
	Compare bool
	// Metrics selects metric IDs ("M-001"…); empty means all.
	Metrics []string
}

// Window is a resolved half-open interval [From, To) of local dates in the
// chat's analysis timezone (§2.1).
type Window struct {
	Kind WindowKind `json:"kind"`
	From Date       `json:"from"`
	To   Date       `json:"to"`
	TZ   string     `json:"tz"`
	// Days is the number of calendar dates in the window; DST never changes it.
	Days int `json:"days"`
	// Coverage is the share of the window's days that lie inside the chat's
	// first..last message range.
	Coverage float64 `json:"coverage"`
	// Partial is true when the window extends past the last imported message
	// or into the future (e.g. the current week).
	Partial bool `json:"partial"`
}

// ResolveWindow turns a query into concrete dates. chatFirst/chatLast are the
// local dates of the first and last human messages (chatLast < chatFirst
// means the chat is empty); today is the current local date.
func ResolveWindow(q Query, s Settings, chatFirst, chatLast, today Date, tz string) (Window, error) {
	w := Window{Kind: q.Kind, TZ: tz}
	a := q.Anchor
	if a == 0 {
		a = chatLast
	}
	switch q.Kind {
	case WindowDay:
		w.From, w.To = a, a+1
	case WindowWeek:
		w.From = weekStart(a, s.WeekStart)
		w.To = w.From + 7
	case WindowMonth:
		y, m, _ := a.Time().Date()
		w.From = DateFromYMD(y, m, 1)
		w.To = DateFromYMD(y, m+1, 1)
	case WindowYear:
		y := a.Time().Year()
		w.From = DateFromYMD(y, time.January, 1)
		w.To = DateFromYMD(y+1, time.January, 1)
	case WindowAll:
		if chatLast < chatFirst {
			return Window{}, ErrEmptyChat
		}
		w.From, w.To = chatFirst, chatLast+1
	case WindowCustom:
		if q.To <= q.From {
			return Window{}, fmt.Errorf("%w: custom window end must be after its start", ErrInvalidWindow)
		}
		w.From, w.To = q.From, q.To
	default:
		return Window{}, fmt.Errorf("%w: unknown kind %q (use day, week, month, year, all or custom)", ErrInvalidWindow, q.Kind)
	}
	w.Days = int(w.To - w.From)
	w.SetCoverage(chatFirst, chatLast, today)
	return w, nil
}

// SetCoverage fills Coverage and Partial from the chat's message range.
func (w *Window) SetCoverage(chatFirst, chatLast, today Date) {
	w.Days = int(w.To - w.From)
	if w.Days <= 0 {
		w.Coverage = 0
		return
	}
	covered := 0
	if chatLast >= chatFirst {
		lo, hi := max(w.From, chatFirst), min(w.To, chatLast+1)
		if hi > lo {
			covered = int(hi - lo)
		}
	}
	w.Coverage = float64(covered) / float64(w.Days)
	w.Partial = w.To > today+1 || chatLast < chatFirst || w.To > chatLast+1
}

// PreviousWindow returns the comparison window of §2.2, or nil for "all".
// Coverage is not set; the caller fills it with SetCoverage.
func PreviousWindow(w Window, s Settings) *Window {
	p := Window{Kind: w.Kind, TZ: w.TZ}
	switch w.Kind {
	case WindowDay:
		p.From, p.To = w.From-1, w.From
	case WindowWeek:
		p.From, p.To = w.From-7, w.From
	case WindowMonth:
		y, m, _ := w.From.Time().Date()
		p.From, p.To = DateFromYMD(y, m-1, 1), w.From
	case WindowYear:
		y := w.From.Time().Year()
		p.From, p.To = DateFromYMD(y-1, time.January, 1), w.From
	case WindowCustom:
		p.From, p.To = w.From.AddDays(-w.Days), w.From
	default:
		return nil
	}
	p.Days = int(p.To - p.From)
	return &p
}

// weekStart returns the first day of the week containing d.
func weekStart(d Date, start time.Weekday) Date {
	off := (int(d.Weekday()) - int(start) + 7) % 7
	return d - Date(off)
}
