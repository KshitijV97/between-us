package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/importer"
	"github.com/kshitijv97/between-us/backend/internal/ingest"
	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/store/sqlite"
	"github.com/kshitijv97/between-us/backend/internal/synth"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"

	_ "github.com/kshitijv97/between-us/backend/internal/ingest/whatsapp"
)

// fixture imports a synthetic WhatsApp chat into a fresh SQLite store.
type fixture struct {
	ctx    context.Context
	st     store.Store
	svc    *Service
	chatID uuid.UUID
}

func newFixture(t *testing.T, n int) fixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn, u, err := st.EnsureLocalTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx = tenancy.WithScope(ctx, tenancy.Scope{TenantID: tn.ID, UserID: u.ID})

	var buf bytes.Buffer
	if err := synth.WriteWhatsApp(&buf, synth.DefaultConfig(n)); err != nil {
		t.Fatal(err)
	}
	im := &importer.Importer{Store: st}
	res, err := im.Run(ctx, importer.Request{PreviewRequest: importer.PreviewRequest{
		Source: ingest.BytesSource{Data: buf.Bytes(), FileName: "WhatsApp Chat with Bob.txt"}, Timezone: "Asia/Kolkata",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Fixed clock after the synthetic chat ends so "partial" is deterministic.
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	return fixture{ctx: ctx, st: st, chatID: res.Chat.ID, svc: &Service{Store: st, Now: func() time.Time { return now }}}
}

// metric decodes one metric of a result into a generic map.
func metric(t *testing.T, res *analytics.Result, id string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.Metrics[id])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v (%s)", id, err, raw)
	}
	return m
}

