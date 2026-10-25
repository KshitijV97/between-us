// Package httpapi is the HTTP edge of the application: routing, middleware,
// problem+json errors, health endpoints and the embedded web UI.
//
// Handlers stay thin. They decode input, call a domain package and encode the
// result; business rules never live here (docs/02-architecture.md §4).
package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/kshitijv/chat-analyzer/backend/internal/config"
)

// Server timeouts. Header and idle limits protect against slow-loris clients;
// there is deliberately no global write timeout because Q&A answers stream over
// SSE (FR-API-003). Long routes set their own deadlines via
// http.ResponseController.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	shutdownTimeout   = 15 * time.Second
)

// Options are the dependencies the HTTP layer needs. Fields added in later
// milestones (store, auth, jobs) keep the zero value meaningful.
type Options struct {
	Config config.Config
	Logger *slog.Logger
	// UI is the static web app; nil or missing index.html serves a placeholder.
	UI fs.FS
	// Checks are readiness probes run by GET /readyz, keyed by component name.
	Checks map[string]ReadinessCheck
}

// Server wires routes and middleware around an http.Server.
type Server struct {
	opts    Options
	handler http.Handler
}

// New builds the server and its full middleware chain.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{opts: opts}
	s.handler = s.middleware(s.routes())
	return s
}

// Handler returns the root handler; tests use it with httptest.
func (s *Server) Handler() http.Handler { return s.handler }

// routes registers every endpoint. Go 1.22+ method-and-wildcard patterns are
// used directly (ADR-0001), so there is no third-party router.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Operational endpoints (NFR-OBS-004). Unversioned and unauthenticated so
	// load balancers and synthetic checks can call them.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", s.handleVersion)

	// Versioned public API (FR-API-001).
	mux.HandleFunc("GET /api/v1/system/info", s.handleSystemInfo)
	// Anything else under /api/ is a JSON 404, never the SPA's index.html.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "No such API endpoint.")
	})

	// The web UI owns every remaining path (client-side routing). It is
	// registered without a method because "GET /" would conflict with "/api/"
	// (each would be more specific on a different axis); the handler itself
	// rejects anything but GET and HEAD.
	mux.Handle("/", getOnly(newSPAHandler(s.opts.UI)))
	return mux
}

// middleware applies cross-cutting concerns, outermost first. The order is
// deliberate: the request ID exists before anything logs, the access log sees
// the final status even when recoverer converted a panic into a 500.
func (s *Server) middleware(h http.Handler) http.Handler {
	h = securityHeaders(s.opts.Config.IsHosted())(h)
	h = recoverer(s.opts.Logger)(h)
	h = accessLog(s.opts.Logger)(h)
	h = requestID(h)
	return h
}

// Run listens on cfg.HTTPAddr and serves until ctx is cancelled, then drains
// in-flight requests for up to shutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Config.HTTPAddr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is Run with a caller-provided listener (used by tests to bind :0).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.opts.Logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.opts.Logger.Info("http server listening",
		"addr", ln.Addr().String(), "public_url", s.opts.Config.PublicBaseURL,
		"mode", string(s.opts.Config.Mode), "profile", string(s.opts.Config.Profile))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// BaseContext is already cancelled, so give shutdown a fresh deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	s.opts.Logger.Info("http server shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
