package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/kshitijv/chat-analyzer/backend/internal/config"
)

func TestJSONLoggerAddsServiceAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{LogFormat: config.LogJSON, Env: "test"}
	log := NewLogger(&buf, cfg)

	ctx := WithRequestID(context.Background(), "req-123")
	log.InfoContext(ctx, "hello", "count", 3)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	for k, want := range map[string]any{
		"msg":                         "hello",
		"request_id":                  "req-123",
		"service.name":                "chatanalyzer",
		"deployment.environment.name": "test",
		"count":                       float64(3),
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestRequestIDAbsent(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID = %q, want empty", got)
	}
}
