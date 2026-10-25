package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

// update rewrites golden files: go test ./internal/ingest/telegram -update
var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

// collectSink records everything a parse emits.
type collectSink struct {
	batches     int
	msgs        []ingest.Message
	diagnostics []ingest.Diagnostic
}

func (s *collectSink) Batch(_ context.Context, msgs []ingest.Message) error {
	s.batches++
	s.msgs = append(s.msgs, msgs...)
	return nil
}

func (s *collectSink) Diagnostic(d ingest.Diagnostic) { s.diagnostics = append(s.diagnostics, d) }

// golden is the serialised outcome of parsing one fixture.
type golden struct {
	Error       string              `json:"error,omitempty"`
	Report      ingest.Report       `json:"report"`
	Diagnostics []ingest.Diagnostic `json:"diagnostics,omitempty"`
	Messages    []ingest.Message    `json:"messages"`
}

func parseFile(t *testing.T, name string, opts ingest.Options) (ingest.Report, *collectSink, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	sink := &collectSink{}
	rep, err := Parser{}.Parse(context.Background(), ingest.BytesSource{Data: data, FileName: name}, opts, sink)
	return rep, sink, err
}

func TestGolden(t *testing.T) {
	cases := []struct {
		golden, fixture string
		opts            ingest.Options
	}{
		{"personal_basic", "personal_basic.json", ingest.Options{}},
		{"entities_array", "entities_array.json", ingest.Options{}},
		{"edited_replies", "edited_replies.json", ingest.Options{}},
		{"media_all_types", "media_all_types.json", ingest.Options{}},
		{"calls_service", "calls_service.json", ingest.Options{}},
		{"forwarded", "forwarded.json", ingest.Options{}},
		{"reactions", "reactions.json", ingest.Options{}},
		{"deleted_account", "deleted_account.json", ingest.Options{}},
		{"poll_location_contact", "poll_location_contact.json", ingest.Options{}},
		{"group_supergroup", "group_supergroup.json", ingest.Options{}},
		{"full_account_unselected", "full_account_export.json", ingest.Options{}},
		{"full_account_bob", "full_account_export.json", ingest.Options{SelectedChatID: "4242424242"}},
		{"full_account_carol", "full_account_export.json", ingest.Options{SelectedChatID: "3333333333"}},
		{"clock_mismatch", "clock_mismatch.json", ingest.Options{}},
		{"truncated", "truncated.json", ingest.Options{}},
		{"incremental_v1", "incremental_v1.json", ingest.Options{}},
		{"incremental_v2", "incremental_v2.json", ingest.Options{}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			rep, sink, err := parseFile(t, tc.fixture, tc.opts)
			got := golden{Report: rep, Diagnostics: sink.diagnostics, Messages: sink.msgs}
			if err != nil {
				got.Error = err.Error()
			}
			if got.Messages == nil {
				got.Messages = []ingest.Message{}
			}
			out, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, '\n')
			path := filepath.Join("testdata", tc.golden+".golden.json")
			if *update {
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if !bytes.Equal(out, want) {
				t.Errorf("output differs from %s (run with -update and review the diff)\n%s", path, out)
			}
		})
	}
}

func TestBasicMapping(t *testing.T) {
	rep, sink, err := parseFile(t, "personal_basic.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Variant != VariantSingle || rep.SuggestedTitle != "Bob" || rep.TimePrecision != ingest.PrecisionSecond {
		t.Errorf("report header %+v", rep)
	}
	if len(sink.msgs) != 3 || rep.Messages != 3 || len(rep.Participants) != 2 {
		t.Fatalf("msgs=%d report=%+v", len(sink.msgs), rep)
	}
	m := sink.msgs[0]
	// date_unixtime is authoritative; "date" only feeds LocalTime.
	if m.SentAt.Unix() != 1704490865 || m.SentAt.Location().String() != "UTC" {
		t.Errorf("SentAt = %v", m.SentAt)
	}
	if m.LocalTime.Format("2006-01-02T15:04:05") != "2024-01-05T21:41:05" {
		t.Errorf("LocalTime = %v", m.LocalTime)
	}
	if m.ExternalID != "1" || m.SenderKey != "user1111111111" || m.SenderDisplay != "Alice" || m.Kind != ingest.KindText {
		t.Errorf("message %+v", m)
	}
	if sink.msgs[1].Text != "You too\nsee you tomorrow" {
		t.Errorf("multi-line text %q", sink.msgs[1].Text)
	}
}

