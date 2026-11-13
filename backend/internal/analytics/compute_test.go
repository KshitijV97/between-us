package analytics

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Reference example of docs/04-metrics-catalog.md §7: participants A and R,
// Asia/Kolkata, week of Mon 2026-03-02. The rollups below are what the
// builder produces for those ten messages (derivation in comments).
var (
	refA = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	refR = uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	ist  = loadLoc("Asia/Kolkata")

	mon = DateFromYMD(2026, time.March, 2)
	tue = mon + 1
)

func loadLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func at(d Date, h, m int) time.Time {
	y, mo, day := d.Time().Date()
	return time.Date(y, mo, day, h, m, 0, 0, ist)
}

func respHist(secs ...int64) (h [ResponseHistBuckets]int) {
	for _, s := range secs {
		h[ResponseBucket(s)]++
	}
	return h
}

func gapHist(secs ...int64) (h [GapHistBuckets]int) {
	for _, s := range secs {
		h[GapBucket(s)]++
	}
	return h
}

func referenceRollups() []Rollup {
	var aMonHours, aTueHours, rMonHours, rTueHours [24]int
	aMonHours[8], aMonHours[14], aMonHours[23] = 3, 1, 1 // msgs 1,4,5 / 7 / 8
	aTueHours[0] = 1                                     // msg 10
	rMonHours[8], rMonHours[14] = 2, 1                   // msgs 2,3 / 6
	rTueHours[0] = 1                                     // msg 9
	return []Rollup{
		{
			// A, Monday: msgs 1,4,5,7,8 (5 is media-only); words 2+5+3+2.
			ParticipantID: refA, Date: mon, MsgCount: 5, TextMsgCount: 4, WordCount: 12, CharCount: 50,
			EmojiCount: 3, EmojiMsgCount: 3, FirstMsgAt: at(mon, 8, 0), LastMsgAt: at(mon, 23, 50),
			HourlyCounts: aMonHours,
			// S1 and S3 initiated by A (S3 starts Monday 23:50, R-02); A ends all three.
			SessionsInitiated: 2, SessionsEnded: 3, SessionMsgSum: 5 + 3, SessionDurationSumS: 1260 + 3000, SessionTurnsSum: 3 + 3,
			TurnCount: 4, TurnsReplied: 2, // A1 (by R1) and A4 (by R3); A2, A3 close sessions without a question
			ResponseCount: 2, ResponseSumS: 840 + 60, ResponseHist: respHist(840, 60),
			MaxIntradayGapS: 33540, GapHist: gapHist(840, 60, 60, 33540), // 3→4, 4→5, 6→7, 7→8 (9 h 19 m)
			MediaImage: 1, AffectionMsgCount: 1, AffectionHits: 1,
			SentimentScoredCount: 3, SentimentSum: 0.4 + 0.5 + 0.6, SentimentPos: 3, SentimentNeu: 1,
			EmojiCounts: map[string]int{"☀": 1, "🤞": 1, "❤": 1},
			TopTerms:    map[string]int{"miss": 1, "morning": 1, "went": 1},
		},
		{
			ParticipantID: refA, Date: tue, MsgCount: 1, TextMsgCount: 1, WordCount: 2, CharCount: 12,
			EmojiCount: 1, EmojiMsgCount: 1, FirstMsgAt: at(tue, 0, 40), LastMsgAt: at(tue, 0, 40),
			HourlyCounts: aTueHours, TurnCount: 1,
			ResponseCount: 1, ResponseSumS: 1800, ResponseHist: respHist(1800),
			MaxIntradayGapS: 1800, GapHist: gapHist(1800),
			SentimentNeu: 1, EmojiCounts: map[string]int{"😴": 1},
			TopTerms: map[string]int{"night": 1},
		},
		{
			// R, Monday: msgs 2,3,6; words 5+4+1; two questions (2, 6).
			ParticipantID: refR, Date: mon, MsgCount: 3, TextMsgCount: 3, WordCount: 10, CharCount: 40,
			EmojiCount: 1, EmojiMsgCount: 1, FirstMsgAt: at(mon, 8, 5), LastMsgAt: at(mon, 14, 30),
			HourlyCounts:      rMonHours,
			SessionsInitiated: 1, SessionMsgSum: 2, SessionDurationSumS: 60, SessionTurnsSum: 2,
			TurnCount: 2, TurnsReplied: 2, // R1, R2
			ResponseCount: 1, ResponseSumS: 300, ResponseHist: respHist(300),
			MaxIntradayGapS: 22140, GapHist: gapHist(300, 60, 22140),
			QuestionCount: 2, SentimentNeu: 3, EmojiCounts: map[string]int{"🙂": 1},
			TopTerms: map[string]int{"interview": 1, "lunch": 1, "morning": 1},
		},
		{
			// R, Tuesday: msg 9; the 23:50 → 00:10 gap crosses midnight, so no intraday gap.
			ParticipantID: refR, Date: tue, MsgCount: 1, TextMsgCount: 1, WordCount: 3, CharCount: 14,
			EmojiCount: 1, EmojiMsgCount: 1, FirstMsgAt: at(tue, 0, 10), LastMsgAt: at(tue, 0, 10),
			HourlyCounts: rTueHours, TurnCount: 1, TurnsReplied: 1,
			ResponseCount: 1, ResponseSumS: 1200, ResponseHist: respHist(1200),
			GapHist: gapHist(1200), AffectionMsgCount: 1, AffectionHits: 1,
			SentimentScoredCount: 1, SentimentSum: 0.7, SentimentPos: 1,
			EmojiCounts: map[string]int{"❤": 1}, TopTerms: map[string]int{"miss": 1},
		},
	}
}

