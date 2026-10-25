package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

// maxTextBytes caps one message body, matching the WhatsApp parser
// (docs/parsers/telegram.md §5, "Very long pre/code entities").
const maxTextBytes = 64 << 10

// localLayout is the format of the "date" fields: wall-clock time of the
// exporting machine without an offset.
const localLayout = "2006-01-02T15:04:05"

// deletedAccountName is the display name used when "from" is null.
const deletedAccountName = "Deleted account"

// mediaTypes maps Telegram's media_type values to canonical media types.
var mediaTypes = map[string]ingest.MediaType{
	"sticker":       ingest.MediaSticker,
	"voice_message": ingest.MediaVoiceNote,
	"video_message": ingest.MediaVideoNote,
	"video_file":    ingest.MediaVideo,
	"animation":     ingest.MediaGIF,
	"audio_file":    ingest.MediaAudio,
}

// mapResult carries one mapped message plus content-free findings for the
// caller to turn into diagnostics.
type mapResult struct {
	msg       ingest.Message
	skip      string // non-empty: the message is unusable; value is the reason code
	truncated bool
	// clockOffset is LocalTime - SentAt, used to detect clock mismatches.
	clockOffset time.Duration
	hasLocal    bool
}

// mapMessage converts one raw export message to the canonical schema
// (spec §3 mapping table).
func mapMessage(r *rawMessage) mapResult {
	var res mapResult
	m := &res.msg
	m.Platform = ingest.PlatformTelegram
	m.ExternalID = r.ID.String()
	m.TimePrecision = ingest.PrecisionSecond

	// date_unixtime is the source of truth for the instant (FR-ING-006).
	sec, err := strconv.ParseInt(strings.TrimSpace(r.DateUnix), 10, 64)
	if err != nil {
		res.skip = "missing_timestamp"
		return res
	}
	m.SentAt = time.Unix(sec, 0).UTC()
	m.LocalTime = m.SentAt
	if lt, err := time.Parse(localLayout, r.Date); err == nil {
		m.LocalTime = lt
		res.hasLocal = true
		res.clockOffset = lt.Sub(m.SentAt)
	}

	if r.EditedUnix != "" {
		if es, err := strconv.ParseInt(strings.TrimSpace(r.EditedUnix), 10, 64); err == nil {
			t := time.Unix(es, 0).UTC()
			m.Edited, m.EditedAt = true, &t
		}
	} else if r.Edited != "" {
		m.Edited = true
	}

	if r.ReplyTo != "" {
		m.ReplyToExternalID = r.ReplyTo.String()
	}
	m.Forwarded = present(r.ForwardedFrom) || present(r.SavedFrom)
	m.Reactions = mapReactions(r.Reactions)

	text := ingest.CleanText(r.flatText())
	if len(text) > maxTextBytes {
		text = truncateUTF8(text, maxTextBytes)
		res.truncated = true
	}
	m.Text = text

	if r.Type == "service" {
		mapService(r, m)
		// A call without an actor ID cannot be attributed to anyone (found
		// by FuzzDecode); skip it like any other sender-less message.
		if m.Kind == ingest.KindCall && m.SenderKey == "" {
			res.skip = "missing_sender"
		}
		return res
	}

	m.SenderKey = r.FromID
	m.SenderDisplay = displayName(r.From)
	switch {
	case r.Poll != nil:
		m.Kind = ingest.KindPoll
		m.Text = ingest.CleanText(r.Poll.Question)
	case present(r.LocationInformation) || r.LiveLocationPeriod != "":
		// Coordinates and place names are not kept (PRV-004).
		m.Kind = ingest.KindLocation
		m.Text = ""
	case r.ContactInformation != nil:
		m.Kind = ingest.KindContact
		m.MediaType = ingest.MediaContact
		m.Text = ingest.CleanText(strings.TrimSpace(r.ContactInformation.FirstName + " " + r.ContactInformation.LastName))
	case r.Photo != "" || r.MediaType != "" || r.File != "":
		m.Kind = ingest.KindMedia
		m.MediaType, m.Media = mapMedia(r)
	case m.Text != "":
		m.Kind = ingest.KindText
	default:
		// Unsupported message types (games, invoices, …) export with no text
		// and no media. They are kept as sender-less system rows so ordering
		// and counts stay faithful (spec §5).
		m.Kind = ingest.KindSystem
		m.SystemType = "unsupported"
		m.SenderKey, m.SenderDisplay = "", ""
	}
	if m.SenderKey == "" && m.Kind != ingest.KindSystem {
		res.skip = "missing_sender"
	}
	return res
}

