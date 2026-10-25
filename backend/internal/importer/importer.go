// Package importer orchestrates an import: detect the format, preview the
// participants, then stream parsed messages into the store with participant
// mapping, de-duplication and local dates (FR-ING-001…014).
//
// It is the only place where parsers (internal/ingest/...) meet the store
// (internal/store). The flow mirrors the UI journey in
// docs/02-architecture.md §6.1:
//
//	Preview  detect + full parse into a counting sink → participants, date
//	         range, date-order evidence, warnings (nothing is written)
//	Run      create/extend the chat, map participants, parse again and insert
//	         batches; the import becomes visible only when it completes
//	         (NFR-REL-002)
//
// Parsing twice keeps memory bounded (FR-ING-013): nothing but counters is
// held between the passes.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
	"github.com/kshitijv/chat-analyzer/backend/internal/store"
	"github.com/kshitijv/chat-analyzer/backend/internal/tenancy"
)

// Errors returned to callers; their messages are safe to show to users.
var (
	// ErrTooManyParticipants means more than two senders were found and the
	// caller did not pick two (FR-ING-012).
	ErrTooManyParticipants = errors.New("this export has more than two senders; choose the two participants to analyse")
	// ErrPlatformMismatch means a re-import targets a chat from another platform.
	ErrPlatformMismatch = errors.New("this export is from a different platform than the chat it is being added to")
	// ErrAlreadyImported means the identical file was already imported into the chat.
	ErrAlreadyImported = errors.New("this exact file was already imported into this chat")
)

// Importer runs imports against a store.
type Importer struct {
	Store store.Store
	Log   *slog.Logger
	// BatchSize overrides ingest.DefaultBatchSize (tests use small batches).
	BatchSize int
}

// PreviewRequest describes an upload to inspect.
type PreviewRequest struct {
	Source ingest.Source
	// Timezone is the IANA name used to interpret naive WhatsApp times.
	Timezone       string
	DateOrder      ingest.DateOrder
	SelectedChatID string // Telegram full-account exports
}

// Preview is what the user confirms before the import is written.
type Preview struct {
	Detection ingest.Detection
	Report    ingest.Report
}

// Preview detects the format and parses the whole upload without writing
// anything, returning participants, date range and warnings.
func (im *Importer) Preview(ctx context.Context, req PreviewRequest) (Preview, error) {
	p, det, loc, err := im.prepare(req.Source, req.Timezone)
	if err != nil {
		return Preview{Detection: det}, err
	}
	rep, err := p.Parse(ctx, req.Source, ingest.Options{
		Location: loc, DateOrder: req.DateOrder, BatchSize: im.batchSize(), SelectedChatID: req.SelectedChatID,
	}, discardSink{})
	return Preview{Detection: det, Report: rep}, err
}

// Request describes a confirmed import.
type Request struct {
	PreviewRequest
	// ChatID appends to an existing chat (incremental re-import, FR-ING-010);
	// uuid.Nil creates a new chat.
	ChatID uuid.UUID
	// Title for a new chat; defaults to the export's suggested title.
	Title string
	// Participants restricts the import to these sender keys (FR-ING-012).
	// Required when the export has more than two senders.
	Participants []string
	// MeKey marks one sender key as the user ("this is me", FR-ING-008).
	MeKey string
	// File metadata for the import record; the file itself is never stored
	// (FR-ING-014).
	FileName   string
	FileSize   int64
	FileSHA256 []byte
	// MediaCounts are media files found next to the chat in a zip
	// (archive.Upload.MediaCounts); kept in the import report for M-019.
	MediaCounts map[ingest.MediaType]int
}

// Result summarises a completed import.
type Result struct {
	Chat       store.Chat
	Import     store.Import
	Report     ingest.Report
	Inserted   int
	Duplicates int
	// Filtered counts messages from senders outside Request.Participants.
	Filtered int
}

