package analytics

import "github.com/google/uuid"

// Participant identifies a sender in results; Label is the display name.
type Participant struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
}

// Result is the response of GET /api/v1/chats/{id}/metrics
// (docs/04-metrics-catalog.md §12). Metrics maps metric IDs ("M-001"…) to the
// typed values below; only requested metrics are present.
type Result struct {
	Window        Window         `json:"window"`
	Compare       *Window        `json:"compare,omitempty"`
	RollupVersion int            `json:"rollup_version"`
	Stale         bool           `json:"stale"`
	Participants  []Participant  `json:"participants"`
	Metrics       map[string]any `json:"metrics"`
}

// Reason codes for null metric values.
const ReasonInsufficientData = "insufficient_data"

// Change compares a value with the previous window (§2.2). All fields are nil
// when the previous window has zero coverage; DeltaPct is nil when Prev is 0.
type Change struct {
	Prev        *float64 `json:"prev"`
	Delta       *float64 `json:"delta"`
	DeltaPct    *float64 `json:"delta_pct"`
	PerDayDelta *float64 `json:"per_day_delta,omitempty"`
}

// Count is an additive per-participant count (M-001, M-009, M-017, M-018…).
type Count struct {
	ByParticipant map[string]int `json:"by_participant"`
	Combined      int            `json:"combined"`
	// Change compares the combined value with the previous window.
	Change *Change `json:"change,omitempty"`
	// Baseline is the all-time per-day mean scaled to this window's length
	// (M-001 only).
	Baseline *float64 `json:"baseline,omitempty"`
}

// ShareOfVoice is M-002: each participant's share of messages, words and
// characters (nil when the denominator is 0).
type ShareOfVoice struct {
	Messages map[string]*float64 `json:"messages"`
	Words    map[string]*float64 `json:"words"`
	Chars    map[string]*float64 `json:"chars"`
}

// TextStats is M-003 for one participant or combined.
type TextStats struct {
	Words        int      `json:"words"`
	Chars        int      `json:"chars"`
	TextMessages int      `json:"text_messages"`
	AvgWords     *float64 `json:"avg_words"`
	AvgChars     *float64 `json:"avg_chars"`
}

// TextLength is M-003.
type TextLength struct {
	ByParticipant map[string]TextStats `json:"by_participant"`
	Combined      TextStats            `json:"combined"`
}

// Activity is M-004.
type Activity struct {
	ActiveDays    int            `json:"active_days"`
	Rate          float64        `json:"rate"`
	CoveredDays   int            `json:"covered_days"`
	RateCovered   *float64       `json:"rate_covered"`
	ByParticipant map[string]int `json:"by_participant"`
}

// Streaks is M-005 (clipped to the window).
type Streaks struct {
	Longest       int `json:"longest"`
	Current       int `json:"current"`
	MutualLongest int `json:"mutual_longest"`
	MutualCurrent int `json:"mutual_current"`
}

// Silence is M-006.
type Silence struct {
	LongestS int64 `json:"longest_s"`
	// Ongoing is true when the longest silence is the open one at the end of
	// the window ("ongoing silence at end of period").
	Ongoing bool `json:"ongoing"`
	// BrokenBy is the participant whose message ended the longest silence
	// (nil when ongoing or when the window has no messages).
	BrokenBy *string `json:"broken_by"`
	// NoMessages is true when the window contains no messages at all.
	NoMessages bool `json:"no_messages"`
	// MaxIntradayS is the longest gap between two messages on the same day.
	MaxIntradayS int64                          `json:"max_intraday_s"`
	GapHist      [GapHistBuckets]int            `json:"gap_hist"`
	GapsBrokenBy map[string][GapHistBuckets]int `json:"gaps_broken_by"`
}

// Sessions is M-007.
type Sessions struct {
	Count        int      `json:"count"`
	AvgMessages  *float64 `json:"avg_messages"`
	AvgDurationS *float64 `json:"avg_duration_s"`
	AvgTurns     *float64 `json:"avg_turns"`
	Change       *Change  `json:"change,omitempty"`
}

