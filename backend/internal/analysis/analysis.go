// Package analysis runs the analytics engine against the store: it streams a
// chat's messages through the builder, persists features, sessions and daily
// rollups in one transaction, and answers metric queries for any window
// (docs/04-metrics-catalog.md §1, §11).
//
// The engine itself (internal/analytics) is pure; this package owns the I/O.
// M2 recomputes a whole chat after each import, which is O(n) and takes a few
// seconds for 100k messages; incremental recompute (§11.2) is a later
// optimisation.
package analysis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/analytics/text"
	"github.com/kshitijv97/between-us/backend/internal/store"
)

// Service analyses chats and computes metrics.
type Service struct {
	Store store.Store
	// LexiconDir overrides the embedded lexicons (BU_LEXICON_DIR).
	LexiconDir string
	Log        *slog.Logger
	// Now is the clock; tests inject a fixed time.
	Now func() time.Time
}

// Summary describes one analysis run.
type Summary struct {
	Messages int
	Sessions int
	Rollups  int
	Duration time.Duration
}

// Analyze recomputes all derived data of a chat. Readers keep seeing the
// previous complete results until the new ones commit.
func (s *Service) Analyze(ctx context.Context, chatID uuid.UUID) (Summary, error) {
	started := time.Now()
	chat, settings, loc, err := s.load(ctx, chatID)
	if err != nil {
		return Summary{}, err
	}
	an, err := s.analyzer(settings)
	if err != nil {
		return Summary{}, err
	}
	w, err := s.Store.BeginAnalysis(ctx, chatID, chat.AnalysisTZ)
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = w.Abort() }() // no-op after Finish

	sink := &writerSink{ctx: ctx, w: w}
	b := analytics.NewBuilder(analytics.BuildConfig{Settings: settings, Location: loc, Text: an}, sink)
	n := 0
	err = s.Store.IterateAnalysisMessages(ctx, chatID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		n++
		return b.Add(m)
	})
	if err != nil {
		return Summary{}, fmt.Errorf("reading messages: %w", err)
	}
	out, err := b.Finish()
	if err != nil {
		return Summary{}, err
	}
	if err := w.Finish(ctx, out.Rollups, analytics.RollupVersion); err != nil {
		return Summary{}, fmt.Errorf("saving analysis: %w", err)
	}
	sum := Summary{Messages: n, Sessions: out.Sessions, Rollups: len(out.Rollups), Duration: time.Since(started)}
	s.logger().InfoContext(ctx, "chat analysed", "chat_id", chatID.String(),
		"messages", sum.Messages, "sessions", sum.Sessions, "rollups", sum.Rollups, "duration", sum.Duration)
	return sum, nil
}

