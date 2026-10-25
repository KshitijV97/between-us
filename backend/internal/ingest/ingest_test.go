package ingest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCleanText(t *testing.T) {
	tests := map[string]string{
		"\ufeffhello\u200e  ":    "hello",
		"line1\r\nline2":         "line1\nline2",
		"cafe\u0301":             "café", // NFD → NFC
		"9:41\u202fPM":           "9:41 PM",
		"\n\nkeep  inner spaces": "keep  inner spaces",
	}
	for in, want := range tests {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFingerprintStableAcrossTimezonesAndSeconds(t *testing.T) {
	base := Message{
		SenderKey: "alice",
		Kind:      KindText,
		Text:      "see you  at 7\r\n",
		LocalTime: time.Date(2024, 3, 9, 21, 15, 4, 0, time.UTC),
		SentAt:    time.Date(2024, 3, 9, 15, 45, 4, 0, time.UTC),
	}
	other := base
	other.Text = "see you at 7"
	other.LocalTime = time.Date(2024, 3, 9, 21, 15, 0, 0, time.UTC) // Android: no seconds
	other.SentAt = time.Date(2024, 3, 9, 21, 15, 0, 0, time.UTC)    // different chosen tz

	if FingerprintKey(&base) != FingerprintKey(&other) {
		t.Fatalf("keys differ:\n%q\n%q", FingerprintKey(&base), FingerprintKey(&other))
	}
	if Fingerprint(FingerprintKey(&base), 0) == Fingerprint(FingerprintKey(&base), 1) {
		t.Error("dup ordinal must change the fingerprint")
	}
}

func TestFingerprintDistinguishesKinds(t *testing.T) {
	text := Message{SenderKey: "a", Kind: KindText, Text: ""}
	media := Message{SenderKey: "a", Kind: KindMedia, MediaType: MediaImage}
	if FingerprintKey(&text) == FingerprintKey(&media) {
		t.Error("empty text and omitted image must not collide")
	}
}

func TestFingerprintTelegramUsesID(t *testing.T) {
	a := Message{ExternalID: "42", Text: "original"}
	b := Message{ExternalID: "42", Text: "edited later", Edited: true}
	if FingerprintKey(&a) != FingerprintKey(&b) {
		t.Error("Telegram messages with the same ID must match across edits")
	}
}

func TestReportObserve(t *testing.T) {
	var r Report
	t1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	t0 := t1.Add(-time.Hour)
	r.Observe(&Message{SenderKey: "a", SenderDisplay: "Alice", Kind: KindText, SentAt: t1})
	r.Observe(&Message{SenderKey: "a", Kind: KindText, SentAt: t0})
	r.Observe(&Message{Kind: KindSystem, SentAt: t1})
	if r.Messages != 2 || r.SystemLines != 1 {
		t.Errorf("messages=%d system=%d", r.Messages, r.SystemLines)
	}
	if !r.FirstAt.Equal(t0) || !r.LastAt.Equal(t1) {
		t.Errorf("range %v..%v", r.FirstAt, r.LastAt)
	}
	if len(r.Participants) != 1 || r.Participants[0].MessageCount != 2 || r.Participants[0].Display != "Alice" {
		t.Errorf("participants %+v", r.Participants)
	}
	for i := 0; i < MaxSkippedSamples+5; i++ {
		r.AddSkipped(i+1, "unparsed_line")
	}
	if r.Skipped["unparsed_line"] != MaxSkippedSamples+5 || len(r.SkippedSamples) != MaxSkippedSamples {
		t.Errorf("skipped=%v samples=%d", r.Skipped, len(r.SkippedSamples))
	}
}

// fakeParser lets registry tests control confidence.
type fakeParser struct {
	platform Platform
	conf     float64
}

func (f fakeParser) Platform() Platform { return f.platform }
func (f fakeParser) Detect([]byte) Detection {
	return Detection{Platform: f.platform, Confidence: f.conf}
}
func (f fakeParser) Parse(context.Context, Source, Options, Sink) (Report, error) {
	return Report{}, nil
}

func TestDetectPicksMostConfident(t *testing.T) {
	registryMu.Lock()
	saved := registry
	registry = []Parser{fakeParser{"a", 0.7}, fakeParser{"b", 0.9}}
	registryMu.Unlock()
	t.Cleanup(func() { registryMu.Lock(); registry = saved; registryMu.Unlock() })

	p, d, err := Detect([]byte("x"))
	if err != nil || p.Platform() != "b" || d.Confidence != 0.9 {
		t.Fatalf("got %v %+v %v", p, d, err)
	}

	registryMu.Lock()
	registry = []Parser{fakeParser{"a", 0.3}}
	registryMu.Unlock()
	if _, _, err := Detect([]byte("x")); !errors.Is(err, ErrUnrecognised) {
		t.Fatalf("err = %v, want ErrUnrecognised", err)
	}
}