// Initiations is M-008.
type Initiations struct {
	ByParticipant map[string]int      `json:"by_participant"`
	Ratio         map[string]*float64 `json:"ratio"`
	Combined      int                 `json:"combined"`
}

// ResponseStats summarises response times in seconds (M-010).
type ResponseStats struct {
	MedianS *float64 `json:"median_s"`
	P90S    *float64 `json:"p90_s"`
	MeanS   *float64 `json:"mean_s"`
	N       int      `json:"n"`
	// Approx is true for percentiles estimated from the log histogram (≤9%).
	Approx bool `json:"approx"`
	// FewData flags n < 5.
	FewData bool `json:"few_data"`
}

// ResponseTime is M-010.
type ResponseTime struct {
	ByParticipant map[string]ResponseStats `json:"by_participant"`
	Combined      ResponseStats            `json:"combined"`
	Change        *Change                  `json:"change,omitempty"` // combined median
}

// ReplyRate is M-011 (nil when the other side had no eligible turns).
type ReplyRate struct {
	ByParticipant map[string]*float64 `json:"by_participant"`
}

// Turns is M-012.
type Turns struct {
	Turns                 map[string]int      `json:"turns"`
	MessagesPerTurn       map[string]*float64 `json:"messages_per_turn"`
	DoubleTexts           map[string]int      `json:"double_texts"`
	FollowupsAfterNoReply map[string]int      `json:"followups_after_no_reply"`
}

// Heatmap is M-013: counts by weekday (0 = Monday … 6 = Sunday) × local hour.
type Heatmap struct {
	ByParticipant map[string][7][24]int `json:"by_participant"`
	Combined      [7][24]int            `json:"combined"`
}

// LateNight is M-014.
type LateNight struct {
	Hours    [2]int              `json:"hours"` // [from, to)
	Messages map[string]int      `json:"messages"`
	Share    map[string]*float64 `json:"share"`
	Combined *float64            `json:"combined"`
}

// Ranked is a top-N entry.
type Ranked struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// EmojiStats is M-015 for one participant or combined.
type EmojiStats struct {
	Total      int      `json:"total"`
	PerTextMsg *float64 `json:"per_text_msg"`
	MsgShare   *float64 `json:"msg_share"`
	Affection  int      `json:"affection"`
	Top        []Ranked `json:"top"`
}

// Emoji is M-015.
type Emoji struct {
	ByParticipant map[string]EmojiStats `json:"by_participant"`
	Combined      EmojiStats            `json:"combined"`
}

// TopTerms is M-016.
type TopTerms struct {
	Terms           map[string][]Ranked `json:"terms"`
	Bigrams         map[string][]Ranked `json:"bigrams"`
	Combined        []Ranked            `json:"combined"`
	CombinedBigrams []Ranked            `json:"combined_bigrams"`
	// Approximate is true when merged daily top-K maps were used (§5.4).
	Approximate bool `json:"approximate"`
}

// Questions is M-017.
type Questions struct {
	Count
	Rate map[string]*float64 `json:"rate"`
}

// Links is M-018.
type Links struct {
	Count
	TopDomains []Ranked `json:"top_domains"`
}

// MediaBreakdown is M-019 for one participant or combined.
type MediaBreakdown struct {
	Image    int `json:"image"`
	Video    int `json:"video"`
	Audio    int `json:"audio"`
	Voice    int `json:"voice"`
	Sticker  int `json:"sticker"`
	GIF      int `json:"gif"`
	Document int `json:"document"`
	Other    int `json:"other"`
	Location int `json:"location"`
	Contact  int `json:"contact"`
	Poll     int `json:"poll"`
	Total    int `json:"total"`
}

