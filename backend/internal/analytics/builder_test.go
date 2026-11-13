package analytics

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeText is a deterministic TextAnalyzer for builder tests, so they do not
// depend on the real lexicons in analytics/text:
//   - words: whitespace-separated fields without emoji
//   - emoji: fields from a small known set
//   - question: contains "?"
//   - sentiment: "miss"/"love"/"well" → +0.6, "hate" → −0.7, otherwise unscored
//   - affection: one hit per "miss"/"love"; conflict: contains "hate"
//   - terms: lower-cased words
type fakeText struct{}

var fakeEmoji = map[string]bool{"☀️": true, "🙂": true, "🤞": true, "❤️": true, "😴": true}

func (fakeText) Analyze(body string) TextFeatures {
	var f TextFeatures
	for _, w := range strings.Fields(body) {
		if fakeEmoji[w] {
			f.Emoji = append(f.Emoji, w)
			continue
		}
		f.WordCount++
		f.CharCount += len([]rune(w))
		f.Terms = append(f.Terms, strings.ToLower(strings.Trim(w, "!?.,")))
	}
	lower := strings.ToLower(body)
	f.IsQuestion = strings.Contains(body, "?")
	switch {
	case strings.Contains(lower, "hate"):
		f.Sentiment, f.SentimentScored, f.Conflict = -0.7, true, true
	case strings.Contains(lower, "miss"), strings.Contains(lower, "love"), strings.Contains(lower, "well"):
		f.Sentiment, f.SentimentScored = 0.6, true
	}
	f.AffectionHits = strings.Count(lower, "miss") + strings.Count(lower, "love")
	if strings.Contains(lower, "http") {
		f.LinkDomains = []string{"example.com"}
	}
	return f
}

// captureSink records everything the builder emits.
type captureSink struct {
	features       []MessageFeatures
	sessions       []Session
	featureBatches int
}

func (c *captureSink) MessageFeatures(b []MessageFeatures) error {
	c.featureBatches++
	c.features = append(c.features, b...)
	return nil
}

func (c *captureSink) Sessions(b []Session) error {
	c.sessions = append(c.sessions, b...)
	return nil
}

var (
	pA = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	pR = uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	pC = uuid.MustParse("00000000-0000-0000-0000-00000000000c")
)

// chat builds messages with sequential IDs and ordinals.
type chat struct {
	loc  *time.Location
	msgs []*Message
}

func (c *chat) add(at time.Time, pid uuid.UUID, kind, body string) *Message {
	m := &Message{
		ID: int64(len(c.msgs) + 1), Ordinal: int64(len(c.msgs) + 1),
		ParticipantID: pid, SentAt: at.UTC(), Kind: kind, Body: body,
	}
	c.msgs = append(c.msgs, m)
	return m
}

func (c *chat) build(t testing.TB, cfg BuildConfig) (BuildOutput, *captureSink) {
	t.Helper()
	if cfg.Location == nil {
		cfg.Location = c.loc
	}
	if cfg.Text == nil {
		cfg.Text = fakeText{}
	}
	if cfg.Settings.SessionGap == 0 {
		cfg.Settings = DefaultSettings()
	}
	sink := &captureSink{}
	b := NewBuilder(cfg, sink)
	for _, m := range c.msgs {
		if err := b.Add(m); err != nil {
			t.Fatalf("Add(%d): %v", m.ID, err)
		}
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out, sink
}

func rollupFor(t testing.TB, out BuildOutput, pid uuid.UUID, date string) Rollup {
	t.Helper()
	d, err := ParseDate(date)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Rollups {
		if r.ParticipantID == pid && r.Date == d {
			return r
		}
	}
	t.Fatalf("no rollup for %v on %s", pid, date)
	return Rollup{}
}

func mustLoc(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// referenceChat is the worked example of docs/04-metrics-catalog.md §7.
func referenceChat(t testing.TB) *chat {
	loc := mustLoc(t, "Asia/Kolkata")
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 3, day, hh, mm, 0, 0, loc) }
	c := &chat{loc: loc}
	c.add(at(2, 8, 0), pA, KindText, "Good morning! ☀️")
	c.add(at(2, 8, 5), pR, KindText, "Morning 🙂 how did it go?")
	c.add(at(2, 8, 6), pR, KindText, "the interview I mean")
	c.add(at(2, 8, 20), pA, KindText, "It went well!! I think 🤞")
	img := c.add(at(2, 8, 21), pA, KindMedia, "")
	img.MediaType = "image"
	c.add(at(2, 14, 30), pR, KindText, "Lunch?")
	c.add(at(2, 14, 31), pA, KindText, "Yes! 20 min")
	c.add(at(2, 23, 50), pA, KindText, "miss you ❤️")
	c.add(at(3, 0, 10), pR, KindText, "miss you too ❤️")
	c.add(at(3, 0, 40), pA, KindText, "😴 good night")
	return c
}

