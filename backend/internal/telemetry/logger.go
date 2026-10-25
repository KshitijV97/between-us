// Package telemetry owns logging today and will own OpenTelemetry tracing and
// metrics (docs/06-observability.md). Everything that emits signals goes
// through this package so the PII rules live in one place.
//
// PII rule (NFR-OBS-003, PRV-007): log attributes carry IDs, counts and
// durations only. Message text, participant names and phone numbers must never
// be logged. Call sites pass values through attributes, never by formatting
// them into the message string, so a scrub test can inspect every key.
package telemetry

import (
	"context"
	"io"
	"log/slog"

	"github.com/kshitijv97/between-us/backend/internal/buildinfo"
	"github.com/kshitijv97/between-us/backend/internal/config"
)

// ctxKey is unexported so no other package can collide with or forge keys.
type ctxKey int

const requestIDKey ctxKey = iota

// NewLogger builds the process logger. JSON output is used for hosted
// deployments (machine-parsed by Loki); text output for local development.
// Every record carries service metadata so logs from several instances can be
// told apart.
func NewLogger(w io.Writer, cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler
	if cfg.LogFormat == config.LogJSON {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	h = contextHandler{h}
	bi := buildinfo.Get()
	return slog.New(h).With(
		slog.String("service.name", "between-us"),
		slog.String("service.version", bi.Version),
		slog.String("deployment.environment.name", cfg.Env),
	)
}

// WithRequestID returns a context that carries the request ID; log records made
// with that context include it automatically.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// contextHandler decorates records with correlation IDs found in the context
// (request_id now; trace_id/span_id once OpenTelemetry lands in M2/M6).
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
