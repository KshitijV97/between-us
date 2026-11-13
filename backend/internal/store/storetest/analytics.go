package storetest

// Contract tests for the M2 analytics persistence: chat settings, the
// (sent_at, ordinal) message cursor, IterateAnalysisMessages, AnalysisWriter
// atomicity, rollups, sessions and their tenant isolation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"
)

// analyticsCalls lists every M2 method for the missing-scope test.
func analyticsCalls(ctx context.Context, s store.Store, id uuid.UUID) map[string]func() error {
	return map[string]func() error{
		"UpdateChatSettings": func() error { return s.UpdateChatSettings(ctx, id, json.RawMessage(`{}`)) },
		"IterateAnalysisMessages": func() error {
			return s.IterateAnalysisMessages(ctx, id, time.Time{}, time.Time{}, func(*analytics.Message) error { return nil })
		},
		"BeginAnalysis": func() error { _, err := s.BeginAnalysis(ctx, id, "UTC"); return err },
		"ListRollups":   func() error { _, err := s.ListRollups(ctx, id, "UTC", store.RollupQuery{WithTerms: true}); return err },
		"RollupIndex":   func() error { _, err := s.RollupIndex(ctx, id, "UTC"); return err },
		"ListSessions":  func() error { _, err := s.ListSessions(ctx, id, time.Time{}, time.Time{}); return err },
	}
}

// pages walks ListMessages with the (sent_at, ordinal) cursor and returns the
// bodies page by page.
func (e *env) pages(t *testing.T, chatID uuid.UUID, limit int) [][]string {
	t.Helper()
	var (
		out   [][]string
		after store.MessageCursor
	)
	for range 1000 {
		ms, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: chatID, After: after, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 0 {
			return out
		}
		out = append(out, bodies(ms))
		last := ms[len(ms)-1]
		after = store.MessageCursor{SentAt: last.SentAt, Ordinal: last.Ordinal}
	}
	t.Fatal("pagination did not terminate")
	return nil
}

// at builds a message at an explicit time and ordinal.
func at(c store.Chat, im store.Import, sent time.Time, ordinal int64, body string) store.Message {
	return store.Message{
		ChatID: c.ID, ImportID: im.ID, Ordinal: ordinal, SentAt: sent,
		LocalDate: sent.Format(time.DateOnly), Kind: "text", Body: body, Fingerprint: sum(c.ID.String() + body),
	}
}

func testCursorPairs(t *testing.T, e *env) {
	c := e.chat(t, "cursor")
	first := e.imp(t, c.ID)
	// Three messages share one minute (minute-precision export); ordinal
	// breaks the tie.
	e.insert(t,
		at(c, first, base.Add(10*time.Minute), 1, "c1"),
		at(c, first, base.Add(10*time.Minute), 2, "c2"),
		at(c, first, base.Add(10*time.Minute), 3, "c3"),
		at(c, first, base.Add(20*time.Minute), 4, "d1"),
	)
	e.complete(t, first)
	// A later import brings older messages with higher ordinals: ordinals are
	// no longer monotonic in time, which a single-ordinal cursor gets wrong.
	second := e.imp(t, c.ID)
	e.insert(t,
		at(c, second, base.Add(1*time.Minute), 5, "a1"),
		at(c, second, base.Add(1*time.Minute), 6, "a2"),
		at(c, second, base.Add(10*time.Minute), 7, "c4"),
	)
	e.complete(t, second)

	want := []string{"a1", "a2", "c1", "c2", "c3", "c4", "d1"}
	for _, limit := range []int{1, 2, 3, 10} {
		var got []string
		for _, p := range e.pages(t, c.ID, limit) {
			got = append(got, p...)
		}
		if !slices.Equal(got, want) {
			t.Errorf("limit %d: order = %v, want %v", limit, got, want)
		}
	}
}

