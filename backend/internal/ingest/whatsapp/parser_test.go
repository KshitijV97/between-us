package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

func TestRegistered(t *testing.T) {
	for _, p := range ingest.Parsers() {
		if p.Platform() == ingest.PlatformWhatsApp {
			return
		}
	}
	t.Fatal("whatsapp parser not registered with ingest")
}

func TestDetect(t *testing.T) {
	p := New()
	for _, f := range fixtures {
		// §8 needs three headers; shorter fixtures are refused by design.
		if f.wantErr || len(f.lines) < 3 {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			d := p.Detect([]byte(f.text()))
			if d.Confidence < ingest.MinConfidence {
				t.Errorf("confidence %.2f < %.2f (hints %v)", d.Confidence, ingest.MinConfidence, d.Hints)
			}
			if d.Platform != ingest.PlatformWhatsApp || d.Variant == "" {
				t.Errorf("detection %+v", d)
			}
		})
	}

	negatives := map[string]string{
		"telegram json": `{"name": "Bob", "type": "personal_chat", "messages": [{"id": 1, "date": "2024-01-01T10:00:00"}]}`,
		"prose":         "Dear Bob,\nthanks for the letter.\n12 apples cost 3 euros.\n",
		"csv":           "id,date,text\n1,2024-01-01,hi\n2,2024-01-02,yo\n",
		"empty":         "",
		"two headers":   "31/12/2023, 21:41 - Alice: hi\n31/12/2023, 21:42 - Bob: yo\n",
	}
	for name, in := range negatives {
		if d := p.Detect([]byte(in)); d.Confidence >= ingest.MinConfidence {
			t.Errorf("%s: confidence %.2f, want < %.2f", name, d.Confidence, ingest.MinConfidence)
		}
	}

	// Variants are named after style and clock.
	for in, want := range map[string]string{
		fixtures[0].text(): "android-12h",
		"[31/12/2023, 21:41:05] A: x\n[31/12/2023, 21:42:05] B: y\n[31/12/2023, 21:43:05] A: z\n": "ios-24h",
	} {
		if d := p.Detect([]byte(in)); d.Variant != want {
			t.Errorf("variant %q, want %q", d.Variant, want)
		}
	}
}

func TestDetectDiscountsMixedStyles(t *testing.T) {
	clean := strings.Repeat("31/12/2023, 21:41 - A: x\n", 10)
	mixed := clean + strings.Repeat("[31/12/2023, 9:41:05 PM] A: x\n", 5)
	pc, pm := New().Detect([]byte(clean)), New().Detect([]byte(mixed))
	if !(pm.Confidence < pc.Confidence) {
		t.Errorf("mixed %.2f should score below clean %.2f", pm.Confidence, pc.Confidence)
	}
}

func TestParseRequiresLocation(t *testing.T) {
	_, err := New().Parse(context.Background(), ingest.BytesSource{Data: []byte(fixtures[0].text())},
		ingest.Options{}, &collectSink{})
	if !errors.Is(err, ErrNoLocation) {
		t.Fatalf("err = %v, want ErrNoLocation", err)
	}
}

func TestToInstantNewYorkDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	wall := func(s string) time.Time {
		w, _ := time.Parse("2006-01-02 15:04", s)
		return w
	}
	utc := func(s string) time.Time {
		u, _ := time.Parse(time.RFC3339, s)
		return u
	}
	tests := []struct {
		name  string
		wall  string
		prev  time.Time
		want  string
		wantR dstResult
	}{
		{"normal winter", "2024-01-15 12:00", time.Time{}, "2024-01-15T17:00:00Z", dstNormal},
		{"normal summer", "2024-07-15 12:00", time.Time{}, "2024-07-15T16:00:00Z", dstNormal},
		// 02:30 does not exist on 2024-03-10: EST interpretation = 03:30 EDT.
		{"spring gap", "2024-03-10 02:30", time.Time{}, "2024-03-10T07:30:00Z", dstGap},
		// 01:30 happens twice on 2024-11-03: first (EDT) unless that goes back in time.
		{"fall overlap first", "2024-11-03 01:30", time.Time{}, "2024-11-03T05:30:00Z", dstOverlap},
		{"fall overlap second", "2024-11-03 01:30", utc("2024-11-03T05:50:00Z"), "2024-11-03T06:30:00Z", dstOverlap},
	}
	for _, tt := range tests {
		got, r := toInstant(wall(tt.wall), ny, tt.prev)
		if !got.Equal(utc(tt.want)) || r != tt.wantR {
			t.Errorf("%s: got %s (%d), want %s (%d)", tt.name, got.UTC().Format(time.RFC3339), r, tt.want, tt.wantR)
		}
	}
}