func referenceInput(t *testing.T, minMsgs int) Input {
	t.Helper()
	s := DefaultSettings()
	s.MinMsgsForScores = minMsgs
	now := at(mon+18, 12, 0)
	w, err := ResolveWindow(Query{Kind: WindowWeek, Anchor: mon + 2}, s, mon, tue, DateOf(now, ist), "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		Window: w, Rollups: referenceRollups(),
		Participants: []Participant{{ID: refA, Label: "A"}, {ID: refR, Label: "R"}},
		Settings:     s, Now: now, Location: ist,
		Exact: &ExactData{ResponseTimes: map[uuid.UUID][]int64{refA: {840, 60, 1800}, refR: {300, 1200}}},
	}
}

func compute(t *testing.T, in Input) *Result {
	t.Helper()
	res, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	// Results must always serialise.
	if _, err := json.Marshal(res); err != nil {
		t.Fatalf("result does not marshal: %v", err)
	}
	return res
}

func near(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", name, want)
		return
	}
	if math.Abs(*got-want) > tol {
		t.Errorf("%s = %v, want %v ± %v", name, *got, want, tol)
	}
}

func TestComputeReferenceExample(t *testing.T) {
	res := compute(t, referenceInput(t, 20))
	a, r := refA.String(), refR.String()
	m := res.Metrics

	if w := res.Window; w.From != mon || w.To != mon+7 || w.Days != 7 {
		t.Fatalf("window %+v", w)
	}

	c := m["M-001"].(*Count)
	if c.ByParticipant[a] != 6 || c.ByParticipant[r] != 4 || c.Combined != 10 {
		t.Errorf("M-001 %+v", c)
	}

	sov := m["M-002"].(ShareOfVoice)
	near(t, "M-002 msgs A", sov.Messages[a], 0.6, 1e-9)
	near(t, "M-002 msgs R", sov.Messages[r], 0.4, 1e-9)
	near(t, "M-002 words A", sov.Words[a], 14.0/27, 1e-9)
	near(t, "M-002 words R", sov.Words[r], 13.0/27, 1e-9)

	tl := m["M-003"].(TextLength)
	near(t, "M-003 avg words A", tl.ByParticipant[a].AvgWords, 2.8, 1e-9)

	act := m["M-004"].(Activity)
	if act.ActiveDays != 2 || math.Abs(act.Rate-2.0/7) > 1e-9 {
		t.Errorf("M-004 %+v", act)
	}

	st := m["M-005"].(Streaks)
	if st.Longest != 2 || st.Current != 0 || st.MutualLongest != 2 {
		t.Errorf("M-005 %+v", st)
	}

	sil := m["M-006"].(Silence)
	wantTrailing := int64(5*86400 + 23*3600 + 20*60) // Tue 00:40 → Mon 03-09 00:00
	if sil.LongestS != wantTrailing || !sil.Ongoing || sil.BrokenBy != nil {
		t.Errorf("M-006 longest=%d ongoing=%v brokenBy=%v, want %d ongoing", sil.LongestS, sil.Ongoing, sil.BrokenBy, wantTrailing)
	}
	if sil.MaxIntradayS != 33540 { // 9 h 19 m, Mon 14:31 → 23:50
		t.Errorf("M-006 intraday = %d", sil.MaxIntradayS)
	}

	ses := m["M-007"].(Sessions)
	if ses.Count != 3 {
		t.Errorf("M-007 count = %d", ses.Count)
	}
	near(t, "M-007 avg msgs", ses.AvgMessages, 10.0/3, 1e-9)
	near(t, "M-007 avg duration", ses.AvgDurationS, 24*60, 1e-9)

	ini := m["M-008"].(Initiations)
	near(t, "M-008 ratio A", ini.Ratio[a], 2.0/3, 1e-9)

	if e := m["M-009"].(*Count); e.ByParticipant[a] != 3 || e.ByParticipant[r] != 0 {
		t.Errorf("M-009 %+v", e)
	}

	rt := m["M-010"].(ResponseTime)
	near(t, "M-010 median A", rt.ByParticipant[a].MedianS, 840, 1e-9)
	near(t, "M-010 mean A", rt.ByParticipant[a].MeanS, 900, 1e-9)
	near(t, "M-010 p90 A", rt.ByParticipant[a].P90S, 1608, 1e-9)
	near(t, "M-010 median R", rt.ByParticipant[r].MedianS, 750, 1e-9)
	near(t, "M-010 median combined", rt.Combined.MedianS, 840, 1e-9)
	if rt.Combined.Approx || !rt.ByParticipant[r].FewData {
		t.Errorf("M-010 flags %+v", rt)
	}

	rr := m["M-011"].(ReplyRate)
	near(t, "M-011 A", rr.ByParticipant[a], 1, 1e-9)
	near(t, "M-011 R", rr.ByParticipant[r], 1, 1e-9)

	tu := m["M-012"].(Turns)
	if tu.Turns[a] != 5 || tu.Turns[r] != 3 {
		t.Errorf("M-012 %+v", tu.Turns)
	}
	near(t, "M-012 msgs/turn A", tu.MessagesPerTurn[a], 1.2, 1e-9)

	hm := m["M-013"].(Heatmap)
	if c := hm.ByParticipant[a]; c[0][8] != 3 || c[0][14] != 1 || c[0][23] != 1 || c[1][0] != 1 {
		t.Errorf("M-013 A cells wrong")
	}

	ln := m["M-014"].(LateNight)
	near(t, "M-014 A", ln.Share[a], 1.0/6, 1e-9)
	near(t, "M-014 R", ln.Share[r], 0.25, 1e-9)

	em := m["M-015"].(Emoji)
	if len(em.Combined.Top) == 0 || em.Combined.Top[0].Key != "❤" || em.Combined.Top[0].Count != 2 {
		t.Errorf("M-015 top %+v", em.Combined.Top)
	}
	if em.Combined.Affection != 2 {
		t.Errorf("M-015 affection = %d", em.Combined.Affection)
	}

	tt := m["M-016"].(TopTerms)
	if !tt.Approximate || tt.Combined[0].Key != "miss" && tt.Combined[0].Key != "morning" {
		t.Errorf("M-016 %+v", tt)
	}

	if q := m["M-017"].(Questions); q.ByParticipant[r] != 2 || q.ByParticipant[a] != 0 {
		t.Errorf("M-017 %+v", q.Count)
	}
	if md := m["M-019"].(Media); md.ByParticipant[a].Image != 1 || md.Combined.Total != 1 {
		t.Errorf("M-019 %+v", md)
	}

	if s := m["M-025"].(Score); s.Value != nil || s.Reason != ReasonInsufficientData {
		t.Errorf("M-025 should be insufficient below 20 messages: %+v", s)
	}
	if s := m["M-026"].(Score); s.Value != nil {
		t.Errorf("M-026 should be insufficient: %+v", s)
	}
}

