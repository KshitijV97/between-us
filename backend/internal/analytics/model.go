package analytics

import (
	"time"

	"github.com/google/uuid"
)

// RollupVersion identifies the formula and lexicon generation of derived data
// (docs/04-metrics-catalog.md §11.1). Bump it whenever a change alters stored
// features, sessions or rollups; chats analysed with an older version are
// served with stale=true until re-analysed.
const RollupVersion = 1

// Message kinds as stored (mirrors ingest.Kind; repeated here so analytics
// does not depend on the parsers).
const (
	KindText     = "text"
	KindMedia    = "media"
	KindCall     = "call"
	KindLocation = "location"
	KindContact  = "contact"
	KindPoll     = "poll"
	KindDeleted  = "deleted"
	KindSystem   = "system"
)

// Message is the analysis view of one stored message, delivered in
// (SentAt, Ordinal) order. System rows are included and must be ignored by
// every metric (R-11); call rows feed only M-021 (R-12).
type Message struct {
	ID int64
	// ParticipantID is uuid.Nil for system rows and for senders outside the
	// two analysed participants (R-21).
	ParticipantID uuid.UUID
	SentAt        time.Time // UTC
	Ordinal       int64
	Kind          string
	MediaType     string // ingest media types: image, video, video_note, voice_note, audio, sticker, gif, document, contact, unknown
	Body          string
	IsEdited      bool
	IsForwarded   bool
	IsDeleted     bool
	// MinutePrecision is true when the source export had no seconds (R-19).
	MinutePrecision bool
	Call            *CallInfo // set for KindCall

	// Derived holds values written by a previous analysis run; zero before the
	// first run. Read by the exact path (§5.5) and drill-downs.
	Derived MessageFeatures
}

// CallInfo describes a call event (M-021).
type CallInfo struct {
	Video       bool
	Missed      bool
	DurationSec int
}

// TextFeatures are the per-message text signals produced by a TextAnalyzer
// (docs/04-metrics-catalog.md M-003, M-015…M-018, M-022…M-024, §9).
type TextFeatures struct {
	WordCount int // UAX #29 word segments containing a letter or digit
	CharCount int // grapheme clusters excluding whitespace
	// Emoji lists every emoji grapheme in order (duplicates kept); identity is
	// normalised (U+FE0F dropped; skin tones kept so the query layer can fold).
	Emoji       []string
	LinkDomains []string // one entry per URL, eTLD+1 lower-cased
	IsQuestion  bool
	// Sentiment is the compound score in [-1, 1]; SentimentScored is false
	// when no lexicon entry matched (the message is neutral and excluded
	// from the mean, M-022).
	Sentiment       float64
	SentimentScored bool
	AffectionHits   int  // M-023 marker hits (phrases + affection emoji), negations removed
	Conflict        bool // M-024 rule (two signals, or one strong phrase)
	// Terms and Bigrams are normalised, stop-word-filtered tokens for M-016.
	Terms   []string
	Bigrams []string
}

// TextAnalyzer computes TextFeatures for one message body. Implementations
// must be safe for concurrent use and deterministic.
type TextAnalyzer interface {
	Analyze(body string) TextFeatures
}

// MessageFeatures are the derived per-message values persisted on the
// messages table (feature columns) for the exact path and drill-downs.
type MessageFeatures struct {
	MessageID int64
	// SessionIndex is the 0-based session number within the chat, or -1 for
	// rows that take no part in sessions (system, call, non-analysed sender).
	SessionIndex int
	IsTurnStart  bool
	// ResponseS is the response time in seconds when this message starts a
	// turn that answers the other participant (M-010); nil otherwise.
	ResponseS     *int64
	WordCount     int
	CharCount     int
	EmojiCount    int
	LinkCount     int
	IsQuestion    bool
	Sentiment     *float64 // nil when not scored
	AffectionHits int
	Conflict      bool
}

// Session is one conversation (§4.1), attributed to its first message's day.
type Session struct {
	Index       int
	StartedAt   time.Time
	EndedAt     time.Time
	Initiator   uuid.UUID
	Ender       uuid.UUID
	MsgCount    int
	TurnCount   int
	FirstMsgID  int64
	LastMsgID   int64
	StartDate   Date // local date in the analysis timezone
	ConflictMsg int  // conflict-flagged messages in the session (M-024 conflict sessions)
}

// Histogram sizes (docs/04-metrics-catalog.md §5).
const (
	ResponseHistBuckets  = 64 // log-scale, bucket i covers [2^(i/4), 2^((i+1)/4)) seconds; bucket 0 also holds 0
	GapHistBuckets       = 16 // 0–1m, 1–5m, 5–15m, 15–60m, 1–3h, 3–6h, 6–12h, 12–24h, 1–2d, 2–4d, 4–7d, 1–2w, 2–4w, 1–3mo, 3–6mo, ≥6mo
	SentimentHistBuckets = 20 // width 0.1 over [-1, 1], last bucket closed
	TopTermsPerDay       = 100
	TopBigramsPerDay     = 50
)

