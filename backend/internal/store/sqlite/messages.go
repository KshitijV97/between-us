package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/store"
)

// Page size bounds for ListMessages and SearchMessages.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

const messageColumns = `m.id, m.tenant_id, m.chat_id, m.import_id, m.participant_id, m.ordinal, m.sent_at,
	m.local_date, m.kind, m.media_type, m.event_type, m.body, m.reply_to_id, m.reply_to_platform_id,
	m.is_edited, m.is_forwarded, m.is_deleted_marker, m.platform_msg_id, m.fingerprint, m.meta`

// visibleJoin restricts messages to completed imports (see package docs).
const visibleJoin = ` JOIN imports i ON i.id = m.import_id AND i.status = 'completed' `

// InsertMessages implements store.Store. All rows go in one write transaction
// as multi-row INSERTs; ON CONFLICT DO NOTHING skips fingerprints the chat
// already has, and RowsAffected tells inserted rows from duplicates.
//
// Every chat and import referenced by the batch is checked against the scope
// first: the foreign keys alone would accept another tenant's chat.
func (s *Store) InsertMessages(ctx context.Context, msgs []store.Message) (int, error) {
	sc, err := scope(ctx)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	inserted := 0
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkOwnership(ctx, tx, sc.TenantID, msgs); err != nil {
			return err
		}
		tenant := sc.TenantID.String()
		for i := range msgs {
			if len(msgs[i].Fingerprint) == 0 {
				return fmt.Errorf("sqlite: message %d has no fingerprint", i)
			}
		}
		// Multi-row INSERTs amortise the per-statement cost of the pure-Go
		// driver: full chunks reuse one prepared statement, the remainder gets
		// its own.
		full, err := tx.PrepareContext(ctx, insertSQL(rowsPerInsert))
		if err != nil {
			return mapErr(err)
		}
		defer full.Close()
		args := make([]any, 0, rowsPerInsert*insertCols)
		for start := 0; start < len(msgs); start += rowsPerInsert {
			chunk := msgs[start:min(start+rowsPerInsert, len(msgs))]
			args = args[:0]
			for j := range chunk {
				args = appendMessageArgs(args, tenant, &chunk[j])
			}
			stmt := full
			if len(chunk) < rowsPerInsert {
				if stmt, err = tx.PrepareContext(ctx, insertSQL(len(chunk))); err != nil {
					return mapErr(err)
				}
				defer stmt.Close()
			}
			res, err := stmt.ExecContext(ctx, args...)
			if err != nil {
				return mapErr(err)
			}
			// With ON CONFLICT DO NOTHING, RowsAffected counts only the rows
			// actually inserted, which is exactly the de-duplicated count.
			n, err := res.RowsAffected()
			if err != nil {
				return mapErr(err)
			}
			inserted += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// rowsPerInsert × insertCols must stay below SQLite's bound-parameter limit
// (32766); 128 rows was the throughput sweet spot in benchmarks.
const (
	rowsPerInsert = 128
	insertCols    = 18
)

// insertSQL builds a multi-row INSERT for n messages. Duplicates of an
// existing (chat_id, fingerprint) are skipped, not errors (FR-ING-010).
func insertSQL(n int) string {
	var b strings.Builder
	b.WriteString(`INSERT INTO messages (tenant_id, chat_id, import_id, participant_id, ordinal, sent_at, local_date,
		kind, media_type, event_type, body, reply_to_platform_id, is_edited, is_forwarded,
		is_deleted_marker, platform_msg_id, fingerprint, meta) VALUES `)
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", insertCols), ",") + ")"
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(row)
	}
	b.WriteString(` ON CONFLICT (chat_id, fingerprint) DO NOTHING`)
	return b.String()
}

// appendMessageArgs appends the insertCols bind values of m in column order.
func appendMessageArgs(args []any, tenant string, m *store.Message) []any {
	var participant any
	if m.ParticipantID != nil && *m.ParticipantID != uuid.Nil {
		participant = m.ParticipantID.String()
	}
	return append(args, tenant, m.ChatID.String(), m.ImportID.String(), participant,
		m.Ordinal, toMS(m.SentAt), m.LocalDate, m.Kind, m.MediaType, m.SystemType, m.Body,
		m.ReplyToPlatformID, boolInt(m.IsEdited), boolInt(m.IsForwarded), boolInt(m.IsDeleted),
		m.PlatformMsgID, m.Fingerprint, jsonOr(m.Meta, "{}"))
}

// checkOwnership verifies that every (chat, import) pair in msgs belongs to
// the tenant and that each import is still writable. Batches normally hold a
// single pair, so this is one or two indexed lookups per call.
func checkOwnership(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, msgs []store.Message) error {
	type pair struct{ chat, imp uuid.UUID }
	seen := map[pair]bool{}
	for i := range msgs {
		p := pair{msgs[i].ChatID, msgs[i].ImportID}
		if seen[p] {
			continue
		}
		seen[p] = true
		var status string
		err := tx.QueryRowContext(ctx, `
			SELECT i.status FROM imports i JOIN chats c ON c.id = i.chat_id
			WHERE i.id = ? AND i.chat_id = ? AND i.tenant_id = ? AND c.tenant_id = ?`,
			p.imp.String(), p.chat.String(), tenantID.String(), tenantID.String()).Scan(&status)
		if err != nil {
			return mapErr(err)
		}
		if status == store.ImportCompleted || status == store.ImportFailed {
			return fmt.Errorf("%w: import %s is %s", store.ErrConflict, p.imp, status)
		}
	}
	return nil
}

