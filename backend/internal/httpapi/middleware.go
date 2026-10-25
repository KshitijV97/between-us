package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/kshitijv97/chat-analyzer/backend/internal/telemetry"
)

// headerRequestID is accepted from trusted proxies and always echoed back so a
// user-reported ID can be found in logs and traces.
const headerRequestID = "X-Request-ID"

// validRequestID bounds client-supplied IDs: short and log-safe, so they cannot
// be used for log injection or to bloat records.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// requestID assigns every request an ID, reusing a well-formed incoming one.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(telemetry.WithRequestID(r.Context(), id)))
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// accessLog writes one structured record per request. Only the route pattern,
// path, status, size and duration are logged: no query string (search terms are
// user content) and no headers (PRV-007).
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
				// Probes run every few seconds; keep them out of normal logs.
				level = slog.LevelDebug
			}
			// ServeMux records the matched pattern on the request it received,
			// which is this same *http.Request (inner middleware does not copy it).
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("http.request.method", r.Method),
				slog.String("http.route", r.Pattern),
				slog.String("url.path", r.URL.Path),
				slog.Int("http.response.status_code", rec.status),
				slog.Int64("http.response.body.size", rec.bytes),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// recoverer turns a handler panic into a 500 problem response and logs the
// stack, so one bad request never takes the process down.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// http.ErrAbortHandler is the stdlib's sanctioned way to abort a
				// response; re-panic so the server handles it as designed.
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic in handler",
					"panic", v, "stack", string(debug.Stack()))
				writeProblem(w, r, http.StatusInternalServerError, "An unexpected error occurred.")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders sets defensive headers on every response (SEC-001, SEC-003).
// The CSP allows only same-origin scripts; inline styles are permitted because
// chart libraries set style attributes. HSTS is sent only in hosted mode, where
// TLS is terminated by the platform; on localhost it would pin plain-HTTP dev
// servers to HTTPS.
func securityHeaders(hosted bool) func(http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
			if hosted {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the status code and body size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which SSE
// streaming needs for Flush and per-request deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
