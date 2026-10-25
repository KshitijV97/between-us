package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/chat-analyzer/backend/internal/store"
	"github.com/kshitijv97/chat-analyzer/backend/internal/store/storetest"
	"github.com/kshitijv97/chat-analyzer/backend/internal/tenancy"
)

// newTestStore opens a migrated store in a fresh temp-file database. A real
// file (not :memory:) exercises the WAL reader/writer pool split.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return newTestStore(t) })
}

func TestMemoryDSN(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The schema must survive across calls: all of them share one connection.
	if _, _, err := s.EnsureLocalTenant(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureLocalTenant(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateIsIdempotentAndPragmasApply(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var mode string
	var fk int
	if err := s.r.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", mode, err)
	}
	if err := s.r.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d (%v), want 1", fk, err)
	}
	// Reader connections must be read-only.
	if _, err := s.r.ExecContext(ctx, "INSERT INTO tenants (id, name, slug, created_at, updated_at) VALUES ('x','x','x',0,0)"); err == nil {
		t.Error("reader pool accepted a write")
	}
}

// TestFTSStaysConsistent runs FTS5's integrity check after inserts, a failed
// import and a chat deletion: the external-content triggers must mirror every
// change exactly or the index silently drifts from the table.
func TestFTSStaysConsistent(t *testing.T) {
	s := newTestStore(t)
	ctx, c, im := seed(t, s)
	msgs := make([]store.Message, 0, 300)
	for i := range 300 {
		body := "word number"
		if i%3 == 0 {
			body = "" // media-only rows are not indexed
		}
		msgs = append(msgs, benchMessage(c.ID, im.ID, int64(i+1), body))
	}
	if _, err := s.InsertMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		t.Helper()
		if _, err := s.w.ExecContext(ctx, "INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')"); err != nil {
			t.Fatalf("FTS integrity after %s: %v", stage, err)
		}
	}
	check("insert")
	if err := s.FailImport(ctx, im.ID, "x", nil); err != nil {
		t.Fatal(err)
	}
	check("FailImport")
	im2, err := s.CreateImport(ctx, store.Import{ChatID: c.ID, Platform: "whatsapp"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range msgs {
		msgs[i].ImportID = im2.ID
	}
	if _, err := s.InsertMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	check("DeleteChat")
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Errorf("messages left after DeleteChat: %d (%v)", n, err)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := map[string]string{
		"hello world": `"hello" "world"`,
		`foo*`:        `"foo" *`,
		`"`:           ``,
		`NEAR(`:       `"NEAR("`,
		`a"b`:         `"ab"`,
		`  `:          ``,
		`--`:          ``,
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// seed creates the local tenant, one chat and one open import.
func seed(t testing.TB, s *Store) (context.Context, store.Chat, store.Import) {
	t.Helper()
	tn, u, err := s.EnsureLocalTenant(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenancy.WithScope(context.Background(), tenancy.Scope{TenantID: tn.ID, UserID: u.ID})
	c, err := s.CreateChat(ctx, store.Chat{Title: "bench", Platform: "whatsapp", AnalysisTZ: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	im, err := s.CreateImport(ctx, store.Import{ChatID: c.ID, Platform: "whatsapp"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, c, im
}

// benchMessage builds a realistic row: ~40-character body (the sizing
// assumption in docs/03-data-model.md §8) and a 32-byte fingerprint.
func benchMessage(chatID, importID uuid.UUID, ordinal int64, body string) store.Message {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(ordinal))
	fp := sha256.Sum256(b[:])
	sent := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ordinal) * 47 * time.Second)
	return store.Message{
		ChatID: chatID, ImportID: importID, Ordinal: ordinal, SentAt: sent,
		LocalDate: sent.Format(time.DateOnly), Kind: "text", Body: body, Fingerprint: fp[:],
	}
}

var benchBodies = []string{
	"haan theek hai, kal milte hain phir",
	"Did you reach home safely? Text me",
	"lol that was hilarious 😂😂 send pics",
	"meeting got moved to 4pm, see you there",
}

// BenchmarkInsertMessages100k measures bulk import throughput (target ≥ 50k
// rows/s, NFR-PERF-002) in batches of the importer's default size, and
// reports the on-disk bytes per message including indexes and FTS.
func BenchmarkInsertMessages100k(b *testing.B) {
	const total, batch = 100_000, 2000
	var (
		insertTime  time.Duration
		bytesPerMsg float64
	)
	for b.Loop() {
		b.StopTimer()
		dir := b.TempDir()
		s, err := Open(context.Background(), filepath.Join(dir, "bench.db"))
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			b.Fatal(err)
		}
		ctx, c, im := seed(b, s)
		msgs := make([]store.Message, total)
		for i := range msgs {
			msgs[i] = benchMessage(c.ID, im.ID, int64(i+1), benchBodies[i%len(benchBodies)])
		}
		b.StartTimer()

		start := time.Now()
		for i := 0; i < total; i += batch {
			if _, err := s.InsertMessages(ctx, msgs[i:i+batch]); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.CompleteImport(ctx, im); err != nil {
			b.Fatal(err)
		}
		insertTime += time.Since(start)

		b.StopTimer()
		_, _ = s.w.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
		size := int64(0)
		for _, f := range []string{"bench.db", "bench.db-wal"} {
			if fi, err := os.Stat(filepath.Join(dir, f)); err == nil {
				size += fi.Size()
			}
		}
		bytesPerMsg = float64(size) / total
		_ = s.Close()
		b.StartTimer()
	}
	// Reported from the measured insert time only, averaged over iterations.
	b.ReportMetric(float64(total*b.N)/insertTime.Seconds(), "rows/s")
	b.ReportMetric(bytesPerMsg, "bytes/msg")
}
