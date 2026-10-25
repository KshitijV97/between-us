package telegram

import (
	"bytes"
	"encoding/json"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// Detect implements ingest.Parser (spec §4.2). It walks the JSON tokens of the
// sample (which is usually truncated, so decoding errors simply end the walk)
// looking for a top-level "messages" array whose first element has
// "date_unixtime", or a "chats" → "list" structure. Anything that is not a JSON
// object, such as WhatsApp text, scores 0.
func (Parser) Detect(sample []byte) ingest.Detection {
	det := ingest.Detection{Platform: ingest.PlatformTelegram, Variant: VariantSingle}
	sample = bytes.TrimPrefix(sample, []byte{0xEF, 0xBB, 0xBF})
	trimmed := bytes.TrimLeft(sample, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return det
	}

	s := sniff(trimmed)
	switch {
	case s.messagesWithUnixtime:
		det.Confidence = 1
	case s.chatsList:
		det.Confidence, det.Variant = 1, VariantAccount
	case s.accountKeys:
		// Account exports can start with long contact lists; the chat list
		// may lie beyond the sample.
		det.Confidence, det.Variant = 0.8, VariantAccount
	case s.emptyMessages && s.typ != "":
		det.Confidence = 0.8
	default:
		det.Confidence = 0.1
		det.Hints = append(det.Hints, "This is a JSON file but not a Telegram Desktop export. In Telegram Desktop use Export chat history → Format: Machine-readable JSON.")
		return det
	}
	if s.typ != "" && s.typ != "personal_chat" {
		det.Hints = append(det.Hints, "This Telegram export is a "+s.typ+"; only one-to-one chats (personal_chat) can be analysed.")
	}
	return det
}

// sniffResult records the structural evidence found in a sample.
type sniffResult struct {
	typ                  string
	messagesWithUnixtime bool
	emptyMessages        bool
	chatsList            bool
	accountKeys          bool
}

// sniff walks the top-level object. It stops at the first decoding error
// (normally the truncation point of the sample).
func sniff(b []byte) sniffResult {
	var s sniffResult
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return s
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return s
		}
		key, _ := tok.(string)
		switch key {
		case "type":
			var v any
			if dec.Decode(&v) != nil {
				return s
			}
			s.typ, _ = v.(string)
		case "messages":
			if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
				return s
			}
			if !dec.More() {
				s.emptyMessages = true
				return s
			}
			s.messagesWithUnixtime = firstObjectHasKey(dec, "date_unixtime")
			return s
		case "chats":
			if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
				return s
			}
			for dec.More() {
				tok, err := dec.Token()
				if err != nil {
					return s
				}
				if tok == "list" {
					if tok, err := dec.Token(); err == nil && tok == json.Delim('[') {
						s.chatsList = true
					}
					return s
				}
				if skipValue(dec) != nil {
					return s
				}
			}
			return s
		case "personal_information", "contacts", "frequent_contacts", "left_chats":
			s.accountKeys = true
			if skipValue(dec) != nil {
				return s
			}
		default:
			if skipValue(dec) != nil {
				return s
			}
		}
	}
	return s
}

// firstObjectHasKey reports whether the next value is an object containing key.
func firstObjectHasKey(dec *json.Decoder, key string) bool {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if tok == key {
			return true
		}
		if skipValue(dec) != nil {
			return false
		}
	}
	return false
}
