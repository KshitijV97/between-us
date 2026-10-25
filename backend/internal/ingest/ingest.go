// Package ingest defines the contract between chat-export parsers and the rest
// of the system: the canonical Message every parser emits, the Parser and Sink
// interfaces, detection, diagnostics and the import report (FR-ING-002…014).
//
// Specifications:
//   - docs/parsers/whatsapp.md  — WhatsApp .txt/.zip
//   - docs/parsers/telegram.md  — Telegram Desktop result.json (§6 schema, §7 contract)
//   - docs/03-data-model.md §2  — how canonical messages map to the messages table
//
// Parsers live in sub-packages (ingest/whatsapp, ingest/telegram) and register
// themselves with Register. They never touch the database: the importer
// (internal/importer) maps senders to participants, computes fingerprints and
// local dates, and writes through internal/store.
//
// Privacy rule (PRV-007): Diagnostics and Report never contain message text or
// sender names; they carry line numbers, codes and counts only. Sender names do
// appear in Report.Participants because the user must confirm the mapping
// (FR-ING-008); that report is user data, never telemetry.
package ingest

import (
	"context"
	"io"
	"time"
)

// Platform identifies the chat application an export came from.
type Platform string

// Supported platforms.
const (
	PlatformWhatsApp Platform = "whatsapp"
	PlatformTelegram Platform = "telegram"
)

// Kind classifies a canonical message (FR-ING-004). Stickers are KindMedia with
// MediaSticker; there is no separate sticker kind.
type Kind string

// Message kinds. Only KindSystem rows have no sender.
const (
	KindText     Kind = "text"
	KindMedia    Kind = "media"
	KindCall     Kind = "call"
	KindLocation Kind = "location"
	KindContact  Kind = "contact"
	KindPoll     Kind = "poll"
	KindDeleted  Kind = "deleted"
	KindSystem   Kind = "system"
)

// MediaType refines KindMedia (and media captions). The MVP stores only the
// type and metadata, never the media itself.
type MediaType string

// Media types, shared by both platforms.
const (
	MediaNone      MediaType = ""
	MediaImage     MediaType = "image"
	MediaVideo     MediaType = "video"
	MediaVideoNote MediaType = "video_note" // round video message
	MediaVoiceNote MediaType = "voice_note"
	MediaAudio     MediaType = "audio" // music/audio file, not a voice note
	MediaSticker   MediaType = "sticker"
	MediaGIF       MediaType = "gif"
	MediaDocument  MediaType = "document"
	MediaContact   MediaType = "contact"
	MediaUnknown   MediaType = "unknown"
)

// TimePrecision records how precise export timestamps are. Android WhatsApp
// headers have no seconds (metrics catalog rule R-19).
type TimePrecision string

// Timestamp precisions.
const (
	PrecisionSecond TimePrecision = "second"
	PrecisionMinute TimePrecision = "minute"
)

// Message is one canonical chat item as emitted by a parser
// (docs/parsers/telegram.md §6). Fields the parser cannot know (participant ID,
// local date in the analysis timezone, fingerprint, ordinal) are added by the
// importer.
type Message struct {
	Platform Platform
	// ExternalID is the platform's message ID (Telegram "id"); empty for
	// WhatsApp, whose exports carry no IDs.
	ExternalID string

	// SentAt is the absolute send time in UTC.
	SentAt time.Time
	// LocalTime is the wall-clock time exactly as written in the export. Its
	// Location is meaningless (always UTC); read only the clock fields. It is the
	// timezone-independent basis of the WhatsApp fingerprint.
	LocalTime     time.Time
	TimePrecision TimePrecision

	// SenderKey is stable per chat: Telegram from_id ("user123"), WhatsApp the
	// normalised sender name or E.164 number. Empty for KindSystem.
	SenderKey string
	// SenderDisplay is the sender name as it appears in the export.
	SenderDisplay string

	Kind Kind
	// SystemType names the event for KindSystem and KindCall, e.g.
	// "encryption_notice", "group_created", "phone_call".
	SystemType string
	// Text is the NFC-normalised body with platform markers (such as
	// "<This message was edited>") removed. For media it is the caption, for
	// polls the question, for contacts the contact's display name.
	Text      string
	MediaType MediaType
	Media     *MediaMeta

	// ReplyToExternalID is the ExternalID of the message replied to (Telegram).
	ReplyToExternalID string
	Forwarded         bool
	Edited            bool
	EditedAt          *time.Time
	// Deleted is true for "This message was deleted" placeholders (Kind is
	// KindDeleted).
	Deleted   bool
	Call      *Call
	Reactions []Reaction

	// SourceLine is the 1-based line (WhatsApp) or array index (Telegram) for
	// diagnostics and pump-mode checkpoints.
	SourceLine int
}

