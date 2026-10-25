package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// rawMessage mirrors the subset of a Telegram Desktop export message that the
// parser maps (docs/parsers/telegram.md §2.2). Unknown fields are skipped by
// encoding/json. Numbers that are identifiers use json.Number so large values
// stay exact.
type rawMessage struct {
	ID         json.Number `json:"id"`
	Type       string      `json:"type"` // "message" | "service"
	Date       string      `json:"date"`
	DateUnix   string      `json:"date_unixtime"`
	Edited     string      `json:"edited"`
	EditedUnix string      `json:"edited_unixtime"`

	// From is a pointer so a JSON null (deleted account) is distinguishable
	// from an absent field.
	From    *string `json:"from"`
	FromID  string  `json:"from_id"`
	Actor   *string `json:"actor"`
	ActorID string  `json:"actor_id"`

	Action        string      `json:"action"`
	DiscardReason string      `json:"discard_reason"`
	Duration      json.Number `json:"duration_seconds"`

	Text         flexText `json:"text"`
	TextEntities []entity `json:"text_entities"`

	ReplyTo json.Number `json:"reply_to_message_id"`
	// Presence (even null) of these keys marks forwarded/saved content, so
	// they are kept raw.
	ForwardedFrom json.RawMessage `json:"forwarded_from"`
	SavedFrom     json.RawMessage `json:"saved_from"`

	MediaType    string      `json:"media_type"`
	Photo        string      `json:"photo"`
	File         string      `json:"file"`
	FileName     string      `json:"file_name"`
	FileSize     json.Number `json:"file_size"`
	PhotoSize    json.Number `json:"photo_file_size"`
	MimeType     string      `json:"mime_type"`
	StickerEmoji string      `json:"sticker_emoji"`

	Poll *struct {
		Question string `json:"question"`
	} `json:"poll"`
	// Location and contact payloads are only tested for presence; their
	// content (coordinates, vCards) is never kept (PRV-004).
	LocationInformation json.RawMessage `json:"location_information"`
	LiveLocationPeriod  json.Number     `json:"live_location_period_seconds"`
	ContactInformation  *struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"contact_information"`

	Reactions []rawReaction `json:"reactions"`
}

// entity is one element of text_entities. Only the text matters for
// flattening; the type is used for link counting downstream (M2).
type entity struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rawReaction struct {
	Type       string `json:"type"` // emoji | custom_emoji | paid
	Count      int    `json:"count"`
	Emoji      string `json:"emoji"`
	DocumentID string `json:"document_id"`
}

// flexText decodes Telegram's "text" field, which is either a plain string or
// an array mixing strings and entity objects {type, text, ...}. The result is
// the concatenated plain text (spec §4.1).
type flexText string

func (t *flexText) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*t = ""
		return nil
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = flexText(s)
		return nil
	case b[0] == '[':
		var parts []json.RawMessage
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			p = bytes.TrimSpace(p)
			if len(p) == 0 {
				continue
			}
			if p[0] == '"' {
				var s string
				if err := json.Unmarshal(p, &s); err != nil {
					return err
				}
				sb.WriteString(s)
				continue
			}
			var e entity
			if err := json.Unmarshal(p, &e); err != nil {
				return err
			}
			sb.WriteString(e.Text)
		}
		*t = flexText(sb.String())
		return nil
	default:
		return errors.New("telegram: text is neither string nor array")
	}
}

// flatText returns the message's plain text. text_entities is preferred
// because it is always an array of plain parts; text is the fallback for
// older exports without entities.
func (m *rawMessage) flatText() string {
	if len(m.TextEntities) > 0 {
		var sb strings.Builder
		for _, e := range m.TextEntities {
			sb.WriteString(e.Text)
		}
		return sb.String()
	}
	return string(m.Text)
}

// present reports whether a raw optional field appeared in the JSON (a JSON
// null counts as present).
func present(r json.RawMessage) bool { return len(r) > 0 }