func TestScoresWithLoweredThreshold(t *testing.T) {
	res := compute(t, referenceInput(t, 1))
	rec := res.Metrics["M-025"].(Score)
	if rec.Value == nil || math.Round(*rec.Value) != 82 {
		t.Fatalf("M-025 = %v, want 82", rec.Value)
	}
	eng := res.Metrics["M-026"].(Score)
	if eng.Value == nil || math.Round(*eng.Value) != 44 {
		t.Fatalf("M-026 = %v, want ≈44 (components %v)", eng.Value, eng.Components)
	}
}

func TestReciprocityRedistributesUndefinedResponseWeight(t *testing.T) {
	in := referenceInput(t, 1)
	in.Exact.ResponseTimes[refR] = nil // R has no responses → response dimension undefined
	rec := compute(t, in).Metrics["M-025"].(Score)
	d := rec.Dimensions["response_time"]
	if d.Imbalance != nil || d.Weight != 0 {
		t.Fatalf("response dimension should be undefined: %+v", d)
	}
	if w := rec.Dimensions["messages"].Weight; math.Abs(w-0.375) > 1e-9 { // 0.30 / 0.80
		t.Errorf("messages weight = %v, want 0.375", w)
	}
}

func TestSelectiveMetrics(t *testing.T) {
	in := referenceInput(t, 20)
	in.Metrics = []string{"M-001", "M-010"}
	res := compute(t, in)
	if len(res.Metrics) != 2 || res.Metrics["M-001"] == nil || res.Metrics["M-010"] == nil {
		t.Fatalf("metrics = %v", res.Metrics)
	}
}