// MaxOrdinal implements store.Store. It includes rows of imports still in
// progress, so ordinals stay unique while an import is running.
func (s *Store) MaxOrdinal(ctx context.Context, chatID uuid.UUID) (int64, error) {
	sc, err := scope(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return 0, err
	}
	var max int64
	// Read through the writer so a just-inserted batch is always included.
	err = s.w.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal), 0) FROM messages WHERE chat_id = ? AND tenant_id = ?`,
		chatID.String(), sc.TenantID.String()).Scan(&max)
	return max, mapErr(err)
}

// ListMessages implements store.Store.
func (s *Store) ListMessages(ctx context.Context, q store.MessageQuery) ([]store.Message, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, q.ChatID); err != nil {
		return nil, err
	}
	var (
		where = []string{"m.chat_id = ?", "m.tenant_id = ?", "m.ordinal > ?"}
		args  = []any{q.ChatID.String(), sc.TenantID.String(), q.AfterOrdinal}
	)
	if !q.From.IsZero() {
		where, args = append(where, "m.sent_at >= ?"), append(args, toMS(q.From))
	}
	if !q.To.IsZero() {
		where, args = append(where, "m.sent_at < ?"), append(args, toMS(q.To))
	}
	args = append(args, clampLimit(q.Limit))
	return s.queryMessages(ctx, `SELECT `+messageColumns+` FROM messages m`+visibleJoin+
		`WHERE `+strings.Join(where, " AND ")+` ORDER BY m.sent_at, m.ordinal LIMIT ?`, args...)
}

// CountMessages implements store.Store.
func (s *Store) CountMessages(ctx context.Context, chatID uuid.UUID) (int64, error) {
	sc, err := scope(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return 0, err
	}
	var n int64
	err = s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m`+visibleJoin+
		`WHERE m.chat_id = ? AND m.tenant_id = ?`, chatID.String(), sc.TenantID.String()).Scan(&n)
	return n, mapErr(err)
}

// SearchMessages implements store.Store using FTS5 with bm25 ranking. The FTS
// table is global, so tenant, chat and visibility filters are applied by
// joining back to messages.
func (s *Store) SearchMessages(ctx context.Context, chatID uuid.UUID, query string, limit int) ([]store.Message, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	return s.queryMessages(ctx, `SELECT `+messageColumns+`
		FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid`+visibleJoin+`
		WHERE messages_fts MATCH ? AND m.chat_id = ? AND m.tenant_id = ?
		ORDER BY bm25(messages_fts), m.sent_at LIMIT ?`,
		match, chatID.String(), sc.TenantID.String(), clampLimit(limit))
}

// ftsQuery turns free text into a safe FTS5 query: every whitespace-separated
// term becomes a quoted string (so operators such as AND, NEAR( and stray
// quotes are plain text), terms are implicitly AND-ed, and a trailing "*" on a
// term is kept as a prefix search. Characters that the tokenizer drops anyway
// are removed so a term like `"` does not become an empty phrase.
func ftsQuery(q string) string {
	var terms []string
	for _, f := range strings.Fields(q) {
		prefix := strings.HasSuffix(f, "*")
		f = strings.Map(func(r rune) rune {
			if r == '"' || r == '*' {
				return -1
			}
			return r
		}, f)
		if !strings.ContainsFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
			continue
		}
		t := `"` + f + `"`
		if prefix {
			t += " *"
		}
		terms = append(terms, t)
	}
	return strings.Join(terms, " ")
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}

// queryMessages runs a query selecting messageColumns and scans the rows.
func (s *Store) queryMessages(ctx context.Context, query string, args ...any) ([]store.Message, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var (
			m           store.Message
			participant uuid.NullUUID
			replyTo     sql.NullInt64
			sent        int64
			meta        string
		)
		if err := rows.Scan(&m.ID, &m.TenantID, &m.ChatID, &m.ImportID, &participant, &m.Ordinal, &sent,
			&m.LocalDate, &m.Kind, &m.MediaType, &m.SystemType, &m.Body, &replyTo, &m.ReplyToPlatformID,
			&m.IsEdited, &m.IsForwarded, &m.IsDeleted, &m.PlatformMsgID, &m.Fingerprint, &meta); err != nil {
			return nil, mapErr(err)
		}
		if participant.Valid {
			id := participant.UUID
			m.ParticipantID = &id
		}
		if replyTo.Valid {
			id := replyTo.Int64
			m.ReplyToID = &id
		}
		m.SentAt = fromMS(sent)
		m.Meta = json.RawMessage(meta)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}
