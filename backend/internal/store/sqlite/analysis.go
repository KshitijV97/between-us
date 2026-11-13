package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/store"
)

// UpdateChatSettings implements store.Store. Validation of the settings
// themselves belongs to analytics.ParseSettings; the column's json_valid CHECK
// only guards against malformed JSON.
func (s *Store) UpdateChatSettings(ctx context.Context, chatID uuid.UUID, settings json.RawMessage) error {
	sc, err := scope(ctx)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, `UPDATE chats SET settings = ?, updated_at = ?
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
		jsonOr(settings, "{}"), toMS(time.Now()), chatID.String(), sc.TenantID.String())
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// analysisColumns feeds IterateAnalysisMessages. i.time_precision comes from
// the visibility join, so precision costs no extra lookup.
const analysisColumns = `m.id, m.participant_id, m.sent_at, m.ordinal, m.kind, m.media_type, m.body,
	m.is_edited, m.is_forwarded, m.is_deleted_marker, m.meta, i.time_precision,
	m.session_index, m.is_turn_start, m.response_s, m.word_count, m.char_count, m.emoji_count,
	m.link_count, m.is_question, m.sentiment, m.affection_hits, m.conflict`

// analysisMeta is the subset of messages.meta the analytics engine needs. The
// importer writes ingest.Call without JSON tags, hence the Go field names
// (encoding/json matches keys case-insensitively anyway).
type analysisMeta struct {
	Call *struct {
		Video       bool
		Missed      bool
		DurationSec int
	} `json:"call"`
	Precision string `json:"precision"`
}

// IterateAnalysisMessages implements store.Store. Rows stream from a reader
// connection, so an AnalysisWriter can update feature columns on the writer
// connection while the iteration runs (WAL keeps the reader's snapshot
// consistent). In-memory databases have a single connection: there, fn must
// not use the store and no AnalysisWriter may be open during iteration.
func (s *Store) IterateAnalysisMessages(ctx context.Context, chatID uuid.UUID, from, to time.Time, fn func(*analytics.Message) error) error {
	sc, err := scope(ctx)
	if err != nil {
		return err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return err
	}
	where := []string{"m.chat_id = ?", "m.tenant_id = ?"}
	args := []any{chatID.String(), sc.TenantID.String()}
	if !from.IsZero() {
		where, args = append(where, "m.sent_at >= ?"), append(args, toMS(from))
	}
	if !to.IsZero() {
		where, args = append(where, "m.sent_at < ?"), append(args, toMS(to))
	}
	// #nosec G202 -- columns, join and conditions are constant fragments; values are bound parameters.
	rows, err := s.r.QueryContext(ctx, `SELECT `+analysisColumns+` FROM messages m`+visibleJoin+
		`WHERE `+strings.Join(where, " AND ")+` ORDER BY m.sent_at, m.ordinal`, args...)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()

	var (
		m           analytics.Message // reused per row; fn must not retain it
		call        analytics.CallInfo
		participant uuid.NullUUID
		sent        int64
		meta, prec  string
		responseS   sql.NullInt64
		sentiment   sql.NullFloat64
	)
	for rows.Next() {
		m = analytics.Message{}
		if err := rows.Scan(&m.ID, &participant, &sent, &m.Ordinal, &m.Kind, &m.MediaType, &m.Body,
			&m.IsEdited, &m.IsForwarded, &m.IsDeleted, &meta, &prec,
			&m.Derived.SessionIndex, &m.Derived.IsTurnStart, &responseS, &m.Derived.WordCount,
			&m.Derived.CharCount, &m.Derived.EmojiCount, &m.Derived.LinkCount, &m.Derived.IsQuestion,
			&sentiment, &m.Derived.AffectionHits, &m.Derived.Conflict); err != nil {
			return mapErr(err)
		}
		m.ParticipantID = participant.UUID // uuid.Nil when NULL
		m.SentAt = fromMS(sent)
		m.Derived.MessageID = m.ID
		if responseS.Valid {
			v := responseS.Int64
			m.Derived.ResponseS = &v
		}
		if sentiment.Valid {
			v := sentiment.Float64
			m.Derived.Sentiment = &v
		}
		m.MinutePrecision = prec == "minute"
		// Most rows carry "{}"; skip the JSON decoder for them.
		if meta != "{}" && meta != "" {
			var am analysisMeta
			if err := json.Unmarshal([]byte(meta), &am); err != nil {
				return fmt.Errorf("sqlite: message %d meta: %w", m.ID, err)
			}
			if am.Precision == "minute" {
				m.MinutePrecision = true
			}
			if am.Call != nil {
				call = analytics.CallInfo{Video: am.Call.Video, Missed: am.Call.Missed, DurationSec: am.Call.DurationSec}
				m.Call = &call
			}
		}
		if err := fn(&m); err != nil {
			return err
		}
	}
	return mapErr(rows.Err())
}

// BeginAnalysis implements store.Store. The returned writer owns the single
// writer connection inside one IMMEDIATE transaction (the writer pool is
// opened with _txlock=immediate) until Finish or Abort, so every other write
// waits for the run; readers keep working against the last committed state.
func (s *Store) BeginAnalysis(ctx context.Context, chatID uuid.UUID, tz string) (store.AnalysisWriter, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if tz == "" {
		return nil, errors.New("sqlite: BeginAnalysis needs a timezone")
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	upd, err := tx.PrepareContext(ctx, `UPDATE messages SET session_index = ?, is_turn_start = ?, response_s = ?,
		word_count = ?, char_count = ?, emoji_count = ?, link_count = ?, is_question = ?, sentiment = ?,
		affection_hits = ?, conflict = ?
		WHERE id = ? AND chat_id = ? AND tenant_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return nil, mapErr(err)
	}
	return &analysisWriter{tx: tx, upd: upd, tenant: sc.TenantID.String(), chat: chatID.String(), tz: tz}, nil
}