func TestHistogramPathApprox(t *testing.T) {
	in := referenceInput(t, 20)
	in.Exact = nil
	rt := compute(t, in).Metrics["M-010"].(ResponseTime)
	a := rt.ByParticipant[refA.String()]
	if !a.Approx {
		t.Error("histogram path must be flagged approx")
	}
	near(t, "mean stays exact", a.MeanS, 900, 1e-9)
}

func TestHistogramPercentileWithinNinePercent(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := 0; trial < 50; trial++ {
		n := 20 + rng.IntN(500)
		vals := make([]int64, n)
		var r Rollup
		for i := range vals {
			vals[i] = int64(math.Exp(rng.Float64()*9)) + 1 // 1 s … ~2 h, log-spread
			r.ResponseHist[ResponseBucket(vals[i])]++
			r.ResponseCount++
		}
		exact := exactPercentile(vals, 0.5)
		got, ok := PercentileFromHist(r.ResponseHist[:], 0.5, ResponseBucketBounds)
		if !ok {
			t.Fatal("no estimate")
		}
		// The interpolated rank can land in a neighbouring value's bucket;
		// allow the documented 9% plus one bucket of discretisation slack.
		if rel := math.Abs(got-exact) / exact; rel > 0.09+0.19 {
			t.Errorf("trial %d: hist %v vs exact %v (rel %.3f)", trial, got, exact, rel)
		}
	}
}

func TestComparisonNullBeforeChat(t *testing.T) {
	in := referenceInput(t, 20)
	in.Prev = PreviousWindow(in.Window, in.Settings) // week before the chat started
	res := compute(t, in)
	if res.Compare == nil || res.Compare.Coverage != 0 {
		t.Fatalf("compare = %+v", res.Compare)
	}
	ch := res.Metrics["M-001"].(*Count).Change
	if ch == nil || ch.Prev != nil || ch.Delta != nil || ch.DeltaPct != nil {
		t.Errorf("change before chat start must be null, got %+v", ch)
	}
}