// Run performs a confirmed import. On any error after the import row exists,
// the import is marked failed and its messages are removed, so a chat never
// shows a half-imported state.
func (im *Importer) Run(ctx context.Context, req Request) (Result, error) {
	scope, err := tenancy.FromContext(ctx)
	if err != nil {
		return Result{}, err
	}
	pv, err := im.Preview(ctx, req.PreviewRequest)
	if err != nil {
		return Result{Report: pv.Report}, err
	}
	keep, err := selectParticipants(pv.Report.Participants, req.Participants)
	if err != nil {
		return Result{Report: pv.Report}, err
	}

	chat, err := im.resolveChat(ctx, req, pv, scope)
	if err != nil {
		return Result{Report: pv.Report}, err
	}
	if len(req.FileSHA256) > 0 && req.ChatID != uuid.Nil {
		if _, err := im.Store.FindCompletedImportBySHA256(ctx, chat.ID, req.FileSHA256); err == nil {
			return Result{Chat: chat, Report: pv.Report}, ErrAlreadyImported
		} else if !errors.Is(err, store.ErrNotFound) {
			return Result{}, err
		}
	}
	senders, err := im.mapParticipants(ctx, chat.ID, pv.Report.Participants, keep, req.MeKey)
	if err != nil {
		return Result{}, err
	}
	chatLoc, err := time.LoadLocation(chat.AnalysisTZ)
	if err != nil {
		return Result{}, fmt.Errorf("chat timezone %q: %w", chat.AnalysisTZ, err)
	}
	base, err := im.Store.MaxOrdinal(ctx, chat.ID)
	if err != nil {
		return Result{}, err
	}

	imp, err := im.Store.CreateImport(ctx, store.Import{
		ChatID: chat.ID, CreatedBy: scope.UserID,
		Platform: string(pv.Detection.Platform), Variant: pv.Detection.Variant,
		Status: store.ImportProcessing, Timezone: req.Timezone, TimePrecision: string(pv.Report.TimePrecision),
		FileName: req.FileName, FileSize: req.FileSize, FileSHA256: req.FileSHA256,
	})
	if err != nil {
		return Result{}, err
	}
	log := im.logger().With("import_id", imp.ID.String(), "chat_id", chat.ID.String())
	log.InfoContext(ctx, "import started", "platform", imp.Platform, "variant", imp.Variant)
	started := time.Now()

	sink := &storeSink{
		store: im.Store, importID: imp.ID, chatID: chat.ID, chatLoc: chatLoc,
		senders: senders, nextOrdinal: base + 1,
	}
	p, _, loc, err := im.prepare(req.Source, req.Timezone)
	if err == nil {
		var rep ingest.Report
		rep, err = p.Parse(ctx, req.Source, ingest.Options{
			Location: loc, DateOrder: firstNonEmpty(req.DateOrder, pv.Report.DateOrder),
			BatchSize: im.batchSize(), SelectedChatID: req.SelectedChatID,
		}, sink)
		pv.Report = rep
	}
	reportJSON, _ := json.Marshal(sink.decorate(pv.Report, req.MediaCounts))
	if err != nil {
		// Use a fresh context so a cancelled import is still cleaned up.
		cleanup := context.WithoutCancel(ctx)
		if ferr := im.Store.FailImport(cleanup, imp.ID, userError(err), reportJSON); ferr != nil {
			log.ErrorContext(ctx, "failed to mark import failed", "error", ferr)
		}
		// A chat created by this very import would otherwise linger empty.
		if req.ChatID == uuid.Nil {
			if derr := im.Store.DeleteChat(cleanup, chat.ID); derr != nil {
				log.ErrorContext(ctx, "failed to remove chat of failed first import", "error", derr)
			}
		}
		log.WarnContext(ctx, "import failed", "error_kind", fmt.Sprintf("%T", err))
		return Result{Chat: chat, Report: pv.Report}, err
	}

	now := time.Now().UTC()
	imp.Status = store.ImportCompleted
	imp.Report = reportJSON
	imp.InsertedCount = sink.inserted
	imp.DuplicateCount = sink.total - sink.inserted
	imp.CompletedAt = &now
	if err := im.Store.CompleteImport(ctx, imp); err != nil {
		_ = im.Store.FailImport(context.WithoutCancel(ctx), imp.ID, "could not finalise the import", reportJSON)
		return Result{}, err
	}
	chat, err = im.Store.GetChat(ctx, chat.ID)
	if err != nil {
		return Result{}, err
	}
	log.InfoContext(ctx, "import completed",
		"inserted", sink.inserted, "duplicates", imp.DuplicateCount, "filtered", sink.filtered,
		"duration", time.Since(started))
	return Result{
		Chat: chat, Import: imp, Report: pv.Report,
		Inserted: sink.inserted, Duplicates: imp.DuplicateCount, Filtered: sink.filtered,
	}, nil
}