// Rollup is one row of daily_rollups: one participant on one local date
// (docs/04-metrics-catalog.md §6). Chat-level values are stored on the row of
// the participant they are attributed to (initiator, gap breaker…), so a
// combined value is always the sum, min or max of the participant rows.
// JSON tags are the persisted form.
type Rollup struct {
	ParticipantID uuid.UUID `json:"participant_id"`
	Date          Date      `json:"date"`

	MsgCount      int `json:"msg_count"`       // M-001
	TextMsgCount  int `json:"text_msg_count"`  // M-003 denominator
	WordCount     int `json:"word_count"`      // M-002, M-003
	CharCount     int `json:"char_count"`      // M-002, M-003
	EmojiCount    int `json:"emoji_count"`     // M-015
	EmojiMsgCount int `json:"emoji_msg_count"` // M-015

	FirstMsgAt   time.Time `json:"first_msg_at"` // M-005, M-006
	LastMsgAt    time.Time `json:"last_msg_at"`
	HourlyCounts [24]int   `json:"hourly_counts"` // M-013, M-014 (local hour)

	SessionsInitiated   int   `json:"sessions_initiated"`     // M-007, M-008 (R-02)
	SessionsEnded       int   `json:"sessions_ended"`         // M-009 (attributed to session start day)
	SessionMsgSum       int   `json:"session_msg_sum"`        // M-007
	SessionDurationSumS int64 `json:"session_duration_sum_s"` // M-007
	SessionTurnsSum     int   `json:"session_turns_sum"`      // M-007

	TurnCount              int `json:"turn_count"`               // M-012
	TurnsReplied           int `json:"turns_replied"`            // M-011 (R-04)
	TurnsAwaitingUnreplied int `json:"turns_awaiting_unreplied"` // M-011
	DoubleTextCount        int `json:"double_text_count"`        // M-012 (R-06)
	FollowupAfterNoReply   int `json:"followup_after_no_reply"`  // M-012

	ResponseCount   int                      `json:"response_count"`     // M-010 (R-03)
	ResponseSumS    int64                    `json:"response_sum_s"`     // M-010 mean
	ResponseHist    [ResponseHistBuckets]int `json:"response_hist"`      // M-010 percentiles
	MaxIntradayGapS int64                    `json:"max_intraday_gap_s"` // M-006 (gaps broken by this participant, both ends on this day)
	GapHist         [GapHistBuckets]int      `json:"gap_hist"`           // M-006 (gaps broken by this participant)

	QuestionCount int            `json:"question_count"` // M-017
	LinkCount     int            `json:"link_count"`     // M-018
	LinkDomains   map[string]int `json:"link_domains,omitempty"`

	MediaImage    int `json:"media_image"` // M-019
	MediaVideo    int `json:"media_video"` // video and video_note
	MediaAudio    int `json:"media_audio"`
	MediaVoice    int `json:"media_voice"`
	MediaSticker  int `json:"media_sticker"`
	MediaGIF      int `json:"media_gif"`
	MediaDocument int `json:"media_document"`
	MediaOther    int `json:"media_other"` // unknown, contact-as-media
	LocationCount int `json:"location_count"`
	ContactCount  int `json:"contact_count"`
	PollCount     int `json:"poll_count"`

	DeletedCount   int `json:"deleted_count"` // M-020
	EditedCount    int `json:"edited_count"`
	ForwardedCount int `json:"forwarded_count"`

	CallVoiceCount  int `json:"call_voice_count"` // M-021 (attributed to the caller)
	CallVideoCount  int `json:"call_video_count"`
	CallMissedCount int `json:"call_missed_count"`
	CallDurationS   int `json:"call_duration_s"`

	SentimentScoredCount int                       `json:"sentiment_scored_count"` // M-022
	SentimentSum         float64                   `json:"sentiment_sum"`
	SentimentPos         int                       `json:"sentiment_pos"`
	SentimentNeu         int                       `json:"sentiment_neu"`
	SentimentNeg         int                       `json:"sentiment_neg"`
	SentimentHist        [SentimentHistBuckets]int `json:"sentiment_hist"`

	AffectionMsgCount int `json:"affection_msg_count"` // M-023
	AffectionHits     int `json:"affection_hits"`
	ConflictMsgCount  int `json:"conflict_msg_count"` // M-024

	EmojiCounts     map[string]int `json:"emoji_counts,omitempty"` // M-015 (exact)
	TopTerms        map[string]int `json:"top_terms,omitempty"`    // M-016 (top 100 per day)
	TopBigrams      map[string]int `json:"top_bigrams,omitempty"`  // M-016 (top 50 per day)
	TermsOtherCount int            `json:"terms_other_count"`
}

// BuildOutput is what a full analysis run produces besides streamed message
// features and sessions.
type BuildOutput struct {
	Rollups []Rollup
	// FirstDate/LastDate span the chat's human messages (zero when none).
	FirstDate, LastDate Date
	Sessions            int
}
