package analytics

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ExactData carries values recomputed from messages for short windows
// (docs/04-metrics-catalog.md §5.5). The caller supplies it when the window
// is within Settings.ExactPercentilesMaxDays / ExactTermsMaxDays; nil fields
// make Compute fall back to the rollup histograms and maps.
type ExactData struct {
	// ResponseTimes are the window's response times per responder, seconds.
	ResponseTimes map[uuid.UUID][]int64
	// Terms and Bigrams are exact counts in the window per participant.
	Terms, Bigrams map[uuid.UUID]map[string]int
}

// Input is everything Compute needs; it performs no I/O.
type Input struct {
	Window Window
	// Prev is the comparison window (PreviousWindow), or nil.
	Prev *Window
	// Rollups must contain every row of the window and the comparison
	// window, in any order; other rows are ignored, so callers may pass all of
	// them or only those ranges.
	Rollups []Rollup
	// Index holds the light facts of every row of the chat (span,
	// participants, baseline, silence clipping). Nil derives it from Rollups,
	// which is then expected to hold every row.
	Index        []RollupLite
	Participants []Participant
	Settings     Settings
	Exact        *ExactData
	PrevExact    *ExactData
	Now          time.Time
	Location     *time.Location
	// Metrics selects metric IDs; empty means all.
	Metrics       []string
	Stale         bool
	RollupVersion int
}

// AllMetrics lists every metric ID in catalog order.
var AllMetrics = []string{
	"M-001", "M-002", "M-003", "M-004", "M-005", "M-006", "M-007", "M-008", "M-009",
	"M-010", "M-011", "M-012", "M-013", "M-014", "M-015", "M-016", "M-017", "M-018",
	"M-019", "M-020", "M-021", "M-022", "M-023", "M-024", "M-025", "M-026", "M-027",
}

// Top-N sizes for ranked outputs.
const (
	topEmoji   = 10
	topTerms   = 20
	topDomains = 10
)

// affectionEmoji is the default affection subset of M-015 (identities without
// U+FE0F, as produced by the text analyzer).
var affectionEmoji = map[string]bool{
	"❤": true, "🧡": true, "💛": true, "💚": true, "💙": true, "💜": true, "🖤": true, "🤍": true,
	"🤎": true, "💕": true, "💞": true, "💓": true, "💗": true, "💖": true, "💘": true, "💝": true,
	"💟": true, "❣": true, "😘": true, "😍": true, "🥰": true, "🤗": true, "😚": true, "😙": true, "💋": true,
}