func TestDateOrderDecisions(t *testing.T) {
	mk := func(lines ...string) decision {
		s := newOrderStats()
		for _, l := range lines {
			h, ok := parseHeader(normalizeLine(l))
			if !ok {
				t.Fatalf("not a header: %q", l)
			}
			s.add(h)
		}
		return s.decide()
	}
	tests := []struct {
		name      string
		lines     []string
		want      ingest.DateOrder
		ambiguous bool
	}{
		{"day first hard evidence", []string{"13/01/24, 10:00 - A: x", "01/02/24, 10:00 - A: x"}, ingest.DateOrderDMY, false},
		{"month first hard evidence", []string{"1/13/24, 10:00 AM - A: x", "2/1/24, 10:00 AM - A: x"}, ingest.DateOrderMDY, false},
		{"year first", []string{"2024-01-02, 10:00 - A: x"}, ingest.DateOrderYMD, false},
		{"symmetric day=month", []string{"01/01/24, 10:00 - A: x", "02/02/24, 10:00 - A: x"}, ingest.DateOrderDMY, false},
		{"monotonicity", ambiguousDaily(), ingest.DateOrderDMY, false},
		{"ambiguous lower pm → DMY guess", []string{"03/04/24, 9:00 pm - A: x"}, ingest.DateOrderDMY, true},
		{"ambiguous upper PM → MDY guess", []string{"03/04/24, 9:00 PM - A: x"}, ingest.DateOrderMDY, true},
	}
	for _, tt := range tests {
		d := mk(tt.lines...)
		if d.order != tt.want || d.ambiguous != tt.ambiguous {
			t.Errorf("%s: got %s ambiguous=%v (%s)", tt.name, d.order, d.ambiguous, d.evidence)
		}
	}

	// Both hard signals at once: conflict reported, chronology decides.
	d := mk("13/01/24, 10:00 - A: x", "1/14/24, 10:00 - A: x")
	if !d.conflict {
		t.Errorf("expected conflict, got %+v", d)
	}
}

func TestHeaderParsing(t *testing.T) {
	tests := []struct {
		line   string
		ok     bool
		ios    bool
		hour   int // 24h
		hasSec bool
		rest   string
	}{
		{"12/31/23, 9:41 PM - Alice: hi", true, false, 21, false, "Alice: hi"},
		{"31/12/2023, 21:41 - Bob: hi", true, false, 21, false, "Bob: hi"},
		{"31.12.23, 21.41 - Bob: Finnish dots", true, false, 21, false, "Bob: Finnish dots"},
		{"31/12/2023 21:41 - Alice: no comma", true, false, 21, false, "Alice: no comma"},
		{"12/31/23, 12:05 AM - A: midnight", true, false, 0, false, "A: midnight"},
		{"12/31/23, 12:05 PM - A: noon", true, false, 12, false, "A: noon"},
		{"31/12/23, 9:41 a. m. - A: es", true, false, 9, false, "A: es"},
		{"[31/12/23, 9:41:05 PM] Bob: hi", true, true, 21, true, "Bob: hi"},
		{"\u200e[12/31/23, 9:41:05\u202fPM] Alice: \u200eimage omitted", true, true, 21, true, "Alice: image omitted"},
		{"31/12/2023, 21:41\u00a0- Bob: nbsp", true, false, 21, false, "Bob: nbsp"},
		{"31.12.23, 9:41 nachm. - A: de", true, false, 21, false, "A: de"},
		{"Just text", false, false, 0, false, ""},
		{"2023 was a good year - right?", false, false, 0, false, ""},
		{"[link] https://example.com", false, false, 0, false, ""},
	}
	for _, tt := range tests {
		h, ok := parseHeader(normalizeLine(tt.line))
		if ok != tt.ok {
			t.Errorf("%q: ok=%v", tt.line, ok)
			continue
		}
		if !ok {
			continue
		}
		hour, _, _, cok := h.clock()
		if !cok || hour != tt.hour || h.ios != tt.ios || h.hasSec != tt.hasSec || h.rest != tt.rest {
			t.Errorf("%q: hour=%d ios=%v sec=%v rest=%q", tt.line, hour, h.ios, h.hasSec, h.rest)
		}
	}
	// 13 PM is not a valid 12-hour time.
	h, _ := parseHeader("12/31/23, 13:41 PM - A: x")
	if _, _, _, ok := h.clock(); ok {
		t.Error("13 PM accepted")
	}
}

