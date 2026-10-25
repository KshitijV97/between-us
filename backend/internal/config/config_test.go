package config

import (
	"log/slog"
	"strings"
	"testing"
)

// env builds a Lookup over a fixed map so tests are hermetic.
func env(kv map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestLoadDefaultsToLocalProfile(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != ProfileLocal || cfg.Mode != ModeLocal {
		t.Errorf("profile/mode = %s/%s, want local/local", cfg.Profile, cfg.Mode)
	}
	// Local mode must bind loopback only (docs/07-deployment.md §2).
	if cfg.HTTPAddr != "127.0.0.1:7420" {
		t.Errorf("HTTPAddr = %q, want 127.0.0.1:7420", cfg.HTTPAddr)
	}
	if cfg.PublicBaseURL != "http://127.0.0.1:7420" {
		t.Errorf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if cfg.LogFormat != LogText || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log = %s/%v, want text/info", cfg.LogFormat, cfg.LogLevel)
	}
	if cfg.DB.Driver != "sqlite" {
		t.Errorf("DB.Driver = %q, want sqlite", cfg.DB.Driver)
	}
}

func TestSQLiteDSNDefaultsToDataDir(t *testing.T) {
	cfg, err := Load(env(map[string]string{"CA_DATA_DIR": "/tmp/ca-test"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "/tmp/ca-test" || cfg.DB.DSN != "file:/tmp/ca-test/chatanalyzer.db" {
		t.Errorf("DataDir=%q DSN=%q", cfg.DataDir, cfg.DB.DSN)
	}
	cfg, err = Load(env(map[string]string{"CA_DB_DSN": "file:/elsewhere.db"}))
	if err != nil || cfg.DB.DSN != "file:/elsewhere.db" {
		t.Errorf("explicit DSN lost: %q %v", cfg.DB.DSN, err)
	}
}

func TestPortOverridesProfilePortButNotExplicitAddr(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"CA_PROFILE": "cloud-free-container", "CA_DB_DSN": "postgres://x", "PORT": "10000",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "0.0.0.0:10000" {
		t.Errorf("HTTPAddr = %q, want 0.0.0.0:10000", cfg.HTTPAddr)
	}
	// The browser-facing URL must never be the wildcard address.
	if cfg.PublicBaseURL != "http://127.0.0.1:10000" {
		t.Errorf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}

	cfg, err = Load(env(map[string]string{"CA_HTTP_ADDR": "127.0.0.1:9000", "PORT": "10000"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" {
		t.Errorf("explicit CA_HTTP_ADDR lost: %q", cfg.HTTPAddr)
	}
}

func TestHostedProfilesDefaultToHostedJSON(t *testing.T) {
	cfg, err := Load(env(map[string]string{"CA_PROFILE": "cloud-free", "CA_DB_DSN": "postgres://x"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsHosted() || cfg.LogFormat != LogJSON || cfg.DB.MaxConns != 5 {
		t.Errorf("got mode=%s format=%s maxConns=%d", cfg.Mode, cfg.LogFormat, cfg.DB.MaxConns)
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CA_MODE":         "cluster",
		"CA_LOG_FORMAT":   "xml",
		"CA_LOG_LEVEL":    "loud",
		"CA_DB_MAX_CONNS": "0",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"CA_MODE", "CA_LOG_FORMAT", "CA_LOG_LEVEL", "CA_DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestCloudProfilesRejectSQLite(t *testing.T) {
	_, err := Load(env(map[string]string{"CA_PROFILE": "cloud-free", "CA_DB_DRIVER": "sqlite"}))
	if err == nil || !strings.Contains(err.Error(), "requires postgres") {
		t.Fatalf("err = %v, want 'requires postgres'", err)
	}
}

func TestUnknownProfile(t *testing.T) {
	if _, err := Load(env(map[string]string{"CA_PROFILE": "prod"})); err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

func TestPublicBaseURLMustBeAbsolute(t *testing.T) {
	_, err := Load(env(map[string]string{"CA_PUBLIC_BASE_URL": "example.com"}))
	if err == nil || !strings.Contains(err.Error(), "CA_PUBLIC_BASE_URL") {
		t.Fatalf("err = %v", err)
	}
}
