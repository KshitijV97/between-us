package analytics

import (
	"testing"
	"time"
)

func d(y int, m time.Month, day int) Date { return DateFromYMD(y, m, day) }

func TestResolveWindowKinds(t *testing.T) {
	s := DefaultSettings()
	first, last, today := d(2023, 1, 1), d(2026, 10, 20), d(2026, 11, 13)
	tests := []struct {
		name     string
		q        Query
		from, to Date
	}{
		{"day", Query{Kind: WindowDay, Anchor: d(2024, 3, 9)}, d(2024, 3, 9), d(2024, 3, 10)},
		{"iso week from wednesday", Query{Kind: WindowWeek, Anchor: d(2026, 3, 4)}, d(2026, 3, 2), d(2026, 3, 9)},
		{"iso week from sunday", Query{Kind: WindowWeek, Anchor: d(2026, 3, 8)}, d(2026, 3, 2), d(2026, 3, 9)},
		{"february leap year", Query{Kind: WindowMonth, Anchor: d(2024, 2, 15)}, d(2024, 2, 1), d(2024, 3, 1)},
		{"february common year", Query{Kind: WindowMonth, Anchor: d(2025, 2, 28)}, d(2025, 2, 1), d(2025, 3, 1)},
		{"december rolls year", Query{Kind: WindowMonth, Anchor: d(2025, 12, 31)}, d(2025, 12, 1), d(2026, 1, 1)},
		{"year", Query{Kind: WindowYear, Anchor: d(2024, 7, 1)}, d(2024, 1, 1), d(2025, 1, 1)},
		{"all", Query{Kind: WindowAll}, first, last + 1},
		{"custom", Query{Kind: WindowCustom, From: d(2024, 1, 10), To: d(2024, 1, 20)}, d(2024, 1, 10), d(2024, 1, 20)},
	}
	for _, tt := range tests {
		w, err := ResolveWindow(tt.q, s, first, last, today, "Asia/Kolkata")
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if w.From != tt.from || w.To != tt.to || w.Days != int(tt.to-tt.from) {
			t.Errorf("%s: got [%s, %s) days %d, want [%s, %s)", tt.name, w.From, w.To, w.Days, tt.from, tt.to)
		}
	}
	// Leap-year lengths.
	if w, _ := ResolveWindow(Query{Kind: WindowYear, Anchor: d(2024, 1, 1)}, s, first, last, today, ""); w.Days != 366 {
		t.Errorf("2024 has %d days", w.Days)
	}
}

func TestWeekStartSetting(t *testing.T) {
	s := DefaultSettings()
	s.WeekStart = time.Sunday
	w, err := ResolveWindow(Query{Kind: WindowWeek, Anchor: d(2026, 3, 4)}, s, d(2026, 1, 1), d(2026, 12, 1), d(2026, 12, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if w.From != d(2026, 3, 1) || w.From.Weekday() != time.Sunday {
		t.Errorf("Sunday week starts %s", w.From)
	}
}

func TestDSTDoesNotChangeDays(t *testing.T) {
	// Europe/London springs forward on 2026-03-29: still one date, one day.
	s := DefaultSettings()
	w, err := ResolveWindow(Query{Kind: WindowDay, Anchor: d(2026, 3, 29)}, s, d(2026, 1, 1), d(2026, 12, 1), d(2026, 12, 1), "Europe/London")
	if err != nil || w.Days != 1 {
		t.Fatalf("days = %d err %v", w.Days, err)
	}
	loc := loadLoc("Europe/London")
	if got := w.To.Start(loc).Sub(w.From.Start(loc)); got != 23*time.Hour {
		t.Errorf("DST day lasts %v, want 23h", got)
	}
}

func TestCoverageAndPartial(t *testing.T) {
	s := DefaultSettings()
	first, last := d(2026, 3, 3), d(2026, 3, 5)
	w, _ := ResolveWindow(Query{Kind: WindowWeek, Anchor: d(2026, 3, 4)}, s, first, last, d(2026, 3, 5), "")
	if w.Coverage != 3.0/7 || !w.Partial {
		t.Errorf("coverage %v partial %v", w.Coverage, w.Partial)
	}
	w, _ = ResolveWindow(Query{Kind: WindowDay, Anchor: d(2026, 3, 4)}, s, first, last, d(2026, 6, 1), "")
	if w.Coverage != 1 || w.Partial {
		t.Errorf("inside day coverage %v partial %v", w.Coverage, w.Partial)
	}
}

func TestResolveWindowErrors(t *testing.T) {
	s := DefaultSettings()
	if _, err := ResolveWindow(Query{Kind: WindowCustom, From: d(2024, 1, 2), To: d(2024, 1, 2)}, s, 0, 10, 10, ""); err == nil {
		t.Error("empty custom window accepted")
	}
	if _, err := ResolveWindow(Query{Kind: WindowAll}, s, 10, 9, 10, ""); err != ErrEmptyChat {
		t.Errorf("err = %v, want ErrEmptyChat", err)
	}
	if _, err := ResolveWindow(Query{Kind: "fortnight"}, s, 0, 10, 10, ""); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestPreviousWindow(t *testing.T) {
	s := DefaultSettings()
	tests := []struct {
		w        Window
		from, to Date
	}{
		{Window{Kind: WindowDay, From: d(2026, 3, 1), To: d(2026, 3, 2)}, d(2026, 2, 28), d(2026, 3, 1)},
		{Window{Kind: WindowWeek, From: d(2026, 3, 2), To: d(2026, 3, 9)}, d(2026, 2, 23), d(2026, 3, 2)},
		{Window{Kind: WindowMonth, From: d(2026, 3, 1), To: d(2026, 4, 1)}, d(2026, 2, 1), d(2026, 3, 1)},
		{Window{Kind: WindowMonth, From: d(2026, 1, 1), To: d(2026, 2, 1)}, d(2025, 12, 1), d(2026, 1, 1)},
		{Window{Kind: WindowYear, From: d(2026, 1, 1), To: d(2027, 1, 1)}, d(2025, 1, 1), d(2026, 1, 1)},
		{Window{Kind: WindowCustom, From: d(2026, 3, 10), To: d(2026, 3, 20), Days: 10}, d(2026, 2, 28), d(2026, 3, 10)},
	}
	for _, tt := range tests {
		p := PreviousWindow(tt.w, s)
		if p == nil || p.From != tt.from || p.To != tt.to {
			t.Errorf("%s %s: got %+v, want [%s, %s)", tt.w.Kind, tt.w.From, p, tt.from, tt.to)
		}
	}
	if PreviousWindow(Window{Kind: WindowAll}, s) != nil {
		t.Error("all must have no previous window")
	}
}

func TestZeroAnchorDefaultsToLastActiveDay(t *testing.T) {
	last := DateFromYMD(2024, time.March, 17)
	w, err := ResolveWindow(Query{Kind: WindowMonth}, DefaultSettings(), DateFromYMD(2023, time.June, 1), last, DateFromYMD(2026, time.January, 1), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if w.From != DateFromYMD(2024, time.March, 1) || w.To != DateFromYMD(2024, time.April, 1) {
		t.Errorf("default month = %s..%s, want March 2024", w.From, w.To)
	}
}