func TestEntitiesFlattened(t *testing.T) {
	_, sink, err := parseFile(t, "entities_array.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Look at https://example.org and this @alice 🎉 x",
		"only array no entities", // text array without text_entities
		"fmt.Println()",          // entities preferred over empty text
	}
	for i, w := range want {
		if sink.msgs[i].Text != w {
			t.Errorf("msg %d text %q, want %q", i, sink.msgs[i].Text, w)
		}
	}
}

func TestEditsAndReplies(t *testing.T) {
	_, sink, err := parseFile(t, "edited_replies.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := sink.msgs[1]
	if !m.Edited || m.EditedAt == nil || m.EditedAt.Unix() != 1709283960 || m.ReplyToExternalID != "20" {
		t.Errorf("edited/reply %+v", m)
	}
	// Replies to messages outside the export are kept unresolved.
	if sink.msgs[2].ReplyToExternalID != "7" {
		t.Errorf("dangling reply %q", sink.msgs[2].ReplyToExternalID)
	}
}

func TestMediaTypes(t *testing.T) {
	_, sink, err := parseFile(t, "media_all_types.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		mt      ingest.MediaType
		omitted bool
		text    string
	}{
		{ingest.MediaImage, false, "our view"},
		{ingest.MediaImage, true, ""},
		{ingest.MediaVoiceNote, true, ""},
		{ingest.MediaVideoNote, false, ""},
		{ingest.MediaVideo, false, "watch"},
		{ingest.MediaSticker, false, ""},
		{ingest.MediaGIF, false, ""},
		{ingest.MediaAudio, false, ""},
		{ingest.MediaDocument, false, "the plan"},
		{ingest.MediaUnknown, true, ""},
		{ingest.MediaImage, true, ""},
	}
	if len(sink.msgs) != len(want) {
		t.Fatalf("got %d messages", len(sink.msgs))
	}
	for i, w := range want {
		m := sink.msgs[i]
		if m.Kind != ingest.KindMedia || m.MediaType != w.mt || m.Media == nil || m.Media.Omitted != w.omitted || m.Text != w.text {
			t.Errorf("msg %d: kind=%s type=%s media=%+v text=%q, want %+v", i, m.Kind, m.MediaType, m.Media, m.Text, w)
		}
	}
	if sink.msgs[0].Media.SizeBytes != 123456 || sink.msgs[0].Media.FileName != "photo_1@01-04-2024_12-00-00.jpg" {
		t.Errorf("photo meta %+v", sink.msgs[0].Media)
	}
	if sink.msgs[2].Media.DurationSec != 14 || sink.msgs[2].Media.MIMEType != "audio/ogg" {
		t.Errorf("voice meta %+v", sink.msgs[2].Media)
	}
	if sink.msgs[4].Media.SizeBytes != 999999 || sink.msgs[4].Media.FileName != "clip.mp4" {
		t.Errorf("video meta %+v", sink.msgs[4].Media)
	}
}

func TestCallsAndService(t *testing.T) {
	_, sink, err := parseFile(t, "calls_service.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		missed bool
		dur    int
		sender string
	}{{false, 312, "user1111111111"}, {true, 0, "user2222222222"}, {true, 0, "user2222222222"}, {false, 5, "user1111111111"}}
	for i, c := range calls {
		m := sink.msgs[i]
		if m.Kind != ingest.KindCall || m.Call == nil || m.Call.Missed != c.missed || m.Call.DurationSec != c.dur || m.SenderKey != c.sender {
			t.Errorf("call %d = %+v call=%+v", i, m, m.Call)
		}
	}
	for i, typ := range []string{"pin_message", "clear_history", "screenshot_taken"} {
		m := sink.msgs[4+i]
		// System rows are sender-less and keep no text (it names people).
		if m.Kind != ingest.KindSystem || m.SystemType != typ || m.SenderKey != "" || m.Text != "" {
			t.Errorf("system %d = %+v", i, m)
		}
	}
}

func TestForwardedReactionsDeletedAccount(t *testing.T) {
	_, sink, err := parseFile(t, "forwarded.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, true, false} {
		if sink.msgs[i].Forwarded != want {
			t.Errorf("forwarded[%d] = %v", i, sink.msgs[i].Forwarded)
		}
	}

	_, sink, err = parseFile(t, "reactions.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := sink.msgs[0].Reactions
	if len(r) != 3 || r[0] != (ingest.Reaction{Emoji: "❤", Count: 1}) ||
		r[1] != (ingest.Reaction{Emoji: "custom_emoji:5368324170671202286", Count: 2}) || r[2] != (ingest.Reaction{Emoji: "paid", Count: 3}) {
		t.Errorf("reactions %+v", r)
	}

	rep, sink, err := parseFile(t, "deleted_account.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sink.msgs[0].SenderKey != "user2222222222" || sink.msgs[0].SenderDisplay != deletedAccountName {
		t.Errorf("deleted account %+v", sink.msgs[0])
	}
	if rep.SuggestedTitle != "" {
		t.Errorf("title %q", rep.SuggestedTitle)
	}
}

func TestPollLocationContactMinimised(t *testing.T) {
	_, sink, err := parseFile(t, "poll_location_contact.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := []ingest.Kind{ingest.KindPoll, ingest.KindLocation, ingest.KindLocation, ingest.KindContact, ingest.KindSystem}
	for i, k := range kinds {
		if sink.msgs[i].Kind != k {
			t.Errorf("msg %d kind %s, want %s", i, sink.msgs[i].Kind, k)
		}
	}
	if sink.msgs[0].Text != "Beach or hills?" || sink.msgs[3].Text != "Carol Example" {
		t.Errorf("poll/contact text %q %q", sink.msgs[0].Text, sink.msgs[3].Text)
	}
	// No coordinates, place names, addresses or phone numbers survive.
	all, _ := json.Marshal(sink.msgs)
	for _, leak := range []string{"12.97", "Cafe Example", "Example Road", "+1555"} {
		if bytes.Contains(all, []byte(leak)) {
			t.Errorf("minimised data leaked: %q", leak)
		}
	}
	if sink.msgs[4].SystemType != "unsupported" || sink.msgs[4].SenderKey != "" {
		t.Errorf("empty message %+v", sink.msgs[4])
	}
}

func TestGroupRefused(t *testing.T) {
	_, sink, err := parseFile(t, "group_supergroup.json", ingest.Options{})
	var cte *ChatTypeError
	if !errors.Is(err, ErrGroupChat) || !errors.As(err, &cte) || cte.Type != "private_supergroup" {
		t.Fatalf("err = %v", err)
	}
	if len(sink.msgs) != 0 {
		t.Error("group messages must not be emitted")
	}
	if !errors.Is(checkType("public_channel"), ErrUnsupportedChatType) || !errors.Is(checkType("bot_chat"), ErrUnsupportedChatType) {
		t.Error("channels/bots must be unsupported")
	}
}

func TestFullAccountExport(t *testing.T) {
	_, sink, err := parseFile(t, "full_account_export.json", ingest.Options{})
	var sel *ChatSelectionError
	if !errors.As(err, &sel) || !errors.Is(err, ErrChatSelectionRequired) {
		t.Fatalf("err = %v", err)
	}
	if len(sel.Chats) != 2 || sel.Chats[0] != (ChatSummary{ID: "4242424242", Name: "Bob", MessageCount: 2}) ||
		sel.Chats[1] != (ChatSummary{ID: "3333333333", Name: "Carol", MessageCount: 3}) {
		t.Errorf("chats %+v", sel.Chats)
	}
	if strings.Contains(err.Error(), "Bob") {
		t.Error("error text must not contain chat names")
	}
	if len(sink.msgs) != 0 {
		t.Error("nothing may be emitted without a selection")
	}

	rep, sink, err := parseFile(t, "full_account_export.json", ingest.Options{SelectedChatID: "4242424242"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Variant != VariantAccount || rep.SuggestedTitle != "Bob" || len(sink.msgs) != 2 {
		t.Fatalf("report %+v msgs %d", rep, len(sink.msgs))
	}
	all, _ := json.Marshal(sink.msgs)
	if bytes.Contains(all, []byte("SECRET")) {
		t.Error("other chats leaked into the selected chat")
	}

	if _, _, err := parseFile(t, "full_account_export.json", ingest.Options{SelectedChatID: "999"}); !errors.Is(err, ErrChatNotFound) {
		t.Errorf("unknown selection err = %v", err)
	}
	if _, _, err := parseFile(t, "full_account_export.json", ingest.Options{SelectedChatID: "1700000000"}); !errors.Is(err, ErrGroupChat) {
		t.Errorf("group selection err = %v", err)
	}
}

func TestClockMismatchAndTimezoneSuggestion(t *testing.T) {
	rep, sink, err := parseFile(t, "clock_mismatch.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var mismatch *ingest.Diagnostic
	for i := range sink.diagnostics {
		if sink.diagnostics[i].Code == "clock_mismatch" {
			mismatch = &sink.diagnostics[i]
		}
	}
	if mismatch == nil || mismatch.Line != 3 {
		t.Errorf("diagnostics %+v", sink.diagnostics)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "UTC+05:30") {
		t.Errorf("warnings %v", rep.Warnings)
	}
}

func TestTruncatedJSON(t *testing.T) {
	rep, sink, err := parseFile(t, "truncated.json", ingest.Options{})
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("err = %v", err)
	}
	// The complete message is handed over so a partial import is possible.
	if len(sink.msgs) != 1 || rep.Messages != 1 || sink.msgs[0].Text != "complete" {
		t.Errorf("msgs=%d report=%+v", len(sink.msgs), rep)
	}
	if len(sink.diagnostics) == 0 || sink.diagnostics[0].Code != "bad_message_json" {
		t.Errorf("diagnostics %+v", sink.diagnostics)
	}
}

func TestIncrementalReimportMatchesByID(t *testing.T) {
	_, v1, err := parseFile(t, "incremental_v1.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, v2, err := parseFile(t, "incremental_v2.json", ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for i := range v2.msgs {
		keys[ingest.FingerprintKey(&v2.msgs[i])] = true
	}
	for i := range v1.msgs {
		if !keys[ingest.FingerprintKey(&v1.msgs[i])] {
			t.Errorf("v1 message %s (edited later) does not match v2", v1.msgs[i].ExternalID)
		}
	}
}

func TestNotATelegramExport(t *testing.T) {
	for _, in := range []string{`{"foo": 1}`, `[1,2,3]`, `12/31/23, 9:41 PM - Alice: hi`} {
		_, err := Parser{}.Parse(context.Background(), ingest.BytesSource{Data: []byte(in)}, ingest.Options{}, &collectSink{})
		if err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestBatchingAndCancellation(t *testing.T) {
	data := generate(10)
	sink := &collectSink{}
	if _, err := (Parser{}).Parse(context.Background(), ingest.BytesSource{Data: data}, ingest.Options{BatchSize: 3}, sink); err != nil {
		t.Fatal(err)
	}
	if sink.batches != 4 || len(sink.msgs) != 10 {
		t.Errorf("batches=%d msgs=%d", sink.batches, len(sink.msgs))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Parser{}).Parse(ctx, ingest.BytesSource{Data: data}, ingest.Options{BatchSize: 3}, &collectSink{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled parse err = %v", err)
	}
}

func TestOutOfOrderSortedWithinBatch(t *testing.T) {
	in := `{"type":"personal_chat","messages":[
	 {"id":2,"type":"message","date_unixtime":"200","from":"A","from_id":"u1","text":"b"},
	 {"id":1,"type":"message","date_unixtime":"100","from":"A","from_id":"u1","text":"a"}]}`
	sink := &collectSink{}
	if _, err := (Parser{}).Parse(context.Background(), ingest.BytesSource{Data: []byte(in)}, ingest.Options{}, sink); err != nil {
		t.Fatal(err)
	}
	if sink.msgs[0].ExternalID != "1" || sink.diagnostics[0].Code != "out_of_order" {
		t.Errorf("msgs %v diags %+v", sink.msgs, sink.diagnostics)
	}
}

func TestDetect(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bigContacts := `{"about":"x","personal_information":{"first_name":"A"},"contacts":{"list":[` +
		strings.Repeat(`{"first_name":"Someone","phone_number":"+10000000000"},`, 3000)
	cases := []struct {
		name    string
		sample  []byte
		conf    float64
		variant string
		hint    bool
	}{
		{"personal", read("personal_basic.json"), 1, VariantSingle, false},
		{"bom", append([]byte{0xEF, 0xBB, 0xBF}, read("personal_basic.json")...), 1, VariantSingle, false},
		{"account", read("full_account_export.json"), 1, VariantAccount, false},
		{"account beyond sample", []byte(bigContacts)[:ingest.SampleSize], 0.8, VariantAccount, false},
		{"group", read("group_supergroup.json"), 1, VariantSingle, true},
		{"truncated sample", read("personal_basic.json")[:200], 1, VariantSingle, false},
		{"other json", []byte(`{"hello":"world"}`), 0.1, VariantSingle, true},
		{"whatsapp android", []byte("12/31/23, 9:41 PM - Alice: hi\n"), 0, VariantSingle, false},
		{"whatsapp ios", []byte("[31/12/23, 21:41:05] Alice: hi\n"), 0, VariantSingle, false},
		{"empty", nil, 0, VariantSingle, false},
	}
	for _, tc := range cases {
		d := Parser{}.Detect(tc.sample)
		if d.Confidence != tc.conf || d.Variant != tc.variant || (len(d.Hints) > 0) != tc.hint || d.Platform != ingest.PlatformTelegram {
			t.Errorf("%s: %+v", tc.name, d)
		}
	}
	p, d, err := ingest.Detect(read("personal_basic.json"))
	if err != nil || p.Platform() != ingest.PlatformTelegram || d.Confidence != 1 {
		t.Errorf("registry detect: %v %+v %v", p, d, err)
	}
}

// generate builds a synthetic single-chat export with n messages.
func generate(n int) []byte {
	var b bytes.Buffer
	g := newGenReader(n)
	_, _ = io.Copy(&b, g)
	return b.Bytes()
}

// genReader produces a synthetic export on the fly so memory tests never hold
// the whole file.
type genReader struct {
	n, i int
	buf  bytes.Buffer
	done bool
}

func newGenReader(n int) *genReader {
	g := &genReader{n: n}
	g.buf.WriteString(`{"name":"Bob","type":"personal_chat","id":1,"messages":[`)
	return g
}

func (g *genReader) Read(p []byte) (int, error) {
	for g.buf.Len() < len(p) && !g.done {
		if g.i == g.n {
			g.buf.WriteString("]}")
			g.done = true
			break
		}
		if g.i > 0 {
			g.buf.WriteByte(',')
		}
		sender, name := "user1111111111", "Alice"
		if g.i%3 == 0 {
			sender, name = "user2222222222", "Bob"
		}
		ts := 1704067200 + int64(g.i)*37
		fmt.Fprintf(&g.buf, `{"id":%d,"type":"message","date":"2024-01-01T00:00:00","date_unixtime":"%d","from":"%s","from_id":"%s",`+
			`"text":["message number %d with a ",{"type":"link","text":"https://example.org/%d"}," and some more words to look realistic"],`+
			`"text_entities":[{"type":"plain","text":"message number %d with a "},{"type":"link","text":"https://example.org/%d"},{"type":"plain","text":" and some more words to look realistic"}]}`,
			g.i+1, ts, name, sender, g.i, g.i, g.i, g.i)
		g.i++
	}
	if g.buf.Len() == 0 && g.done {
		return 0, io.EOF
	}
	return g.buf.Read(p)
}

// genSource is a Source over a generated export.
type genSource struct{ n int }

func (s genSource) Open() (io.ReadCloser, error) { return io.NopCloser(newGenReader(s.n)), nil }
func (genSource) Name() string                   { return "generated.json" }
func (genSource) Size() int64                    { return -1 }

// countingSink counts messages and samples the heap without retaining batches.
type countingSink struct {
	n        int
	peakHeap uint64
}

func (s *countingSink) Batch(_ context.Context, msgs []ingest.Message) error {
	s.n += len(msgs)
	if s.n%20000 < len(msgs) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		s.peakHeap = max(s.peakHeap, ms.HeapAlloc)
	}
	return nil
}
func (s *countingSink) Diagnostic(ingest.Diagnostic) {}

// TestBoundedMemory streams 300k messages (~100 MB of JSON) and checks the
// heap stays far below the input size (FR-ING-013).
func TestBoundedMemory(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("large stream; skipped in -short and -race runs")
	}
	const n = 300_000
	sink := &countingSink{}
	runtime.GC()
	rep, err := Parser{}.Parse(context.Background(), genSource{n: n}, ingest.Options{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.n != n || rep.Messages != n {
		t.Fatalf("parsed %d/%d", sink.n, rep.Messages)
	}
	const limit = 64 << 20
	t.Logf("peak heap %.1f MiB for %d messages", float64(sink.peakHeap)/(1<<20), n)
	if sink.peakHeap > limit {
		t.Errorf("peak heap %d bytes exceeds %d", sink.peakHeap, limit)
	}
}

func BenchmarkParse100k(b *testing.B) {
	data := generate(100_000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		sink := &countingSink{}
		if _, err := (Parser{}).Parse(context.Background(), ingest.BytesSource{Data: data}, ingest.Options{}, sink); err != nil {
			b.Fatal(err)
		}
	}
}

// FuzzDecode checks the parser never panics or emits invalid messages on
// arbitrary input. Run: go test ./internal/ingest/telegram -fuzz FuzzDecode
func FuzzDecode(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, name := range files {
		if strings.HasSuffix(name, ".golden.json") {
			continue
		}
		if b, err := os.ReadFile(name); err == nil {
			f.Add(b, "")
		}
	}
	f.Add([]byte(`{"chats":{"list":[{"id":1,"type":"personal_chat","messages":[{}]}]}}`), "1")
	f.Add([]byte(`{"messages":[{"text":[{"text":1}]}]}`), "")
	f.Fuzz(func(t *testing.T, data []byte, selected string) {
		_ = Parser{}.Detect(data)
		sink := &collectSink{}
		rep, err := Parser{}.Parse(context.Background(), ingest.BytesSource{Data: data},
			ingest.Options{BatchSize: 7, SelectedChatID: selected}, sink)
		if rep.Messages+rep.SystemLines != len(sink.msgs) {
			t.Fatalf("report counts %d+%d != emitted %d", rep.Messages, rep.SystemLines, len(sink.msgs))
		}
		for _, m := range sink.msgs {
			if m.SentAt.IsZero() || m.Kind == "" || m.Platform != ingest.PlatformTelegram {
				t.Fatalf("invalid message %+v", m)
			}
			if (m.Kind == ingest.KindSystem) != (m.SenderKey == "") {
				t.Fatalf("sender/kind mismatch %+v", m)
			}
			if len(m.Text) > maxTextBytes {
				t.Fatalf("text not capped")
			}
		}
		_ = err
	})
}