// analysisWriter implements store.AnalysisWriter over one transaction.
type analysisWriter struct {
	tx      *sql.Tx
	upd     *sql.Stmt
	tenant  string
	chat    string
	tz      string
	cleared bool // sessions of the chat deleted in this run
	done    bool // committed or rolled back
}

// MessageFeatures implements store.AnalysisWriter. Each row must belong to
// the writer's chat; an unknown or foreign message ID aborts with ErrNotFound
// rather than being silently skipped, because it signals a pipeline bug.
func (w *analysisWriter) MessageFeatures(ctx context.Context, rows []analytics.MessageFeatures) error {
	if w.done {
		return errors.New("sqlite: analysis writer already finished")
	}
	for i := range rows {
		r := &rows[i]
		var resp, sent any
		if r.ResponseS != nil {
			resp = *r.ResponseS
		}
		if r.Sentiment != nil {
			sent = *r.Sentiment
		}
		res, err := w.upd.ExecContext(ctx, r.SessionIndex, boolInt(r.IsTurnStart), resp, r.WordCount, r.CharCount,
			r.EmojiCount, r.LinkCount, boolInt(r.IsQuestion), sent, r.AffectionHits, boolInt(r.Conflict),
			r.MessageID, w.chat, w.tenant)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: message %d is not in chat %s", store.ErrNotFound, r.MessageID, w.chat)
		}
	}
	return nil
}

// clearSessions deletes the chat's sessions once per run.
func (w *analysisWriter) clearSessions(ctx context.Context) error {
	if w.cleared {
		return nil
	}
	if _, err := w.tx.ExecContext(ctx, `DELETE FROM sessions WHERE chat_id = ? AND tenant_id = ?`, w.chat, w.tenant); err != nil {
		return mapErr(err)
	}
	w.cleared = true
	return nil
}