// Metrics resolves the window in q and computes the requested metrics. A
// chat whose derived data is missing or from an older RollupVersion is
// analysed first (local mode has no background jobs yet).
func (s *Service) Metrics(ctx context.Context, chatID uuid.UUID, q analytics.Query) (*analytics.Result, error) {
	chat, settings, loc, err := s.load(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if chat.RollupVersion != analytics.RollupVersion {
		if _, err := s.Analyze(ctx, chatID); err != nil {
			return nil, err
		}
		if chat, settings, loc, err = s.load(ctx, chatID); err != nil {
			return nil, err
		}
	}
	// The light index answers chat-level questions (span, baseline,
	// silences); full rows are decoded only for the windows being shown.
	index, err := s.Store.RollupIndex(ctx, chatID, chat.AnalysisTZ)
	if err != nil {
		return nil, err
	}
	parts, err := s.Store.ListParticipants(ctx, chatID)
	if err != nil {
		return nil, err
	}
	participants := make([]analytics.Participant, 0, len(parts))
	for _, p := range parts {
		label := p.DisplayName
		if p.Alias != "" {
			label = p.Alias
		}
		participants = append(participants, analytics.Participant{ID: p.ID, Label: label})
	}

	first, last := dateSpan(index)
	now := s.now()
	win, err := analytics.ResolveWindow(q, settings, first, last, analytics.DateOf(now, loc), chat.AnalysisTZ)
	if err != nil {
		return nil, err
	}
	var prev *analytics.Window
	if q.Compare {
		prev = analytics.PreviousWindow(win, settings)
	}

	rq := store.RollupQuery{Ranges: []store.DateRange{{From: win.From, To: win.To}}}
	// Exact top terms cover short windows; only longer ones merge daily maps.
	rq.WithTerms = win.Days > settings.ExactTermsMaxDays
	if prev != nil {
		rq.Ranges = append(rq.Ranges, store.DateRange{From: prev.From, To: prev.To})
		rq.WithTerms = rq.WithTerms || prev.Days > settings.ExactTermsMaxDays
	}
	rollups, err := s.Store.ListRollups(ctx, chatID, chat.AnalysisTZ, rq)
	if err != nil {
		return nil, err
	}

	in := analytics.Input{
		Window: win, Prev: prev, Rollups: rollups, Index: index, Participants: participants,
		Settings: settings, Now: now, Location: loc, Metrics: q.Metrics,
		Stale: chat.RollupVersion != analytics.RollupVersion, RollupVersion: chat.RollupVersion,
	}
	if in.Exact, err = s.exact(ctx, chatID, win, settings, loc); err != nil {
		return nil, err
	}
	if prev != nil {
		if in.PrevExact, err = s.exact(ctx, chatID, *prev, settings, loc); err != nil {
			return nil, err
		}
	}
	return analytics.Compute(in)
}

// exact gathers per-message data for short windows so percentiles and top
// terms are exact rather than approximated from rollups (§5.5).
func (s *Service) exact(ctx context.Context, chatID uuid.UUID, w analytics.Window, set analytics.Settings, loc *time.Location) (*analytics.ExactData, error) {
	wantRT := w.Days <= set.ExactPercentilesMaxDays
	wantTerms := w.Days <= set.ExactTermsMaxDays
	if !wantRT && !wantTerms {
		return nil, nil
	}
	var an analytics.TextAnalyzer
	if wantTerms {
		a, err := s.analyzer(set)
		if err != nil {
			return nil, err
		}
		an = a
	}
	ex := &analytics.ExactData{
		ResponseTimes: map[uuid.UUID][]int64{},
		Terms:         map[uuid.UUID]map[string]int{},
		Bigrams:       map[uuid.UUID]map[string]int{},
	}
	from, to := w.From.Start(loc), w.To.Start(loc)
	err := s.Store.IterateAnalysisMessages(ctx, chatID, from, to, func(m *analytics.Message) error {
		if m.ParticipantID == uuid.Nil {
			return nil
		}
		if wantRT && m.Derived.ResponseS != nil {
			ex.ResponseTimes[m.ParticipantID] = append(ex.ResponseTimes[m.ParticipantID], *m.Derived.ResponseS)
		}
		if wantTerms && termsEligible(m, set) {
			f := an.Analyze(m.Body)
			addCounts(ex.Terms, m.ParticipantID, f.Terms)
			addCounts(ex.Bigrams, m.ParticipantID, f.Bigrams)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !wantRT {
		ex.ResponseTimes = nil
	}
	if !wantTerms {
		ex.Terms, ex.Bigrams = nil, nil
	}
	return ex, nil
}

// termsEligible mirrors the builder's rule for top terms (M-016, R-14,
// R-15, R-17): text and captioned media, not deleted, not forwarded unless
// the chat opts in.
func termsEligible(m *analytics.Message, set analytics.Settings) bool {
	if m.Body == "" || m.IsDeleted || (m.IsForwarded && !set.IncludeForwardedInText) {
		return false
	}
	return m.Kind == analytics.KindText || m.Kind == analytics.KindMedia
}

func addCounts(dst map[uuid.UUID]map[string]int, p uuid.UUID, items []string) {
	if len(items) == 0 {
		return
	}
	m := dst[p]
	if m == nil {
		m = map[string]int{}
		dst[p] = m
	}
	for _, it := range items {
		m[it]++
	}
}

// load reads the chat and parses its settings and timezone.
func (s *Service) load(ctx context.Context, chatID uuid.UUID) (store.Chat, analytics.Settings, *time.Location, error) {
	chat, err := s.Store.GetChat(ctx, chatID)
	if err != nil {
		return store.Chat{}, analytics.Settings{}, nil, err
	}
	settings, err := analytics.ParseSettings(chat.Settings)
	if err != nil {
		return store.Chat{}, analytics.Settings{}, nil, err
	}
	loc, err := time.LoadLocation(chat.AnalysisTZ)
	if err != nil {
		return store.Chat{}, analytics.Settings{}, nil, fmt.Errorf("chat timezone %q: %w", chat.AnalysisTZ, err)
	}
	return chat, settings, loc, nil
}

func (s *Service) analyzer(set analytics.Settings) (*text.Analyzer, error) {
	return text.New(text.Config{
		Languages: set.TextLanguages, IncludeEmoticons: set.CountTextEmoticons, LexiconDir: s.LexiconDir,
	})
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// dateSpan returns the first and last dates with messages (zero when none).
// Call-only days (MsgCount 0) do not extend the span.
func dateSpan(idx []analytics.RollupLite) (first, last analytics.Date) {
	seen := false
	for _, r := range idx {
		if r.MsgCount == 0 {
			continue
		}
		if !seen || r.Date < first {
			first = r.Date
		}
		if !seen || r.Date > last {
			last = r.Date
		}
		seen = true
	}
	return first, last
}

// writerSink adapts the store's AnalysisWriter to the builder's sink.
type writerSink struct {
	ctx context.Context
	w   store.AnalysisWriter
}

func (s *writerSink) MessageFeatures(batch []analytics.MessageFeatures) error {
	return s.w.MessageFeatures(s.ctx, batch)
}

func (s *writerSink) Sessions(batch []analytics.Session) error {
	return s.w.Sessions(s.ctx, batch)
}

// ErrNotAnalysed is returned when metrics are requested for a chat with no
// messages to analyse.
var ErrNotAnalysed = errors.New("this chat has no messages to analyse yet")
