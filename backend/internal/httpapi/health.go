package httpapi

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kshitijv97/chat-analyzer/backend/internal/buildinfo"
)

// ReadinessCheck reports whether a dependency (database, migrations…) is ready
// to serve traffic. It must honour ctx cancellation.
type ReadinessCheck func(ctx context.Context) error

// readinessTimeout bounds the whole /readyz call; synthetic monitors and
// platform probes typically give up after a few seconds.
const readinessTimeout = 2 * time.Second

// handleHealthz is the liveness probe: the process is up and serving HTTP.
// It deliberately checks no dependencies, so a database outage never makes an
// orchestrator restart healthy processes.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyResponse is the body of GET /readyz.
type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// handleReadyz runs all readiness checks concurrently. Failure details are
// reduced to "fail" because the endpoint is unauthenticated; the cause goes to
// the log instead.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	names := make([]string, 0, len(s.opts.Checks))
	for name := range s.opts.Checks {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { results[i] = s.opts.Checks[name](ctx) })
	}
	wg.Wait()

	resp := readyResponse{Status: "ok", Checks: make(map[string]string, len(names))}
	for i, name := range names {
		if err := results[i]; err != nil {
			resp.Status = "unavailable"
			resp.Checks[name] = "fail"
			s.opts.Logger.WarnContext(r.Context(), "readiness check failed", "check", name, "error", err)
			continue
		}
		resp.Checks[name] = "ok"
	}

	status := http.StatusOK
	if resp.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// handleVersion returns build metadata (NFR-OBS-004).
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

// SystemInfo is the body of GET /api/v1/system/info, which the web UI uses to
// adapt itself (for example, hide sign-in in local mode).
type SystemInfo struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Profile string `json:"profile"`
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, SystemInfo{
		Version: buildinfo.Get().Version,
		Mode:    string(s.opts.Config.Mode),
		Profile: string(s.opts.Config.Profile),
	})
}
