package analytics

import (
	"encoding/json"
	"fmt"
	"time"
)

// Settings are the per-chat analysis parameters (docs/04-metrics-catalog.md
// §10), stored as JSON in chats.settings. Fields marked "recompute" change
// derived rows and require re-running the builder; the rest apply at query time.
type Settings struct {
	SessionGap    Duration `json:"session_gap"`     // recompute; default 6h, range 30m–24h
	DoubleTextGap Duration `json:"double_text_gap"` // recompute; default 30m, range 5m–6h
	// WeekStart is the first day of a "week" window (query time).
	WeekStart time.Weekday `json:"week_start"`
	// LateNightFrom/To are the half-open hour range [from, to) for M-014
	// (query time); default [0, 5).
	LateNightFrom int `json:"late_night_from"`
	LateNightTo   int `json:"late_night_to"`
	// TextLanguages select the lexicons (recompute); default ["en","hi-Latn"].
	TextLanguages []string `json:"text_languages"`
	// IncludeForwardedInText counts forwarded messages in text-feeling
	// metrics M-022…M-024 and top terms (recompute; default false, R-17).
	IncludeForwardedInText  bool `json:"include_forwarded_in_text_metrics"`
	EmojiNormaliseModifiers bool `json:"emoji_normalise_modifiers"`  // query time; default true
	CountTextEmoticons      bool `json:"count_text_emoticons"`       // recompute; default false
	ExactTermsMaxDays       int  `json:"exact_terms_max_days"`       // query time; default 90
	ExactPercentilesMaxDays int  `json:"exact_percentiles_max_days"` // query time; default 31
	MinMsgsForScores        int  `json:"min_msgs_for_scores"`        // query time; default 20
	// EngagementWeights for M-026 in order: frequency, volume,
	// responsiveness, reciprocity, depth (query time; must sum to 1).
	EngagementWeights [5]float64 `json:"engagement_weights"`
}

// DefaultSettings returns the documented defaults.
func DefaultSettings() Settings {
	return Settings{
		SessionGap:              Duration(6 * time.Hour),
		DoubleTextGap:           Duration(30 * time.Minute),
		WeekStart:               time.Monday,
		LateNightFrom:           0,
		LateNightTo:             5,
		TextLanguages:           []string{"en", "hi-Latn"},
		EmojiNormaliseModifiers: true,
		ExactTermsMaxDays:       90,
		ExactPercentilesMaxDays: 31,
		MinMsgsForScores:        20,
		EngagementWeights:       [5]float64{0.25, 0.15, 0.20, 0.20, 0.20},
	}
}

// ParseSettings overlays stored JSON on the defaults and validates the result.
// Unknown keys are ignored so older binaries can read newer settings.
func ParseSettings(raw []byte) (Settings, error) {
	s := DefaultSettings()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return Settings{}, fmt.Errorf("chat settings: %w", err)
		}
	}
	return s, s.Validate()
}

// Validate enforces the documented ranges.
func (s Settings) Validate() error {
	switch {
	case s.SessionGap.D() < 30*time.Minute || s.SessionGap.D() > 24*time.Hour:
		return fmt.Errorf("session_gap must be between 30m and 24h")
	case s.DoubleTextGap.D() < 5*time.Minute || s.DoubleTextGap.D() > 6*time.Hour:
		return fmt.Errorf("double_text_gap must be between 5m and 6h")
	case s.LateNightFrom < 0 || s.LateNightFrom > 23 || s.LateNightTo < 1 || s.LateNightTo > 24:
		return fmt.Errorf("late-night hours must be within 0–24")
	case len(s.TextLanguages) == 0:
		return fmt.Errorf("text_languages must not be empty")
	case s.MinMsgsForScores < 1:
		return fmt.Errorf("min_msgs_for_scores must be at least 1")
	}
	sum := 0.0
	for _, w := range s.EngagementWeights {
		if w < 0 {
			return fmt.Errorf("engagement weights must not be negative")
		}
		sum += w
	}
	if sum < 0.999 || sum > 1.001 {
		return fmt.Errorf("engagement weights must sum to 1")
	}
	return nil
}

// Duration is a time.Duration that serialises as "6h0m0s"-style strings.
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
