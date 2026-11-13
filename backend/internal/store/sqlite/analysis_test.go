package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"
)

// analysedChat creates a completed chat with n messages from one participant.
func analysedChat(tb testing.TB, s *Store, n int) (context.Context, store.Chat, uuid.UUID, []int64) {
	tb.Helper()
	ctx := context.Background()
	tn, u, err := s.EnsureLocalTenant(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	ctx = tenancy.WithScope(ctx, tenancy.Scope{TenantID: tn.ID, UserID: u.ID})
	c, err := s.CreateChat(ctx, store.Chat{Title: "bench", Platform: "whatsapp", AnalysisTZ: "UTC"})
	if err != nil {
		tb.Fatal(err)
	}
	p, err := s.UpsertParticipant(ctx, store.Participant{ChatID: c.ID, DisplayName: "A"})
	if err != nil {
		tb.Fatal(err)
	}
	im, err := s.CreateImport(ctx, store.Import{ChatID: c.ID, Platform: "whatsapp"})
	if err != nil {
		tb.Fatal(err)
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	batch := make([]store.Message, 0, 2000)
	flush := func() {
		if _, err := s.InsertMessages(ctx, batch); err != nil {
			tb.Fatal(err)
		}
		batch = batch[:0]
	}
	pid := p.ID
	for i := range n {
		var k [8]byte
		binary.LittleEndian.PutUint64(k[:], uint64(i))
		fp := sha256.Sum256(k[:])
		sent := base.Add(time.Duration(i) * time.Minute)
		batch = append(batch, store.Message{ChatID: c.ID, ImportID: im.ID, ParticipantID: &pid, Ordinal: int64(i + 1),
			SentAt: sent, LocalDate: sent.Format(time.DateOnly), Kind: "text", Body: "hello", Fingerprint: fp[:]})
		if len(batch) == cap(batch) {
			flush()
		}
	}
	if len(batch) > 0 {
		flush()
	}
	if err := s.CompleteImport(ctx, im); err != nil {
		tb.Fatal(err)
	}
	ids := make([]int64, 0, n)
	if err := s.IterateAnalysisMessages(ctx, c.ID, time.Time{}, time.Time{}, func(m *analytics.Message) error {
		ids = append(ids, m.ID)
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
	return ctx, c, p.ID, ids
}

// TestDeleteChatPurgesDerivedRows checks at the table level that sessions and
// rollups disappear with their chat (the contract suite can only observe the
// chat becoming unreachable).
func TestDeleteChatPurgesDerivedRows(t *testing.T) {
	s := newTestStore(t)
	ctx, c, pid, ids := analysedChat(t, s, 10)
	d := analytics.DateOf(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	w, err := s.BeginAnalysis(ctx, c.ID, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.MessageFeatures(ctx, []analytics.MessageFeatures{{MessageID: ids[0], SessionIndex: 0}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sessions(ctx, []analytics.Session{{Index: 0, StartedAt: time.Now(), EndedAt: time.Now(), StartDate: d}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(ctx, []analytics.Rollup{{ParticipantID: pid, Date: d, MsgCount: 10}}, analytics.RollupVersion); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE chat_id = ?`, c.ID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("sessions") != 1 || count("daily_rollups") != 1 {
		t.Fatalf("fixture: sessions=%d rollups=%d", count("sessions"), count("daily_rollups"))
	}
	if err := s.DeleteChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n := count("sessions"); n != 0 {
		t.Errorf("sessions left after DeleteChat: %d", n)
	}
	if n := count("daily_rollups"); n != 0 {
		t.Errorf("rollups left after DeleteChat: %d", n)
	}
}

// BenchmarkMessageFeatures measures feature-column updates for 100k messages
// in one analysis run (target ≥ 50k rows/s).
func BenchmarkMessageFeatures(b *testing.B) {
	s := newTestStore(b)
	ctx, c, _, ids := analysedChat(b, s, 100_000)
	rows := make([]analytics.MessageFeatures, len(ids))
	sent := 0.3
	for i, id := range ids {
		rows[i] = analytics.MessageFeatures{MessageID: id, SessionIndex: i / 20, IsTurnStart: i%3 == 0,
			WordCount: 4, CharCount: 20, Sentiment: &sent}
	}
	b.ResetTimer()
	for b.Loop() {
		w, err := s.BeginAnalysis(ctx, c.ID, "UTC")
		if err != nil {
			b.Fatal(err)
		}
		for start := 0; start < len(rows); start += 2000 {
			if err := w.MessageFeatures(ctx, rows[start:min(start+2000, len(rows))]); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.Finish(ctx, nil, analytics.RollupVersion); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(rows))*float64(b.N)/b.Elapsed().Seconds(), "rows/s")
}

// BenchmarkListRollups reads ~5 years of rollups for two participants
// (3,600 rows) in the worst case: a full 100-term map on every day.
func BenchmarkListRollups(b *testing.B) { benchListRollups(b, 100) }

// BenchmarkListRollupsTypical uses 20 terms per day, closer to a real chat
// where most days have few distinct non-stop-words (target < 30 ms).
func BenchmarkListRollupsTypical(b *testing.B) { benchListRollups(b, 20) }

func benchListRollups(b *testing.B, terms int) {
	s := newTestStore(b)
	ctx, c, pid, _ := analysedChat(b, s, 10)
	other := uuid.Must(uuid.NewV7())
	start := analytics.DateOf(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	rollups := make([]analytics.Rollup, 0, 3600)
	for i := range 1800 {
		for _, p := range []uuid.UUID{pid, other} {
			r := analytics.Rollup{ParticipantID: p, Date: start.AddDays(i), MsgCount: 40, WordCount: 300,
				EmojiCounts: map[string]int{"❤️": 3, "😂": 2}, TopTerms: map[string]int{}}
			for t := range terms {
				r.TopTerms["term"+string(rune('a'+t%26))+string(rune('a'+t/26))] = t
			}
			r.ResponseHist[10] = 5
			rollups = append(rollups, r)
		}
	}
	w, err := s.BeginAnalysis(ctx, c.ID, "UTC")
	if err != nil {
		b.Fatal(err)
	}
	if err := w.Finish(ctx, rollups, analytics.RollupVersion); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		rs, err := s.ListRollups(ctx, c.ID, "UTC", store.RollupQuery{WithTerms: true})
		if err != nil || len(rs) != 3600 {
			b.Fatalf("%d rows, %v", len(rs), err)
		}
	}
}