// mapService handles type=service messages: calls become KindCall with the
// caller as sender; every other action is a sender-less system event.
func mapService(r *rawMessage, m *ingest.Message) {
	if r.Action == "phone_call" {
		m.Kind = ingest.KindCall
		m.SystemType = "phone_call"
		m.SenderKey = r.ActorID
		m.SenderDisplay = displayName(r.Actor)
		dur, _ := strconv.Atoi(r.Duration.String())
		m.Call = &ingest.Call{
			Missed:      r.DiscardReason == "missed" || r.DiscardReason == "busy",
			DurationSec: dur,
		}
		m.Text = ""
		return
	}
	m.Kind = ingest.KindSystem
	m.SystemType = r.Action
	if m.SystemType == "" {
		m.SystemType = "unknown"
	}
	// System text such as "Alice pinned a message" names people; keep only
	// the event type.
	m.Text = ""
}

// mapMedia derives the media type and metadata. "(File not included…)" and
// "(File exceeds maximum size…)" path strings mean the export omitted the file.
func mapMedia(r *rawMessage) (ingest.MediaType, *ingest.MediaMeta) {
	meta := &ingest.MediaMeta{MIMEType: r.MimeType}
	path := r.File
	mt := ingest.MediaDocument
	switch {
	case r.Photo != "":
		mt, path = ingest.MediaImage, r.Photo
		meta.SizeBytes, _ = r.PhotoSize.Int64()
	case r.MediaType != "":
		if t, ok := mediaTypes[r.MediaType]; ok {
			mt = t
		} else {
			mt = ingest.MediaUnknown
		}
	}
	if meta.SizeBytes == 0 {
		meta.SizeBytes, _ = r.FileSize.Int64()
	}
	meta.Omitted = strings.HasPrefix(path, "(File ")
	meta.FileName = r.FileName
	if meta.FileName == "" && path != "" && !meta.Omitted {
		meta.FileName = path[strings.LastIndexByte(path, '/')+1:]
	}
	if d, err := strconv.Atoi(r.Duration.String()); err == nil {
		meta.DurationSec = d
	}
	return mt, meta
}

// mapReactions keeps emoji and counts only; "recent" lists name who reacted
// and are not needed for the MVP.
func mapReactions(in []rawReaction) []ingest.Reaction {
	if len(in) == 0 {
		return nil
	}
	out := make([]ingest.Reaction, 0, len(in))
	for _, r := range in {
		emoji := r.Emoji
		switch r.Type {
		case "custom_emoji":
			emoji = "custom_emoji"
			if r.DocumentID != "" {
				emoji += ":" + r.DocumentID
			}
		case "paid":
			emoji = "paid"
		}
		if emoji == "" {
			emoji = r.Type
		}
		out = append(out, ingest.Reaction{Emoji: emoji, Count: r.Count})
	}
	return out
}

func displayName(name *string) string {
	if name == nil || strings.TrimSpace(*name) == "" {
		return deletedAccountName
	}
	return ingest.CleanText(*name)
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// formatOffset renders a clock offset as +HH:MM for the timezone suggestion.
func formatOffset(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	h := int(d / time.Hour)
	mnt := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%s%02d:%02d", sign, h, mnt)
}