// MediaMeta describes an attachment without its content. Location
// coordinates and contact vCards are deliberately not modelled (data
// minimisation, PRV-004).
type MediaMeta struct {
	FileName    string
	MIMEType    string
	SizeBytes   int64
	DurationSec int
	// Omitted is true when the export did not include the file
	// ("<Media omitted>", "File not included").
	Omitted bool
}

// Call describes a voice or video call event (M-021).
type Call struct {
	Video       bool
	Missed      bool
	DurationSec int
}

// Reaction is an aggregated emoji reaction (Telegram only; WhatsApp exports
// omit reactions).
type Reaction struct {
	Emoji string
	Count int
}

// Detection is the result of sniffing a sample of an upload (FR-ING-002).
type Detection struct {
	Platform Platform
	// Variant names the header/format variant, e.g. "android-24h-dmy",
	// "ios-12h", "telegram-json-personal".
	Variant string
	// Confidence is in [0, 1]; the registry accepts the best parser at or
	// above MinConfidence.
	Confidence float64
	// Hints explain a low confidence to the user (no message content).
	Hints []string
}

// MinConfidence is the detection threshold below which an upload is rejected
// with an actionable "unrecognised format" error.
const MinConfidence = 0.6

// DateOrder is the field order of numeric dates in WhatsApp headers.
type DateOrder string

// Date orders. DateOrderAuto asks the parser to infer it (docs/parsers/whatsapp.md §4).
const (
	DateOrderAuto DateOrder = ""
	DateOrderDMY  DateOrder = "DMY"
	DateOrderMDY  DateOrder = "MDY"
	DateOrderYMD  DateOrder = "YMD"
)

// Options configure one parse.
type Options struct {
	// Location interprets naive wall-clock times (WhatsApp). Required for
	// WhatsApp; Telegram uses epoch seconds and ignores it.
	Location *time.Location
	// DateOrder overrides inference when the user resolved an ambiguity.
	DateOrder DateOrder
	// BatchSize is the maximum number of messages per Sink.Batch call.
	BatchSize int
	// SelectedChatID picks one chat from a Telegram full-account export.
	SelectedChatID string
}

// DefaultBatchSize balances transaction size against memory (FR-ING-013).
const DefaultBatchSize = 2000

// Source is re-readable input, so two-pass parsers (WhatsApp date-order
// inference) can rewind. Backed by a file, an in-memory buffer or a zip entry.
type Source interface {
	// Open returns a fresh reader positioned at the start.
	Open() (io.ReadCloser, error)
	// Name is a display name (file or entry name), used only as a hint.
	Name() string
	// Size is the uncompressed size in bytes, or -1 if unknown.
	Size() int64
}

// Sink receives parser output in order.
type Sink interface {
	// Batch receives up to Options.BatchSize messages in export order. An
	// error aborts the parse.
	Batch(ctx context.Context, msgs []Message) error
	// Diagnostic records a non-fatal finding. Never put message text in it.
	Diagnostic(d Diagnostic)
}

// Parser converts one export format into canonical messages.
type Parser interface {
	// Platform reports which platform this parser handles.
	Platform() Platform
	// Detect inspects at most SampleSize bytes from the start of the input.
	// It must be side-effect free and fast.
	Detect(sample []byte) Detection
	// Parse streams the whole input to sink using bounded memory
	// (FR-ING-013) and honours ctx cancellation between batches. It returns a
	// Report even when it fails, describing what was read so far.
	Parse(ctx context.Context, src Source, opts Options, sink Sink) (Report, error)
}

