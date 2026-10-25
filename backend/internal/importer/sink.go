package importer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
	"github.com/kshitijv/chat-analyzer/backend/internal/store"
)

// maxDiagnostics bounds how many diagnostics are kept for the report.
const maxDiagnostics = 200

// storeSink converts canonical messages to store rows and inserts them batch
// by batch. It holds only per-minute de-duplication state, never the whole
// chat (FR-ING-013).
type storeSink struct {
	store    store.Store
	importID uuid.UUID
	chatID   uuid.UUID
	chatLoc  *time.Location
	senders  map[string]uuid.UUID

	nextOrdinal int64
	// Duplicate ordinals (docs/03-data-model.md §7.1) only matter among
	// messages with an identical fingerprint key. WhatsApp keys contain the
	// minute and Telegram keys are unique IDs, so state for the current
	// minute suffices; exports are chronological.
	dupMinute string
	dupCounts map[string]int

	total, inserted, filtered int
	diagnostics               []ingest.Diagnostic
}

// Batch implements ingest.Sink.
func (s *storeSink) Batch(ctx context.Context, msgs []ingest.Message) error {
	rows := make([]store.Message, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		var pid *uuid.UUID
		if m.SenderKey != "" {
			id, ok := s.senders[m.SenderKey]
			if !ok {
				// A sender the user chose not to analyse (FR-ING-012).
				s.filtered++
				continue
			}
			pid = &id
		}
		key := ingest.FingerprintKey(m)
		fp := ingest.Fingerprint(key, s.dupOrdinal(m, key))
		rows = append(rows, store.Message{
			ChatID:            s.chatID,
			ImportID:          s.importID,
			ParticipantID:     pid,
			Ordinal:           s.nextOrdinal,
			SentAt:            m.SentAt.UTC(),
			LocalDate:         m.SentAt.In(s.chatLoc).Format(time.DateOnly),
			Kind:              string(m.Kind),
			MediaType:         string(m.MediaType),
			SystemType:        m.SystemType,
			Body:              m.Text,
			ReplyToPlatformID: m.ReplyToExternalID,
			IsEdited:          m.Edited,
			IsForwarded:       m.Forwarded,
			IsDeleted:         m.Deleted,
			PlatformMsgID:     m.ExternalID,
			Fingerprint:       fp[:],
			Meta:              metaJSON(m),
		})
		s.nextOrdinal++
	}
	s.total += len(rows)
	if len(rows) == 0 {
		return nil
	}
	n, err := s.store.InsertMessages(ctx, rows)
	s.inserted += n
	return err
}

// Diagnostic implements ingest.Sink.
func (s *storeSink) Diagnostic(d ingest.Diagnostic) {
	if len(s.diagnostics) < maxDiagnostics {
		s.diagnostics = append(s.diagnostics, d)
	}
}

func (s *storeSink) dupOrdinal(m *ingest.Message, key string) int {
	if m.ExternalID != "" {
		return 0 // IDs are unique
	}
	minute := m.LocalTime.Format("2006-01-02T15:04")
	if minute != s.dupMinute || s.dupCounts == nil {
		s.dupMinute, s.dupCounts = minute, map[string]int{}
	}
	n := s.dupCounts[key]
	s.dupCounts[key] = n + 1
	return n
}

// reportWithStats is the JSON persisted on the import row: the parser's
// report plus what the importer did with it.
type reportWithStats struct {
	ingest.Report
	Inserted    int                      `json:"inserted"`
	Duplicates  int                      `json:"duplicates"`
	Filtered    int                      `json:"filtered,omitempty"`
	MediaFiles  map[ingest.MediaType]int `json:"media_files,omitempty"`
	Diagnostics []ingest.Diagnostic      `json:"diagnostics,omitempty"`
}

func (s *storeSink) decorate(r ingest.Report, media map[ingest.MediaType]int) reportWithStats {
	return reportWithStats{
		Report: r, Inserted: s.inserted, Duplicates: s.total - s.inserted,
		Filtered: s.filtered, MediaFiles: media, Diagnostics: s.diagnostics,
	}
}

// messageMeta is the JSON stored in messages.meta: platform extras that the
// MVP keeps for later analysis but does not query.
type messageMeta struct {
	Media     *ingest.MediaMeta `json:"media,omitempty"`
	Call      *ingest.Call      `json:"call,omitempty"`
	Reactions []ingest.Reaction `json:"reactions,omitempty"`
	EditedAt  *time.Time        `json:"edited_at,omitempty"`
	Precision string            `json:"precision,omitempty"`
}

func metaJSON(m *ingest.Message) json.RawMessage {
	meta := messageMeta{Media: m.Media, Call: m.Call, Reactions: m.Reactions, EditedAt: m.EditedAt}
	if m.TimePrecision == ingest.PrecisionMinute {
		meta.Precision = string(m.TimePrecision)
	}
	b, err := json.Marshal(meta)
	if err != nil || string(b) == "{}" {
		return json.RawMessage(`{}`)
	}
	return b
}