// Compute evaluates the requested metrics for in.Window.
func Compute(in Input) (*Result, error) {
	if in.Window.To <= in.Window.From {
		return nil, errors.New("analytics: empty window")
	}
	if in.Location == nil {
		in.Location = time.UTC
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	c := newCalc(in)

	res := &Result{
		Window:        in.Window,
		RollupVersion: in.RollupVersion,
		Stale:         in.Stale,
		Participants:  c.parts,
		Metrics:       map[string]any{},
	}
	if c.prevWin != nil {
		pw := *c.prevWin
		res.Compare = &pw
	}

	want := map[string]bool{}
	for _, id := range in.Metrics {
		want[id] = true
	}
	for _, id := range AllMetrics {
		if len(want) > 0 && !want[id] {
			continue
		}
		if v := c.metric(id); v != nil {
			res.Metrics[id] = v
		}
	}
	return res, nil
}

// calc holds aggregates shared by the metric functions.
type calc struct {
	in        Input
	index     []RollupLite
	parts     []Participant
	ids       []uuid.UUID
	chatFirst Date
	chatLast  Date
	today     Date
	cur       *aggregate
	prev      *aggregate // nil when not compared or the previous window has no coverage
	prevWin   *Window
	resp      ResponseTime // M-010, used by M-025…M-027
}

func newCalc(in Input) *calc {
	c := &calc{in: in, today: DateOf(in.Now, in.Location), chatFirst: 1, chatLast: 0}

	seen := map[uuid.UUID]bool{}
	for _, p := range in.Participants {
		if !seen[p.ID] {
			seen[p.ID] = true
			c.parts = append(c.parts, p)
		}
	}
	// Chat-level facts come from the light index so callers only need to
	// decode the rows of the window and comparison window.
	c.index = in.Index
	if c.index == nil {
		c.index = make([]RollupLite, len(in.Rollups))
		for i := range in.Rollups {
			c.index[i] = in.Rollups[i].Lite()
		}
	}
	first := true
	for i := range c.index {
		r := &c.index[i]
		if !seen[r.ParticipantID] && r.ParticipantID != uuid.Nil {
			seen[r.ParticipantID] = true
			c.parts = append(c.parts, Participant{ID: r.ParticipantID})
		}
		if r.MsgCount > 0 {
			if first || r.Date < c.chatFirst {
				c.chatFirst = r.Date
			}
			if first || r.Date > c.chatLast {
				c.chatLast = r.Date
			}
			first = false
		}
	}
	if c.parts == nil {
		c.parts = []Participant{}
	}
	for _, p := range c.parts {
		c.ids = append(c.ids, p.ID)
	}

	c.cur = aggregateRollups(in.Rollups, in.Window.From, in.Window.To)
	if in.Prev != nil {
		pw := *in.Prev
		pw.SetCoverage(c.chatFirst, c.chatLast, c.today)
		c.prevWin = &pw
		if pw.Coverage > 0 {
			c.prev = aggregateRollups(in.Rollups, pw.From, pw.To)
		}
	}
	c.resp = c.responseTime()
	return c
}

func (c *calc) metric(id string) any {
	switch id {
	case "M-001":
		m := c.count(func(r *Rollup) int { return r.MsgCount })
		if span := int(c.chatLast-c.chatFirst) + 1; span > 0 {
			total := 0
			for i := range c.index {
				total += c.index[i].MsgCount
			}
			m.Baseline = fp(float64(total) / float64(span) * float64(c.in.Window.Days))
		}
		return m
	case "M-002":
		return c.shareOfVoice()
	case "M-003":
		return c.textLength()
	case "M-004":
		return c.activity()
	case "M-005":
		return c.streaks()
	case "M-006":
		return c.silence()
	case "M-007":
		return c.sessions()
	case "M-008":
		return c.initiations()
	case "M-009":
		return c.count(func(r *Rollup) int { return r.SessionsEnded })
	case "M-010":
		return c.resp
	case "M-011":
		return c.replyRate()
	case "M-012":
		return c.turns()
	case "M-013":
		return c.heatmap()
	case "M-014":
		return c.lateNight()
	case "M-015":
		return c.emoji()
	case "M-016":
		return c.topTerms()
	case "M-017":
		q := Questions{Count: *c.count(func(r *Rollup) int { return r.QuestionCount }), Rate: map[string]*float64{}}
		for _, id := range c.ids {
			r := c.cur.of(id)
			q.Rate[id.String()] = ratio(float64(r.QuestionCount), float64(r.TextMsgCount))
		}
		return q
	case "M-018":
		l := Links{Count: *c.count(func(r *Rollup) int { return r.LinkCount })}
		l.TopDomains = topN(c.cur.comb.LinkDomains, topDomains)
		return l
	case "M-019":
		return c.media()
	case "M-020":
		f := Flags{ByParticipant: map[string]MessageFlags{}}
		conv := func(r *Rollup) MessageFlags {
			return MessageFlags{Deleted: r.DeletedCount, Edited: r.EditedCount, Forwarded: r.ForwardedCount}
		}
		for _, id := range c.ids {
			f.ByParticipant[id.String()] = conv(c.cur.of(id))
		}
		f.Combined = conv(&c.cur.comb)
		return f
	case "M-021":
		cl := Calls{ByParticipant: map[string]CallStats{}}
		conv := func(r *Rollup) CallStats {
			return CallStats{Voice: r.CallVoiceCount, Video: r.CallVideoCount, Missed: r.CallMissedCount, DurationS: r.CallDurationS}
		}
		for _, id := range c.ids {
			cl.ByParticipant[id.String()] = conv(c.cur.of(id))
		}
		cl.Combined = conv(&c.cur.comb)
		return cl
	case "M-022":
		return c.sentiment()
	case "M-023":
		a := Affection{Messages: map[string]int{}, Hits: map[string]int{}, RatePer100: map[string]*float64{}}
		for _, id := range c.ids {
			r := c.cur.of(id)
			k := id.String()
			a.Messages[k] = r.AffectionMsgCount
			a.Hits[k] = r.AffectionHits
			if v := ratio(float64(r.AffectionMsgCount), float64(r.TextMsgCount)); v != nil {
				a.RatePer100[k] = fp(*v * 100)
			} else {
				a.RatePer100[k] = nil
			}
		}
		a.Combined = c.cur.comb.AffectionMsgCount
		return a
	case "M-024":
		cf := Conflict{Messages: map[string]int{}, Combined: c.cur.comb.ConflictMsgCount}
		for _, id := range c.ids {
			cf.Messages[id.String()] = c.cur.of(id).ConflictMsgCount
		}
		return cf
	case "M-025":
		s, _ := c.reciprocity()
		return s
	case "M-026":
		return c.engagement()
	case "M-027":
		return c.trend()
	}
	return nil
}

// ---- aggregation ---------------------------------------------------------

// aggregate is the sum of rollups in [from, to) per participant and combined.
type aggregate struct {
	from, to Date
	per      map[uuid.UUID]*Rollup
	comb     Rollup
	// days lists the rollup rows of each date in the window.
	days map[Date][]*Rollup
}

func aggregateRollups(rs []Rollup, from, to Date) *aggregate {
	a := &aggregate{from: from, to: to, per: map[uuid.UUID]*Rollup{}, days: map[Date][]*Rollup{}}
	for i := range rs {
		r := &rs[i]
		if r.Date < from || r.Date >= to {
			continue
		}
		p := a.per[r.ParticipantID]
		if p == nil {
			p = &Rollup{ParticipantID: r.ParticipantID}
			a.per[r.ParticipantID] = p
		}
		addRollup(p, r)
		addRollupCounters(&a.comb, r)
		a.days[r.Date] = append(a.days[r.Date], r)
	}
	// Combined maps are the sum of the participants' maps: a handful of
	// merges instead of one per row (the all-time view of a long chat has
	// tens of thousands of rows).
	for _, p := range a.per {
		addRollupMaps(&a.comb, p)
	}
	return a
}

// of returns the participant's aggregate (an empty one when absent).
func (a *aggregate) of(id uuid.UUID) *Rollup {
	if r := a.per[id]; r != nil {
		return r
	}
	return &Rollup{ParticipantID: id}
}

// sortedDays returns the window's dates that have rows, ascending.
func (a *aggregate) sortedDays() []Date {
	out := make([]Date, 0, len(a.days))
	for d := range a.days {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// addRollup merges s into dst using each column's combine rule (§6).
// addRollup adds every field of s into dst, maps included.
func addRollup(dst, s *Rollup) {
	addRollupCounters(dst, s)
	addRollupMaps(dst, s)
}

// addRollupMaps merges the map fields (link domains, emoji, terms). They are
// the expensive part of aggregation, so callers that only need counters and
// histograms use addRollupCounters alone.
func addRollupMaps(dst, s *Rollup) {
	dst.LinkDomains = addMap(dst.LinkDomains, s.LinkDomains)
	dst.EmojiCounts = addMap(dst.EmojiCounts, s.EmojiCounts)
	dst.TopTerms = addMap(dst.TopTerms, s.TopTerms)
	dst.TopBigrams = addMap(dst.TopBigrams, s.TopBigrams)
}

// addRollupCounters adds the counters, histograms and first/last times of s.
func addRollupCounters(dst, s *Rollup) {
	dst.MsgCount += s.MsgCount
	dst.TextMsgCount += s.TextMsgCount
	dst.WordCount += s.WordCount
	dst.CharCount += s.CharCount
	dst.EmojiCount += s.EmojiCount
	dst.EmojiMsgCount += s.EmojiMsgCount
	if s.MsgCount > 0 && !s.FirstMsgAt.IsZero() && (dst.FirstMsgAt.IsZero() || s.FirstMsgAt.Before(dst.FirstMsgAt)) {
		dst.FirstMsgAt = s.FirstMsgAt
	}
	if s.MsgCount > 0 && s.LastMsgAt.After(dst.LastMsgAt) {
		dst.LastMsgAt = s.LastMsgAt
	}
	for i := range dst.HourlyCounts {
		dst.HourlyCounts[i] += s.HourlyCounts[i]
	}
	dst.SessionsInitiated += s.SessionsInitiated
	dst.SessionsEnded += s.SessionsEnded
	dst.SessionMsgSum += s.SessionMsgSum
	dst.SessionDurationSumS += s.SessionDurationSumS
	dst.SessionTurnsSum += s.SessionTurnsSum
	dst.TurnCount += s.TurnCount
	dst.TurnsReplied += s.TurnsReplied
	dst.TurnsAwaitingUnreplied += s.TurnsAwaitingUnreplied
	dst.DoubleTextCount += s.DoubleTextCount
	dst.FollowupAfterNoReply += s.FollowupAfterNoReply
	dst.ResponseCount += s.ResponseCount
	dst.ResponseSumS += s.ResponseSumS
	for i := range dst.ResponseHist {
		dst.ResponseHist[i] += s.ResponseHist[i]
	}
	dst.MaxIntradayGapS = max(dst.MaxIntradayGapS, s.MaxIntradayGapS)
	for i := range dst.GapHist {
		dst.GapHist[i] += s.GapHist[i]
	}
	dst.QuestionCount += s.QuestionCount
	dst.LinkCount += s.LinkCount
	dst.MediaImage += s.MediaImage
	dst.MediaVideo += s.MediaVideo
	dst.MediaAudio += s.MediaAudio
	dst.MediaVoice += s.MediaVoice
	dst.MediaSticker += s.MediaSticker
	dst.MediaGIF += s.MediaGIF
	dst.MediaDocument += s.MediaDocument
	dst.MediaOther += s.MediaOther
	dst.LocationCount += s.LocationCount
	dst.ContactCount += s.ContactCount
	dst.PollCount += s.PollCount
	dst.DeletedCount += s.DeletedCount
	dst.EditedCount += s.EditedCount
	dst.ForwardedCount += s.ForwardedCount
	dst.CallVoiceCount += s.CallVoiceCount
	dst.CallVideoCount += s.CallVideoCount
	dst.CallMissedCount += s.CallMissedCount
	dst.CallDurationS += s.CallDurationS
	dst.SentimentScoredCount += s.SentimentScoredCount
	dst.SentimentSum += s.SentimentSum
	dst.SentimentPos += s.SentimentPos
	dst.SentimentNeu += s.SentimentNeu
	dst.SentimentNeg += s.SentimentNeg
	for i := range dst.SentimentHist {
		dst.SentimentHist[i] += s.SentimentHist[i]
	}
	dst.AffectionMsgCount += s.AffectionMsgCount
	dst.AffectionHits += s.AffectionHits
	dst.ConflictMsgCount += s.ConflictMsgCount
	dst.TermsOtherCount += s.TermsOtherCount
}

func addMap(dst, src map[string]int) map[string]int {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]int, len(src))
	}
	for k, v := range src {
		dst[k] += v
	}
	return dst
}

