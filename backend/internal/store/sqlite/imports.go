package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv/chat-analyzer/backend/internal/store"
)

const importColumns = `id, tenant_id, chat_id, created_by, platform, variant, status, timezone, time_precision,
	file_name, file_size, file_sha256, report, inserted_count, duplicate_count, error, started_at, completed_at`

// CreateImport implements store.Store. The chat must belong to the tenant.
func (s *Store) CreateImport(ctx context.Context, im store.Import) (store.Import, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Import{}, err
	}
	if err := s.requireChat(ctx, sc.TenantID, im.ChatID); err != nil {
		return store.Import{}, err
	}
	if im.ID == uuid.Nil {
		im.ID = uuid.Must(uuid.NewV7())
	}
	if im.CreatedBy == uuid.Nil {
		im.CreatedBy = sc.UserID
	}
	if im.Status == "" {
		im.Status = store.ImportProcessing
	}
	now := time.Now()
	if im.StartedAt.IsZero() {
		im.StartedAt = now
	}
	im.StartedAt = fromMS(toMS(im.StartedAt))
	im.TenantID = sc.TenantID
	im.Report = json.RawMessage(jsonOr(im.Report, "{}"))

	_, err = s.w.ExecContext(ctx, `
		INSERT INTO imports (id, tenant_id, chat_id, created_by, platform, variant, status, timezone, time_precision,
			file_name, file_size, file_sha256, report, inserted_count, duplicate_count, error,
			started_at, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		im.ID.String(), im.TenantID.String(), im.ChatID.String(), nullUUID(im.CreatedBy), im.Platform, im.Variant,
		im.Status, im.Timezone, im.TimePrecision, im.FileName, im.FileSize, nullBytes(im.FileSHA256),
		string(im.Report), im.InsertedCount, im.DuplicateCount, im.Error,
		toMS(im.StartedAt), toNullMS(im.CompletedAt), toMS(now), toMS(now))
	if err != nil {
		return store.Import{}, mapErr(err)
	}
	return im, nil
}

// GetImport implements store.Store.
func (s *Store) GetImport(ctx context.Context, id uuid.UUID) (store.Import, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Import{}, err
	}
	return scanImport(s.r.QueryRowContext(ctx, `SELECT `+importColumns+` FROM imports WHERE id = ? AND tenant_id = ?`,
		id.String(), sc.TenantID.String()))
}

// ListImports implements store.Store; newest first.
func (s *Store) ListImports(ctx context.Context, chatID uuid.UUID) ([]store.Import, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+importColumns+` FROM imports
		WHERE chat_id = ? AND tenant_id = ? ORDER BY started_at DESC, id DESC`, chatID.String(), sc.TenantID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Import
	for rows.Next() {
		im, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, mapErr(rows.Err())
}

// FindCompletedImportBySHA256 implements store.Store.
func (s *Store) FindCompletedImportBySHA256(ctx context.Context, chatID uuid.UUID, sum []byte) (store.Import, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Import{}, err
	}
	if len(sum) == 0 {
		return store.Import{}, store.ErrNotFound
	}
	return scanImport(s.r.QueryRowContext(ctx, `SELECT `+importColumns+` FROM imports
		WHERE chat_id = ? AND tenant_id = ? AND file_sha256 = ? AND status = 'completed'
		ORDER BY completed_at DESC LIMIT 1`, chatID.String(), sc.TenantID.String(), sum))
}

// CompleteImport implements store.Store. Everything happens in one write
// transaction, so readers see either the old chat or the fully imported one,
// never a half-applied state (NFR-REL-002).
func (s *Store) CompleteImport(ctx context.Context, im store.Import) error {
	sc, err := scope(ctx)
	if err != nil {
		return err
	}
	completed := time.Now()
	if im.CompletedAt != nil && !im.CompletedAt.IsZero() {
		completed = *im.CompletedAt
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var chatID string
		err := tx.QueryRowContext(ctx, `SELECT chat_id FROM imports WHERE id = ? AND tenant_id = ?`,
			im.ID.String(), sc.TenantID.String()).Scan(&chatID)
		if err != nil {
			return mapErr(err)
		}

		// Only fields that are known at the end of an import are updated; the
		// identity fields (chat, platform, file) were fixed at creation.
		if _, err := tx.ExecContext(ctx, `
			UPDATE imports SET status = 'completed', variant = ?, time_precision = ?, report = ?,
				inserted_count = ?, duplicate_count = ?, error = '', completed_at = ?, updated_at = ?
			WHERE id = ?`,
			im.Variant, im.TimePrecision, jsonOr(im.Report, "{}"), im.InsertedCount, im.DuplicateCount,
			toMS(completed), toMS(time.Now()), im.ID.String()); err != nil {
			return mapErr(err)
		}

		// Link replies (Telegram reply_to_message_id) to our row IDs. A target
		// may have arrived in an earlier import, so the lookup spans the chat.
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET reply_to_id = (
				SELECT t.id FROM messages t
				WHERE t.chat_id = messages.chat_id AND t.platform_msg_id = messages.reply_to_platform_id
				ORDER BY t.id LIMIT 1)
			WHERE chat_id = ? AND reply_to_platform_id <> '' AND reply_to_id IS NULL`, chatID); err != nil {
			return mapErr(err)
		}

		return refreshChatCounters(ctx, tx, chatID)
	})
}

