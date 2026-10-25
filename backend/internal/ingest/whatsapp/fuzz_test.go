package whatsapp

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// FuzzParse feeds arbitrary bytes through Detect and the full two-pass parse.
// Exports are untrusted (SEC-003), so the parser must never panic, must keep
// bodies within the cap, and must emit only well-formed messages.
//
//	go test -fuzz=FuzzParse -fuzztime=60s ./internal/ingest/whatsapp/
func FuzzParse(f *testing.F) {
	for _, fx := range fixtures {
		f.Add([]byte(fx.text()))
	}
	f.Add([]byte("31/12/2023, 21:41 - : \n[1/1/1, 1:1:1 a.m.] x: y\n"))
	f.Add([]byte("29/02/2023, 10:00 - A: not a leap year\n"))

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p := New()
		d := p.Detect(data)
		if d.Confidence < 0 || d.Confidence > 1 {
			t.Fatalf("confidence out of range: %v", d.Confidence)
		}
		sink := &collectSink{}
		rep, err := p.Parse(context.Background(), ingest.BytesSource{Data: data},
			ingest.Options{Location: ny, BatchSize: 7}, sink)
		if err != nil {
			return // refusing input is fine; panicking is not
		}
		if rep.Messages+rep.SystemLines != len(sink.msgs) {
			t.Fatalf("report counts %d+%d != emitted %d", rep.Messages, rep.SystemLines, len(sink.msgs))
		}
		for _, m := range sink.msgs {
			if m.Kind == "" || m.SentAt.IsZero() || m.SourceLine < 1 {
				t.Fatalf("malformed message %+v", m)
			}
			if len(m.Text) > maxBodyBytes {
				t.Fatalf("body %d bytes exceeds cap", len(m.Text))
			}
			if m.Kind != ingest.KindSystem && m.SenderKey == "" && m.SenderDisplay != "" {
				t.Fatalf("display without key: %+v", m)
			}
			if utf8.ValidString(string(data)) && !utf8.ValidString(m.Text) {
				t.Fatalf("valid input produced invalid UTF-8 text")
			}
		}
	})
}