// ---- helpers -------------------------------------------------------------

func fp(v float64) *float64 { return &v }

// ratio returns a/b, or nil when b is 0.
func ratio(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	return fp(a / b)
}

// change builds the §2.2 comparison of an additive value. perDay adds the
// per-day delta that keeps unequal window lengths fair.
func (c *calc) change(cur, prev float64, perDay bool) *Change {
	if c.prevWin == nil {
		return nil
	}
	if c.prev == nil {
		return &Change{} // previous window before the chat started: null, not 0
	}
	ch := &Change{Prev: fp(prev), Delta: fp(cur - prev), DeltaPct: ratio(cur-prev, prev)}
	if perDay && c.in.Window.Days > 0 && c.prevWin.Days > 0 {
		ch.PerDayDelta = fp(cur/float64(c.in.Window.Days) - prev/float64(c.prevWin.Days))
	}
	return ch
}

// changePtr compares two optional values (medians, means).
func (c *calc) changePtr(cur, prev *float64) *Change {
	if c.prevWin == nil {
		return nil
	}
	if c.prev == nil || cur == nil || prev == nil {
		return &Change{Prev: prev}
	}
	return &Change{Prev: prev, Delta: fp(*cur - *prev), DeltaPct: ratio(*cur-*prev, *prev)}
}

