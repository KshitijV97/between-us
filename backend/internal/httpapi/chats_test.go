package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kshitijv97/between-us/backend/internal/analysis"
	"github.com/kshitijv97/between-us/backend/internal/config"
	"github.com/kshitijv97/between-us/backend/internal/importer"
	"github.com/kshitijv97/between-us/backend/internal/ingest"
	"github.com/kshitijv97/between-us/backend/internal/store/sqlite"
	"github.com/kshitijv97/between-us/backend/internal/synth"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"

	_ "github.com/kshitijv97/between-us/backend/internal/ingest/whatsapp"
)

// apiFixture is a server backed by a real SQLite store holding one imported
// synthetic chat.
type apiFixture struct {
	h      http.Handler
	chatID string
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn, u, err := st.EnsureLocalTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope := tenancy.Scope{TenantID: tn.ID, UserID: u.ID}
	var buf bytes.Buffer
	if err := synth.WriteWhatsApp(&buf, synth.DefaultConfig(800)); err != nil {
		t.Fatal(err)
	}
	res, err := (&importer.Importer{Store: st}).Run(tenancy.WithScope(ctx, scope), importer.Request{
		PreviewRequest: importer.PreviewRequest{Source: ingest.BytesSource{Data: buf.Bytes(), FileName: "WhatsApp Chat with Bob.txt"}, Timezone: "Asia/Kolkata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{
		Config:     config.Config{Mode: config.ModeLocal, Profile: config.ProfileLocal},
		Store:      st,
		Analysis:   &analysis.Service{Store: st},
		LocalScope: &scope,
	})
	return apiFixture{h: srv.Handler(), chatID: res.Chat.ID.String()}
}

func (f apiFixture) call(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestChatEndpoints(t *testing.T) {
	f := newAPIFixture(t)

	rec, body := f.call(t, http.MethodGet, "/api/v1/chats", "")
	if rec.Code != 200 || len(body["chats"].([]any)) != 1 {
		t.Fatalf("list chats: %d %s", rec.Code, rec.Body)
	}

	rec, body = f.call(t, http.MethodGet, "/api/v1/chats/"+f.chatID, "")
	if rec.Code != 200 || len(body["participants"].([]any)) != 2 || body["settings"] == nil {
		t.Fatalf("get chat: %d %s", rec.Code, rec.Body)
	}

	// Unknown and malformed IDs both look like "no such chat".
	for _, id := range []string{"0198f0a0-0000-7000-8000-000000000000", "not-a-uuid"} {
		if rec, _ := f.call(t, http.MethodGet, "/api/v1/chats/"+id, ""); rec.Code != 404 {
			t.Errorf("GET chat %s = %d, want 404", id, rec.Code)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/chats/" + f.chatID + "/metrics"

	rec, body := f.call(t, http.MethodGet, base+"?window=all", "")
	if rec.Code != 200 {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body)
	}
	metrics := body["metrics"].(map[string]any)
	if len(metrics) != 27 {
		t.Errorf("got %d metrics, want 27", len(metrics))
	}

	rec, body = f.call(t, http.MethodGet, base+"?window=custom&from=2023-01-02&to=2023-01-08&compare=previous&metrics=M-001,M-010", "")
	if rec.Code != 200 {
		t.Fatalf("custom metrics: %d %s", rec.Code, rec.Body)
	}
	win := body["window"].(map[string]any)
	// The API takes an inclusive end date and returns an exclusive one.
	if win["from"] != "2023-01-02" || win["to"] != "2023-01-09" || win["days"].(float64) != 7 {
		t.Errorf("window = %v", win)
	}
	if body["compare"] == nil {
		t.Error("compare=previous did not return a comparison window")
	}
	if got := len(body["metrics"].(map[string]any)); got != 2 {
		t.Errorf("selective metrics returned %d, want 2", got)
	}

	for _, q := range []string{"?window=fortnight", "?window=custom&from=2023-01-02", "?anchor=yesterday"} {
		if rec, _ := f.call(t, http.MethodGet, base+q, ""); rec.Code != 400 {
			t.Errorf("GET metrics%s = %d, want 400 (%s)", q, rec.Code, rec.Body)
		}
	}
}

func TestMessagesPaginationAndSearch(t *testing.T) {
	f := newAPIFixture(t)
	base := "/api/v1/chats/" + f.chatID + "/messages?limit=300"

	seen := map[float64]bool{}
	path := base
	for pages := 0; pages < 10; pages++ {
		rec, body := f.call(t, http.MethodGet, path, "")
		if rec.Code != 200 {
			t.Fatalf("messages: %d %s", rec.Code, rec.Body)
		}
		for _, m := range body["messages"].([]any) {
			id := m.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatalf("message %v returned twice across pages", id)
			}
			seen[id] = true
		}
		next, ok := body["next"].(map[string]any)
		if !ok {
			break
		}
		path = base + "&after_sent_at=" + url.QueryEscape(next["after_sent_at"].(string)) +
			"&after_ordinal=" + url.QueryEscape(strings.TrimSuffix(strings.TrimSuffix(jsonNum(next["after_ordinal"]), ".0"), " "))
	}
	if len(seen) != 801 { // 800 messages + the encryption notice
		t.Errorf("paged through %d messages, want 801", len(seen))
	}

	rec, body := f.call(t, http.MethodGet, "/api/v1/chats/"+f.chatID+"/search?q=dal+chawal", "")
	if rec.Code != 200 || len(body["messages"].([]any)) == 0 {
		t.Errorf("search: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := f.call(t, http.MethodGet, "/api/v1/chats/"+f.chatID+"/search", ""); rec.Code != 400 {
		t.Errorf("search without q = %d, want 400", rec.Code)
	}
}

func TestSettingsAndDelete(t *testing.T) {
	f := newAPIFixture(t)
	path := "/api/v1/chats/" + f.chatID

	rec, body := f.call(t, http.MethodPut, path+"/settings", `{"session_gap":"2h"}`)
	if rec.Code != 200 || body["settings"].(map[string]any)["session_gap"] != "2h0m0s" {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{"session_gap":"5m"}`, `{not json`} {
		if rec, _ := f.call(t, http.MethodPut, path+"/settings", bad); rec.Code != 422 {
			t.Errorf("PUT settings %s = %d, want 422", bad, rec.Code)
		}
	}

	if rec, _ := f.call(t, http.MethodDelete, path, ""); rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec, _ := f.call(t, http.MethodGet, path, ""); rec.Code != 404 {
		t.Errorf("GET after delete = %d, want 404", rec.Code)
	}
}

func TestHostedModeRefusesDataEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(Options{Config: config.Config{Mode: config.ModeHosted}, Store: st, Analysis: &analysis.Service{Store: st}})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("hosted /api/v1/chats = %d, want 501 until sign-in exists", rec.Code)
	}
}

// jsonNum renders a decoded JSON number without exponent notation.
func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
