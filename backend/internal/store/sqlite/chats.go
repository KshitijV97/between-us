package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/store"
)

// deleteBatch bounds each DELETE so a large chat is purged in short write
// transactions instead of one long lock (docs/03-data-model.md §7.2).
const deleteBatch = 5000

const chatColumns = `id, tenant_id, title, platform, analysis_tz, settings, message_count,
	first_message_at, last_message_at, created_by, created_at, archived_at`

// CreateChat implements store.Store. The tenant always comes from the scope,
// never from c.TenantID, so a caller cannot write into another tenant.
func (s *Store) CreateChat(ctx context.Context, c store.Chat) (store.Chat, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Chat{}, err
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.Must(uuid.NewV7())
	}
	if c.CreatedBy == uuid.Nil {
		c.CreatedBy = sc.UserID
	}
	if c.AnalysisTZ == "" {
		c.AnalysisTZ = "UTC"
	}
	now := time.Now()
	c.TenantID = sc.TenantID
	c.CreatedAt = fromMS(toMS(now))
	c.Settings = json.RawMessage(jsonOr(c.Settings, "{}"))
	c.MessageCount, c.FirstMessageAt, c.LastMessageAt = 0, nil, nil

	_, err = s.w.ExecContext(ctx, `
		INSERT INTO chats (id, tenant_id, title, platform, analysis_tz, settings, created_by, created_at, updated_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID.String(), c.TenantID.String(), c.Title, c.Platform, c.AnalysisTZ, string(c.Settings),
		nullUUID(c.CreatedBy), toMS(now), toMS(now), toNullMS(c.ArchivedAt))
	if err != nil {
		return store.Chat{}, mapErr(err)
	}
	return c, nil
}

// GetChat implements store.Store.
func (s *Store) GetChat(ctx context.Context, id uuid.UUID) (store.Chat, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Chat{}, err
	}
	row := s.r.QueryRowContext(ctx, `SELECT `+chatColumns+` FROM chats
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`, id.String(), sc.TenantID.String())
	return scanChat(row)
}

// ListChats implements store.Store; newest first.
func (s *Store) ListChats(ctx context.Context) ([]store.Chat, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+chatColumns+` FROM chats
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY created_at DESC, id DESC`, sc.TenantID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

// DeleteChat implements store.Store: messages first in bounded batches (FTS
// rows go with them via trigger), then the chat row, whose foreign keys
// cascade to participants and imports.
func (s *Store) DeleteChat(ctx context.Context, id uuid.UUID) error {
	sc, err := scope(ctx)
	if err != nil {
		return err
	}
	if err := s.requireChat(ctx, sc.TenantID, id); err != nil {
		return err
	}
	if err := s.deleteMessagesWhere(ctx, "chat_id = ? AND tenant_id = ?", id.String(), sc.TenantID.String()); err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM chats WHERE id = ? AND tenant_id = ?`, id.String(), sc.TenantID.String())
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// deleteMessagesWhere deletes matching messages deleteBatch rows at a time,
// each batch in its own short transaction.
func (s *Store) deleteMessagesWhere(ctx context.Context, where string, args ...any) error {
	// #nosec G202 -- where is a constant fragment supplied by callers in this
	// package; all values are bound parameters.
	q := `DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE ` + where + fmt.Sprintf(` LIMIT %d)`, deleteBatch)
	for {
		res, err := s.w.ExecContext(ctx, q, args...)
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr(err)
		}
		if n < deleteBatch {
			return nil
		}
	}
}

// requireChat returns store.ErrNotFound unless the chat exists in the tenant.
// List methods use it so another tenant's chat ID behaves like a missing one.
func (s *Store) requireChat(ctx context.Context, tenantID, chatID uuid.UUID) error {
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM chats WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
		chatID.String(), tenantID.String()).Scan(&one)
	return mapErr(err)
}

// ListParticipants implements store.Store.
func (s *Store) ListParticipants(ctx context.Context, chatID uuid.UUID) ([]store.Participant, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT id, tenant_id, chat_id, display_name, alias, is_me, platform_keys
		FROM participants WHERE chat_id = ? AND tenant_id = ? ORDER BY created_at, id`,
		chatID.String(), sc.TenantID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Participant
	for rows.Next() {
		var (
			p    store.Participant
			keys string
		)
		if err := rows.Scan(&p.ID, &p.TenantID, &p.ChatID, &p.DisplayName, &p.Alias, &p.IsMe, &keys); err != nil {
			return nil, mapErr(err)
		}
		if err := json.Unmarshal([]byte(keys), &p.PlatformKeys); err != nil {
			return nil, fmt.Errorf("sqlite: participant %s platform_keys: %w", p.ID, err)
		}
		out = append(out, p)
	}
	return out, mapErr(rows.Err())
}

// UpsertParticipant implements store.Store. With p.ID unset it creates a new
// participant; otherwise it updates that participant within the tenant, or
// creates it with the given ID if it does not exist anywhere. An ID that
// belongs to another tenant yields store.ErrNotFound.
func (s *Store) UpsertParticipant(ctx context.Context, p store.Participant) (store.Participant, error) {
	sc, err := scope(ctx)
	if err != nil {
		return store.Participant{}, err
	}
	if err := s.requireChat(ctx, sc.TenantID, p.ChatID); err != nil {
		return store.Participant{}, err
	}
	p.TenantID = sc.TenantID
	if p.PlatformKeys == nil {
		p.PlatformKeys = []string{}
	}
	keys, err := json.Marshal(p.PlatformKeys)
	if err != nil {
		return store.Participant{}, err
	}
	now := toMS(time.Now())

	if p.ID != uuid.Nil {
		res, err := s.w.ExecContext(ctx, `
			UPDATE participants SET display_name = ?, alias = ?, is_me = ?, platform_keys = ?, updated_at = ?
			WHERE id = ? AND tenant_id = ? AND chat_id = ?`,
			p.DisplayName, p.Alias, boolInt(p.IsMe), string(keys), now, p.ID.String(), sc.TenantID.String(), p.ChatID.String())
		if err != nil {
			return store.Participant{}, mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return p, nil
		}
	} else {
		p.ID = uuid.Must(uuid.NewV7())
	}

	_, err = s.w.ExecContext(ctx, `
		INSERT INTO participants (id, tenant_id, chat_id, display_name, alias, is_me, platform_keys, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID.String(), sc.TenantID.String(), p.ChatID.String(), p.DisplayName, p.Alias, boolInt(p.IsMe), string(keys), now, now)
	if err != nil {
		err = mapErr(err)
		// A primary-key clash means the ID exists in another tenant or chat.
		var exists int
		if errors.Is(err, store.ErrConflict) &&
			s.r.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE id = ?`, p.ID.String()).Scan(&exists) == nil {
			return store.Participant{}, store.ErrNotFound
		}
		return store.Participant{}, err
	}
	return p, nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanChat(r rowScanner) (store.Chat, error) {
	var (
		c                    store.Chat
		settings             string
		first, last, archive sql.NullInt64
		createdBy            uuid.NullUUID
		created              int64
	)
	err := r.Scan(&c.ID, &c.TenantID, &c.Title, &c.Platform, &c.AnalysisTZ, &settings, &c.MessageCount,
		&first, &last, &createdBy, &created, &archive)
	if err != nil {
		return store.Chat{}, mapErr(err)
	}
	c.Settings = json.RawMessage(settings)
	c.FirstMessageAt, c.LastMessageAt, c.ArchivedAt = fromNullMS(first), fromNullMS(last), fromNullMS(archive)
	c.CreatedBy = createdBy.UUID
	c.CreatedAt = fromMS(created)
	return c, nil
}