func TestBuilderReferenceExample(t *testing.T) {
	out, sink := referenceChat(t).build(t, BuildConfig{})

	if out.Sessions != 3 || len(sink.sessions) != 3 {
		t.Fatalf("sessions = %d / %d, want 3", out.Sessions, len(sink.sessions))
	}
	wantInit := []uuid.UUID{pA, pR, pA}
	for i, s := range sink.sessions {
		if s.Index != i || s.Initiator != wantInit[i] || s.Ender != pA {
			t.Errorf("session %d = %+v", i, s)
		}
		if s.StartDate.String() != "2026-03-02" { // S3 spans midnight: attributed to Monday (R-02)
			t.Errorf("session %d start date %s", i, s.StartDate)
		}
	}
	if sink.sessions[2].MsgCount != 3 || sink.sessions[2].TurnCount != 3 {
		t.Errorf("S3 = %+v", sink.sessions[2])
	}

	aMon := rollupFor(t, out, pA, "2026-03-02")
	aTue := rollupFor(t, out, pA, "2026-03-03")
	rMon := rollupFor(t, out, pR, "2026-03-02")
	rTue := rollupFor(t, out, pR, "2026-03-03")

	checks := []struct {
		name      string
		got, want int64
	}{
		// M-001: A 6 messages (5 Mon + 1 Tue), R 4 (3 + 1).
		{"A Mon msgs", int64(aMon.MsgCount), 5}, {"A Tue msgs", int64(aTue.MsgCount), 1},
		{"R Mon msgs", int64(rMon.MsgCount), 3}, {"R Tue msgs", int64(rTue.MsgCount), 1},
		// M-003: message 5 is media-only.
		{"A Mon text msgs", int64(aMon.TextMsgCount), 4},
		{"A Mon images", int64(aMon.MediaImage), 1},
		// M-007/M-008: S1 and S3 by A, S2 by R, all on Monday.
		{"A sessions initiated", int64(aMon.SessionsInitiated), 2},
		{"R sessions initiated", int64(rMon.SessionsInitiated), 1},
		{"A session msgs", int64(aMon.SessionMsgSum), 8},          // S1 5 + S3 3
		{"A session duration", aMon.SessionDurationSumS, 71 * 60}, // 21 + 50 min
		{"A session turns", int64(aMon.SessionTurnsSum), 6},
		{"R session duration", rMon.SessionDurationSumS, 60},
		// M-009: A ended all three sessions; credited on the start day.
		{"A enders", int64(aMon.SessionsEnded), 3}, {"R enders", int64(rMon.SessionsEnded), 0},
		// M-010: A {14, 1} min Mon + {30} Tue; R {5} Mon + {20} Tue (R-03).
		{"A Mon responses", int64(aMon.ResponseCount), 2}, {"A Mon resp sum", aMon.ResponseSumS, 15 * 60},
		{"A Tue responses", int64(aTue.ResponseCount), 1}, {"A Tue resp sum", aTue.ResponseSumS, 30 * 60},
		{"R Mon resp sum", rMon.ResponseSumS, 5 * 60}, {"R Tue resp sum", rTue.ResponseSumS, 20 * 60},
		// M-012: A turns A1–A4 Mon, A5 Tue; R R1, R2 Mon, R3 Tue.
		{"A Mon turns", int64(aMon.TurnCount), 4}, {"A Tue turns", int64(aTue.TurnCount), 1},
		{"R Mon turns", int64(rMon.TurnCount), 2}, {"R Tue turns", int64(rTue.TurnCount), 1},
		{"double texts", int64(aMon.DoubleTextCount + rMon.DoubleTextCount), 0},
		// M-011: A1 and A4 replied (both Mon); R1, R2 (Mon) and R3 (Tue)
		// replied; closing turns A2, A3, A5 asked nothing.
		{"A replied", int64(aMon.TurnsReplied), 2}, {"R Mon replied", int64(rMon.TurnsReplied), 2},
		{"R Tue replied", int64(rTue.TurnsReplied), 1},
		{"awaiting", int64(aMon.TurnsAwaitingUnreplied + aTue.TurnsAwaitingUnreplied + rMon.TurnsAwaitingUnreplied), 0},
		// M-006: 9 h 19 m broken by A and 6 h 09 m broken by R (same day);
		// R's Tue 00:10 gap crosses midnight so it is not intraday.
		{"A Mon max gap", aMon.MaxIntradayGapS, 9*3600 + 19*60},
		{"R Mon max gap", rMon.MaxIntradayGapS, 6*3600 + 9*60},
		{"R Tue max gap", rTue.MaxIntradayGapS, 0},
		{"A Tue max gap", aTue.MaxIntradayGapS, 30 * 60},
		// M-013: A Mon 08 → 3, 14 → 1, 23 → 1; Tue 00 → 1.
		{"A Mon 08h", int64(aMon.HourlyCounts[8]), 3}, {"A Mon 14h", int64(aMon.HourlyCounts[14]), 1},
		{"A Mon 23h", int64(aMon.HourlyCounts[23]), 1}, {"A Tue 00h", int64(aTue.HourlyCounts[0]), 1},
		// M-017: R asked twice (messages 2 and 6).
		{"R questions", int64(rMon.QuestionCount), 2},
		// M-023: one affection message each (8 and 9).
		{"A affection", int64(aMon.AffectionMsgCount), 1}, {"R affection", int64(rTue.AffectionMsgCount), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// Per-message features: turn starts, sessions and response times.
	type want struct {
		session   int
		turnStart bool
		resp      int64 // -1 = nil
	}
	wants := []want{
		{0, true, -1}, {0, true, 300}, {0, false, -1}, {0, true, 840}, {0, false, -1},
		{1, true, -1}, {1, true, 60}, {2, true, -1}, {2, true, 1200}, {2, true, 1800},
	}
	if len(sink.features) != 10 {
		t.Fatalf("features = %d", len(sink.features))
	}
	for i, w := range wants {
		f := sink.features[i]
		got := int64(-1)
		if f.ResponseS != nil {
			got = *f.ResponseS
		}
		if f.MessageID != int64(i+1) || f.SessionIndex != w.session || f.IsTurnStart != w.turnStart || got != w.resp {
			t.Errorf("message %d features = %+v (resp %d), want %+v", i+1, f, got, w)
		}
	}
	if out.FirstDate.String() != "2026-03-02" || out.LastDate.String() != "2026-03-03" {
		t.Errorf("date range %s..%s", out.FirstDate, out.LastDate)
	}
}

func TestBuilderDSTSpringForward(t *testing.T) {
	// Europe/London jumps 01:00 GMT → 02:00 BST on 2026-03-29.
	loc := mustLoc(t, "Europe/London")
	c := &chat{loc: loc}
	c.add(time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC), pA, KindText, "late")  // 00:30 GMT
	c.add(time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), pR, KindText, "later") // 02:30 BST
	c.add(time.Date(2026, 3, 29, 23, 30, 0, 0, time.UTC), pA, KindText, "next") // 00:30 BST next day
	out, _ := c.build(t, BuildConfig{})
	a := rollupFor(t, out, pA, "2026-03-29")
	r := rollupFor(t, out, pR, "2026-03-29")
	if a.HourlyCounts[0] != 1 || r.HourlyCounts[2] != 1 || r.HourlyCounts[1] != 0 {
		t.Errorf("hourly A=%v R=%v", a.HourlyCounts, r.HourlyCounts)
	}
	// One wall-clock hour apart → a 3600 s response on the same local day.
	if r.ResponseSumS != 3600 || r.MaxIntradayGapS != 3600 {
		t.Errorf("response %d gap %d", r.ResponseSumS, r.MaxIntradayGapS)
	}
	if next := rollupFor(t, out, pA, "2026-03-30"); next.MsgCount != 1 {
		t.Errorf("next-day row = %+v", next)
	}
}

