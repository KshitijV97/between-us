package analytics

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// featureBatchSize and sessionBatchSize bound how much derived output the
// builder holds before handing it to the sink (FR-ING-013 bounded memory).
const (
	featureBatchSize = 2000
	sessionBatchSize = 500
)

// BuildConfig configures one full analysis run of a chat.
type BuildConfig struct {
	Settings Settings
	// Location is the chat's analysis timezone; nil means UTC.
	Location *time.Location
	// Text computes per-message text features; nil yields zero features.
	Text TextAnalyzer
}

// BuildSink receives the builder's streamed output. Implementations persist it
// (internal/analysis wires it to store.AnalysisWriter).
type BuildSink interface {
	MessageFeatures(batch []MessageFeatures) error
	Sessions(batch []Session) error
}

// Builder turns a chat's messages, delivered in (SentAt, Ordinal) order, into
// per-message features, sessions and daily rollups in one streaming pass
// (docs/04-metrics-catalog.md §4, §6, §11.2).
//
// Memory is bounded: the builder keeps the state of the current session and
// turn, one accumulator per (participant, local date) that has activity, and
// the full term counts of the current local day only. Everything else is
// flushed to the sink in batches.
type Builder struct {
	cfg  BuildConfig
	loc  *time.Location
	sink BuildSink

	sessionGap    time.Duration
	doubleTextGap time.Duration
	textForward   bool // R-17 override

	rollups map[rollupKey]*accumulator

	// Ordering check (R-18).
	seen        bool
	lastSentAt  time.Time
	lastOrdinal int64

	// Last human message, for gaps (§4.4) and sessionisation (§4.1).
	prevHuman     bool
	prevHumanAt   time.Time
	prevHumanDate Date

	cur          *sessionState // nil between sessions
	nextSession  int
	prevClosing  closingTurn // closing turn of the previous session (M-012 follow-ups)
	firstDate    Date
	lastDate     Date
	haveHumanDay bool

	// Term counts are kept in full for the current local date only and
	// truncated to the per-day top-K when the date moves on (§5.4).
	termDate Date
	termDay  []*accumulator

	features []MessageFeatures
	sessions []Session
	err      error
}

type rollupKey struct {
	pid  uuid.UUID
	date Date
}

// accumulator is a Rollup under construction plus the untruncated term maps of
// its day.
type accumulator struct {
	Rollup
	terms, bigrams map[string]int
	termsFinal     bool
}

// sessionState tracks the session in progress (§4.1, §4.2).
type sessionState struct {
	s Session

	// Current turn.
	turnSender      uuid.UUID
	turnFirstDate   Date
	turnLastAt      time.Time
	turnHasQuestion bool

	// Previous turn (always the other participant when set).
	havePrevTurn      bool
	prevTurnSender    uuid.UUID
	prevTurnFirstDate Date
}

// closingTurn remembers how the previous session ended.
type closingTurn struct {
	valid    bool
	sender   uuid.UUID
	awaiting bool
}

// NewBuilder returns a builder that streams output to sink.
func NewBuilder(cfg BuildConfig, sink BuildSink) *Builder {
	def := DefaultSettings()
	b := &Builder{
		cfg:           cfg,
		loc:           cfg.Location,
		sink:          sink,
		sessionGap:    cfg.Settings.SessionGap.D(),
		doubleTextGap: cfg.Settings.DoubleTextGap.D(),
		textForward:   cfg.Settings.IncludeForwardedInText,
		rollups:       map[rollupKey]*accumulator{},
	}
	if b.loc == nil {
		b.loc = time.UTC
	}
	// A zero-value Settings would make every gap start a new session; fall
	// back to the documented defaults instead.
	if b.sessionGap <= 0 {
		b.sessionGap = def.SessionGap.D()
	}
	if b.doubleTextGap <= 0 {
		b.doubleTextGap = def.DoubleTextGap.D()
	}
	return b
}

// isHuman implements R-10: a message by an analysed participant whose kind
// takes part in sessions and turns.
func isHuman(m *Message) bool {
	if m.ParticipantID == uuid.Nil {
		return false
	}
	switch m.Kind {
	case KindText, KindMedia, KindLocation, KindContact, KindPoll, KindDeleted:
		return true
	}
	return false
}

