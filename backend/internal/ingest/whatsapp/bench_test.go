package whatsapp

import (
	"context"
	"testing"
	"time"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

// discardSink counts messages without retaining them, so the benchmark
// measures the parser rather than slice growth.
type discardSink struct{ n int }

func (s *discardSink) Batch(_ context.Context, m []ingest.Message) error { s.n += len(m); return nil }
func (s *discardSink) Diagnostic(ingest.Diagnostic)                      {}

// BenchmarkParse100k parses a synthetic 100k-message export (NFR-PERF-002
// budgets 15 s for parse + store + rollup; parsing should be a small share).
//
//	go test -run=^$ -bench=Parse100k -benchmem ./internal/ingest/whatsapp/
func BenchmarkParse100k(b *testing.B) {
	data := synth(100_000)
	loc, _ := time.LoadLocation("Asia/Kolkata")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink := &discardSink{}
		if _, err := New().Parse(context.Background(), ingest.BytesSource{Data: data},
			ingest.Options{Location: loc}, sink); err != nil {
			b.Fatal(err)
		}
		if sink.n != 100_000 {
			b.Fatalf("parsed %d messages", sink.n)
		}
	}
	b.ReportMetric(float64(100_000*b.N)/b.Elapsed().Seconds(), "msgs/s")
}