func TestComparisonDeltas(t *testing.T) {
	rs := referenceRollups()
	// Shift a copy one week earlier with half the messages for a previous week.
	for _, r := range referenceRollups() {
		r.Date -= 7
		r.FirstMsgAt = r.FirstMsgAt.AddDate(0, 0, -7)
		r.LastMsgAt = r.LastMsgAt.AddDate(0, 0, -7)
		r.MsgCount = r.MsgCount * 2
		rs = append(rs, r)
	}
	in := referenceInput(t, 20)
	in.Rollups = rs
	in.Prev = PreviousWindow(in.Window, in.Settings)
	c := compute(t, in).Metrics["M-001"].(*Count)
	near(t, "prev", c.Change.Prev, 20, 1e-9)
	near(t, "delta", c.Change.Delta, -10, 1e-9)
	near(t, "delta_pct", c.Change.DeltaPct, -0.5, 1e-9)
	near(t, "per_day_delta", c.Change.PerDayDelta, -10.0/7, 1e-9)
	if c.Baseline == nil {
		t.Error("baseline missing")
	}
}

func TestNoMessagesWindow(t *testing.T) {
	in := referenceInput(t, 20)
	in.Window, _ = ResolveWindow(Query{Kind: WindowDay, Anchor: mon + 5}, in.Settings, mon, tue, mon+18, "Asia/Kolkata")
	res := compute(t, in)
	sil := res.Metrics["M-006"].(Silence)
	if !sil.NoMessages || sil.LongestS != 86400 {
		t.Errorf("empty day silence %+v", sil)
	}
	if c := res.Metrics["M-001"].(*Count); c.Combined != 0 {
		t.Errorf("M-001 %+v", c)
	}
}

func TestMoreThanTwoParticipants(t *testing.T) {
	refC := uuid.MustParse("00000000-0000-0000-0000-00000000000c")
	in := referenceInput(t, 1)
	in.Rollups = append(in.Rollups, Rollup{ParticipantID: refC, Date: mon, MsgCount: 1, TextMsgCount: 1, FirstMsgAt: at(mon, 9, 0), LastMsgAt: at(mon, 9, 0)})
	res := compute(t, in)
	if c := res.Metrics["M-001"].(*Count); len(c.ByParticipant) != 3 || c.Combined != 11 {
		t.Errorf("M-001 %+v", c)
	}
	if s := res.Metrics["M-025"].(Score); len(s.Pair) != 2 || s.Pair[0] != refA.String() || s.Pair[1] != refR.String() {
		t.Errorf("pair = %v", s.Pair)
	}
}

func TestTrendLabels(t *testing.T) {
	start := DateFromYMD(2026, time.January, 1)
	mk := func(perDay func(i int) int) []Rollup {
		var rs []Rollup
		for i := 0; i < 30; i++ {
			d := start + Date(i)
			n := perDay(i)
			if n == 0 {
				continue
			}
			rs = append(rs, Rollup{ParticipantID: refA, Date: d, MsgCount: n, FirstMsgAt: at(d, 9, 0), LastMsgAt: at(d, 10, 0)})
		}
		return rs
	}
	cases := []struct {
		name  string
		f     func(int) int
		label string
	}{
		{"rising", func(i int) int { return 5 + i }, "rising"},
		{"falling", func(i int) int { return 40 - i }, "falling"},
		{"steady", func(i int) int { return 10 + i%2 }, "steady"},
	}
	for _, tc := range cases {
		rs := mk(tc.f)
		w := Window{Kind: WindowCustom, From: start, To: start + 30, Days: 30, Coverage: 1}
		res := compute(t, Input{Window: w, Rollups: rs, Participants: []Participant{{ID: refA}}, Settings: DefaultSettings(), Now: at(start+40, 0, 0), Location: ist, Metrics: []string{"M-027"}})
		tr := res.Metrics["M-027"].(Trend).MessagesPerDay.Combined
		if tr.Label == nil || *tr.Label != tc.label {
			t.Errorf("%s: label = %v (rel %v r2 %v)", tc.name, tr.Label, tr.Rel, tr.R2)
		}
	}
	// Fewer than 7 points → no label.
	w := Window{Kind: WindowCustom, From: start, To: start + 5, Days: 5, Coverage: 1}
	res := compute(t, Input{Window: w, Rollups: mk(func(i int) int { return i + 1 }), Settings: DefaultSettings(), Now: at(start+40, 0, 0), Location: ist, Metrics: []string{"M-027"}})
	if l := res.Metrics["M-027"].(Trend).MessagesPerDay.Combined.Label; l != nil {
		t.Errorf("label with 5 points = %v", *l)
	}
}

