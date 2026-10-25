// Package synth generates deterministic, entirely fictional two-person chats
// in WhatsApp and Telegram export formats. It powers load tests (FR-ING-013,
// NFR-PERF-002), demos and analytics fixtures without ever touching real
// conversations (CONTRIBUTING: synthetic data only).
//
// Output is reproducible for a given Config.Seed. The conversation model is
// simple but exercises what parsers and metrics care about: sessions separated
// by gaps, alternating and double-texting senders, multi-line bodies, emoji,
// Hinglish, media placeholders, deletions, edits and calls.
package synth

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Config controls generation.
type Config struct {
	Seed     uint64
	Messages int
	// Start is the wall-clock time of the first message.
	Start time.Time
	// Location interprets wall-clock times (WhatsApp) and converts them to
	// epoch seconds (Telegram).
	Location *time.Location
	// Names of the two participants.
	A, B string
}

// DefaultConfig returns a reproducible configuration with n messages.
func DefaultConfig(n int) Config {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		loc = time.UTC
	}
	return Config{
		Seed: 42, Messages: n, Location: loc,
		Start: time.Date(2023, 1, 1, 9, 0, 0, 0, loc),
		A:     "Alice", B: "Bob",
	}
}

// phrases mixes English, Hinglish and emoji so tokenisation and lexicons see
// realistic input.
var phrases = []string{
	"good morning ☀️", "kya kar rahe ho?", "haha 😂", "ok", "on my way",
	"miss you ❤️", "call me when free", "kal milte hain", "that meeting was so long 😩",
	"did you eat?", "yes, dal chawal 😋", "running late, sorry!", "love you", "kitna kaam hai yaar",
	"see you at 7", "👍", "the movie was amazing", "where are you?", "traffic bahut hai 🚗",
	"happy birthday!! 🎉🎂", "can we talk later?", "I'm so tired", "send me the link", "thank you 🙏",
}

// event is one generated chat item in a platform-neutral form.
type event struct {
	at      time.Time
	sender  int // 0 = A, 1 = B
	kind    string
	text    string
	edited  bool
	callSec int
}

// generate yields events in chronological order to fn.
func generate(cfg Config, fn func(i int, e event) error) error {
	// #nosec G404 -- deterministic synthetic test data must be reproducible
	// from a seed; nothing here is security-sensitive.
	r := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	t := cfg.Start
	sender := 0
	for i := 0; i < cfg.Messages; i++ {
		// Gaps: mostly quick replies, sometimes a pause, occasionally a new
		// session hours later (exercises sessionisation, M-007). The mean gap
		// is ~13 minutes, so 100k messages span roughly 2.5 years, like a
		// busy real chat.
		switch p := r.Float64(); {
		case p < 0.81:
			t = t.Add(time.Duration(5+r.IntN(240)) * time.Second)
		case p < 0.99:
			t = t.Add(time.Duration(5+r.IntN(55)) * time.Minute)
		default:
			t = t.Add(time.Duration(4+r.IntN(12)) * time.Hour)
		}
		// Alternate senders, with occasional double-texts (M-012).
		if r.Float64() < 0.65 {
			sender = 1 - sender
		}
		e := event{at: t, sender: sender, kind: "text", text: phrases[r.IntN(len(phrases))]}
		switch p := r.Float64(); {
		case p < 0.04:
			e.kind, e.text = "image", ""
		case p < 0.05:
			e.kind, e.text = "voice", ""
		case p < 0.055:
			e.kind, e.text = "deleted", ""
		case p < 0.06:
			e.kind, e.callSec = "call", r.IntN(900)
		case p < 0.08:
			e.text += "\n" + phrases[r.IntN(len(phrases))] // multi-line
		case p < 0.09:
			e.edited = true
		}
		if err := fn(i, e); err != nil {
			return err
		}
	}
	return nil
}

// WriteWhatsApp writes an Android-style export ("dd/mm/yyyy, HH:MM - Name: text",
// 24-hour clock, day-first dates) beginning with the encryption notice.
func WriteWhatsApp(w io.Writer, cfg Config) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	names := [2]string{cfg.A, cfg.B}
	header := func(t time.Time) string { return t.In(cfg.Location).Format("02/01/2006, 15:04") }
	fmt.Fprintf(bw, "%s - Messages and calls are end-to-end encrypted. Only people in this chat can read, listen to, or share them. Learn more.\n", header(cfg.Start))
	err := generate(cfg, func(_ int, e event) error {
		var body string
		switch e.kind {
		case "image", "voice":
			body = "<Media omitted>"
		case "deleted":
			body = "This message was deleted"
		case "call":
			body = "Missed voice call"
		default:
			body = e.text
			if e.edited {
				body += " <This message was edited>"
			}
		}
		_, err := fmt.Fprintf(bw, "%s - %s: %s\n", header(e.at), names[e.sender], body)
		return err
	})
	if err != nil {
		return err
	}
	return bw.Flush()
}

// WriteTelegram writes a Telegram Desktop personal-chat JSON export, streaming
// one message object at a time so arbitrarily large files need little memory.
func WriteTelegram(w io.Writer, cfg Config) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	ids := [2]string{"user1001", "user2002"}
	names := [2]string{cfg.A, cfg.B}
	fmt.Fprintf(bw, "{\n \"name\": %q,\n \"type\": \"personal_chat\",\n \"id\": 2002,\n \"messages\": [\n", cfg.B)
	err := generate(cfg, func(i int, e event) error {
		local := e.at.In(cfg.Location)
		m := map[string]any{
			"id":            i + 1,
			"type":          "message",
			"date":          local.Format("2006-01-02T15:04:05"),
			"date_unixtime": strconv.FormatInt(e.at.Unix(), 10),
			"from":          names[e.sender],
			"from_id":       ids[e.sender],
		}
		switch e.kind {
		case "image":
			m["photo"] = "(File not included. Change data exporting settings to download.)"
			m["text"] = ""
		case "voice":
			m["media_type"] = "voice_message"
			m["file"] = "(File not included. Change data exporting settings to download.)"
			m["duration_seconds"] = 7
			m["text"] = ""
		case "call":
			m["type"] = "service"
			m["actor"], m["actor_id"] = names[e.sender], ids[e.sender]
			delete(m, "from")
			delete(m, "from_id")
			m["action"] = "phone_call"
			m["duration_seconds"] = e.callSec
			m["text"] = ""
		case "deleted":
			// Telegram exports omit deleted messages; emit a plain text instead.
			m["text"] = "ok"
		default:
			m["text"] = e.text
			m["text_entities"] = []map[string]string{{"type": "plain", "text": e.text}}
			if e.edited {
				m["edited"] = local.Add(time.Minute).Format("2006-01-02T15:04:05")
				m["edited_unixtime"] = strconv.FormatInt(e.at.Add(time.Minute).Unix(), 10)
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		sep := ",\n"
		if i == cfg.Messages-1 {
			sep = "\n"
		}
		_, err = bw.WriteString("  " + string(b) + sep)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := bw.WriteString(" ]\n}\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// Words returns the phrase list, for tests that assert on generated content.
func Words() string { return strings.Join(phrases, " ") }