// Add consumes the next message. Messages must arrive in (SentAt, Ordinal)
// order (R-18); an earlier position than the previous message is an error.
func (b *Builder) Add(m *Message) error {
	if b.err != nil {
		return b.err
	}
	if b.seen && (m.SentAt.Before(b.lastSentAt) || (m.SentAt.Equal(b.lastSentAt) && m.Ordinal < b.lastOrdinal)) {
		b.err = fmt.Errorf("analytics: message %d is out of (sent_at, ordinal) order", m.ID)
		return b.err
	}
	b.seen, b.lastSentAt, b.lastOrdinal = true, m.SentAt, m.Ordinal

	mf := MessageFeatures{MessageID: m.ID, SessionIndex: -1}
	switch {
	case m.Kind == KindCall && m.ParticipantID != uuid.Nil:
		b.addCall(m) // R-12: calls feed M-021 only
	case isHuman(m):
		b.addHuman(m, &mf)
	default:
		// R-11 system rows and R-21 non-analysed senders: ignored.
	}
	b.features = append(b.features, mf)
	if len(b.features) >= featureBatchSize {
		return b.flushFeatures()
	}
	return b.err
}

// addCall attributes a call event to the caller's row on its day (R-07).
func (b *Builder) addCall(m *Message) {
	r := b.roll(m.ParticipantID, DateOf(m.SentAt, b.loc))
	c := m.Call
	if c == nil {
		c = &CallInfo{}
	}
	if c.Video {
		r.CallVideoCount++
	} else {
		r.CallVoiceCount++
	}
	if c.Missed {
		r.CallMissedCount++
	}
	r.CallDurationS += c.DurationSec
}

func (b *Builder) addHuman(m *Message, mf *MessageFeatures) {
	pid := m.ParticipantID
	date := DateOf(m.SentAt, b.loc)
	b.rollTerms(date)
	r := b.roll(pid, date)

	if !b.haveHumanDay {
		b.firstDate, b.haveHumanDay = date, true
	}
	b.lastDate = date

	// --- Volume and rhythm (M-001, M-013) ---
	r.MsgCount++
	r.HourlyCounts[m.SentAt.In(b.loc).Hour()]++
	if r.FirstMsgAt.IsZero() || m.SentAt.Before(r.FirstMsgAt) {
		r.FirstMsgAt = m.SentAt
	}
	if m.SentAt.After(r.LastMsgAt) {
		r.LastMsgAt = m.SentAt
	}
	b.countKind(r, m)

	// --- Text features (R-14, R-15, R-17) ---
	isQuestion, conflict := b.addText(r, m, mf)

	// --- Gaps (§4.4, R-05): attributed to the breaker on the breaker's day ---
	var gap time.Duration
	if b.prevHuman {
		gap = max(m.SentAt.Sub(b.prevHumanAt), 0) // R-18 clamp
		gs := int64(gap / time.Second)
		r.GapHist[GapBucket(gs)]++
		if b.prevHumanDate == date && gs > r.MaxIntradayGapS {
			r.MaxIntradayGapS = gs
		}
	}

	// --- Sessions (§4.1) ---
	if b.cur == nil || gap > b.sessionGap {
		b.closeSession()
		b.openSession(m, date)
	}
	st := b.cur
	st.s.MsgCount++
	st.s.EndedAt = m.SentAt
	st.s.Ender = pid
	st.s.LastMsgID = m.ID
	if conflict {
		st.s.ConflictMsg++
	}
	mf.SessionIndex = st.s.Index

	// --- Turns, replies and response times (§4.2) ---
	if st.s.TurnCount == 0 || st.turnSender != pid {
		if st.s.TurnCount > 0 {
			// This turn answers the previous one (always the other sender).
			rt := int64(max(m.SentAt.Sub(st.turnLastAt), 0) / time.Second) // R-18
			mf.ResponseS = &rt
			r.ResponseCount++ // R-03: on the reply's day
			r.ResponseSumS += rt
			r.ResponseHist[ResponseBucket(rt)]++
			// R-04: the replied turn is credited on its own first day.
			b.roll(st.turnSender, st.turnFirstDate).TurnsReplied++
			st.havePrevTurn = true
			st.prevTurnSender, st.prevTurnFirstDate = st.turnSender, st.turnFirstDate
		}
		st.s.TurnCount++
		st.turnSender, st.turnFirstDate, st.turnHasQuestion = pid, date, false
		r.TurnCount++
		mf.IsTurnStart = true
	} else if m.SentAt.Sub(st.turnLastAt) >= b.doubleTextGap {
		r.DoubleTextCount++ // R-06: intra-turn follow-up after a long pause
	}
	st.turnLastAt = m.SentAt
	st.turnHasQuestion = st.turnHasQuestion || isQuestion

	b.prevHuman, b.prevHumanAt, b.prevHumanDate = true, m.SentAt, date
}