// Media is M-019.
type Media struct {
	ByParticipant map[string]MediaBreakdown `json:"by_participant"`
	Combined      MediaBreakdown            `json:"combined"`
}

// MessageFlags is M-020 for one participant or combined.
type MessageFlags struct {
	Deleted   int `json:"deleted"`
	Edited    int `json:"edited"`
	Forwarded int `json:"forwarded"`
}

// Flags is M-020.
type Flags struct {
	ByParticipant map[string]MessageFlags `json:"by_participant"`
	Combined      MessageFlags            `json:"combined"`
}

// CallStats is M-021 for one participant (as caller) or combined.
type CallStats struct {
	Voice     int `json:"voice"`
	Video     int `json:"video"`
	Missed    int `json:"missed"`
	DurationS int `json:"duration_s"`
}

// Calls is M-021.
type Calls struct {
	ByParticipant map[string]CallStats `json:"by_participant"`
	Combined      CallStats            `json:"combined"`
}

// SentimentStats is M-022 for one participant or combined.
type SentimentStats struct {
	Mean     *float64                  `json:"mean"`
	Median   *float64                  `json:"median"`
	Scored   int                       `json:"scored"`
	PosShare *float64                  `json:"pos_share"`
	NeuShare *float64                  `json:"neu_share"`
	NegShare *float64                  `json:"neg_share"`
	Hist     [SentimentHistBuckets]int `json:"hist"`
}

// Sentiment is M-022.
type Sentiment struct {
	ByParticipant map[string]SentimentStats `json:"by_participant"`
	Combined      SentimentStats            `json:"combined"`
	Change        *Change                   `json:"change,omitempty"` // combined mean
}

// Affection is M-023.
type Affection struct {
	Messages   map[string]int      `json:"messages"`
	Hits       map[string]int      `json:"hits"`
	RatePer100 map[string]*float64 `json:"rate_per_100"`
	Combined   int                 `json:"combined"`
}

// Conflict is M-024 (wording in the UI: "tense moments").
type Conflict struct {
	Messages map[string]int `json:"messages"`
	Combined int            `json:"combined"`
}

// Dimension is one input of M-025.
type Dimension struct {
	A         float64  `json:"a"`
	B         float64  `json:"b"`
	Imbalance *float64 `json:"imbalance"` // nil when undefined
	Weight    float64  `json:"weight"`    // effective weight after redistribution
}

// Score is M-025 or M-026: a 0–100 value or nil with a reason.
type Score struct {
	Value   *float64 `json:"value"`
	Reason  string   `json:"reason,omitempty"`
	MinMsgs int      `json:"min_msgs"`
	// Pair names the two participants the score compares.
	Pair []string `json:"pair,omitempty"`
	// Dimensions (M-025) or Components (M-026) explain the score.
	Dimensions map[string]Dimension `json:"dimensions,omitempty"`
	Components map[string]float64   `json:"components,omitempty"`
	Weights    map[string]float64   `json:"weights,omitempty"`
	Version    string               `json:"version,omitempty"`
}

// TrendSeries is M-027 for one series.
type TrendSeries struct {
	Current  *float64 `json:"current"`
	Prev     *float64 `json:"prev"`
	Delta    *float64 `json:"delta"`
	DeltaPct *float64 `json:"delta_pct"`
	Slope    *float64 `json:"slope"`
	Rel      *float64 `json:"rel"`
	R2       *float64 `json:"r2"`
	Label    *string  `json:"label"` // rising | falling | steady; nil with < 7 points
	Points   int      `json:"points"`
}

// TrendGroup holds one series per participant plus combined.
type TrendGroup struct {
	ByParticipant map[string]TrendSeries `json:"by_participant"`
	Combined      TrendSeries            `json:"combined"`
}

// Trend is M-027.
type Trend struct {
	MessagesPerDay TrendGroup `json:"messages_per_day"`
	ResponseMedian TrendGroup `json:"response_median_s"`
	Sentiment      TrendGroup `json:"sentiment"`
}
