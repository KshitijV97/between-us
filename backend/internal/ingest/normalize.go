package ingest

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// invisibleMarks are characters exports insert around names and times that
// carry no meaning: LEFT-TO-RIGHT MARK, RIGHT-TO-LEFT MARK, BOM, zero-width
// no-break space and NARROW NO-BREAK SPACE (iOS puts it before AM/PM).
var invisibleMarks = strings.NewReplacer(
	"\u200e", "", "\u200f", "", "\ufeff", "", "\u202a", "", "\u202c", "",
	"\u202f", " ", "\u00a0", " ",
)

// CleanText applies the canonical text normalisation used everywhere a body is
// stored or compared: invisible marks removed, NFC, CRLF → LF, trailing
// whitespace trimmed. Inner whitespace and newlines are preserved because they
// are part of the message.
func CleanText(s string) string {
	s = invisibleMarks.Replace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = norm.NFC.String(s)
	return strings.TrimRightFunc(strings.TrimLeft(s, "\n"), unicode.IsSpace)
}

// fingerprintText is the comparison form of a body for de-duplication: clean
// text with all whitespace runs collapsed, so line-ending or spacing changes
// between two exports of the same chat do not create duplicates.
func fingerprintText(m *Message) string {
	t := strings.Join(strings.Fields(CleanText(m.Text)), " ")
	if m.Kind != KindText {
		// Distinguish e.g. an omitted image from a text "ok" sent the same minute.
		t = "<" + string(m.Kind) + ":" + string(m.MediaType) + ":" + m.SystemType + ">" + t
	}
	return t
}

// FingerprintKey returns the de-duplication key material for m without the
// duplicate ordinal (docs/03-data-model.md §7.1). The importer counts
// identical keys within one export to derive dupOrdinal.
//
//   - Telegram (ExternalID set): message IDs are stable per chat, so the key is
//     the ID alone. Edits between two exports therefore do not duplicate.
//   - WhatsApp: sender, wall-clock time truncated to the minute (Android has no
//     seconds; re-exports from another device or with another chosen timezone
//     still match), and the normalised text. An edited WhatsApp message whose
//     text changed between exports cannot be matched and is imported again;
//     this is a documented limitation.
func FingerprintKey(m *Message) string {
	if m.ExternalID != "" {
		return "id\x1f" + m.ExternalID
	}
	return m.SenderKey + "\x1f" + m.LocalTime.Format("2006-01-02T15:04") + "\x1f" + fingerprintText(m)
}

// Fingerprint hashes the key with the duplicate ordinal, which keeps
// legitimately repeated messages ("ok", "ok" in the same minute) apart.
func Fingerprint(key string, dupOrdinal int) [32]byte {
	return sha256.Sum256([]byte(key + "\x1f" + strconv.Itoa(dupOrdinal)))
}