// refreshChatCounters recomputes the denormalised counters on chats from the
// visible messages. message_count counts every visible row (system events
// included), matching CountMessages.
func refreshChatCounters(ctx context.Context, tx *sql.Tx, chatID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE chats SET (message_count, first_message_at, last_message_at, updated_at) = (
			SELECT COUNT(*), MIN(m.sent_at), MAX(m.sent_at), ?
			FROM messages m JOIN imports i ON i.id = m.import_id
			WHERE m.chat_id = chats.id AND i.status = 'completed')
		WHERE id = ?`, toMS(time.Now()), chatID)
	return mapErr(err)
}

// FailImport implements store.Store. The import is marked failed first, which
// already hides its messages; they are then deleted in bounded batches.
func (s *Store) FailImport(ctx context.Context, importID uuid.UUID, userError string, report json.RawMessage) error {
	sc, err := scope(ctx)
	if err != nil {
		return err
	}
	now := toMS(time.Now())
	res, err := s.w.ExecContext(ctx, `
		UPDATE imports SET status = 'failed', error = ?, report = CASE WHEN ? = '' THEN report ELSE ? END,
			completed_at = ?, updated_at = ?
		WHERE id = ? AND tenant_id = ?`,
		userError, string(report), string(report), now, now, importID.String(), sc.TenantID.String())
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return s.deleteMessagesWhere(ctx, "import_id = ? AND tenant_id = ?", importID.String(), sc.TenantID.String())
}

func scanImport(r rowScanner) (store.Import, error) {
	var (
		im        store.Import
		createdBy uuid.NullUUID
		report    string
		started   int64
		completed sql.NullInt64
	)
	err := r.Scan(&im.ID, &im.TenantID, &im.ChatID, &createdBy, &im.Platform, &im.Variant, &im.Status,
		&im.Timezone, &im.TimePrecision, &im.FileName, &im.FileSize, &im.FileSHA256, &report,
		&im.InsertedCount, &im.DuplicateCount, &im.Error, &started, &completed)
	if err != nil {
		return store.Import{}, mapErr(err)
	}
	im.CreatedBy = createdBy.UUID
	im.Report = json.RawMessage(report)
	im.StartedAt = fromMS(started)
	im.CompletedAt = fromNullMS(completed)
	return im, nil
}

// nullBytes stores an empty slice as NULL so the partial index on file_sha256
// stays small.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