// prepare detects the parser and resolves the timezone.
func (im *Importer) prepare(src ingest.Source, tz string) (ingest.Parser, ingest.Detection, *time.Location, error) {
	sample, err := ingest.ReadSample(src)
	if err != nil {
		return nil, ingest.Detection{}, nil, fmt.Errorf("reading upload: %w", err)
	}
	p, det, err := ingest.Detect(sample)
	if err != nil {
		return nil, det, nil, err
	}
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, det, nil, fmt.Errorf("unknown timezone %q", tz)
	}
	return p, det, loc, nil
}

// resolveChat loads the target chat for a re-import, or creates a new one.
func (im *Importer) resolveChat(ctx context.Context, req Request, pv Preview, scope tenancy.Scope) (store.Chat, error) {
	if req.ChatID != uuid.Nil {
		chat, err := im.Store.GetChat(ctx, req.ChatID)
		if err != nil {
			return store.Chat{}, err
		}
		if chat.Platform != string(pv.Detection.Platform) {
			return store.Chat{}, ErrPlatformMismatch
		}
		return chat, nil
	}
	title := firstNonEmpty(req.Title, pv.Report.SuggestedTitle, "Imported chat")
	tz := firstNonEmpty(req.Timezone, "UTC")
	return im.Store.CreateChat(ctx, store.Chat{
		Title: title, Platform: string(pv.Detection.Platform), AnalysisTZ: tz,
		Settings: json.RawMessage(`{}`), CreatedBy: scope.UserID,
	})
}

// mapParticipants returns sender key → participant ID for the kept senders,
// reusing existing participants (matched by any of their platform keys) and
// creating the rest.
func (im *Importer) mapParticipants(ctx context.Context, chatID uuid.UUID, found []ingest.ParticipantSummary, keep map[string]bool, meKey string) (map[string]uuid.UUID, error) {
	existing, err := im.Store.ListParticipants(ctx, chatID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]uuid.UUID, len(found))
	for _, ps := range found {
		if !keep[ps.Key] {
			continue
		}
		idx := slices.IndexFunc(existing, func(p store.Participant) bool { return slices.Contains(p.PlatformKeys, ps.Key) })
		var p store.Participant
		if idx >= 0 {
			p = existing[idx]
			if !(meKey != "" && ps.Key == meKey && !p.IsMe) {
				out[ps.Key] = p.ID
				continue
			}
		} else {
			p = store.Participant{ChatID: chatID, DisplayName: ps.Display, PlatformKeys: []string{ps.Key}}
		}
		if meKey != "" && ps.Key == meKey {
			p.IsMe = true
		}
		saved, err := im.Store.UpsertParticipant(ctx, p)
		if err != nil {
			return nil, err
		}
		existing = append(existing, saved)
		out[ps.Key] = saved.ID
	}
	return out, nil
}

// selectParticipants enforces FR-ING-012: at most two senders are analysed.
func selectParticipants(found []ingest.ParticipantSummary, chosen []string) (map[string]bool, error) {
	keep := map[string]bool{}
	if len(chosen) == 0 {
		if len(found) > 2 {
			return nil, ErrTooManyParticipants
		}
		for _, p := range found {
			keep[p.Key] = true
		}
		return keep, nil
	}
	if len(chosen) > 2 {
		return nil, fmt.Errorf("choose at most two participants, got %d", len(chosen))
	}
	for _, key := range chosen {
		if !slices.ContainsFunc(found, func(p ingest.ParticipantSummary) bool { return p.Key == key }) {
			return nil, fmt.Errorf("participant %q is not in this export", key)
		}
		keep[key] = true
	}
	return keep, nil
}

func (im *Importer) batchSize() int {
	if im.BatchSize > 0 {
		return im.BatchSize
	}
	return ingest.DefaultBatchSize
}

func (im *Importer) logger() *slog.Logger {
	if im.Log != nil {
		return im.Log
	}
	return slog.New(slog.DiscardHandler)
}

// userError converts internal errors into a message safe for the import
// record: known sentinel errors keep their text, everything else is generic.
func userError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "the import was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the import took too long and was stopped"
	}
	msg := err.Error()
	// Parser errors are written to be user-facing and content-free; cap length
	// so an unexpected error cannot bloat the row.
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return strings.TrimSpace(msg)
}

func firstNonEmpty[T ~string](vals ...T) T {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// discardSink is used by Preview: it keeps nothing, so memory stays bounded.
type discardSink struct{}

func (discardSink) Batch(context.Context, []ingest.Message) error { return nil }
func (discardSink) Diagnostic(ingest.Diagnostic)                  {}
