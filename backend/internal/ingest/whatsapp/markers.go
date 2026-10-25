package whatsapp

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// Marker catalogue (docs/parsers/whatsapp.md §7). English strings plus the
// most common translations of the two most frequent placeholders. The spec
// envisions per-language data files; until a second language needs the full
// catalogue, the tables live here and are matched in every language at once.

// editedMarker trails edited messages (WhatsApp 2023+).
const editedMarker = "<This message was edited>"

var (
	// mediaOmitted are Android "without media" placeholders.
	mediaOmitted = map[string]bool{
		"<Media omitted>": true, "<Medien ausgeschlossen>": true, "<Medien weggelassen>": true,
		"<Multimedia omitido>": true, "<Mídia oculta>": true, "<Médias omis>": true,
		"<Media omessi>": true, "<Media weggelaten>": true,
	}
	// iosOmitted maps iOS "<type> omitted" placeholders to media types.
	iosOmitted = []struct {
		suffix string
		typ    ingest.MediaType
	}{
		{"image omitted", ingest.MediaImage},
		{"video omitted", ingest.MediaVideo},
		{"audio omitted", ingest.MediaAudio},
		{"sticker omitted", ingest.MediaSticker},
		{"GIF omitted", ingest.MediaGIF},
		{"Contact card omitted", ingest.MediaContact},
		{"document omitted", ingest.MediaDocument},
	}
	deletedMarkers = map[string]bool{
		"This message was deleted": true, "You deleted this message": true,
		"Diese Nachricht wurde gelöscht": true, "Du hast diese Nachricht gelöscht": true,
		"Se eliminó este mensaje": true, "Eliminaste este mensaje": true,
		"Este mensaje fue eliminado": true,
	}

	// fileAttached: Android "with media" export, "IMG-…jpg (file attached)".
	fileAttached = regexp.MustCompile(`^(.+?\.[A-Za-z0-9]{1,5}) \(file attached\)$`)
	// iosAttached: iOS "with media" export, "<attached: 00000012-PHOTO-….jpg>".
	iosAttached = regexp.MustCompile(`^<attached: ([^>]+)>$`)
	// callRe: "Missed voice call", "Video call, 12 min", "Voice call, 1 hr 5 min".
	callRe     = regexp.MustCompile(`(?i)^(missed )?(voice|video) call(?:[,.]?\s*(.*))?$`)
	durationRe = regexp.MustCompile(`(?i)(\d+)\s*(h|hr|hrs|hour|hours|m|min|mins|minute|minutes|s|sec|secs|second|seconds)\b`)
)

// mediaByExt infers a media type from a file extension.
func mediaByExt(name string) ingest.MediaType {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")) {
	case "jpg", "jpeg", "png", "heic", "heif", "bmp":
		return ingest.MediaImage
	case "mp4", "3gp", "mov", "mkv", "webm":
		return ingest.MediaVideo
	case "gif":
		return ingest.MediaGIF
	case "webp":
		return ingest.MediaSticker // WhatsApp stickers are WebP
	case "opus":
		return ingest.MediaVoiceNote
	case "mp3", "m4a", "aac", "ogg", "wav", "amr":
		return ingest.MediaAudio
	case "vcf":
		return ingest.MediaContact
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "csv", "zip", "rtf", "odt", "pages", "key", "numbers":
		return ingest.MediaDocument
	}
	return ingest.MediaUnknown
}

// mediaByName refines the type with WhatsApp's file-name tokens, which are
// more reliable than extensions (a voice note and a song are both audio).
func mediaByName(name string) ingest.MediaType {
	base := strings.ToUpper(path.Base(name))
	switch {
	case strings.HasPrefix(base, "PTT-"), strings.Contains(base, "-AUDIO-"):
		return ingest.MediaVoiceNote
	case strings.HasPrefix(base, "IMG-"), strings.Contains(base, "-PHOTO-"):
		return ingest.MediaImage
	case strings.HasPrefix(base, "VID-"), strings.Contains(base, "-VIDEO-"):
		return ingest.MediaVideo
	case strings.HasPrefix(base, "STK-"), strings.Contains(base, "-STICKER-"):
		return ingest.MediaSticker
	case strings.Contains(base, "-GIF-"):
		return ingest.MediaGIF
	case strings.HasPrefix(base, "AUD-"):
		return ingest.MediaAudio
	case strings.HasPrefix(base, "DOC-"):
		return ingest.MediaDocument
	}
	return mediaByExt(name)
}