// humanCounts counts human messages per participant directly from the store,
// independently of the analytics engine (R-10).
func humanCounts(t *testing.T, f fixture, from, to time.Time) (map[uuid.UUID]int, int) {
	t.Helper()
	per := map[uuid.UUID]int{}
	total := 0
	err := f.st.IterateAnalysisMessages(f.ctx, f.chatID, from, to, func(m *analytics.Message) error {
		switch m.Kind {
		case analytics.KindSystem, analytics.KindCall:
			return nil
		}
		if m.ParticipantID != uuid.Nil {
			per[m.ParticipantID]++
			total++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return per, total
}

func TestAnalyzeThenMetricsMatchesStoredMessages(t *testing.T) {
	f := newFixture(t, 3000)
	sum, err := f.svc.Analyze(f.ctx, f.chatID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Messages != 3001 || sum.Sessions == 0 || sum.Rollups == 0 {
		t.Fatalf("summary = %+v", sum)
	}
	chat, _ := f.st.GetChat(f.ctx, f.chatID)
	if chat.RollupVersion != analytics.RollupVersion || chat.AnalyzedAt == nil {
		t.Fatalf("chat not marked analysed: %+v", chat)
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	for _, q := range []analytics.Query{
		{Kind: analytics.WindowAll},
		{Kind: analytics.WindowMonth, Anchor: analytics.DateFromYMD(2023, time.February, 10)},
		{Kind: analytics.WindowWeek, Anchor: analytics.DateFromYMD(2023, time.January, 20)},
		{Kind: analytics.WindowCustom, From: analytics.DateFromYMD(2023, time.January, 5), To: analytics.DateFromYMD(2023, time.January, 19)},
	} {
		res, err := f.svc.Metrics(f.ctx, f.chatID, q)
		if err != nil {
			t.Fatalf("%v: %v", q.Kind, err)
		}
		per, total := humanCounts(t, f, res.Window.From.Start(loc), res.Window.To.Start(loc))
		m := metric(t, res, "M-001")
		if got := int(m["combined"].(float64)); got != total {
			t.Errorf("%s %s..%s: M-001 combined = %d, stored human messages = %d", q.Kind, res.Window.From, res.Window.To, got, total)
		}
		bp := m["by_participant"].(map[string]any)
		for pid, n := range per {
			if got := int(bp[pid.String()].(float64)); got != n {
				t.Errorf("%s: M-001 for %s = %d, want %d", q.Kind, pid, got, n)
			}
		}
	}
}

func TestMetricsAnalysesOnDemand(t *testing.T) {
	f := newFixture(t, 500)
	// No explicit Analyze: Metrics must notice the missing rollups.
	res, err := f.svc.Metrics(f.ctx, f.chatID, analytics.Query{Kind: analytics.WindowAll})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stale {
		t.Error("freshly analysed result marked stale")
	}
	// Compare with the store, not the generator: calls are not messages (R-12).
	_, total := humanCounts(t, f, time.Time{}, time.Time{})
	if got := int(metric(t, res, "M-001")["combined"].(float64)); got != total || total < 450 {
		t.Errorf("M-001 = %d, stored human messages = %d", got, total)
	}
}

func TestShortWindowUsesExactPercentiles(t *testing.T) {
	f := newFixture(t, 2000)
	res, err := f.svc.Metrics(f.ctx, f.chatID, analytics.Query{Kind: analytics.WindowWeek, Anchor: analytics.DateFromYMD(2023, time.January, 9)})
	if err != nil {
		t.Fatal(err)
	}
	combined := metric(t, res, "M-010")["combined"].(map[string]any)
	if combined["approx"] == true {
		t.Errorf("a 7-day window should use exact percentiles: %v", combined)
	}
	if n, _ := combined["n"].(float64); n == 0 {
		t.Errorf("no responses found in a busy week: %v", combined)
	}
}

func TestReanalysisAfterSettingsChange(t *testing.T) {
	f := newFixture(t, 1500)
	before, err := f.svc.Metrics(f.ctx, f.chatID, analytics.Query{Kind: analytics.WindowAll})
	if err != nil {
		t.Fatal(err)
	}
	set := analytics.DefaultSettings()
	set.SessionGap = analytics.Duration(30 * time.Minute) // shorter gap → more sessions
	raw, _ := json.Marshal(set)
	if err := f.st.UpdateChatSettings(f.ctx, f.chatID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Analyze(f.ctx, f.chatID); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.Metrics(f.ctx, f.chatID, analytics.Query{Kind: analytics.WindowAll})
	if err != nil {
		t.Fatal(err)
	}
	b := metric(t, before, "M-007")["count"].(float64)
	a := metric(t, after, "M-007")["count"].(float64)
	if a <= b {
		t.Errorf("sessions with a 30m gap (%v) should exceed sessions with 6h (%v)", a, b)
	}
}

func TestInvalidWindow(t *testing.T) {
	f := newFixture(t, 50)
	_, err := f.svc.Metrics(f.ctx, f.chatID, analytics.Query{Kind: "fortnight"})
	if err == nil {
		t.Fatal("expected an error for an unknown window kind")
	}
}

// Decoding only the window's rows (plus the light index) must give exactly the
// same result as computing over every row with term maps.
func TestWindowedRollupsMatchFullComputation(t *testing.T) {
	f := newFixture(t, 4000)
	if _, err := f.svc.Analyze(f.ctx, f.chatID); err != nil {
		t.Fatal(err)
	}
	chat, set, loc, err := f.svc.load(f.ctx, f.chatID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := f.st.ListRollups(f.ctx, f.chatID, chat.AnalysisTZ, store.RollupQuery{WithTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := f.st.ListParticipants(f.ctx, f.chatID)
	var ps []analytics.Participant
	for _, p := range parts {
		ps = append(ps, analytics.Participant{ID: p.ID, Label: p.DisplayName})
	}
	idx := make([]analytics.RollupLite, len(all))
	for i := range all {
		idx[i] = all[i].Lite()
	}
	first, last := dateSpan(idx)
	now := f.svc.now()

	for _, q := range []analytics.Query{
		{Kind: analytics.WindowAll},
		{Kind: analytics.WindowYear, Anchor: analytics.DateFromYMD(2023, time.June, 1), Compare: true},
		{Kind: analytics.WindowMonth, Anchor: analytics.DateFromYMD(2023, time.March, 3), Compare: true},
		{Kind: analytics.WindowWeek, Anchor: analytics.DateFromYMD(2023, time.February, 8), Compare: true},
	} {
		got, err := f.svc.Metrics(f.ctx, f.chatID, q)
		if err != nil {
			t.Fatal(err)
		}
		win, _ := analytics.ResolveWindow(q, set, first, last, analytics.DateOf(now, loc), chat.AnalysisTZ)
		var prev *analytics.Window
		if q.Compare {
			prev = analytics.PreviousWindow(win, set)
		}
		in := analytics.Input{Window: win, Prev: prev, Rollups: all, Participants: ps, Settings: set,
			Now: now, Location: loc, RollupVersion: chat.RollupVersion}
		if in.Exact, err = f.svc.exact(f.ctx, f.chatID, win, set, loc); err != nil {
			t.Fatal(err)
		}
		if prev != nil {
			if in.PrevExact, err = f.svc.exact(f.ctx, f.chatID, *prev, set, loc); err != nil {
				t.Fatal(err)
			}
		}
		want, err := analytics.Compute(in)
		if err != nil {
			t.Fatal(err)
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		if !bytes.Equal(gj, wj) {
			t.Errorf("%s window: windowed result differs from full computation", q.Kind)
		}
	}
}
