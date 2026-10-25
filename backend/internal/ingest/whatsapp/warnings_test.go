package whatsapp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// countingSink counts emitted messages.
type countingSink struct{ n int }

func (c *countingSink) Batch(_ context.Context, msgs []ingest.Message) error {
	c.n += len(msgs)
	return nil
}
func (*countingSink) Diagnostic(ingest.Diagnostic) {}

// A header-shaped line whose date is out of range must not vanish silently
// into the previous message: the report has to say so.
func TestSuspiciousHeadersAreReported(t *testing.T) {
	src := ingest.BytesSource{Data: []byte(
		"01/02/2024, 10:00 - Alice: hi\n" +
			"01/02/2024, 10:01 - Bob: hello\n" +
			"01/02/2150, 10:02 - Alice: from the future\n" +
			"01/02/2024, 10:03 - Bob: ok\n"), FileName: "chat.txt"}
	var sink countingSink
	rep, err := New().Parse(context.Background(), src, ingest.Options{Location: time.UTC, DateOrder: ingest.DateOrderDMY}, &sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.n != 3 {
		t.Errorf("emitted %d messages, want 3", sink.n)
	}
	var found bool
	for _, w := range rep.Warnings {
		if strings.HasPrefix(w, "1 lines look like message headers") {
			found = true
		}
	}
	if !found {
		t.Errorf("no suspicious-header warning in %q", rep.Warnings)
	}
}