func TestBuilderMinutePrecisionTies(t *testing.T) {
	loc := time.UTC
	c := &chat{loc: loc}
	at := time.Date(2024, 5, 1, 10, 0, 0, 0, loc)
	c.add(at, pA, KindText, "hi")
	c.add(at, pR, KindText, "hey") // same minute: response 0 s, bucket 0
	c.add(at, pA, KindText, "how are you")
	out, sink := c.build(t, BuildConfig{})
	r := rollupFor(t, out, pR, "2024-05-01")
	if r.ResponseCount != 1 || r.ResponseSumS != 0 || r.ResponseHist[0] != 1 {
		t.Errorf("tie response: %+v", r)
	}
	if *sink.features[2].ResponseS != 0 {
		t.Errorf("third message response %d", *sink.features[2].ResponseS)
	}
}

func TestBuilderMessageKinds(t *testing.T) {
	loc := time.UTC
	c := &chat{loc: loc}
	at := func(min int) time.Time { return time.Date(2024, 5, 1, 10, min, 0, 0, loc) }
	c.add(at(0), uuid.Nil, KindSystem, "Messages are end-to-end encrypted")
	del := c.add(at(1), pA, KindDeleted, "")
	del.IsDeleted = true
	ed := c.add(at(2), pR, KindText, "I love it")
	ed.IsEdited = true
	fw := c.add(at(3), pA, KindText, "love this forwarded text")
	fw.IsForwarded = true
	cap := c.add(at(4), pR, KindMedia, "look http://x")
	cap.MediaType = "video_note"
	call := c.add(at(5), pA, KindCall, "")
	call.Call = &CallInfo{Video: true, Missed: true, DurationSec: 0}
	call2 := c.add(at(6), pR, KindCall, "")
	call2.Call = &CallInfo{DurationSec: 120}
	c.add(at(7), uuid.Nil, KindText, "a sender outside the analysed pair")
	voice := c.add(at(8), pA, KindMedia, "")
	voice.MediaType = "voice_note"
	loc2 := c.add(at(9), pR, KindLocation, "")
	_ = loc2

	out, sink := c.build(t, BuildConfig{})
	a := rollupFor(t, out, pA, "2024-05-01")
	r := rollupFor(t, out, pR, "2024-05-01")

	checks := map[string][2]int{
		"A msgs (deleted, forwarded, voice)": {a.MsgCount, 3},
		"R msgs (edited, caption, location)": {r.MsgCount, 3},
		"A deleted":                          {a.DeletedCount, 1},
		"R edited":                           {r.EditedCount, 1},
		"A forwarded":                        {a.ForwardedCount, 1},
		"A text msgs (forwarded only)":       {a.TextMsgCount, 1},
		"A words (forwarded still counts)":   {a.WordCount, 4},
		"A sentiment scored (forwarded no)":  {a.SentimentScoredCount, 0},
		"A affection (forwarded no)":         {a.AffectionMsgCount, 0},
		"R text msgs (edited + caption)":     {r.TextMsgCount, 2},
		"R sentiment scored":                 {r.SentimentScoredCount, 1},
		"R video (video_note)":               {r.MediaVideo, 1},
		"R links":                            {r.LinkCount, 1},
		"R location":                         {r.LocationCount, 1},
		"A voice":                            {a.MediaVoice, 1},
		"A video calls":                      {a.CallVideoCount, 1},
		"A missed calls":                     {a.CallMissedCount, 1},
		"R voice calls":                      {r.CallVoiceCount, 1},
		"R call duration":                    {r.CallDurationS, 120},
	}
	for name, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %d, want %d", name, v[0], v[1])
		}
	}
	if len(a.TopTerms) != 0 {
		t.Errorf("forwarded text leaked into top terms: %v", a.TopTerms)
	}
	// Every message has a features row; system, call and outsider rows take
	// no part in sessions.
	if len(sink.features) != len(c.msgs) {
		t.Fatalf("features %d for %d messages", len(sink.features), len(c.msgs))
	}
	for _, i := range []int{0, 5, 6, 7} {
		if sink.features[i].SessionIndex != -1 || sink.features[i].IsTurnStart {
			t.Errorf("message %d should be outside sessions: %+v", i+1, sink.features[i])
		}
	}
	// Calls do not break the session: one session for all human rows.
	if out.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", out.Sessions)
	}
	// Forwarded text: words counted on the row, sentiment not exposed.
	if f := sink.features[3]; f.WordCount != 4 || f.Sentiment != nil || f.AffectionHits != 0 {
		t.Errorf("forwarded features %+v", f)
	}
	// With the override, forwarded text feeds sentiment too.
	s := DefaultSettings()
	s.IncludeForwardedInText = true
	out2, _ := c.build(t, BuildConfig{Settings: s})
	if rollupFor(t, out2, pA, "2024-05-01").SentimentScoredCount != 1 {
		t.Error("IncludeForwardedInText did not include forwarded text")
	}
}