func TestSenderKey(t *testing.T) {
	for in, want := range map[string]string{
		"\u202a+91 98765 43210\u202c": "+919876543210",
		"+91-98765-43210":             "+919876543210",
		"(555) 010-2345":              "5550102345",
		"Alice":                       "Alice",
		"\u200e🌸 Alice 🌸":             "🌸 Alice 🌸",
	} {
		if got := senderKey(in); got != want {
			t.Errorf("senderKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleFromName(t *testing.T) {
	for in, want := range map[string]string{
		"WhatsApp Chat with Bob.txt":       "Bob",
		"WhatsApp Chat - Chandra.zip":      "Chandra",
		"_chat.txt":                        "",
		"exports/WhatsApp Chat with A.txt": "A",
		"random.txt":                       "",
	} {
		if got := titleFromName(in); got != want {
			t.Errorf("titleFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

// synth builds an Android 24h export with n messages one minute apart.
func synth(n int) []byte {
	var b strings.Builder
	b.Grow(n * 48)
	start := time.Date(2023, 1, 13, 8, 0, 0, 0, time.UTC) // day 13: DMY by hard evidence
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * time.Minute)
		sender := "Alice"
		if i%3 == 0 {
			sender = "Bob"
		}
		fmt.Fprintf(&b, "%s - %s: message number %d, theek hai?\n", ts.Format("02/01/2006, 15:04"), sender, i)
	}
	return []byte(b.String())
}

func TestBatching(t *testing.T) {
	rep, sink, err := parseWith(t, synth(5000), "x.txt", "UTC", ingest.Options{BatchSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sink.batches != 5 || len(sink.msgs) != 5000 || rep.Messages != 5000 {
		t.Errorf("batches=%d msgs=%d report=%d", sink.batches, len(sink.msgs), rep.Messages)
	}
	for i := 1; i < len(sink.msgs); i++ {
		if !sink.msgs[i].SentAt.After(sink.msgs[i-1].SentAt) {
			t.Fatalf("messages out of order at %d", i)
		}
	}
}

// cancelSink cancels the parse after its first batch.
type cancelSink struct {
	collectSink
	cancel context.CancelFunc
}

func (s *cancelSink) Batch(ctx context.Context, m []ingest.Message) error {
	s.cancel()
	return s.collectSink.Batch(ctx, m)
}

func TestCancellationBetweenBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sink := &cancelSink{cancel: cancel}
	_, err := New().Parse(ctx, ingest.BytesSource{Data: synth(3000)},
		ingest.Options{Location: time.UTC, BatchSize: 1000}, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if sink.batches != 1 {
		t.Errorf("batches after cancel = %d, want 1", sink.batches)
	}
}

type failingSink struct{ collectSink }

func (failingSink) Batch(context.Context, []ingest.Message) error { return errors.New("disk full") }

func TestSinkErrorPropagates(t *testing.T) {
	_, err := New().Parse(context.Background(), ingest.BytesSource{Data: synth(10)},
		ingest.Options{Location: time.UTC}, &failingSink{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverlongLineAndBodyCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("13/01/2024, 10:00 - Alice: start\n")
	b.WriteString(strings.Repeat("x", maxLineBytes+10) + "\n") // skipped line
	for i := 0; i < 200; i++ {                                 // ~100 KiB of continuation
		b.WriteString(strings.Repeat("y", 500) + "\n")
	}
	b.WriteString("13/01/2024, 10:01 - Bob: next\n")
	b.WriteString("13/01/2024, 10:02 - Alice: last\n")
	rep, sink, err := parseWith(t, []byte(b.String()), "x.txt", "UTC", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped["line_too_long"] != 1 {
		t.Errorf("skipped = %v", rep.Skipped)
	}
	if len(sink.msgs) != 3 || len(sink.msgs[0].Text) > maxBodyBytes {
		t.Fatalf("msgs=%d len0=%d", len(sink.msgs), len(sink.msgs[0].Text))
	}
	var truncated bool
	for _, d := range sink.diags {
		truncated = truncated || d.Code == "message_truncated"
	}
	if !truncated {
		t.Error("missing message_truncated diagnostic")
	}
}

// TestIncrementalFingerprints checks the parser side of FR-ING-010: a longer
// re-export yields identical fingerprint keys for the shared prefix, and two
// identical "ok" messages in one minute share a key (the importer's duplicate
// ordinal keeps them apart).
func TestIncrementalFingerprints(t *testing.T) {
	v1 := "13/01/2024, 10:00 - Alice: hi\n13/01/2024, 10:01 - Bob: ok\n13/01/2024, 10:01 - Bob: ok\n"
	v2 := v1 + "13/01/2024, 10:05 - Alice: new tail\n"
	// The same chat exported from an iPhone in another timezone: seconds and
	// brackets differ, the minute-level wall clock does not.
	v2ios := "[13/01/2024, 10:00:07] Alice: hi\n[13/01/2024, 10:01:02] Bob: ok\n[13/01/2024, 10:01:40] Bob: ok\n[13/01/2024, 10:05:00] Alice: new tail\n"

	keys := func(data string, tz string) []string {
		_, sink, err := parseWith(t, []byte(data), "x.txt", tz, ingest.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for i := range sink.msgs {
			out = append(out, ingest.FingerprintKey(&sink.msgs[i]))
		}
		return out
	}
	k1, k2, k3 := keys(v1, "UTC"), keys(v2, "UTC"), keys(v2ios, "Asia/Kolkata")
	if len(k2) != 4 || strings.Join(k1, "|") != strings.Join(k2[:3], "|") {
		t.Fatalf("prefix keys differ:\n%v\n%v", k1, k2)
	}
	if strings.Join(k2, "|") != strings.Join(k3, "|") {
		t.Errorf("iOS/timezone re-export keys differ:\n%v\n%v", k2, k3)
	}
	if k1[1] != k1[2] {
		t.Error("repeated messages in one minute should share a key")
	}
}

func TestEmptyAndGarbage(t *testing.T) {
	for _, in := range []string{"", "\n\n", "\x00\x01\x02 binary"} {
		if _, _, err := parseWith(t, []byte(in), "x.txt", "UTC", ingest.Options{}); !errors.Is(err, ErrNoHeaders) {
			t.Errorf("%q: err = %v, want ErrNoHeaders", in, err)
		}
	}
}

func TestClassifyPlaceholders(t *testing.T) {
	tests := []struct {
		body      string
		kind      ingest.Kind
		media     ingest.MediaType
		text      string
		edited    bool
		deleted   bool
		durSec    int
		file      string
		systemTyp string
	}{
		{"hello", ingest.KindText, "", "hello", false, false, 0, "", ""},
		{"my image omitted", ingest.KindText, "", "my image omitted", false, false, 0, "", ""},
		{"\u200eimage omitted", ingest.KindMedia, ingest.MediaImage, "", false, false, 0, "", ""},
		{"<Media omitted> <This message was edited>", ingest.KindMedia, ingest.MediaUnknown, "", true, false, 0, "", ""},
		{"This message was deleted.", ingest.KindDeleted, "", "", false, true, 0, "", ""},
		{"Voice call, 45 sec", ingest.KindCall, "", "", false, false, 45, "", "phone_call"},
		{"Missed video call", ingest.KindCall, "", "", false, false, 0, "", "phone_call"},
		{"photo.HEIC (file attached)\ncaption here", ingest.KindMedia, ingest.MediaImage, "caption here", false, false, 0, "photo.HEIC", ""},
		{"song.mp3 (file attached)", ingest.KindMedia, ingest.MediaAudio, "", false, false, 0, "song.mp3", ""},
		{"POLL:\nWhere?\nOPTION: A (1 vote)", ingest.KindPoll, "", "Where?", false, false, 0, "", ""},
		{"location: https://maps.google.com/?q=1,2", ingest.KindLocation, "", "", false, false, 0, "", ""},
	}
	for _, tt := range tests {
		var m ingest.Message
		classify(&m, tt.body)
		file := ""
		if m.Media != nil {
			file = m.Media.FileName
		}
		dur := 0
		if m.Call != nil {
			dur = m.Call.DurationSec
		}
		if m.Kind != tt.kind || m.MediaType != tt.media || m.Text != tt.text || m.Edited != tt.edited ||
			m.Deleted != tt.deleted || dur != tt.durSec || file != tt.file || m.SystemType != tt.systemTyp {
			t.Errorf("classify(%q) = %+v (media %+v, call %+v)", tt.body, m, m.Media, m.Call)
		}
	}
}
