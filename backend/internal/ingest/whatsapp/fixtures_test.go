package whatsapp

// Golden-file tests over the fixture matrix of docs/parsers/whatsapp.md §11.
//
// Fixtures are 100% synthetic and defined here as Go strings so invisible
// characters (U+200E, U+202F, BOM) are explicit escapes. Running
//
//	go test ./internal/ingest/whatsapp/ -update
//
// rewrites testdata/<name>.txt from these definitions and testdata/<name>.golden.json
// from the parser's current output. Without -update the test fails if either
// file drifts, so every behaviour change shows up as a reviewable golden diff.
// Zip-based fixtures belong to the archive package (FR-ING-005).

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

var update = flag.Bool("update", false, "rewrite testdata fixtures and golden files")

type fixture struct {
	name  string // file name without extension
	tz    string
	lines []string
	// order overrides inference (simulates the user's answer), if set.
	order ingest.DateOrder
	// wantErr: the parse must fail (golden still records the report).
	wantErr bool
}

func (f fixture) text() string { return strings.Join(f.lines, "\n") + "\n" }

const enc = "Messages and calls are end-to-end encrypted. No one outside of this chat, not even WhatsApp, can read or listen to them. Tap to learn more."

var fixtures = []fixture{
	{name: "android_en_us_12h", tz: "America/New_York", lines: []string{
		"12/30/23, 9:40 PM - " + enc,
		"12/30/23, 9:41 PM - Alice: Happy almost new year!",
		"12/30/23, 9:43 PM - Bob: You too 🎉",
		"12/31/23, 12:05 AM - Alice: Midnight snack?",
		"12/31/23, 12:30 PM - Bob: Lunch at half past twelve",
		"1/2/24, 8:15 AM - Alice: Back to work",
	}},
	{name: "android_en_gb_24h", tz: "Europe/London", lines: []string{
		"31/12/2023, 21:40 - " + enc,
		"31/12/2023, 21:41 - Bob: Happy new year!",
		"31/12/2023, 21:42 - Alice: You too!",
		"01/01/2024, 00:01 - Bob: It's 2024 🥳",
		"13/01/2024, 09:30 - Alice: Coffee on Saturday?",
	}},
	{name: "android_de_dots", tz: "Europe/Berlin", lines: []string{
		"31.12.23, 21:41 - Alice: Frohes neues!",
		"31.12.23, 21:42 - Bob: <Medien ausgeschlossen>",
		"31.12.23, 21:43 - Alice: Danke 😊",
		"14.01.24, 10:00 - Bob: Bis bald",
	}},
	{name: "android_iso", tz: "Asia/Tokyo", lines: []string{
		"2023-12-31, 21:41 - Bob: Hi",
		"2023-12-31, 21:42 - Alice: Hello",
		"2024-01-02, 08:00 - Bob: Morning",
	}},
	{name: "android_no_comma", tz: "Asia/Kolkata", lines: []string{
		"31/12/2023 21:41 - Alice: Hi",
		"31/12/2023 21:42 - Bob: Kal milte hain yaar 😊",
		"31/12/2023 21:45 - Alice: haan pakka",
	}},
	{name: "android_202f", tz: "America/Los_Angeles", lines: []string{
		"12/31/23, 9:41\u202fPM - Alice: Hi",
		"12/31/23, 9:42\u202fPM - Bob: Hey",
		"12/31/23, 11:59\u202fPM - Alice: Almost midnight",
		"1/13/24, 7:05\u202fAM - Bob: Good morning",
	}},
	{name: "android_lower_dotted_meridiem", tz: "Asia/Kolkata", lines: []string{
		"31/12/23, 9:41 p.m. - Alice: Hi",
		"31/12/23, 9:42 p.m. - Bob: Hello ji",
		"13/01/24, 7:05 a.m. - Alice: Good morning",
	}},
	{name: "ios_en_seconds", tz: "Europe/London", lines: []string{
		"[31/12/23, 9:40:00 PM] Bob: " + enc,
		"[31/12/23, 9:41:05 PM] Bob: Hi",
		"[31/12/23, 9:41:30 PM] Alice: image omitted",
		"[31/12/23, 9:42:10 PM] Alice: Look at this view",
		"[13/01/24, 8:00:00 AM] Bob: video omitted",
	}},
	{name: "ios_17_marks", tz: "America/Chicago", lines: []string{
		"\u200e[12/31/23, 9:41:05\u202fPM] Alice: \u200eimage omitted",
		"\u200e[12/31/23, 9:41:10\u202fPM] Bob: \u200esticker omitted",
		"\u200e[12/31/23, 9:41:20\u202fPM] Alice: \u200eGIF omitted",
		"\u200e[12/31/23, 9:42:00\u202fPM] Bob: \u200eReport.pdf \u2022 3 pages \u200edocument omitted",
		"\u200e[12/31/23, 9:43:00\u202fPM] Alice: \u200eContact card omitted",
		"\u200e[12/31/23, 9:44:00\u202fPM] Bob: \u200eaudio omitted",
		"[1/13/24, 10:00:00\u202fAM] Alice: All good",
	}},
	{name: "android_with_media", tz: "Asia/Kolkata", lines: []string{
		"31/12/2023, 21:41 - Alice: IMG-20231231-WA0001.jpg (file attached)",
		"nice pic na?",
		"31/12/2023, 21:42 - Bob: PTT-20231231-WA0002.opus (file attached)",
		"31/12/2023, 21:43 - Alice: STK-20231231-WA0003.webp (file attached)",
		"31/12/2023, 21:44 - Bob: Chandra.vcf (file attached)",
		"31/12/2023, 21:45 - Alice: VID-20231231-WA0004.mp4 (file attached)",
		"13/01/2024, 09:00 - Bob: DOC-20240113-WA0005.pdf (file attached)",
	}},
	{name: "ios_with_media", tz: "Europe/London", lines: []string{
		"[31/12/2023, 21:41:05] Alice: <attached: 00000012-PHOTO-2023-12-31-21-41-05.jpg>",
		"[31/12/2023, 21:42:00] Bob: <attached: 00000013-AUDIO-2023-12-31-21-42-00.opus>",
		"[31/12/2023, 21:43:00] Alice: <attached: 00000014-VIDEO-2023-12-31-21-43-00.mp4>",
		"[31/12/2023, 21:44:00] Bob: <attached: 00000015-STICKER-2023-12-31-21-44-00.webp>",
		"[13/01/2024, 09:00:00] Alice: <attached: 00000016-GIF-2024-01-13-09-00-00.mp4>",
	}},
	{name: "ambiguous_dates", tz: "Asia/Kolkata", lines: ambiguousDaily()},
	{name: "ambiguous_needs_user", tz: "America/New_York", lines: []string{
		"03/04/24, 9:00 AM - Alice: Morning",
		"03/04/24, 9:05 AM - Bob: Morning!",
		"03/04/24, 6:30 PM - Alice: Dinner?",
	}},
	{name: "ambiguous_confirmed_by_user", tz: "America/New_York", order: ingest.DateOrderDMY, lines: []string{
		"03/04/24, 9:00 AM - Alice: Morning",
		"03/04/24, 9:05 AM - Bob: Morning!",
	}},
	{name: "multiline", tz: "Europe/London", lines: []string{
		"stray text before the first header",
		"31/12/2023, 21:41 - Alice: Shopping list:",
		"- milk",
		"",
		"- eggs",
		"31/12/2023, 21:42 - Bob: Quote from the old chat:",
		"99/99/2023, 25:61 - not a real header",
		"done",
		"13/01/2024, 10:00 - Alice: ok",
	}},
	{name: "names_with_colon", tz: "Europe/London", lines: []string{
		"31/12/2023, 21:41 - Dr: Alice: see you at 7",
		"31/12/2023, 21:42 - Bob: Note: buy milk",
		"31/12/2023, 21:43 - Dr: Alice: sure",
		"31/12/2023, 21:44 - Bob: ok",
		"31/12/2023, 21:45 - Dr: Alice: Reminder: bring the book",
		"13/01/2024, 10:00 - Bob: thanks",
	}},
	{name: "phone_sender", tz: "Asia/Kolkata", lines: []string{
		"12/31/23, 9:41 PM - \u202a+91 98765 43210\u202c: hi",
		"12/31/23, 9:42 PM - Bob: who is this?",
		"12/31/23, 9:43 PM - \u202a+91 98765 43210\u202c: Chandra here",
		"1/13/24, 9:44 AM - \u202a+91 98765 43210\u202c: save my number",
	}},
	{name: "group_three", tz: "Europe/London", lines: []string{
		"31/12/2023, 21:40 - Alice created group \"Trip\"",
		"31/12/2023, 21:41 - Alice: Hi all",
		"31/12/2023, 21:42 - Bob: Hey",
		"31/12/2023, 21:43 - Chandra: Namaste",
		"13/01/2024, 10:00 - Alice: Plan?",
	}},
	{name: "bom_crlf", tz: "Europe/London", lines: []string{
		"\ufeff31/12/2023, 21:41 - Bob: hi\r",
		"31/12/2023, 21:42 - Alice: two\r",
		"lines\r",
		"13/01/2024, 10:00 - Bob: bye\r",
	}},
	{name: "deleted_edited", tz: "America/New_York", lines: []string{
		"12/31/23, 9:41 PM - Alice: This message was deleted",
		"12/31/23, 9:42 PM - Bob: You deleted this message",
		"12/31/23, 9:43 PM - Alice: see you at 7 <This message was edited>",
		"12/31/23, 9:44 PM - Bob: <Media omitted>",
		"1/13/24, 9:45 AM - Alice: ok",
	}},
	{name: "calls_location_poll", tz: "Europe/London", lines: []string{
		"31/12/2023, 21:41 - Alice: Missed voice call",
		"31/12/2023, 21:42 - Bob: Video call, 12 min",
		"31/12/2023, 21:43 - Alice: Voice call, 1 hr 5 min",
		"31/12/2023, 21:44 - Bob: location: https://maps.google.com/?q=51.5,-0.12",
		"31/12/2023, 21:45 - Alice: Live location shared",
		"31/12/2023, 21:46 - Bob: POLL:",
		"Dinner where?",
		"OPTION: Thai (1 vote)",
		"OPTION: Pizza (0 votes)",
		"31/12/2023, 21:47 - Alice: null",
		"13/01/2024, 10:00 - Bob: Waiting for this message. This may take a while.",
	}},
	{name: "system_lines", tz: "America/New_York", lines: []string{
		"12/31/23, 9:40 PM - " + enc,
		"12/31/23, 9:41 PM - Bob changed their phone number to a new number. Tap to message or add the new number.",
		"12/31/23, 9:42 PM - Disappearing messages were turned on. New messages will disappear from this chat 7 days after they're sent.",
		"12/31/23, 9:43 PM - You blocked this contact. Tap to unblock.",
		"12/31/23, 9:44 PM - Your security code with Bob changed. Tap to learn more.",
		"12/31/23, 9:45 PM - Bob is a contact.",
		"12/31/23, 9:46 PM - Missed voice call",
		"12/31/23, 9:47 PM - Alice: hello",
		"1/13/24, 9:48 AM - Something new happened",
	}},
	{name: "dst_london", tz: "Europe/London", lines: []string{
		"31/03/2024, 00:59 - Alice: before the clocks change",
		"31/03/2024, 01:30 - Bob: this time does not exist",
		"31/03/2024, 02:10 - Alice: after",
		"27/10/2024, 01:20 - Bob: first 01:20",
		"27/10/2024, 01:40 - Alice: still first pass",
		"27/10/2024, 01:10 - Bob: second pass, earlier clock",
		"27/10/2024, 02:05 - Alice: done",
	}},
	{name: "empty", tz: "Europe/London", lines: []string{""}, wantErr: true},
	{name: "garbage", tz: "Europe/London", lines: []string{"%PDF-1.7", "binary-ish junk", "{\"not\": \"whatsapp\"}"}, wantErr: true},
}