func TestBuilderDoubleTextFollowupAndAwaiting(t *testing.T) {
	loc := time.UTC
	c := &chat{loc: loc}
	at := func(h, m int) time.Time { return time.Date(2024, 5, 1, h, m, 0, 0, loc) }
	c.add(at(9, 0), pR, KindText, "morning")
	c.add(at(10, 0), pA, KindText, "hi")
	c.add(at(10, 40), pA, KindText, "you there?") // double-text (40 min ≥ 30 min), closing question
	c.add(at(18, 0), pA, KindText, "hello??")     // new session (gap 7h20m) by A after unanswered question
	c.add(at(18, 5), pR, KindText, "sorry, busy")
	out, sink := c.build(t, BuildConfig{})
	a := rollupFor(t, out, pA, "2024-05-01")
	if a.DoubleTextCount != 1 {
		t.Errorf("double texts = %d", a.DoubleTextCount)
	}
	if a.TurnsAwaitingUnreplied != 1 {
		t.Errorf("awaiting = %d", a.TurnsAwaitingUnreplied)
	}
	if a.FollowupAfterNoReply != 1 {
		t.Errorf("follow-ups = %d", a.FollowupAfterNoReply)
	}
	if out.Sessions != 2 || sink.sessions[1].Initiator != pA {
		t.Errorf("sessions %+v", sink.sessions)
	}
	// The reply in session 1 (A answering R) is ≤ session gap; the new
	// session's first message has no response time.
	if sink.features[3].ResponseS != nil {
		t.Error("session opener must not have a response time")
	}
}