// countKind updates the media, deleted, edited and forwarded counters
// (M-019, M-020).
func (b *Builder) countKind(r *accumulator, m *Message) {
	switch m.Kind {
	case KindMedia:
		switch m.MediaType {
		case "image":
			r.MediaImage++
		case "video", "video_note":
			r.MediaVideo++
		case "audio":
			r.MediaAudio++
		case "voice_note":
			r.MediaVoice++
		case "sticker":
			r.MediaSticker++
		case "gif":
			r.MediaGIF++
		case "document":
			r.MediaDocument++
		default: // contact-as-media, unknown, empty
			r.MediaOther++
		}
	case KindLocation:
		r.LocationCount++
	case KindContact:
		r.ContactCount++
	case KindPoll:
		r.PollCount++
	}
	if m.IsDeleted || m.Kind == KindDeleted {
		r.DeletedCount++
	}
	if m.IsEdited {
		r.EditedCount++
	}
	if m.IsForwarded {
		r.ForwardedCount++
	}
}

// addText analyses the body of text messages and media captions. Deleted
// placeholders, media without a caption, locations, contacts and polls are
// not text (R-14, R-15). Forwarded messages count for words, emoji, links
// and questions but not for sentiment, affection, conflict or top terms
// unless IncludeForwardedInText is set (R-17). It reports whether the
// message is a question and whether it is conflict-flagged.
func (b *Builder) addText(r *accumulator, m *Message, mf *MessageFeatures) (question, conflict bool) {
	isText := m.Kind == KindText || (m.Kind == KindMedia && m.Body != "")
	if !isText || m.IsDeleted {
		return false, false
	}
	var f TextFeatures
	if b.cfg.Text != nil {
		f = b.cfg.Text.Analyze(m.Body)
	}
	r.TextMsgCount++
	r.WordCount += f.WordCount
	r.CharCount += f.CharCount
	r.EmojiCount += len(f.Emoji)
	if len(f.Emoji) > 0 {
		r.EmojiMsgCount++
		if r.EmojiCounts == nil {
			r.EmojiCounts = map[string]int{}
		}
		for _, e := range f.Emoji {
			r.EmojiCounts[e]++
		}
	}
	r.LinkCount += len(f.LinkDomains)
	if len(f.LinkDomains) > 0 {
		if r.LinkDomains == nil {
			r.LinkDomains = map[string]int{}
		}
		for _, d := range f.LinkDomains {
			r.LinkDomains[d]++
		}
	}
	if f.IsQuestion {
		r.QuestionCount++
	}
	mf.WordCount, mf.CharCount = f.WordCount, f.CharCount
	mf.EmojiCount, mf.LinkCount, mf.IsQuestion = len(f.Emoji), len(f.LinkDomains), f.IsQuestion

	if m.IsForwarded && !b.textForward {
		return f.IsQuestion, false
	}
	// Sentiment (M-022): unscored messages are neutral but excluded from the
	// scored count, so "ok"-heavy chats do not dilute the mean.
	if f.SentimentScored {
		s := f.Sentiment
		r.SentimentScoredCount++
		r.SentimentSum += s
		r.SentimentHist[SentimentBucket(s)]++
		switch {
		case s >= 0.05:
			r.SentimentPos++
		case s <= -0.05:
			r.SentimentNeg++
		default:
			r.SentimentNeu++
		}
		mf.Sentiment = &s
	} else {
		r.SentimentNeu++
	}
	if f.AffectionHits > 0 { // M-023
		r.AffectionMsgCount++
		r.AffectionHits += f.AffectionHits
		mf.AffectionHits = f.AffectionHits
	}
	if f.Conflict { // M-024
		r.ConflictMsgCount++
		mf.Conflict = true
	}
	// Top terms (M-016): full counts for the current day only.
	if len(f.Terms) > 0 || len(f.Bigrams) > 0 {
		if r.terms == nil {
			r.terms, r.bigrams = map[string]int{}, map[string]int{}
			b.termDay = append(b.termDay, r)
		}
		for _, t := range f.Terms {
			r.terms[t]++
		}
		for _, bg := range f.Bigrams {
			r.bigrams[bg]++
		}
	}
	return f.IsQuestion, f.Conflict
}

// openSession starts a new session with m as its first message (§4.1).
func (b *Builder) openSession(m *Message, date Date) {
	b.cur = &sessionState{s: Session{
		Index:      b.nextSession,
		StartedAt:  m.SentAt,
		EndedAt:    m.SentAt,
		Initiator:  m.ParticipantID,
		Ender:      m.ParticipantID,
		FirstMsgID: m.ID,
		LastMsgID:  m.ID,
		StartDate:  date,
	}}
	b.nextSession++
	// M-012: a session started by the same person whose question closed the
	// previous session unanswered.
	if b.prevClosing.valid && b.prevClosing.awaiting && b.prevClosing.sender == m.ParticipantID {
		b.roll(m.ParticipantID, date).FollowupAfterNoReply++
	}
}