// Sessions implements store.AnalysisWriter.
func (w *analysisWriter) Sessions(ctx context.Context, sessions []analytics.Session) error {
	if w.done {
		return errors.New("sqlite: analysis writer already finished")
	}
	if err := w.clearSessions(ctx); err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	ins, err := w.tx.PrepareContext(ctx, `INSERT INTO sessions (tenant_id, chat_id, idx, started_at, ended_at,
		initiator_id, ender_id, msg_count, turn_count, first_message_id, last_message_id, start_date, conflict_msg)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return mapErr(err)
	}
	defer ins.Close()
	for i := range sessions {
		x := &sessions[i]
		if _, err := ins.ExecContext(ctx, w.tenant, w.chat, x.Index, toMS(x.StartedAt), toMS(x.EndedAt),
			nullUUID(x.Initiator), nullUUID(x.Ender), x.MsgCount, x.TurnCount, x.FirstMsgID, x.LastMsgID,
			x.StartDate.String(), x.ConflictMsg); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// Finish implements store.AnalysisWriter: sessions are cleared even when the
// run produced none (a chat whose messages were all removed), the rollups of
// this timezone are replaced, and the chat records the run.
func (w *analysisWriter) Finish(ctx context.Context, rollups []analytics.Rollup, rollupVersion int) (err error) {
	if w.done {
		return errors.New("sqlite: analysis writer already finished")
	}
	defer func() {
		if err != nil {
			_ = w.Abort()
		}
	}()
	if err := w.clearSessions(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `DELETE FROM daily_rollups WHERE chat_id = ? AND tenant_id = ? AND tz = ?`,
		w.chat, w.tenant, w.tz); err != nil {
		return mapErr(err)
	}
	now := toMS(time.Now())
	if len(rollups) > 0 {
		ins, err := w.tx.PrepareContext(ctx, `INSERT INTO daily_rollups (tenant_id, chat_id, tz, local_date,
			participant_id, rollup_version, computed_at, msg_count, last_msg_at, payload, terms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return mapErr(err)
		}
		defer ins.Close()
		for i := range rollups {
			// Copy so the caller's slice keeps its term maps.
			r := rollups[i]
			terms, err := json.Marshal(rollupTerms{TopTerms: r.TopTerms, TopBigrams: r.TopBigrams})
			if err != nil {
				return fmt.Errorf("sqlite: encode rollup terms: %w", err)
			}
			r.TopTerms, r.TopBigrams = nil, nil
			payload, err := json.Marshal(&r)
			if err != nil {
				return fmt.Errorf("sqlite: encode rollup: %w", err)
			}
			var lastMsg any
			if !r.LastMsgAt.IsZero() {
				lastMsg = toMS(r.LastMsgAt)
			}
			if _, err := ins.ExecContext(ctx, w.tenant, w.chat, w.tz, r.Date.String(), r.ParticipantID.String(),
				rollupVersion, now, r.MsgCount, lastMsg, string(payload), string(terms)); err != nil {
				return mapErr(err)
			}
		}
	}
	if _, err := w.tx.ExecContext(ctx, `UPDATE chats SET rollup_version = ?, analyzed_at = ?, updated_at = ?
		WHERE id = ? AND tenant_id = ?`, rollupVersion, now, now, w.chat, w.tenant); err != nil {
		return mapErr(err)
	}
	_ = w.upd.Close()
	if err := w.tx.Commit(); err != nil {
		return mapErr(err)
	}
	w.done = true
	return nil
}

// Abort implements store.AnalysisWriter.
func (w *analysisWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.upd.Close()
	return mapErr(w.tx.Rollback())
}

// rollupTerms is the separately stored part of a rollup (terms column).
type rollupTerms struct {
	TopTerms   map[string]int `json:"top_terms,omitempty"`
	TopBigrams map[string]int `json:"top_bigrams,omitempty"`
}

