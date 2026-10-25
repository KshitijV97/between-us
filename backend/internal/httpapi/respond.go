package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/kshitijv97/between-us/backend/internal/telemetry"
)

// Problem is an RFC 9457 "problem details" error body. Every API error uses it
// so clients have one error shape to handle (docs/02-architecture.md §13).
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// writeProblem sends a problem+json response. detail must be safe to show to
// end users: never SQL, stack traces or message content.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := Problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		RequestID: telemetry.RequestID(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// writeJSON sends v as JSON. API responses are never cached by intermediaries
// because they may contain private data.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error can only be
	// logged by the access log as a short body; nothing else to do here.
	_ = json.NewEncoder(w).Encode(v)
}
