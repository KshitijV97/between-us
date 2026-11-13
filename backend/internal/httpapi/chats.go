package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analysis"
	"github.com/kshitijv97/between-us/backend/internal/analytics"
	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"
)

// Limits for list endpoints.
const (
	defaultPageSize = 100
	maxPageSize     = 500
	maxSettingsBody = 16 << 10
)

// registerChatRoutes adds the chat, metrics and message endpoints
// (docs/04-metrics-catalog.md §12, FR-LIB-001…004, FR-ANA-001…006). They are
// only registered when a store is configured.
func (s *Server) registerChatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/chats", s.scoped(s.handleListChats))
	mux.HandleFunc("GET /api/v1/chats/{chatID}", s.scoped(s.handleGetChat))
	mux.HandleFunc("DELETE /api/v1/chats/{chatID}", s.scoped(s.handleDeleteChat))
	mux.HandleFunc("PUT /api/v1/chats/{chatID}/settings", s.scoped(s.handlePutSettings))
	mux.HandleFunc("GET /api/v1/chats/{chatID}/metrics", s.scoped(s.handleMetrics))
	mux.HandleFunc("GET /api/v1/chats/{chatID}/messages", s.scoped(s.handleListMessages))
	mux.HandleFunc("GET /api/v1/chats/{chatID}/search", s.scoped(s.handleSearch))
}

// scoped attaches the tenant scope. Local mode has one implicit tenant
// (FR-ADM-001); hosted authentication arrives in M5, until then hosted mode
// refuses data endpoints rather than guessing a tenant.
func (s *Server) scoped(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.LocalScope == nil {
			writeProblem(w, r, http.StatusNotImplemented, "Data endpoints need sign-in, which arrives in milestone M5.")
			return
		}
		h(w, r.WithContext(tenancy.WithScope(r.Context(), *s.opts.LocalScope)))
	}
}

// ChatDTO is the API representation of a chat.
type ChatDTO struct {
	ID             uuid.UUID        `json:"id"`
	Title          string           `json:"title"`
	Platform       string           `json:"platform"`
	AnalysisTZ     string           `json:"analysis_tz"`
	MessageCount   int64            `json:"message_count"`
	FirstMessageAt *time.Time       `json:"first_message_at,omitempty"`
	LastMessageAt  *time.Time       `json:"last_message_at,omitempty"`
	AnalyzedAt     *time.Time       `json:"analyzed_at,omitempty"`
	Settings       json.RawMessage  `json:"settings,omitempty"`
	Participants   []ParticipantDTO `json:"participants,omitempty"`
}

// ParticipantDTO is the API representation of a participant.
type ParticipantDTO struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	Alias       string    `json:"alias,omitempty"`
	IsMe        bool      `json:"is_me"`
}