func testChatSettings(t *testing.T, e *env) {
	c := e.chat(t, "settings")
	raw := json.RawMessage(`{"session_gap":"2h0m0s","week_start":0}`)
	if err := e.s.UpdateChatSettings(e.ctx, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetChat(e.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Settings, &m); err != nil || m["session_gap"] != "2h0m0s" {
		t.Errorf("settings = %s (%v)", got.Settings, err)
	}
	if got.RollupVersion != 0 || got.AnalyzedAt != nil {
		t.Errorf("new chat must be unanalysed: version=%d analyzed=%v", got.RollupVersion, got.AnalyzedAt)
	}
	if err := e.s.UpdateChatSettings(e.ctx, c.ID, json.RawMessage(`{not json`)); err == nil {
		t.Error("malformed settings JSON accepted")
	}
	wantErr(t, "UpdateChatSettings(unknown chat)",
		e.s.UpdateChatSettings(e.ctx, uuid.Must(uuid.NewV7()), json.RawMessage(`{}`)), store.ErrNotFound)
}

// analysisFixture is a completed chat with two participants, ready to analyse.
type analysisFixture struct {
	chat store.Chat
	imp  store.Import
	a, b store.Participant
	ids  []int64 // message IDs in (sent_at, ordinal) order
}

func (e *env) analysisFixture(t *testing.T) analysisFixture {
	t.Helper()
	c := e.chat(t, "analysis")
	pa, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "Alice", PlatformKeys: []string{"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "Bob", PlatformKeys: []string{"bob"}})
	if err != nil {
		t.Fatal(err)
	}
	im, err := e.s.CreateImport(e.ctx, store.Import{ChatID: c.ID, Platform: "whatsapp", Timezone: "Asia/Kolkata", TimePrecision: "second"})
	if err != nil {
		t.Fatal(err)
	}
	from := func(p store.Participant) *uuid.UUID { id := p.ID; return &id }
	m1 := at(c, im, base, 1, "hello bob")
	m1.ParticipantID = from(pa)
	m2 := at(c, im, base.Add(5*time.Minute), 2, "hi alice")
	m2.ParticipantID = from(pb)
	sys := at(c, im, base.Add(6*time.Minute), 3, "Messages are end-to-end encrypted")
	sys.Kind = "system"
	call := at(c, im, base.Add(7*time.Minute), 4, "")
	call.Kind, call.ParticipantID = "call", from(pb)
	call.Meta = json.RawMessage(`{"call":{"Video":true,"Missed":false,"DurationSec":95}}`)
	m5 := at(c, im, base.Add(26*time.Hour), 5, "next day")
	m5.ParticipantID = from(pa)
	m5.Meta = json.RawMessage(`{"precision":"minute"}`)
	e.insert(t, m1, m2, sys, call, m5)
	im.TimePrecision = "second"
	e.complete(t, im)

	f := analysisFixture{chat: c, imp: im, a: pa, b: pb}
	err = e.s.IterateAnalysisMessages(e.ctx, c.ID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		f.ids = append(f.ids, m.ID)
		return nil
	})
	if err != nil || len(f.ids) != 5 {
		t.Fatalf("fixture iteration: %v ids=%v", err, f.ids)
	}
	return f
}

// rollup builds a recognisable rollup row; marker shows up in several fields
// so round-trips of arrays and maps are checked.
func rollup(p uuid.UUID, d analytics.Date, marker int) analytics.Rollup {
	r := analytics.Rollup{
		ParticipantID: p, Date: d, MsgCount: marker, WordCount: marker * 3,
		FirstMsgAt: base.Add(time.Duration(marker) * time.Minute), LastMsgAt: base.Add(time.Duration(marker) * time.Hour),
		ResponseSumS: int64(marker) * 60, SentimentSum: float64(marker) / 10,
		EmojiCounts: map[string]int{"❤️": marker}, TopTerms: map[string]int{"accha": marker},
	}
	r.HourlyCounts[marker%24] = marker
	r.ResponseHist[marker%analytics.ResponseHistBuckets] = marker
	r.SentimentHist[19] = marker
	return r
}

func ptr[T any](v T) *T { return &v }

// run performs a complete analysis run with the given rows.
func (e *env) run(t *testing.T, chatID uuid.UUID, tz string, feats []analytics.MessageFeatures, sessions []analytics.Session, rollups []analytics.Rollup) {
	t.Helper()
	w, err := e.s.BeginAnalysis(e.ctx, chatID, tz)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.MessageFeatures(e.ctx, feats); err != nil {
		t.Fatal(err)
	}
	if sessions != nil {
		if err := w.Sessions(e.ctx, sessions); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish(e.ctx, rollups, analytics.RollupVersion); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Errorf("Abort after Finish must be a no-op, got %v", err)
	}
}

func (e *env) derived(t *testing.T, chatID uuid.UUID) map[int64]analytics.MessageFeatures {
	t.Helper()
	out := map[int64]analytics.MessageFeatures{}
	err := e.s.IterateAnalysisMessages(e.ctx, chatID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		out[m.ID] = m.Derived
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func testAnalysisRoundTrip(t *testing.T, e *env) {
	f := e.analysisFixture(t)
	d0 := analytics.DateOf(base, time.UTC)
	feats := []analytics.MessageFeatures{
		{MessageID: f.ids[0], SessionIndex: 0, IsTurnStart: true, WordCount: 2, CharCount: 8, Sentiment: ptr(0.4)},
		{MessageID: f.ids[1], SessionIndex: 0, IsTurnStart: true, ResponseS: ptr(int64(300)), WordCount: 2, EmojiCount: 1,
			LinkCount: 2, IsQuestion: true, AffectionHits: 3, Conflict: true},
		{MessageID: f.ids[4], SessionIndex: 1, IsTurnStart: true, WordCount: 2},
	}
	sessions := []analytics.Session{
		{Index: 0, StartedAt: base, EndedAt: base.Add(5 * time.Minute), Initiator: f.a.ID, Ender: f.b.ID,
			MsgCount: 2, TurnCount: 2, FirstMsgID: f.ids[0], LastMsgID: f.ids[1], StartDate: d0, ConflictMsg: 1},
		{Index: 1, StartedAt: base.Add(26 * time.Hour), EndedAt: base.Add(26 * time.Hour), Initiator: f.a.ID, Ender: f.a.ID,
			MsgCount: 1, TurnCount: 1, FirstMsgID: f.ids[4], LastMsgID: f.ids[4], StartDate: d0.AddDays(1)},
	}
	rollups := []analytics.Rollup{rollup(f.a.ID, d0, 1), rollup(f.b.ID, d0, 2), rollup(f.a.ID, d0.AddDays(1), 3)}
	e.run(t, f.chat.ID, "Asia/Kolkata", feats, sessions, rollups)

	c, err := e.s.GetChat(e.ctx, f.chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.RollupVersion != analytics.RollupVersion || c.AnalyzedAt == nil {
		t.Errorf("chat analysis state: version=%d analyzed=%v", c.RollupVersion, c.AnalyzedAt)
	}

	got := e.derived(t, f.chat.ID)
	m2 := got[f.ids[1]]
	if m2.MessageID != f.ids[1] || m2.SessionIndex != 0 || !m2.IsTurnStart || m2.ResponseS == nil || *m2.ResponseS != 300 ||
		m2.EmojiCount != 1 || m2.LinkCount != 2 || !m2.IsQuestion || m2.AffectionHits != 3 || !m2.Conflict || m2.Sentiment != nil {
		t.Errorf("derived round-trip: %+v", m2)
	}
	if s := got[f.ids[0]].Sentiment; s == nil || *s != 0.4 {
		t.Errorf("sentiment round-trip: %v", s)
	}
	if sys := got[f.ids[2]]; sys.SessionIndex != -1 || sys.IsTurnStart || sys.ResponseS != nil {
		t.Errorf("untouched row must keep defaults: %+v", sys)
	}

	rs, err := e.s.ListRollups(e.ctx, f.chat.ID, "Asia/Kolkata", store.RollupQuery{WithTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].Date != d0 || rs[2].Date != d0.AddDays(1) {
		t.Fatalf("rollups order: %+v", rs)
	}
	// Within one date, rows are ordered by participant ID.
	if rs[0].ParticipantID.String() > rs[1].ParticipantID.String() {
		t.Errorf("rollups not ordered by participant within a date")
	}
	var r2 analytics.Rollup
	for _, r := range rs {
		if r.ParticipantID == f.b.ID {
			r2 = r
		}
	}
	want := rollup(f.b.ID, d0, 2)
	if r2.MsgCount != 2 || r2.WordCount != 6 || r2.HourlyCounts != want.HourlyCounts || r2.ResponseHist != want.ResponseHist ||
		r2.SentimentHist != want.SentimentHist || r2.EmojiCounts["❤️"] != 2 || r2.TopTerms["accha"] != 2 ||
		!r2.FirstMsgAt.Equal(want.FirstMsgAt) || !r2.LastMsgAt.Equal(want.LastMsgAt) || r2.SentimentSum != 0.2 {
		t.Errorf("rollup round-trip: %+v", r2)
	}

	// Date ranges select rows; term maps are only decoded on request.
	day0, err := e.s.ListRollups(e.ctx, f.chat.ID, "Asia/Kolkata", store.RollupQuery{Ranges: []store.DateRange{{From: d0, To: d0.AddDays(1)}}})
	if err != nil || len(day0) != 2 {
		t.Fatalf("ranged rollups: %d rows, %v", len(day0), err)
	}
	for _, r := range day0 {
		if r.TopTerms != nil || r.TopBigrams != nil {
			t.Errorf("term maps decoded without WithTerms: %+v", r.TopTerms)
		}
		if r.ParticipantID == f.b.ID && (r.EmojiCounts["❤️"] != 2 || r.MsgCount != 2) {
			t.Errorf("ranged rollup lost fields: %+v", r)
		}
	}
	if two, _ := e.s.ListRollups(e.ctx, f.chat.ID, "Asia/Kolkata", store.RollupQuery{Ranges: []store.DateRange{
		{From: d0, To: d0.AddDays(1)}, {From: d0.AddDays(1), To: d0.AddDays(2)}}}); len(two) != 3 {
		t.Errorf("two ranges returned %d rows, want 3", len(two))
	}

	// The light index matches the full rows without decoding payloads.
	idx, err := e.s.RollupIndex(e.ctx, f.chat.ID, "Asia/Kolkata")
	if err != nil || len(idx) != len(rs) {
		t.Fatalf("rollup index: %d rows, %v", len(idx), err)
	}
	for i := range rs {
		if idx[i] != rs[i].Lite() {
			t.Errorf("index row %d = %+v, want %+v", i, idx[i], rs[i].Lite())
		}
	}

	ss, err := e.s.ListSessions(e.ctx, f.chat.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].Initiator != f.a.ID || ss[0].Ender != f.b.ID || ss[0].ConflictMsg != 1 || ss[0].StartDate != d0 ||
		!ss[0].StartedAt.Equal(base) || ss[1].FirstMsgID != f.ids[4] || ss[1].StartDate != d0.AddDays(1) {
		t.Errorf("sessions round-trip: %+v", ss)
	}
	// Half-open range on started_at.
	ss, err = e.s.ListSessions(e.ctx, f.chat.ID, base.Add(time.Minute), base.Add(26*time.Hour+time.Second))
	if err != nil || len(ss) != 1 || ss[0].Index != 1 {
		t.Errorf("ListSessions range: %+v %v", ss, err)
	}
	ss, _ = e.s.ListSessions(e.ctx, f.chat.ID, time.Time{}, base)
	if len(ss) != 0 {
		t.Errorf("ListSessions upper bound is exclusive: %+v", ss)
	}

	// A second timezone keeps its own rollup set.
	e.run(t, f.chat.ID, "UTC", nil, nil, []analytics.Rollup{rollup(f.a.ID, d0, 9)})
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "Asia/Kolkata", store.RollupQuery{WithTerms: true}); len(rs) != 3 {
		t.Errorf("other timezone's rollups replaced: %d rows", len(rs))
	}
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "UTC", store.RollupQuery{WithTerms: true}); len(rs) != 1 || rs[0].MsgCount != 9 {
		t.Errorf("UTC rollups: %+v", rs)
	}
	// That run produced no sessions, so the chat's sessions were cleared.
	if ss, _ := e.s.ListSessions(e.ctx, f.chat.ID, time.Time{}, time.Time{}); len(ss) != 0 {
		t.Errorf("a run without sessions must clear old ones: %d left", len(ss))
	}

	// A re-run replaces rollups and sessions for its timezone.
	e.run(t, f.chat.ID, "Asia/Kolkata", feats[:1], sessions[:1], []analytics.Rollup{rollup(f.a.ID, d0, 5)})
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "Asia/Kolkata", store.RollupQuery{WithTerms: true}); len(rs) != 1 || rs[0].MsgCount != 5 {
		t.Errorf("re-run rollups: %+v", rs)
	}
	if ss, _ := e.s.ListSessions(e.ctx, f.chat.ID, time.Time{}, time.Time{}); len(ss) != 1 {
		t.Errorf("re-run sessions: %d", len(ss))
	}
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "Europe/London", store.RollupQuery{WithTerms: true}); len(rs) != 0 {
		t.Errorf("unknown timezone must return no rollups: %d", len(rs))
	}
}

func testAnalysisAbort(t *testing.T, e *env) {
	f := e.analysisFixture(t)
	d0 := analytics.DateOf(base, time.UTC)
	e.run(t, f.chat.ID, "UTC",
		[]analytics.MessageFeatures{{MessageID: f.ids[0], SessionIndex: 0, WordCount: 7}},
		[]analytics.Session{{Index: 0, StartedAt: base, EndedAt: base, StartDate: d0, MsgCount: 1}},
		[]analytics.Rollup{rollup(f.a.ID, d0, 1)})
	before, _ := e.s.GetChat(e.ctx, f.chat.ID)

	w, err := e.s.BeginAnalysis(e.ctx, f.chat.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.MessageFeatures(e.ctx, []analytics.MessageFeatures{{MessageID: f.ids[0], SessionIndex: 4, WordCount: 99}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sessions(e.ctx, []analytics.Session{{Index: 7, StartedAt: base, EndedAt: base, StartDate: d0}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(e.ctx, nil, analytics.RollupVersion); err == nil {
		t.Error("Finish after Abort must fail")
	}
	if err := w.Abort(); err != nil {
		t.Errorf("second Abort: %v", err)
	}

	if d := e.derived(t, f.chat.ID)[f.ids[0]]; d.WordCount != 7 || d.SessionIndex != 0 {
		t.Errorf("aborted run leaked features: %+v", d)
	}
	if ss, _ := e.s.ListSessions(e.ctx, f.chat.ID, time.Time{}, time.Time{}); len(ss) != 1 || ss[0].Index != 0 {
		t.Errorf("aborted run changed sessions: %+v", ss)
	}
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "UTC", store.RollupQuery{WithTerms: true}); len(rs) != 1 || rs[0].MsgCount != 1 {
		t.Errorf("aborted run changed rollups: %+v", rs)
	}
	after, _ := e.s.GetChat(e.ctx, f.chat.ID)
	if !after.AnalyzedAt.Equal(*before.AnalyzedAt) {
		t.Errorf("aborted run touched analyzed_at")
	}
	// The writer connection is free again: ordinary writes work.
	if err := e.s.UpdateChatSettings(e.ctx, f.chat.ID, json.RawMessage(`{}`)); err != nil {
		t.Errorf("write after Abort: %v", err)
	}
	// A cancelled context also ends a run without leaking the transaction.
	ctx, cancel := context.WithCancel(e.ctx)
	w, err = e.s.BeginAnalysis(ctx, f.chat.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = w.Abort()
	if err := e.s.UpdateChatSettings(e.ctx, f.chat.ID, json.RawMessage(`{}`)); err != nil {
		t.Errorf("write after cancelled run: %v", err)
	}
}

// testAnalysisReaders checks that while a run is open, readers keep working and
// see the previous complete results, never the in-progress ones.
func testAnalysisReaders(t *testing.T, e *env) {
	f := e.analysisFixture(t)
	d0 := analytics.DateOf(base, time.UTC)
	e.run(t, f.chat.ID, "UTC",
		[]analytics.MessageFeatures{{MessageID: f.ids[0], SessionIndex: 0, WordCount: 1}},
		[]analytics.Session{{Index: 0, StartedAt: base, EndedAt: base, StartDate: d0, MsgCount: 1}},
		[]analytics.Rollup{rollup(f.a.ID, d0, 1)})

	w, err := e.s.BeginAnalysis(e.ctx, f.chat.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Abort() }()
	if err := w.MessageFeatures(e.ctx, []analytics.MessageFeatures{{MessageID: f.ids[0], SessionIndex: 3, WordCount: 50}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sessions(e.ctx, nil); err != nil { // clears sessions inside the transaction
		t.Fatal(err)
	}

	// Readers must not block on the open write transaction.
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 4 {
		wg.Go(func() {
			got := map[int64]analytics.MessageFeatures{}
			if err := e.s.IterateAnalysisMessages(ctx, f.chat.ID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
				got[m.ID] = m.Derived
				return nil
			}); err != nil {
				errs <- err
				return
			}
			if d := got[f.ids[0]]; d.WordCount != 1 || d.SessionIndex != 0 {
				errs <- fmt.Errorf("reader saw in-progress features: %+v", d)
			}
			if ss, err := e.s.ListSessions(ctx, f.chat.ID, time.Time{}, time.Time{}); err != nil || len(ss) != 1 {
				errs <- fmt.Errorf("reader sessions during run: %d %v", len(ss), err)
			}
			if rs, err := e.s.ListRollups(ctx, f.chat.ID, "UTC", store.RollupQuery{WithTerms: true}); err != nil || len(rs) != 1 {
				errs <- fmt.Errorf("reader rollups during run: %d %v", len(rs), err)
			}
			if _, err := e.s.ListMessages(ctx, store.MessageQuery{ChatID: f.chat.ID}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := w.Finish(e.ctx, []analytics.Rollup{rollup(f.a.ID, d0, 2), rollup(f.b.ID, d0, 3)}, analytics.RollupVersion); err != nil {
		t.Fatal(err)
	}
	if d := e.derived(t, f.chat.ID)[f.ids[0]]; d.WordCount != 50 {
		t.Errorf("committed features not visible: %+v", d)
	}
	if rs, _ := e.s.ListRollups(e.ctx, f.chat.ID, "UTC", store.RollupQuery{WithTerms: true}); len(rs) != 2 {
		t.Errorf("committed rollups not visible: %d", len(rs))
	}
}

func testIterateAnalysis(t *testing.T, e *env) {
	f := e.analysisFixture(t)
	// An import still processing is invisible to analysis too.
	open := e.imp(t, f.chat.ID)
	e.insert(t, at(f.chat, open, base.Add(time.Minute), 9, "invisible"))

	var got []analytics.Message
	err := e.s.IterateAnalysisMessages(e.ctx, f.chat.ID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		got = append(got, *m) // copy: the pointer is reused
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d messages, want 5 (in-progress import excluded)", len(got))
	}
	kinds := []string{got[0].Kind, got[1].Kind, got[2].Kind, got[3].Kind, got[4].Kind}
	if !slices.Equal(kinds, []string{"text", "text", "system", "call", "text"}) {
		t.Errorf("order/kinds = %v", kinds)
	}
	if got[0].ParticipantID != f.a.ID || got[1].ParticipantID != f.b.ID || got[2].ParticipantID != uuid.Nil {
		t.Errorf("participants: %v %v %v", got[0].ParticipantID, got[1].ParticipantID, got[2].ParticipantID)
	}
	if got[0].Body != "hello bob" || !got[1].SentAt.Equal(base.Add(5*time.Minute)) || got[1].Ordinal != 2 {
		t.Errorf("fields: %+v / %+v", got[0], got[1])
	}
	if c := got[3].Call; c == nil || !c.Video || c.Missed || c.DurationSec != 95 {
		t.Errorf("call decoding: %+v", got[3].Call)
	}
	if got[0].Call != nil {
		t.Error("text message must have no call info")
	}
	// Precision: the import says seconds, message 5's meta says minute.
	if got[0].MinutePrecision || !got[4].MinutePrecision {
		t.Errorf("precision: m1=%v m5=%v", got[0].MinutePrecision, got[4].MinutePrecision)
	}
	if got[0].Derived.SessionIndex != -1 || got[0].Derived.MessageID != got[0].ID {
		t.Errorf("unanalysed defaults: %+v", got[0].Derived)
	}

	// [from, to) bounds on sent_at.
	var bodiesIn []string
	err = e.s.IterateAnalysisMessages(e.ctx, f.chat.ID, base.Add(5*time.Minute), base.Add(7*time.Minute), func(m *analytics.Message) error {
		bodiesIn = append(bodiesIn, m.Kind+":"+m.Body)
		return nil
	})
	if err != nil || !slices.Equal(bodiesIn, []string{"text:hi alice", "system:Messages are end-to-end encrypted"}) {
		t.Errorf("range = %v (%v)", bodiesIn, err)
	}

	// A minute-precision import marks all its messages.
	pc := e.chat(t, "minute")
	mi, err := e.s.CreateImport(e.ctx, store.Import{ChatID: pc.ID, Platform: "whatsapp", TimePrecision: "minute"})
	if err != nil {
		t.Fatal(err)
	}
	e.insert(t, at(pc, mi, base, 1, "x"))
	mi.TimePrecision = "minute"
	e.complete(t, mi)
	_ = e.s.IterateAnalysisMessages(e.ctx, pc.ID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		if !m.MinutePrecision {
			t.Error("import time_precision=minute not applied")
		}
		return nil
	})

	// fn errors stop the iteration and are returned unchanged.
	stop := errors.New("stop")
	calls := 0
	err = e.s.IterateAnalysisMessages(e.ctx, f.chat.ID, time.Time{}, time.Time{}, func(*analytics.Message) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("fn error: %v after %d calls", err, calls)
	}
	err = e.s.IterateAnalysisMessages(e.ctx, uuid.Must(uuid.NewV7()), time.Time{}, time.Time{}, func(*analytics.Message) error { return nil })
	wantErr(t, "IterateAnalysisMessages(unknown chat)", err, store.ErrNotFound)
}

func testAnalysisForeignMessages(t *testing.T, e *env) {
	f := e.analysisFixture(t)
	other := e.analysisFixture(t)
	w, err := e.s.BeginAnalysis(e.ctx, f.chat.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	err = w.MessageFeatures(e.ctx, []analytics.MessageFeatures{{MessageID: other.ids[0], WordCount: 42}})
	wantErr(t, "MessageFeatures(other chat's message)", err, store.ErrNotFound)
	_ = w.Abort()
	if d := e.derived(t, other.chat.ID)[other.ids[0]]; d.WordCount != 0 {
		t.Errorf("other chat's message was modified: %+v", d)
	}
	_, err = e.s.BeginAnalysis(e.ctx, uuid.Must(uuid.NewV7()), "UTC")
	wantErr(t, "BeginAnalysis(unknown chat)", err, store.ErrNotFound)
	_, err = e.s.ListRollups(e.ctx, uuid.Must(uuid.NewV7()), "UTC", store.RollupQuery{WithTerms: true})
	wantErr(t, "ListRollups(unknown chat)", err, store.ErrNotFound)
	_, err = e.s.ListSessions(e.ctx, uuid.Must(uuid.NewV7()), time.Time{}, time.Time{})
	wantErr(t, "ListSessions(unknown chat)", err, store.ErrNotFound)
}

func testAnalyticsTenantIsolation(t *testing.T, e *env) {
	tc, ok := e.s.(TenantCreator)
	if !ok {
		t.Skip("store cannot create extra tenants (store.Store gains tenant creation in M5)")
	}
	tb, err := tc.CreateTenant(context.Background(), "Tenant B", "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	b := &env{s: e.s, tenant: tb, ctx: tenancy.WithScope(context.Background(), tenancy.Scope{TenantID: tb.ID})}
	bf := b.analysisFixture(t)
	d0 := analytics.DateOf(base, time.UTC)
	b.run(t, bf.chat.ID, "UTC",
		[]analytics.MessageFeatures{{MessageID: bf.ids[0], SessionIndex: 0, WordCount: 5}},
		[]analytics.Session{{Index: 0, StartedAt: base, EndedAt: base, StartDate: d0, MsgCount: 1}},
		[]analytics.Rollup{rollup(bf.a.ID, d0, 1)})

	// Tenant A, using B's IDs, must see nothing and change nothing.
	for name, call := range analyticsCalls(e.ctx, e.s, bf.chat.ID) {
		wantErr(t, name, call(), store.ErrNotFound)
	}
	af := e.analysisFixture(t)
	w, err := e.s.BeginAnalysis(e.ctx, af.chat.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	err = w.MessageFeatures(e.ctx, []analytics.MessageFeatures{{MessageID: bf.ids[0], WordCount: 99}})
	wantErr(t, "MessageFeatures(B's message from A's run)", err, store.ErrNotFound)
	_ = w.Abort()
	// A run for A in the same timezone leaves B's rollups and sessions alone.
	e.run(t, af.chat.ID, "UTC", nil, nil, nil)

	if d := b.derived(t, bf.chat.ID)[bf.ids[0]]; d.WordCount != 5 {
		t.Errorf("B's features changed: %+v", d)
	}
	if rs, _ := b.s.ListRollups(b.ctx, bf.chat.ID, "UTC", store.RollupQuery{WithTerms: true}); len(rs) != 1 {
		t.Errorf("B's rollups changed: %d", len(rs))
	}
	if ss, _ := b.s.ListSessions(b.ctx, bf.chat.ID, time.Time{}, time.Time{}); len(ss) != 1 {
		t.Errorf("B's sessions changed: %d", len(ss))
	}
	if c, _ := b.s.GetChat(b.ctx, bf.chat.ID); string(c.Settings) != "{}" {
		t.Errorf("B's settings changed: %s", c.Settings)
	}
}