// closeSession finalises the current session: closing-turn status (M-011),
// session attribution to its start day (R-02) and emission to the sink.
func (b *Builder) closeSession() {
	st := b.cur
	if st == nil {
		return
	}
	b.cur = nil
	s := st.s
	// The closing turn counts as "awaiting a reply" only if it asked
	// something; otherwise a natural "good night" would look ignored.
	if st.turnHasQuestion {
		b.roll(st.turnSender, st.turnFirstDate).TurnsAwaitingUnreplied++
	}
	b.prevClosing = closingTurn{valid: true, sender: st.turnSender, awaiting: st.turnHasQuestion}

	ini := b.roll(s.Initiator, s.StartDate)
	ini.SessionsInitiated++
	ini.SessionMsgSum += s.MsgCount
	ini.SessionDurationSumS += int64(s.EndedAt.Sub(s.StartedAt) / time.Second)
	ini.SessionTurnsSum += s.TurnCount
	b.roll(s.Ender, s.StartDate).SessionsEnded++ // R-02: ender credited on the start day

	b.sessions = append(b.sessions, s)
	if len(b.sessions) >= sessionBatchSize {
		b.flushSessions()
	}
}

// roll returns the accumulator for (pid, date), creating it on first use.
func (b *Builder) roll(pid uuid.UUID, date Date) *accumulator {
	k := rollupKey{pid, date}
	r, ok := b.rollups[k]
	if !ok {
		r = &accumulator{Rollup: Rollup{ParticipantID: pid, Date: date}}
		b.rollups[k] = r
	}
	return r
}

// rollTerms truncates the previous day's term maps when the local date moves
// forward. Dates are non-decreasing because messages arrive in time order.
func (b *Builder) rollTerms(date Date) {
	if date == b.termDate {
		return
	}
	for _, r := range b.termDay {
		r.finalizeTerms()
	}
	b.termDay = b.termDay[:0]
	b.termDate = date
}

// finalizeTerms keeps the day's top 100 terms and top 50 bigrams (§5.4); the
// counts of the remaining terms go to TermsOtherCount. Ties break by term so
// output is deterministic.
func (r *accumulator) finalizeTerms() {
	if r.termsFinal || r.terms == nil {
		return
	}
	var other int
	r.TopTerms, other = topK(r.terms, TopTermsPerDay)
	r.TermsOtherCount += other
	r.TopBigrams, _ = topK(r.bigrams, TopBigramsPerDay)
	r.terms, r.bigrams, r.termsFinal = nil, nil, true
}

func topK(counts map[string]int, k int) (map[string]int, int) {
	if len(counts) == 0 {
		return nil, 0
	}
	keys := make([]string, 0, len(counts))
	for t := range counts {
		keys = append(keys, t)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]int, min(k, len(keys)))
	other := 0
	for i, t := range keys {
		if i < k {
			out[t] = counts[t]
		} else {
			other += counts[t]
		}
	}
	return out, other
}

func (b *Builder) flushFeatures() error {
	if len(b.features) == 0 || b.err != nil {
		return b.err
	}
	if err := b.sink.MessageFeatures(b.features); err != nil {
		b.err = err
		return err
	}
	b.features = make([]MessageFeatures, 0, featureBatchSize)
	return nil
}

func (b *Builder) flushSessions() {
	if len(b.sessions) == 0 || b.err != nil {
		return
	}
	if err := b.sink.Sessions(b.sessions); err != nil {
		b.err = err
		return
	}
	b.sessions = make([]Session, 0, sessionBatchSize)
}

// Finish closes the last session, flushes all pending output and returns the
// rollups sorted by (Date, ParticipantID). Rows with MsgCount 0 can exist
// for days that only had calls; their FirstMsgAt/LastMsgAt are zero.
func (b *Builder) Finish() (BuildOutput, error) {
	if b.err != nil {
		return BuildOutput{}, b.err
	}
	b.closeSession()
	for _, r := range b.termDay {
		r.finalizeTerms()
	}
	b.termDay = nil
	b.flushSessions()
	if err := b.flushFeatures(); err != nil {
		return BuildOutput{}, err
	}
	if b.err != nil {
		return BuildOutput{}, b.err
	}
	out := BuildOutput{Rollups: make([]Rollup, 0, len(b.rollups)), Sessions: b.nextSession}
	for _, r := range b.rollups {
		r.finalizeTerms() // defensive: every term map is final by now
		out.Rollups = append(out.Rollups, r.Rollup)
	}
	sort.Slice(out.Rollups, func(i, j int) bool {
		a, c := out.Rollups[i], out.Rollups[j]
		if a.Date != c.Date {
			return a.Date < c.Date
		}
		return a.ParticipantID.String() < c.ParticipantID.String()
	})
	if b.haveHumanDay {
		out.FirstDate, out.LastDate = b.firstDate, b.lastDate
	}
	return out, nil
}