// ListRollups implements store.Store. Payloads are decoded in parallel
// because a long chat can have tens of thousands of rows.
func (s *Store) ListRollups(ctx context.Context, chatID uuid.UUID, tz string, q store.RollupQuery) ([]analytics.Rollup, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	termsCol := "''"
	if q.WithTerms {
		termsCol = "terms"
	}
	// #nosec G202 -- termsCol and the range conditions are constant fragments; values are bound parameters.
	query := `SELECT local_date, participant_id, payload, ` + termsCol + ` FROM daily_rollups
		WHERE chat_id = ? AND tenant_id = ? AND tz = ?`
	args := []any{chatID.String(), sc.TenantID.String(), tz}
	if len(q.Ranges) > 0 {
		var conds []string
		for _, r := range q.Ranges {
			conds = append(conds, "(local_date >= ? AND local_date < ?)")
			args = append(args, r.From.String(), r.To.String())
		}
		query += " AND (" + strings.Join(conds, " OR ") + ")" // #nosec G202 -- constant fragments, bound values
	}
	query += " ORDER BY local_date, participant_id"
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	type raw struct {
		date           string
		pid            uuid.UUID
		payload, terms []byte
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.date, &r.pid, &r.payload, &r.terms); err != nil {
			_ = rows.Close()
			return nil, mapErr(err)
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, mapErr(err)
	}
	_ = rows.Close()

	out := make([]analytics.Rollup, len(raws))
	decode := func(i int) error {
		r := &raws[i]
		if err := json.Unmarshal(r.payload, &out[i]); err != nil {
			return fmt.Errorf("sqlite: rollup %s %s: %w", r.date, r.pid, err)
		}
		if len(r.terms) > 0 {
			var tm rollupTerms
			if err := json.Unmarshal(r.terms, &tm); err != nil {
				return fmt.Errorf("sqlite: rollup terms %s %s: %w", r.date, r.pid, err)
			}
			out[i].TopTerms, out[i].TopBigrams = tm.TopTerms, tm.TopBigrams
		}
		d, err := analytics.ParseDate(r.date)
		if err != nil {
			return err
		}
		out[i].Date, out[i].ParticipantID = d, r.pid
		return nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(raws)/64))
	if workers <= 1 {
		for i := range raws {
			if err := decode(i); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		next     atomic.Int64
	)
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(raws) {
					return
				}
				if err := decode(i); err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// RollupIndex implements store.Store using only plain columns (no JSON).
func (s *Store) RollupIndex(ctx context.Context, chatID uuid.UUID, tz string) ([]analytics.RollupLite, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT local_date, participant_id, msg_count, last_msg_at FROM daily_rollups
		WHERE chat_id = ? AND tenant_id = ? AND tz = ? ORDER BY local_date, participant_id`,
		chatID.String(), sc.TenantID.String(), tz)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []analytics.RollupLite
	for rows.Next() {
		var (
			date string
			l    analytics.RollupLite
			last sql.NullInt64
		)
		if err := rows.Scan(&date, &l.ParticipantID, &l.MsgCount, &last); err != nil {
			return nil, mapErr(err)
		}
		if l.Date, err = analytics.ParseDate(date); err != nil {
			return nil, err
		}
		if last.Valid {
			l.LastMsgAt = fromMS(last.Int64)
		}
		out = append(out, l)
	}
	return out, mapErr(rows.Err())
}

// ListSessions implements store.Store.
func (s *Store) ListSessions(ctx context.Context, chatID uuid.UUID, from, to time.Time) ([]analytics.Session, error) {
	sc, err := scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireChat(ctx, sc.TenantID, chatID); err != nil {
		return nil, err
	}
	where := []string{"chat_id = ?", "tenant_id = ?"}
	args := []any{chatID.String(), sc.TenantID.String()}
	if !from.IsZero() {
		where, args = append(where, "started_at >= ?"), append(args, toMS(from))
	}
	if !to.IsZero() {
		where, args = append(where, "started_at < ?"), append(args, toMS(to))
	}
	// #nosec G202 -- conditions are constant fragments; values are bound parameters.
	rows, err := s.r.QueryContext(ctx, `SELECT idx, started_at, ended_at, initiator_id, ender_id, msg_count,
		turn_count, first_message_id, last_message_id, start_date, conflict_msg FROM sessions
		WHERE `+strings.Join(where, " AND ")+` ORDER BY started_at, idx`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []analytics.Session
	for rows.Next() {
		var (
			x                analytics.Session
			started, ended   int64
			initiator, ender uuid.NullUUID
			startDate        string
		)
		if err := rows.Scan(&x.Index, &started, &ended, &initiator, &ender, &x.MsgCount, &x.TurnCount,
			&x.FirstMsgID, &x.LastMsgID, &startDate, &x.ConflictMsg); err != nil {
			return nil, mapErr(err)
		}
		x.StartedAt, x.EndedAt = fromMS(started), fromMS(ended)
		x.Initiator, x.Ender = initiator.UUID, ender.UUID
		if x.StartDate, err = analytics.ParseDate(startDate); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, mapErr(rows.Err())
}