// ambiguousDaily returns daily DMY messages on days 1–12 of January–March
// 2024: every field is ≤ 12, so only chronology can tell DMY from MDY.
func ambiguousDaily() []string {
	var out []string
	for m := 1; m <= 3; m++ {
		for d := 1; d <= 12; d++ {
			out = append(out, time.Date(2024, time.Month(m), d, 0, 0, 0, 0, time.UTC).Format("02/01/06")+
				", 20:00 - "+[]string{"Alice: kya haal hai", "Bob: sab badhiya"}[d%2])
		}
	}
	return out
}

// goldenMessage is the stable, reviewable projection of ingest.Message.
type goldenMessage struct {
	Line       int              `json:"line"`
	SentAt     string           `json:"sent_at"`
	Local      string           `json:"local"`
	Sender     string           `json:"sender,omitempty"`
	Key        string           `json:"key,omitempty"`
	Kind       ingest.Kind      `json:"kind"`
	SystemType string           `json:"system_type,omitempty"`
	MediaType  ingest.MediaType `json:"media_type,omitempty"`
	FileName   string           `json:"file_name,omitempty"`
	Omitted    bool             `json:"omitted,omitempty"`
	Text       string           `json:"text,omitempty"`
	Edited     bool             `json:"edited,omitempty"`
	Deleted    bool             `json:"deleted,omitempty"`
	Call       *ingest.Call     `json:"call,omitempty"`
}