// count builds an additive per-participant Count with a combined comparison.
func (c *calc) count(f func(*Rollup) int) *Count {
	m := &Count{ByParticipant: map[string]int{}}
	for _, id := range c.ids {
		m.ByParticipant[id.String()] = f(c.cur.of(id))
	}
	m.Combined = f(&c.cur.comb)
	prev := 0
	if c.prev != nil {
		prev = f(&c.prev.comb)
	}
	m.Change = c.change(float64(m.Combined), float64(prev), true)
	return m
}

// exactPercentile is percentile_cont over values (sorted copy).
func exactPercentile(vals []int64, p float64) float64 {
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := p * float64(len(s)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	frac := rank - float64(lo)
	return float64(s[lo]) + frac*float64(s[hi]-s[lo])
}

// topN ranks a map by count desc, key asc.
func topN(m map[string]int, n int) []Ranked {
	out := make([]Ranked, 0, len(m))
	for k, v := range m {
		if v > 0 {
			out = append(out, Ranked{Key: k, Count: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// pair returns the two most active participants in the window (by M-001),
// ties broken by participant order; fewer when the chat has fewer.
func (c *calc) pair() []uuid.UUID {
	ids := append([]uuid.UUID(nil), c.ids...)
	sort.SliceStable(ids, func(i, j int) bool { return c.cur.of(ids[i]).MsgCount > c.cur.of(ids[j]).MsgCount })
	if len(ids) > 2 {
		ids = ids[:2]
	}
	return ids
}

// ---- metric implementations ---------------------------------------------

// shareOfVoice is M-002: ratios of window sums, never averages of daily ratios.
func (c *calc) shareOfVoice() ShareOfVoice {
	s := ShareOfVoice{Messages: map[string]*float64{}, Words: map[string]*float64{}, Chars: map[string]*float64{}}
	tot := &c.cur.comb
	for _, id := range c.ids {
		r, k := c.cur.of(id), id.String()
		s.Messages[k] = ratio(float64(r.MsgCount), float64(tot.MsgCount))
		s.Words[k] = ratio(float64(r.WordCount), float64(tot.WordCount))
		s.Chars[k] = ratio(float64(r.CharCount), float64(tot.CharCount))
	}
	return s
}

// textLength is M-003.
func (c *calc) textLength() TextLength {
	conv := func(r *Rollup) TextStats {
		return TextStats{
			Words: r.WordCount, Chars: r.CharCount, TextMessages: r.TextMsgCount,
			AvgWords: ratio(float64(r.WordCount), float64(r.TextMsgCount)),
			AvgChars: ratio(float64(r.CharCount), float64(r.TextMsgCount)),
		}
	}
	t := TextLength{ByParticipant: map[string]TextStats{}}
	for _, id := range c.ids {
		t.ByParticipant[id.String()] = conv(c.cur.of(id))
	}
	t.Combined = conv(&c.cur.comb)
	return t
}

// activity is M-004.
func (c *calc) activity() Activity {
	a := Activity{ByParticipant: map[string]int{}}
	for _, id := range c.ids {
		a.ByParticipant[id.String()] = 0
	}
	for _, rows := range c.cur.days {
		total := 0
		for _, r := range rows {
			total += r.MsgCount
			if r.MsgCount > 0 {
				a.ByParticipant[r.ParticipantID.String()]++
			}
		}
		if total > 0 {
			a.ActiveDays++
		}
	}
	w := c.in.Window
	a.Rate = float64(a.ActiveDays) / float64(w.Days)
	a.CoveredDays = int(math.Round(w.Coverage * float64(w.Days)))
	a.RateCovered = ratio(float64(a.ActiveDays), float64(a.CoveredDays))
	return a
}

// streaks is M-005 (§4.3), clipped to the window.
func (c *calc) streaks() Streaks {
	pr := c.pair()
	var active, mutual []Date
	for _, d := range c.cur.sortedDays() {
		total := 0
		per := map[uuid.UUID]int{}
		for _, r := range c.cur.days[d] {
			total += r.MsgCount
			per[r.ParticipantID] += r.MsgCount
		}
		if total == 0 {
			continue
		}
		active = append(active, d)
		if len(pr) == 2 && per[pr[0]] > 0 && per[pr[1]] > 0 {
			mutual = append(mutual, d)
		}
	}
	ref := min(c.in.Window.To-1, c.today)
	var s Streaks
	s.Longest, s.Current = streakOf(active, ref)
	s.MutualLongest, s.MutualCurrent = streakOf(mutual, ref)
	return s
}

// streakOf returns the longest run of consecutive dates and the run ending at
// ref (or ref-1, the "today" grace).
func streakOf(days []Date, ref Date) (longest, current int) {
	run := 0
	for i, d := range days {
		if i > 0 && d == days[i-1]+1 {
			run++
		} else {
			run = 1
		}
		longest = max(longest, run)
	}
	if n := len(days); n > 0 && (days[n-1] == ref || days[n-1] == ref-1) {
		current = run
	}
	return longest, current
}

// silence is M-006, following the clipping algorithm of §4.4.
func (c *calc) silence() Silence {
	loc := c.in.Location
	w := c.in.Window
	fromT := w.From.Start(loc)
	end := w.To.Start(loc)
	if c.in.Now.Before(end) {
		end = c.in.Now
	}
	s := Silence{GapHist: c.cur.comb.GapHist, GapsBrokenBy: map[string][GapHistBuckets]int{}, MaxIntradayS: c.cur.comb.MaxIntradayGapS}
	for _, id := range c.ids {
		s.GapsBrokenBy[id.String()] = c.cur.of(id).GapHist
	}

	// Last message before the window, for silences that started earlier.
	var prevLast time.Time
	for i := range c.index {
		r := &c.index[i]
		if r.Date < w.From && r.MsgCount > 0 && r.LastMsgAt.After(prevLast) {
			prevLast = r.LastMsgAt
		}
	}

	var best time.Duration
	var bestBy *uuid.UUID
	seen := false
	for _, d := range c.cur.sortedDays() {
		var first, last time.Time
		var firstBy, intraBy uuid.UUID
		var intra int64
		for _, r := range c.cur.days[d] {
			if r.MsgCount == 0 {
				continue
			}
			if first.IsZero() || r.FirstMsgAt.Before(first) {
				first, firstBy = r.FirstMsgAt, r.ParticipantID
			}
			if r.LastMsgAt.After(last) {
				last = r.LastMsgAt
			}
			if r.MaxIntradayGapS > intra {
				intra, intraBy = r.MaxIntradayGapS, r.ParticipantID
			}
		}
		if first.IsZero() {
			continue
		}
		seen = true
		start := prevLast
		if start.IsZero() {
			start = first // nothing before: no silence into the chat's first message
		}
		if start.Before(fromT) {
			start = fromT
		}
		if g := first.Sub(start); g > best {
			best, bestBy = g, &firstBy
		}
		if g := time.Duration(intra) * time.Second; g > best {
			best, bestBy = g, &intraBy
		}
		prevLast = last
	}
	if !seen {
		s.NoMessages = true
		if end.After(fromT) {
			s.LongestS = int64(end.Sub(fromT) / time.Second)
		}
		s.Ongoing = true
		return s
	}
	if prevLast.Before(fromT) {
		prevLast = fromT
	}
	if g := end.Sub(prevLast); g > best {
		best, bestBy = g, nil
		s.Ongoing = true
	}
	s.LongestS = int64(best / time.Second)
	if bestBy != nil {
		k := bestBy.String()
		s.BrokenBy = &k
	}
	return s
}

// sessions is M-007.
func (c *calc) sessions() Sessions {
	r := &c.cur.comb
	n := float64(r.SessionsInitiated)
	s := Sessions{
		Count:        r.SessionsInitiated,
		AvgMessages:  ratio(float64(r.SessionMsgSum), n),
		AvgDurationS: ratio(float64(r.SessionDurationSumS), n),
		AvgTurns:     ratio(float64(r.SessionTurnsSum), n),
	}
	prev := 0
	if c.prev != nil {
		prev = c.prev.comb.SessionsInitiated
	}
	s.Change = c.change(n, float64(prev), true)
	return s
}

// initiations is M-008.
func (c *calc) initiations() Initiations {
	m := Initiations{ByParticipant: map[string]int{}, Ratio: map[string]*float64{}, Combined: c.cur.comb.SessionsInitiated}
	for _, id := range c.ids {
		n := c.cur.of(id).SessionsInitiated
		m.ByParticipant[id.String()] = n
		m.Ratio[id.String()] = ratio(float64(n), float64(m.Combined))
	}
	return m
}

// responseStats summarises one responder (M-010): exact when values are
// given, otherwise mean exact and percentiles from the log histogram.
func responseStats(r *Rollup, exact []int64, useExact bool) ResponseStats {
	var s ResponseStats
	if useExact {
		s.N = len(exact)
		if s.N > 0 {
			sum := int64(0)
			for _, v := range exact {
				sum += v
			}
			s.MeanS = fp(float64(sum) / float64(s.N))
			s.MedianS = fp(exactPercentile(exact, 0.5))
			s.P90S = fp(exactPercentile(exact, 0.9))
		}
	} else {
		s.N = r.ResponseCount
		if s.N > 0 {
			s.MeanS = fp(float64(r.ResponseSumS) / float64(s.N))
			if v, ok := PercentileFromHist(r.ResponseHist[:], 0.5, ResponseBucketBounds); ok {
				s.MedianS = fp(v)
			}
			if v, ok := PercentileFromHist(r.ResponseHist[:], 0.9, ResponseBucketBounds); ok {
				s.P90S = fp(v)
			}
			s.Approx = true
		}
	}
	s.FewData = s.N > 0 && s.N < 5
	return s
}

func (c *calc) responseTime() ResponseTime {
	m := ResponseTime{ByParticipant: map[string]ResponseStats{}}
	useExact := c.in.Exact != nil && c.in.Exact.ResponseTimes != nil
	var all []int64
	for _, id := range c.ids {
		var ex []int64
		if useExact {
			ex = c.in.Exact.ResponseTimes[id]
			all = append(all, ex...)
		}
		m.ByParticipant[id.String()] = responseStats(c.cur.of(id), ex, useExact)
	}
	m.Combined = responseStats(&c.cur.comb, all, useExact)
	if c.prevWin != nil {
		var prevMedian *float64
		if c.prev != nil {
			pe := c.in.PrevExact != nil && c.in.PrevExact.ResponseTimes != nil
			var pall []int64
			if pe {
				for _, v := range c.in.PrevExact.ResponseTimes {
					pall = append(pall, v...)
				}
			}
			prevMedian = responseStats(&c.prev.comb, pall, pe).MedianS
		}
		m.Change = c.changePtr(m.Combined.MedianS, prevMedian)
	}
	return m
}

// replyRate is M-011: the share of the other side's eligible turns that P
// answered; closing turns without a question are not eligible.
func (c *calc) replyRate() ReplyRate {
	m := ReplyRate{ByParticipant: map[string]*float64{}}
	for _, id := range c.ids {
		replied, awaiting := 0, 0
		for _, other := range c.ids {
			if other == id {
				continue
			}
			r := c.cur.of(other)
			replied += r.TurnsReplied
			awaiting += r.TurnsAwaitingUnreplied
		}
		m.ByParticipant[id.String()] = ratio(float64(replied), float64(replied+awaiting))
	}
	return m
}

// turns is M-012.
func (c *calc) turns() Turns {
	t := Turns{Turns: map[string]int{}, MessagesPerTurn: map[string]*float64{}, DoubleTexts: map[string]int{}, FollowupsAfterNoReply: map[string]int{}}
	for _, id := range c.ids {
		r, k := c.cur.of(id), id.String()
		t.Turns[k] = r.TurnCount
		t.MessagesPerTurn[k] = ratio(float64(r.MsgCount), float64(r.TurnCount))
		t.DoubleTexts[k] = r.DoubleTextCount
		t.FollowupsAfterNoReply[k] = r.FollowupAfterNoReply
	}
	return t
}

// heatmap is M-013; weekday index 0 is Monday.
func (c *calc) heatmap() Heatmap {
	h := Heatmap{ByParticipant: map[string][7][24]int{}}
	for _, id := range c.ids {
		h.ByParticipant[id.String()] = [7][24]int{}
	}
	for d, rows := range c.cur.days {
		wd := (int(d.Weekday()) + 6) % 7
		for _, r := range rows {
			k := r.ParticipantID.String()
			cell := h.ByParticipant[k]
			for hr, n := range r.HourlyCounts {
				cell[wd][hr] += n
				h.Combined[wd][hr] += n
			}
			h.ByParticipant[k] = cell
		}
	}
	return h
}

// lateNight is M-014, derived from hourly counts so the setting applies at
// query time. A range with from >= to wraps past midnight.
func (c *calc) lateNight() LateNight {
	from, to := c.in.Settings.LateNightFrom, c.in.Settings.LateNightTo
	inRange := func(h int) bool {
		if from < to {
			return h >= from && h < to
		}
		return h >= from || h < to
	}
	late := func(r *Rollup) int {
		n := 0
		for h, v := range r.HourlyCounts {
			if inRange(h) {
				n += v
			}
		}
		return n
	}
	l := LateNight{Hours: [2]int{from, to}, Messages: map[string]int{}, Share: map[string]*float64{}}
	for _, id := range c.ids {
		r, k := c.cur.of(id), id.String()
		l.Messages[k] = late(r)
		l.Share[k] = ratio(float64(late(r)), float64(r.MsgCount))
	}
	l.Combined = ratio(float64(late(&c.cur.comb)), float64(c.cur.comb.MsgCount))
	return l
}

// foldModifiers removes skin-tone modifiers U+1F3FB–U+1F3FF.
func foldModifiers(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		k = strings.Map(func(r rune) rune {
			if r >= 0x1F3FB && r <= 0x1F3FF {
				return -1
			}
			return r
		}, k)
		out[k] += v
	}
	return out
}

// emoji is M-015.
func (c *calc) emoji() Emoji {
	conv := func(r *Rollup) EmojiStats {
		counts := r.EmojiCounts
		if c.in.Settings.EmojiNormaliseModifiers {
			counts = foldModifiers(counts)
		}
		aff := 0
		for k, v := range counts {
			if affectionEmoji[k] {
				aff += v
			}
		}
		return EmojiStats{
			Total:      r.EmojiCount,
			PerTextMsg: ratio(float64(r.EmojiCount), float64(r.TextMsgCount)),
			MsgShare:   ratio(float64(r.EmojiMsgCount), float64(r.TextMsgCount)),
			Affection:  aff,
			Top:        topN(counts, topEmoji),
		}
	}
	e := Emoji{ByParticipant: map[string]EmojiStats{}}
	for _, id := range c.ids {
		e.ByParticipant[id.String()] = conv(c.cur.of(id))
	}
	e.Combined = conv(&c.cur.comb)
	return e
}

// topTerms is M-016: exact counts for short windows, merged daily top-K maps
// (approximate) otherwise.
func (c *calc) topTerms() TopTerms {
	t := TopTerms{Terms: map[string][]Ranked{}, Bigrams: map[string][]Ranked{}}
	var combT, combB map[string]int
	exact := c.in.Exact != nil && c.in.Exact.Terms != nil
	for _, id := range c.ids {
		var terms, bigrams map[string]int
		if exact {
			terms = c.in.Exact.Terms[id]
			if c.in.Exact.Bigrams != nil {
				bigrams = c.in.Exact.Bigrams[id]
			}
		} else {
			r := c.cur.of(id)
			terms, bigrams = r.TopTerms, r.TopBigrams
		}
		t.Terms[id.String()] = topN(terms, topTerms)
		t.Bigrams[id.String()] = topN(bigrams, topTerms)
		combT = addMap(combT, terms)
		combB = addMap(combB, bigrams)
	}
	t.Combined = topN(combT, topTerms)
	t.CombinedBigrams = topN(combB, topTerms)
	t.Approximate = !exact && c.in.Window.Days > 1
	return t
}

// media is M-019.
func (c *calc) media() Media {
	conv := func(r *Rollup) MediaBreakdown {
		b := MediaBreakdown{
			Image: r.MediaImage, Video: r.MediaVideo, Audio: r.MediaAudio, Voice: r.MediaVoice,
			Sticker: r.MediaSticker, GIF: r.MediaGIF, Document: r.MediaDocument, Other: r.MediaOther,
			Location: r.LocationCount, Contact: r.ContactCount, Poll: r.PollCount,
		}
		b.Total = b.Image + b.Video + b.Audio + b.Voice + b.Sticker + b.GIF + b.Document + b.Other + b.Location + b.Contact + b.Poll
		return b
	}
	m := Media{ByParticipant: map[string]MediaBreakdown{}}
	for _, id := range c.ids {
		m.ByParticipant[id.String()] = conv(c.cur.of(id))
	}
	m.Combined = conv(&c.cur.comb)
	return m
}

func sentimentStats(r *Rollup) SentimentStats {
	var median *float64
	if v, ok := LinearPercentileFromHist(r.SentimentHist[:], 0.5, SentimentBucketBounds); ok {
		median = fp(v)
	}
	return SentimentStats{
		Mean:     ratio(r.SentimentSum, float64(r.SentimentScoredCount)),
		Median:   median,
		Scored:   r.SentimentScoredCount,
		PosShare: ratio(float64(r.SentimentPos), float64(r.TextMsgCount)),
		NeuShare: ratio(float64(r.SentimentNeu), float64(r.TextMsgCount)),
		NegShare: ratio(float64(r.SentimentNeg), float64(r.TextMsgCount)),
		Hist:     r.SentimentHist,
	}
}

// sentiment is M-022.
func (c *calc) sentiment() Sentiment {
	s := Sentiment{ByParticipant: map[string]SentimentStats{}}
	for _, id := range c.ids {
		s.ByParticipant[id.String()] = sentimentStats(c.cur.of(id))
	}
	s.Combined = sentimentStats(&c.cur.comb)
	if c.prevWin != nil {
		var prev *float64
		if c.prev != nil {
			prev = sentimentStats(&c.prev.comb).Mean
		}
		s.Change = c.changePtr(s.Combined.Mean, prev)
	}
	return s
}

// reciprocity is M-025. It also returns the unrounded value for M-026.
func (c *calc) reciprocity() (Score, *float64) {
	minMsgs := c.in.Settings.MinMsgsForScores
	s := Score{MinMsgs: minMsgs}
	pr := c.pair()
	for _, id := range pr {
		s.Pair = append(s.Pair, id.String())
	}
	if len(pr) < 2 || c.cur.of(pr[0]).MsgCount < minMsgs || c.cur.of(pr[1]).MsgCount < minMsgs {
		s.Reason = ReasonInsufficientData
		return s, nil
	}
	a, b := c.cur.of(pr[0]), c.cur.of(pr[1])
	ra, rb := c.resp.ByParticipant[pr[0].String()], c.resp.ByParticipant[pr[1].String()]

	type dim struct {
		name   string
		a, b   float64
		ok     bool
		weight float64
	}
	// The response dimension needs enough replies on both sides; the minimum
	// scales with min_msgs_for_scores so tiny test windows stay meaningful.
	minResp := min(3, minMsgs)
	dims := []dim{
		{"messages", float64(a.MsgCount), float64(b.MsgCount), true, 0.30},
		{"words", float64(a.WordCount), float64(b.WordCount), true, 0.20},
		{"initiations", float64(a.SessionsInitiated), float64(b.SessionsInitiated), true, 0.30},
		{"response_time", 0, 0, false, 0.20},
	}
	if ra.MedianS != nil && rb.MedianS != nil && ra.N >= minResp && rb.N >= minResp {
		dims[3].a, dims[3].b, dims[3].ok = *ra.MedianS, *rb.MedianS, true
	}
	total := 0.0
	for _, d := range dims {
		if d.ok {
			total += d.weight
		}
	}
	s.Dimensions = map[string]Dimension{}
	sum := 0.0
	for _, d := range dims {
		out := Dimension{A: d.a, B: d.b}
		if d.ok {
			imb := 0.0
			if d.a+d.b > 0 {
				imb = math.Abs(d.a-d.b) / (d.a + d.b)
			}
			out.Imbalance = fp(imb)
			out.Weight = d.weight / total
			sum += out.Weight * imb
		}
		s.Dimensions[d.name] = out
	}
	v := 100 * (1 - sum)
	s.Value = fp(math.Round(v*10) / 10)
	return s, &v
}

// engagement is M-026 (engagement_v1).
func (c *calc) engagement() Score {
	minMsgs := c.in.Settings.MinMsgsForScores
	s := Score{MinMsgs: minMsgs, Version: "engagement_v1"}
	_, recip := c.reciprocity()
	if recip == nil {
		s.Reason = ReasonInsufficientData
		return s
	}
	w := c.in.Settings.EngagementWeights
	names := [5]string{"frequency", "volume", "responsiveness", "reciprocity", "depth"}
	act := c.activity()
	days := float64(c.in.Window.Days)
	comb := &c.cur.comb
	avgSession := 0.0
	if comb.SessionsInitiated > 0 {
		avgSession = float64(comb.SessionMsgSum) / float64(comb.SessionsInitiated)
	}
	comp := [5]*float64{
		fp(float64(act.ActiveDays) / days),
		fp(math.Min(1, math.Log(1+float64(comb.MsgCount)/days)/math.Log(101))),
		nil,
		fp(*recip / 100),
		fp(math.Min(1, math.Log(1+avgSession)/math.Log(51))),
	}
	if m := c.resp.Combined.MedianS; m != nil {
		comp[2] = fp(1 / (1 + *m/900))
	}
	total := 0.0
	for i, v := range comp {
		if v != nil {
			total += w[i]
		}
	}
	s.Components, s.Weights = map[string]float64{}, map[string]float64{}
	sum := 0.0
	for i, v := range comp {
		s.Weights[names[i]] = w[i]
		if v == nil || total == 0 {
			continue
		}
		s.Components[names[i]] = *v
		sum += w[i] / total * *v
	}
	s.Value = fp(math.Round(100*sum*10) / 10)
	return s
}

// ---- M-027 trend ----------------------------------------------------------

// trend is M-027: change vs the previous window plus an OLS slope over the
// daily series inside the window.
func (c *calc) trend() Trend {
	t := Trend{
		MessagesPerDay: TrendGroup{ByParticipant: map[string]TrendSeries{}},
		ResponseMedian: TrendGroup{ByParticipant: map[string]TrendSeries{}},
		Sentiment:      TrendGroup{ByParticipant: map[string]TrendSeries{}},
	}
	w := c.in.Window
	lo, hi := max(w.From, c.chatFirst), min(w.To, c.chatLast+1)

	type series struct{ x, y []float64 }
	build := func(sel func(rows []*Rollup) (float64, bool)) series {
		var s series
		for d := lo; d < hi; d++ {
			if v, ok := sel(c.cur.days[d]); ok {
				s.x = append(s.x, float64(d-w.From))
				s.y = append(s.y, v)
			}
		}
		return s
	}
	filter := func(rows []*Rollup, id *uuid.UUID) *Rollup {
		var r Rollup
		for _, x := range rows {
			if id == nil || x.ParticipantID == *id {
				addRollupCounters(&r, x) // daily series never need the maps
			}
		}
		return &r
	}
	msgsSel := func(id *uuid.UUID) func([]*Rollup) (float64, bool) {
		return func(rows []*Rollup) (float64, bool) { return float64(filter(rows, id).MsgCount), true }
	}
	respSel := func(id *uuid.UUID) func([]*Rollup) (float64, bool) {
		return func(rows []*Rollup) (float64, bool) {
			r := filter(rows, id)
			if r.ResponseCount < 3 {
				return 0, false
			}
			return PercentileFromHist(r.ResponseHist[:], 0.5, ResponseBucketBounds)
		}
	}
	sentSel := func(id *uuid.UUID) func([]*Rollup) (float64, bool) {
		return func(rows []*Rollup) (float64, bool) {
			r := filter(rows, id)
			if r.SentimentScoredCount == 0 {
				return 0, false
			}
			return r.SentimentSum / float64(r.SentimentScoredCount), true
		}
	}
	finish := func(s series, cur, prev *float64) TrendSeries {
		ts := TrendSeries{Current: cur, Points: len(s.y)}
		if c.prevWin != nil && c.prev != nil {
			ts.Prev = prev
			if cur != nil && prev != nil {
				ts.Delta = fp(*cur - *prev)
				ts.DeltaPct = ratio(*cur-*prev, *prev)
			}
		}
		if len(s.y) < 7 {
			return ts
		}
		slope, r2, mean := ols(s.x, s.y)
		ts.Slope, ts.R2 = fp(slope), fp(r2)
		label := "steady"
		if mean != 0 {
			rel := slope * float64(w.Days) / mean
			ts.Rel = fp(rel)
			if math.Abs(rel) >= 0.15 && r2 >= 0.2 {
				if rel > 0 {
					label = "rising"
				} else {
					label = "falling"
				}
			}
		}
		ts.Label = &label
		return ts
	}
	perDay := func(r *Rollup, days int) *float64 { return ratio(float64(r.MsgCount), float64(days)) }
	prevDays := 0
	if c.prevWin != nil {
		prevDays = c.prevWin.Days
	}
	prevOf := func(id *uuid.UUID) *Rollup {
		if c.prev == nil {
			return &Rollup{}
		}
		if id == nil {
			return &c.prev.comb
		}
		return c.prev.of(*id)
	}
	curOf := func(id *uuid.UUID) *Rollup {
		if id == nil {
			return &c.cur.comb
		}
		return c.cur.of(*id)
	}
	medianOf := func(r *Rollup) *float64 {
		if r.ResponseCount == 0 {
			return nil
		}
		if v, ok := PercentileFromHist(r.ResponseHist[:], 0.5, ResponseBucketBounds); ok {
			return fp(v)
		}
		return nil
	}
	one := func(id *uuid.UUID) (TrendSeries, TrendSeries, TrendSeries) {
		cr, pr := curOf(id), prevOf(id)
		curMed := medianOf(cr)
		if id == nil {
			curMed = c.resp.Combined.MedianS
		} else if st, ok := c.resp.ByParticipant[id.String()]; ok {
			curMed = st.MedianS
		}
		return finish(build(msgsSel(id)), perDay(cr, w.Days), perDay(pr, prevDays)),
			finish(build(respSel(id)), curMed, medianOf(pr)),
			finish(build(sentSel(id)), sentimentStats(cr).Mean, sentimentStats(pr).Mean)
	}
	for _, id := range c.ids {
		id := id
		m, r, s := one(&id)
		k := id.String()
		t.MessagesPerDay.ByParticipant[k], t.ResponseMedian.ByParticipant[k], t.Sentiment.ByParticipant[k] = m, r, s
	}
	t.MessagesPerDay.Combined, t.ResponseMedian.Combined, t.Sentiment.Combined = one(nil)
	return t
}

// ols fits y = a + b·x and returns the slope, R² and the mean of y.
func ols(x, y []float64) (slope, r2, mean float64) {
	n := float64(len(x))
	var sx, sy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
	}
	mx, my := sx/n, sy/n
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 {
		return 0, 0, my
	}
	slope = sxy / sxx
	if syy > 0 {
		r2 = (sxy * sxy) / (sxx * syy)
	}
	return slope, r2, my
}
