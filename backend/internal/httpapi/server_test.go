package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kshitijv97/between-us/backend/internal/config"
)

// testUI is a minimal built SPA: an index page and one fingerprinted asset.
var testUI = fstest.MapFS{
	"index.html":          {Data: []byte("<!doctype html><title>app</title>")},
	"assets/app-abc.js":   {Data: []byte("console.log(1)")},
	"favicon.svg":         {Data: []byte("<svg/>")},
	"assets/nested/x.css": {Data: []byte("body{}")},
}

func newTestServer(t *testing.T, opts Options) (*Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if opts.Config.Mode == "" {
		opts.Config = config.Config{Mode: config.ModeLocal, Profile: config.ProfileLocal}
	}
	return New(opts), &logs
}

func do(t *testing.T, h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	rec := do(t, s.Handler(), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	// Method-specific patterns: POST must be rejected by the mux.
	if rec := do(t, s.Handler(), http.MethodPost, "/healthz"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
}

func TestReadyzAggregatesChecks(t *testing.T) {
	s, logs := newTestServer(t, Options{Checks: map[string]ReadinessCheck{
		"db":    func(context.Context) error { return nil },
		"queue": func(context.Context) error { return errors.New("secret internal detail") },
	}})
	rec := do(t, s.Handler(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Checks["db"] != "ok" || body.Checks["queue"] != "fail" {
		t.Errorf("checks = %v", body.Checks)
	}
	// The unauthenticated response must not leak error details; the log keeps them.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("readyz response leaks error details")
	}
	if !strings.Contains(logs.String(), "secret internal detail") {
		t.Error("readiness failure cause not logged")
	}
}

func TestReadyzWithoutChecksIsReady(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	if rec := do(t, s.Handler(), http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestUnknownAPIRouteIsProblemJSON(t *testing.T) {
	s, _ := newTestServer(t, Options{UI: testUI})
	rec := do(t, s.Handler(), http.MethodGet, "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 404 || p.RequestID == "" || p.RequestID != rec.Header().Get(headerRequestID) {
		t.Errorf("problem = %+v", p)
	}
}

func TestSystemInfo(t *testing.T) {
	s, _ := newTestServer(t, Options{Config: config.Config{Mode: config.ModeHosted, Profile: config.ProfileCloudFree}})
	rec := do(t, s.Handler(), http.MethodGet, "/api/v1/system/info")
	var info SystemInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Mode != "hosted" || info.Profile != "cloud-free" || info.Version == "" {
		t.Errorf("info = %+v", info)
	}
}

func TestSPAFallbackAndCaching(t *testing.T) {
	s, _ := newTestServer(t, Options{UI: testUI})
	h := s.Handler()

	tests := []struct {
		path, wantBody, wantCache string
		wantCode                  int
	}{
		{"/", "<title>app</title>", "no-cache", 200},
		{"/chats/123/insights", "<title>app</title>", "no-cache", 200}, // client-side route
		{"/assets/app-abc.js", "console.log", "public, max-age=31536000, immutable", 200},
		{"/favicon.svg", "<svg/>", "", 200},
		{"/assets/missing-chunk.js", "", "", 404}, // never HTML for a missing asset
	}
	for _, tt := range tests {
		rec := do(t, h, http.MethodGet, tt.path)
		if rec.Code != tt.wantCode {
			t.Errorf("%s: status %d, want %d", tt.path, rec.Code, tt.wantCode)
			continue
		}
		if !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("%s: body %q lacks %q", tt.path, rec.Body, tt.wantBody)
		}
		if got := rec.Header().Get("Cache-Control"); tt.wantCache != "" && got != tt.wantCache {
			t.Errorf("%s: Cache-Control %q, want %q", tt.path, got, tt.wantCache)
		}
	}
}

func TestSPANotBuiltPlaceholder(t *testing.T) {
	s, _ := newTestServer(t, Options{UI: fstest.MapFS{}})
	if rec := do(t, s.Handler(), http.MethodPost, "/chats"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to SPA path = %d, want 405", rec.Code)
	}
	rec := do(t, s.Handler(), http.MethodGet, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "make build") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	local, _ := newTestServer(t, Options{})
	rec := do(t, local.Handler(), http.MethodGet, "/healthz")
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent in local mode")
	}

	hosted, _ := newTestServer(t, Options{Config: config.Config{Mode: config.ModeHosted}})
	if do(t, hosted.Handler(), http.MethodGet, "/healthz").Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing in hosted mode")
	}
}

func TestRequestIDPropagation(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	h := s.Handler()

	if got := do(t, h, http.MethodGet, "/healthz", headerRequestID, "abc-12345678").Header().Get(headerRequestID); got != "abc-12345678" {
		t.Errorf("well-formed incoming ID not reused: %q", got)
	}
	// Hostile IDs (log injection, oversized) are replaced, not echoed.
	for _, bad := range []string{"x", "bad id\nINJECT", strings.Repeat("a", 65)} {
		got := do(t, h, http.MethodGet, "/healthz", headerRequestID, bad).Header().Get(headerRequestID)
		if got == bad || len(got) != 24 {
			t.Errorf("bad ID %q produced %q", bad, got)
		}
	}
}

func TestPanicBecomes500AndIsLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := requestID(accessLog(log)(recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))
	rec := do(t, h, http.MethodGet, "/x")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic value leaked to client")
	}
	out := logs.String()
	if !strings.Contains(out, "panic in handler") || !strings.Contains(out, `"http.response.status_code":500`) {
		t.Errorf("logs missing panic or 500 access record:\n%s", out)
	}
}

func TestAccessLogOmitsQueryString(t *testing.T) {
	s, logs := newTestServer(t, Options{UI: testUI})
	do(t, s.Handler(), http.MethodGet, "/api/v1/system/info?q=private+words")
	out := logs.String()
	if strings.Contains(out, "private") {
		t.Errorf("query string leaked into logs:\n%s", out)
	}
	if !strings.Contains(out, `"http.route":"GET /api/v1/system/info"`) {
		t.Errorf("route pattern not logged:\n%s", out)
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}