// SampleSize is how much of an upload detection may inspect.
const SampleSize = 64 << 10

// Severity grades a diagnostic.
type Severity string

// Diagnostic severities.
const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Diagnostic is a non-fatal, content-free finding, e.g. an unparseable line.
type Diagnostic struct {
	Severity Severity
	// Code is a stable machine-readable identifier, e.g. "unparsed_line",
	// "dst_gap_adjusted", "reply_target_missing".
	Code string
	// Line is the 1-based source line or array index; 0 when not applicable.
	Line int
	// Detail is a short human-readable explanation without message content.
	Detail string
}

// Report summarises a parse for the import report (FR-ING-011). It is
// persisted as JSON on the imports row, hence the tags.
type Report struct {
	Platform      Platform      `json:"platform"`
	Variant       string        `json:"variant"`
	TimePrecision TimePrecision `json:"time_precision"`
	// DateOrder is the order used (inferred or supplied) and the evidence for
	// it, e.g. "a day field exceeded 12 on 34 lines".
	DateOrder         DateOrder `json:"date_order,omitempty"`
	DateOrderEvidence string    `json:"date_order_evidence,omitempty"`
	// DateOrderAmbiguous is true when inference could not decide and fell
	// back to a locale default; the UI asks the user to confirm.
	DateOrderAmbiguous bool `json:"date_order_ambiguous,omitempty"`

	LinesRead   int `json:"lines_read"`
	Messages    int `json:"messages"`
	SystemLines int `json:"system_lines"`
	// Skipped counts unparseable items by reason code.
	Skipped map[string]int `json:"skipped,omitempty"`
	// SkippedSamples lists up to MaxSkippedSamples skipped line numbers.
	SkippedSamples []SkippedLine `json:"skipped_samples,omitempty"`

	Participants []ParticipantSummary `json:"participants"`
	FirstAt      *time.Time           `json:"first_at,omitempty"`
	LastAt       *time.Time           `json:"last_at,omitempty"`
	Warnings     []string             `json:"warnings,omitempty"`
	// SuggestedTitle comes from file names or the Telegram chat name.
	SuggestedTitle string `json:"suggested_title,omitempty"`
}

// MaxSkippedSamples bounds Report.SkippedSamples.
const MaxSkippedSamples = 50

// SkippedLine identifies one skipped input line without its content.
type SkippedLine struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// ParticipantSummary is one detected sender (FR-ING-008, FR-ING-012).
type ParticipantSummary struct {
	Key          string `json:"key"`
	Display      string `json:"display"`
	MessageCount int    `json:"message_count"`
}

// AddSkipped records a skipped line, keeping only the first MaxSkippedSamples
// samples but counting all of them.
func (r *Report) AddSkipped(line int, reason string) {
	if r.Skipped == nil {
		r.Skipped = map[string]int{}
	}
	r.Skipped[reason]++
	if len(r.SkippedSamples) < MaxSkippedSamples {
		r.SkippedSamples = append(r.SkippedSamples, SkippedLine{Line: line, Reason: reason})
	}
}

// Observe updates the date range and participant counts with m. Parsers call
// it once per emitted message so every parser reports the same way.
func (r *Report) Observe(m *Message) {
	if m.Kind == KindSystem {
		r.SystemLines++
	} else {
		r.Messages++
	}
	if !m.SentAt.IsZero() {
		t := m.SentAt
		if r.FirstAt == nil || t.Before(*r.FirstAt) {
			r.FirstAt = &t
		}
		if r.LastAt == nil || t.After(*r.LastAt) {
			t2 := t
			r.LastAt = &t2
		}
	}
	if m.SenderKey == "" {
		return
	}
	for i := range r.Participants {
		if r.Participants[i].Key == m.SenderKey {
			r.Participants[i].MessageCount++
			return
		}
	}
	r.Participants = append(r.Participants, ParticipantSummary{Key: m.SenderKey, Display: m.SenderDisplay, MessageCount: 1})
}