// Property: every additive metric over a window equals the sum over any
// partition of that window into sub-windows (§13).
func TestMetricAdditivityProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	start := DateFromYMD(2025, time.June, 1)
	var rs []Rollup
	for i := 0; i < 90; i++ {
		for _, p := range []uuid.UUID{refA, refR} {
			if rng.IntN(3) == 0 {
				continue
			}
			d := start + Date(i)
			rs = append(rs, Rollup{
				ParticipantID: p, Date: d, MsgCount: 1 + rng.IntN(50), WordCount: rng.IntN(300),
				SessionsInitiated: rng.IntN(3), QuestionCount: rng.IntN(5), LinkCount: rng.IntN(2),
				FirstMsgAt: at(d, 8, 0), LastMsgAt: at(d, 22, 0),
			})
		}
	}
	whole := Window{Kind: WindowCustom, From: start, To: start + 90, Days: 90, Coverage: 1}
	get := func(w Window) (msgs, words, sess, q int) {
		res := compute(t, Input{Window: w, Rollups: rs, Participants: []Participant{{ID: refA}, {ID: refR}}, Settings: DefaultSettings(), Now: at(start+200, 0, 0), Location: ist,
			Metrics: []string{"M-001", "M-003", "M-007", "M-017"}})
		return res.Metrics["M-001"].(*Count).Combined, res.Metrics["M-003"].(TextLength).Combined.Words,
			res.Metrics["M-007"].(Sessions).Count, res.Metrics["M-017"].(Questions).Combined
	}
	wm, ww, ws, wq := get(whole)
	for trial := 0; trial < 20; trial++ {
		cuts := []int{0, 90}
		for k := 0; k < 1+rng.IntN(5); k++ {
			cuts = append(cuts, 1+rng.IntN(89))
		}
		sort.Ints(cuts)
		var sm, sw, ss, sq int
		for i := 1; i < len(cuts); i++ {
			if cuts[i] == cuts[i-1] {
				continue
			}
			sub := Window{Kind: WindowCustom, From: start + Date(cuts[i-1]), To: start + Date(cuts[i]), Days: cuts[i] - cuts[i-1], Coverage: 1}
			m, w, s, q := get(sub)
			sm, sw, ss, sq = sm+m, sw+w, ss+s, sq+q
		}
		if sm != wm || sw != ww || ss != ws || sq != wq {
			t.Fatalf("partition %v: sums %d/%d/%d/%d != whole %d/%d/%d/%d", cuts, sm, sw, ss, sq, wm, ww, ws, wq)
		}
	}
}

// Call-only days produce rows with MsgCount 0 and zero timestamps; they must
// not count as active, extend streaks or break silences (M-004…M-006).
func TestCallOnlyDayIgnoredForActivity(t *testing.T) {
	in := referenceInput(t, 20)
	in.Rollups = append(in.Rollups, Rollup{ParticipantID: refA, Date: mon + 2, CallVoiceCount: 1, CallDurationS: 120})
	res := compute(t, in)
	if a := res.Metrics["M-004"].(Activity); a.ActiveDays != 2 {
		t.Errorf("active days = %d, want 2", a.ActiveDays)
	}
	if s := res.Metrics["M-005"].(Streaks); s.Longest != 2 {
		t.Errorf("streak = %d, want 2", s.Longest)
	}
	if s := res.Metrics["M-006"].(Silence); s.LongestS != int64(5*86400+23*3600+20*60) {
		t.Errorf("silence = %d", s.LongestS)
	}
	if c := res.Metrics["M-021"].(Calls); c.ByParticipant[refA.String()].Voice != 1 {
		t.Errorf("calls %+v", c)
	}
}