func TestBuilderThreeParticipants(t *testing.T) {
	loc := time.UTC
	c := &chat{loc: loc}
	for i, p := range []uuid.UUID{pA, pR, pC, pA, pA, pC} {
		c.add(time.Date(2024, 5, 1, 10, i, 0, 0, loc), p, KindText, "x")
	}
	out, _ := c.build(t, BuildConfig{})
	turns := 0
	for _, r := range out.Rollups {
		turns += r.TurnCount
	}
	if turns != 5 { // A, R, C, A A, C
		t.Errorf("turns = %d, want 5", turns)
	}
	if rollupFor(t, out, pC, "2024-05-01").ResponseCount != 2 {
		t.Error("C should have two responses")
	}
}

func TestBuilderBatchesFeatures(t *testing.T) {
	c := &chat{loc: time.UTC}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		p := pA
		if i%2 == 1 {
			p = pR
		}
		c.add(start.Add(time.Duration(i)*time.Minute), p, KindText, "x")
	}
	_, sink := c.build(t, BuildConfig{})
	if sink.featureBatches != 3 || len(sink.features) != 5000 {
		t.Fatalf("batches=%d features=%d", sink.featureBatches, len(sink.features))
	}
	for i, f := range sink.features {
		if f.MessageID != int64(i+1) {
			t.Fatalf("feature %d has id %d", i, f.MessageID)
		}
	}
}