// classify fills Kind, MediaType, Media, Call, Deleted and Text of a message
// that has a sender, from its raw body (all lines). Placeholders carry no
// text so they never count as words or sentiment (§7.2).
func classify(m *ingest.Message, body string) {
	clean := ingest.CleanText(body)

	// The edited marker trails the text; strip it before anything else so an
	// edited placeholder is still recognised.
	if strings.HasSuffix(clean, editedMarker) {
		clean = strings.TrimRight(strings.TrimSuffix(clean, editedMarker), " \n")
		m.Edited = true
	}
	first, caption, _ := strings.Cut(clean, "\n")
	caption = strings.TrimSpace(caption)
	m.Kind = ingest.KindText
	m.Text = clean

	switch {
	case mediaOmitted[clean]:
		setMedia(m, ingest.MediaUnknown, "", true, "")
		return
	case clean == "null":
		// Android writes "null" for view-once media.
		setMedia(m, ingest.MediaUnknown, "", true, "")
		m.SystemType = "view_once"
		return
	case deletedMarkers[strings.TrimSuffix(clean, ".")]:
		m.Kind, m.Deleted, m.Text = ingest.KindDeleted, true, ""
		return
	case clean == "Waiting for this message. This may take a while.":
		m.Kind, m.SystemType, m.Text = ingest.KindSystem, "undecryptable", ""
		return
	case clean == "Live location shared" || strings.HasPrefix(clean, "Live location shared"):
		m.Kind, m.SystemType, m.Text = ingest.KindLocation, "live_location", ""
		return
	case strings.HasPrefix(clean, "location: "):
		// The URL contains coordinates; they are dropped (data minimisation).
		m.Kind, m.Text = ingest.KindLocation, ""
		return
	case strings.HasPrefix(clean, "POLL:"):
		m.Kind = ingest.KindPoll
		m.Text = pollQuestion(clean)
		return
	}

	if sm := fileAttached.FindStringSubmatch(first); sm != nil {
		setMedia(m, mediaByName(sm[1]), sm[1], false, caption)
		return
	}
	if sm := iosAttached.FindStringSubmatch(first); sm != nil {
		setMedia(m, mediaByName(sm[1]), sm[1], false, caption)
		return
	}
	for _, o := range iosOmitted {
		if first == o.suffix || strings.HasSuffix(first, " "+o.suffix) {
			name := ""
			if o.typ == ingest.MediaDocument {
				// "Report.pdf \u2022 3 pages document omitted" keeps the file name.
				name = strings.TrimSuffix(first, " "+o.suffix)
				name, _, _ = strings.Cut(name, " \u2022 ")
				name = strings.TrimSpace(name)
			} else if first != o.suffix {
				continue // "my image omitted" is ordinary text
			}
			setMedia(m, o.typ, name, true, caption)
			return
		}
	}
	if sm := callRe.FindStringSubmatch(clean); sm != nil && !strings.Contains(clean, "\n") {
		m.Kind, m.SystemType, m.Text = ingest.KindCall, "phone_call", ""
		m.Call = &ingest.Call{
			Missed:      sm[1] != "",
			Video:       strings.EqualFold(sm[2], "video"),
			DurationSec: parseDuration(sm[3]),
		}
		return
	}
}

func setMedia(m *ingest.Message, t ingest.MediaType, name string, omitted bool, caption string) {
	m.Kind, m.MediaType, m.Text = ingest.KindMedia, t, caption
	m.Media = &ingest.MediaMeta{FileName: name, Omitted: omitted}
}

// pollQuestion extracts the question from "POLL:\n<question>\nOPTION: …".
func pollQuestion(body string) string {
	lines := strings.Split(body, "\n")
	if q := strings.TrimSpace(strings.TrimPrefix(lines[0], "POLL:")); q != "" {
		return q
	}
	if len(lines) > 1 {
		return strings.TrimSpace(lines[1])
	}
	return ""
}

// parseDuration reads "12 min", "1 hr 5 min" or "45 sec" into seconds.
func parseDuration(s string) int {
	total := 0
	for _, m := range durationRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch u := strings.ToLower(m[2]); {
		case strings.HasPrefix(u, "h"):
			total += n * 3600
		case strings.HasPrefix(u, "m"):
			total += n * 60
		default:
			total += n
		}
	}
	return total
}

// attributedSystem recognises system notices that iOS writes with the chat's
// name as the sender ("[…] Bob: Messages and calls are end-to-end
// encrypted…"). They are not the person's words, so the parser drops the
// sender. Only exact prefixes are matched: a user typing about encryption is
// still a normal message.
func attributedSystem(clean string) (string, bool) {
	for _, s := range []struct{ prefix, typ string }{
		{"Messages and calls are end-to-end encrypted", "encryption_notice"},
		{"Your security code with ", "security_code_changed"},
		{"Disappearing messages were turned ", "disappearing_toggle"},
		{"You blocked this contact", "block_toggle"},
		{"You unblocked this contact", "block_toggle"},
	} {
		if strings.HasPrefix(clean, s.prefix) {
			return s.typ, true
		}
	}
	return "", false
}

// systemType classifies a sender-less line (§7.1). The text is kept for
// display; system rows are excluded from every metric.
func systemType(text string) string {
	switch {
	case strings.Contains(text, "end-to-end encrypted"):
		return "encryption_notice"
	case strings.Contains(text, "changed their phone number"), strings.Contains(text, " changed to "):
		return "number_changed"
	case strings.Contains(text, "isappearing messages"), strings.Contains(text, "default message timer"),
		strings.Contains(text, "uses a default timer"):
		return "disappearing_toggle"
	case strings.HasPrefix(text, "You blocked this contact"), strings.HasPrefix(text, "You unblocked this contact"):
		return "block_toggle"
	case strings.Contains(text, "security code"):
		return "security_code_changed"
	case strings.HasSuffix(text, " is a contact."), strings.HasSuffix(text, " is a contact"):
		return "contact_info"
	case strings.HasPrefix(text, "Missed voice call"), strings.HasPrefix(text, "Missed video call"):
		return "call_missed"
	case strings.Contains(text, "created group"), strings.Contains(text, "created this group"):
		return "group_created"
	}
	return "system_other"
}