type golden struct {
	Error       string              `json:"error,omitempty"`
	Report      ingest.Report       `json:"report"`
	Messages    []goldenMessage     `json:"messages"`
	Diagnostics []ingest.Diagnostic `json:"diagnostics,omitempty"`
}

func toGolden(rep ingest.Report, sink *collectSink, err error) golden {
	g := golden{Report: rep, Diagnostics: sink.diags, Messages: []goldenMessage{}}
	if err != nil {
		g.Error = err.Error()
	}
	for _, m := range sink.msgs {
		gm := goldenMessage{
			Line: m.SourceLine, SentAt: m.SentAt.UTC().Format(time.RFC3339),
			Local: m.LocalTime.Format("2006-01-02T15:04:05"), Sender: m.SenderDisplay, Key: m.SenderKey,
			Kind: m.Kind, SystemType: m.SystemType, MediaType: m.MediaType, Text: m.Text,
			Edited: m.Edited, Deleted: m.Deleted, Call: m.Call,
		}
		if m.Media != nil {
			gm.FileName, gm.Omitted = m.Media.FileName, m.Media.Omitted
		}
		g.Messages = append(g.Messages, gm)
	}
	return g
}

func TestGoldenFixtures(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			txtPath := filepath.Join("testdata", f.name+".txt")
			goldPath := filepath.Join("testdata", f.name+".golden.json")
			text := f.text()
			if *update {
				writeFile(t, txtPath, []byte(text))
			}
			onDisk, err := os.ReadFile(txtPath)
			if err != nil {
				t.Fatalf("missing fixture (run with -update): %v", err)
			}
			if string(onDisk) != text {
				t.Fatalf("%s drifted from its definition; run with -update", txtPath)
			}

			rep, sink, perr := parseWith(t, onDisk, f.name+".txt", f.tz, ingest.Options{DateOrder: f.order})
			if f.wantErr != (perr != nil) {
				t.Fatalf("err = %v, wantErr %v", perr, f.wantErr)
			}
			got, _ := json.MarshalIndent(toGolden(rep, sink, perr), "", "  ")
			got = append(got, '\n')
			if *update {
				writeFile(t, goldPath, got)
			}
			want, err := os.ReadFile(goldPath)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if string(want) != string(got) {
				t.Errorf("output differs from %s (run with -update and review the diff)\n--- got ---\n%s", goldPath, got)
			}
		})
	}
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// collectSink records everything the parser emits.
type collectSink struct {
	msgs    []ingest.Message
	diags   []ingest.Diagnostic
	batches int
}

func (s *collectSink) Batch(_ context.Context, m []ingest.Message) error {
	s.batches++
	s.msgs = append(s.msgs, m...)
	return nil
}

func (s *collectSink) Diagnostic(d ingest.Diagnostic) { s.diags = append(s.diags, d) }

func parseWith(t testing.TB, data []byte, name, tz string, opts ingest.Options) (ingest.Report, *collectSink, error) {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	opts.Location = loc
	sink := &collectSink{}
	rep, err := New().Parse(context.Background(), ingest.BytesSource{Data: data, FileName: name}, opts, sink)
	return rep, sink, err
}