func TestBuilderRejectsOutOfOrder(t *testing.T) {
	b := NewBuilder(BuildConfig{Settings: DefaultSettings(), Text: fakeText{}}, &captureSink{})
	at := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	if err := b.Add(&Message{ID: 1, Ordinal: 5, ParticipantID: pA, Kind: KindText, SentAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(&Message{ID: 2, Ordinal: 4, ParticipantID: pR, Kind: KindText, SentAt: at}); err == nil {
		t.Fatal("ordinal going backwards at the same time must fail")
	}
	if _, err := b.Finish(); err == nil {
		t.Fatal("Finish must report the earlier error")
	}
	b = NewBuilder(BuildConfig{Settings: DefaultSettings()}, &captureSink{})
	_ = b.Add(&Message{ID: 1, Ordinal: 1, ParticipantID: pA, Kind: KindText, SentAt: at})
	if err := b.Add(&Message{ID: 2, Ordinal: 2, ParticipantID: pA, Kind: KindText, SentAt: at.Add(-time.Second)}); err == nil {
		t.Fatal("earlier timestamp must fail")
	}
}

func TestBuilderTopTermsTruncation(t *testing.T) {
	c := &chat{loc: time.UTC}
	day1 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	var words []string
	for i := 0; i < 150; i++ {
		words = append(words, "w"+string(rune('a'+i/26))+string(rune('a'+i%26)))
	}
	c.add(day1, pA, KindText, strings.Join(words, " "))
	c.add(day1.Add(time.Minute), pA, KindText, "wab wab") // wab ends up with count 3
	c.add(day1.Add(48*time.Hour), pA, KindText, "next day word")
	out, _ := c.build(t, BuildConfig{})
	r := rollupFor(t, out, pA, "2024-01-01")
	if len(r.TopTerms) != TopTermsPerDay || r.TermsOtherCount != 50 {
		t.Fatalf("top terms %d other %d", len(r.TopTerms), r.TermsOtherCount)
	}
	if r.TopTerms["wab"] != 3 {
		t.Errorf("wab = %d", r.TopTerms["wab"])
	}
	// Deterministic tie-break: alphabetically first terms survive.
	if _, ok := r.TopTerms["waa"]; !ok {
		t.Error("expected waa in top terms")
	}
	if next := rollupFor(t, out, pA, "2024-01-03"); next.TopTerms["next"] != 1 {
		t.Errorf("next day terms %v", next.TopTerms)
	}
}

// TestBuilderInvariants checks structural properties on random chats.
func TestBuilderInvariants(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	for seed := uint64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewPCG(seed, seed*7))
		c := &chat{loc: loc}
		at := time.Date(2024, 3, 1, 0, 0, 0, 0, loc)
		humans := 0
		for i := 0; i < 600; i++ {
			switch r.IntN(10) {
			case 0:
				at = at.Add(time.Duration(r.IntN(30)) * time.Hour)
			default:
				at = at.Add(time.Duration(r.IntN(3600)) * time.Second)
			}
			p := []uuid.UUID{pA, pR}[r.IntN(2)]
			switch r.IntN(20) {
			case 0:
				c.add(at, uuid.Nil, KindSystem, "sys")
			case 1:
				m := c.add(at, p, KindCall, "")
				m.Call = &CallInfo{DurationSec: 10}
			default:
				c.add(at, p, KindText, "hello?")
				humans++
			}
		}
		set := DefaultSettings()
		out, sink := c.build(t, BuildConfig{Settings: set})

		var msgs, respCount, histTotal, inits, ends, gaps int
		for _, ro := range out.Rollups {
			msgs += ro.MsgCount
			respCount += ro.ResponseCount
			inits += ro.SessionsInitiated
			ends += ro.SessionsEnded
			n := 0
			for _, h := range ro.ResponseHist {
				n += h
			}
			if n != ro.ResponseCount {
				t.Fatalf("seed %d: hist total %d != response count %d", seed, n, ro.ResponseCount)
			}
			histTotal += n
			for _, g := range ro.GapHist {
				gaps += g
			}
			hourly := 0
			for _, h := range ro.HourlyCounts {
				hourly += h
			}
			if hourly != ro.MsgCount {
				t.Fatalf("seed %d: hourly %d != msgs %d", seed, hourly, ro.MsgCount)
			}
		}
		if msgs != humans || inits != out.Sessions || ends != out.Sessions || len(sink.sessions) != out.Sessions {
			t.Fatalf("seed %d: msgs %d/%d inits %d ends %d sessions %d/%d", seed, msgs, humans, inits, ends, out.Sessions, len(sink.sessions))
		}
		if gaps != humans-1 {
			t.Fatalf("seed %d: gaps %d, want %d", seed, gaps, humans-1)
		}
		if len(sink.features) != len(c.msgs) {
			t.Fatalf("seed %d: features %d", seed, len(sink.features))
		}
		turnStarts := 0
		for _, f := range sink.features {
			if f.ResponseS != nil && time.Duration(*f.ResponseS)*time.Second > set.SessionGap.D() {
				t.Fatalf("seed %d: response %d s exceeds the session gap", seed, *f.ResponseS)
			}
			if f.ResponseS != nil {
				turnStarts++
			}
		}
		if turnStarts != respCount || histTotal != respCount {
			t.Fatalf("seed %d: responses %d features vs %d rollups", seed, turnStarts, respCount)
		}
	}
}