func chatDTO(c store.Chat) ChatDTO {
	return ChatDTO{
		ID: c.ID, Title: c.Title, Platform: c.Platform, AnalysisTZ: c.AnalysisTZ,
		MessageCount: c.MessageCount, FirstMessageAt: c.FirstMessageAt, LastMessageAt: c.LastMessageAt,
		AnalyzedAt: c.AnalyzedAt,
	}
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.opts.Store.ListChats(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]ChatDTO, 0, len(chats))
	for _, c := range chats {
		out = append(out, chatDTO(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": out})
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	c, err := s.opts.Store.GetChat(r.Context(), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	parts, err := s.opts.Store.ListParticipants(r.Context(), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	dto := chatDTO(c)
	set, err := analytics.ParseSettings(c.Settings)
	if err == nil {
		dto.Settings, _ = json.Marshal(set) // effective settings, defaults filled in
	}
	for _, p := range parts {
		dto.Participants = append(dto.Participants, ParticipantDTO{ID: p.ID, DisplayName: p.DisplayName, Alias: p.Alias, IsMe: p.IsMe})
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	if err := s.opts.Store.DeleteChat(r.Context(), id); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePutSettings validates and stores analysis settings, then re-analyses
// the chat so "recompute" settings take effect (§10).
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	raw, err := readBody(r, maxSettingsBody)
	if err != nil {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "Settings body is too large.")
		return
	}
	set, err := analytics.ParseSettings(raw)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	normalised, _ := json.Marshal(set)
	if err := s.opts.Store.UpdateChatSettings(r.Context(), id, normalised); err != nil {
		s.storeError(w, r, err)
		return
	}
	if _, err := s.opts.Analysis.Analyze(r.Context(), id); err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"settings": normalised})
}

// handleMetrics serves GET /api/v1/chats/{id}/metrics (§12).
//
//	window   day|week|month|year|all|custom (default month)
//	anchor   YYYY-MM-DD inside the period (default: the chat's last day)
//	from,to  YYYY-MM-DD, inclusive dates for window=custom
//	compare  "previous" to include the previous window
//	metrics  comma-separated IDs, e.g. M-001,M-010
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	q, err := parseMetricsQuery(r)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.opts.Analysis.Metrics(r.Context(), id, q)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func parseMetricsQuery(r *http.Request) (analytics.Query, error) {
	v := r.URL.Query()
	q := analytics.Query{Kind: analytics.WindowKind(v.Get("window")), Compare: v.Get("compare") == "previous"}
	if q.Kind == "" {
		q.Kind = "month"
	}
	var err error
	if a := v.Get("anchor"); a != "" {
		if q.Anchor, err = analytics.ParseDate(a); err != nil {
			return q, err
		}
	}
	if q.Kind == "custom" {
		from, err := analytics.ParseDate(v.Get("from"))
		if err != nil {
			return q, errors.New("window=custom needs from=YYYY-MM-DD")
		}
		to, err := analytics.ParseDate(v.Get("to"))
		if err != nil {
			return q, errors.New("window=custom needs to=YYYY-MM-DD (inclusive)")
		}
		q.From, q.To = from, to.AddDays(1) // API takes an inclusive end date
	}
	if m := v.Get("metrics"); m != "" {
		for _, id := range strings.Split(m, ",") {
			if id = strings.TrimSpace(id); id != "" {
				q.Metrics = append(q.Metrics, id)
			}
		}
	}
	return q, nil
}

// MessageDTO is the API representation of a message.
type MessageDTO struct {
	ID            int64      `json:"id"`
	ParticipantID *uuid.UUID `json:"participant_id,omitempty"`
	SentAt        time.Time  `json:"sent_at"`
	Ordinal       int64      `json:"ordinal"`
	Kind          string     `json:"kind"`
	MediaType     string     `json:"media_type,omitempty"`
	Body          string     `json:"body,omitempty"`
	IsEdited      bool       `json:"is_edited,omitempty"`
	IsForwarded   bool       `json:"is_forwarded,omitempty"`
	IsDeleted     bool       `json:"is_deleted,omitempty"`
	ReplyToID     *int64     `json:"reply_to_id,omitempty"`
}

func messageDTO(m store.Message) MessageDTO {
	return MessageDTO{
		ID: m.ID, ParticipantID: m.ParticipantID, SentAt: m.SentAt, Ordinal: m.Ordinal, Kind: m.Kind,
		MediaType: m.MediaType, Body: m.Body, IsEdited: m.IsEdited, IsForwarded: m.IsForwarded,
		IsDeleted: m.IsDeleted, ReplyToID: m.ReplyToID,
	}
}

// handleListMessages pages through messages (FR-LIB-003, FR-LIB-004) and
// doubles as the drill-down for chart points (FR-ANA-005) via from/to/
// participant filters. Cursor: after_sent_at (RFC 3339) + after_ordinal.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	v := r.URL.Query()
	q := store.MessageQuery{ChatID: id, Limit: defaultPageSize}
	var err error
	if q.From, err = optTime(v.Get("from")); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	if q.To, err = optTime(v.Get("to")); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "to: "+err.Error())
		return
	}
	if q.After.SentAt, err = optTime(v.Get("after_sent_at")); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "after_sent_at: "+err.Error())
		return
	}
	if o := v.Get("after_ordinal"); o != "" {
		if q.After.Ordinal, err = strconv.ParseInt(o, 10, 64); err != nil {
			writeProblem(w, r, http.StatusBadRequest, "after_ordinal must be an integer")
			return
		}
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeProblem(w, r, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		q.Limit = min(n, maxPageSize)
	}
	var participant *uuid.UUID
	if p := v.Get("participant"); p != "" {
		pid, err := uuid.Parse(p)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "participant must be a UUID")
			return
		}
		participant = &pid
	}

	msgs, err := s.opts.Store.ListMessages(r.Context(), q)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		if participant != nil && (m.ParticipantID == nil || *m.ParticipantID != *participant) {
			continue
		}
		out = append(out, messageDTO(m))
	}
	resp := map[string]any{"messages": out}
	// The cursor follows the last row read, not the last row returned, so a
	// participant filter never stalls pagination.
	if len(msgs) == q.Limit {
		last := msgs[len(msgs)-1]
		resp["next"] = map[string]any{"after_sent_at": last.SentAt.Format(time.RFC3339Nano), "after_ordinal": last.Ordinal}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := chatIDParam(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeProblem(w, r, http.StatusBadRequest, "q is required")
		return
	}
	msgs, err := s.opts.Store.SearchMessages(r.Context(), id, query, defaultPageSize)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageDTO(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// storeError maps domain errors to problem responses; anything unexpected is
// logged and reported generically (never leaking SQL or content).
func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "No such chat.")
	case errors.Is(err, analysis.ErrNotAnalysed):
		writeProblem(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, analytics.ErrEmptyChat):
		writeProblem(w, r, http.StatusConflict, "This chat has no messages to analyse yet.")
	case errors.Is(err, analytics.ErrInvalidWindow):
		writeProblem(w, r, http.StatusBadRequest, err.Error())
	default:
		s.opts.Logger.ErrorContext(r.Context(), "request failed", "error", err)
		writeProblem(w, r, http.StatusInternalServerError, "An unexpected error occurred.")
	}
}

func chatIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("chatID"))
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "No such chat.")
		return uuid.Nil, false
	}
	return id, true
}

// optTime parses an optional RFC 3339 timestamp or YYYY-MM-DD date (UTC).
func optTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, errors.New("must be RFC 3339 or YYYY-MM-DD")
	}
	return t, nil
}

// readBody reads at most limit bytes; validation of the content is the
// caller's job (e.g. analytics.ParseSettings reports malformed JSON as 422).
func readBody(r *http.Request, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
}