// trivialText keeps the benchmark focused on sessionisation and rollups.
type trivialText struct{}

func (trivialText) Analyze(string) TextFeatures { return TextFeatures{WordCount: 3, CharCount: 12} }

type discardSink struct{}

func (discardSink) MessageFeatures([]MessageFeatures) error { return nil }
func (discardSink) Sessions([]Session) error                { return nil }

func BenchmarkBuilder100k(b *testing.B) {
	loc := mustLoc(b, "Asia/Kolkata")
	r := rand.New(rand.NewPCG(1, 2))
	msgs := make([]Message, 100_000)
	at := time.Date(2023, 1, 1, 9, 0, 0, 0, loc)
	for i := range msgs {
		at = at.Add(time.Duration(5+r.IntN(1500)) * time.Second)
		p := pA
		if r.IntN(3) > 0 {
			p = pR
		}
		msgs[i] = Message{ID: int64(i + 1), Ordinal: int64(i + 1), ParticipantID: p, SentAt: at.UTC(), Kind: KindText, Body: "hello there friend"}
	}
	b.ResetTimer()
	for b.Loop() {
		bl := NewBuilder(BuildConfig{Settings: DefaultSettings(), Location: loc, Text: trivialText{}}, discardSink{})
		for i := range msgs {
			if err := bl.Add(&msgs[i]); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := bl.Finish(); err != nil {
			b.Fatal(err)
		}
	}
}
