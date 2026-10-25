package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/kshitijv97/chat-analyzer/backend/internal/config"
	"github.com/kshitijv97/chat-analyzer/backend/internal/store"
	"github.com/kshitijv97/chat-analyzer/backend/internal/store/sqlite"
	"github.com/kshitijv97/chat-analyzer/backend/internal/telemetry"
	"github.com/kshitijv97/chat-analyzer/backend/internal/tenancy"
)

// errPostgresNotYet is returned until the Postgres store lands in M5.
var errPostgresNotYet = errors.New("the postgres store arrives in milestone M5; use CA_DB_DRIVER=sqlite for now")

// app bundles what CLI commands share: configuration, logger and store.
type app struct {
	cfg   config.Config
	log   *slog.Logger
	store store.Store
}

// openApp loads configuration and opens (and migrates) the store. Commands
// call close when done.
func openApp(ctx context.Context, stderr io.Writer, migrate bool) (*app, error) {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("configuration error:\n%w", err)
	}
	// CLI output is for people: progress logs only appear when asked for.
	if _, set := os.LookupEnv("CA_LOG_LEVEL"); !set {
		cfg.LogLevel = slog.LevelWarn
	}
	log := telemetry.NewLogger(stderr, cfg)
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if migrate {
		if err := st.Migrate(ctx); err != nil {
			_ = st.Close() // the migration error is the one worth reporting
			return nil, fmt.Errorf("migrating database: %w", err)
		}
	}
	return &app{cfg: cfg, log: log, store: st}, nil
}

func (a *app) close() { _ = a.store.Close() }

// openStore opens the configured database driver.
func openStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	switch cfg.DB.Driver {
	case "sqlite":
		if err := ensureDBDir(cfg); err != nil {
			return nil, err
		}
		return sqlite.Open(ctx, cfg.DB.DSN)
	case "postgres":
		return nil, errPostgresNotYet
	default:
		return nil, fmt.Errorf("unsupported CA_DB_DRIVER %q", cfg.DB.Driver)
	}
}

// ensureDBDir creates the directory of a file-backed SQLite DSN with 0700
// permissions, because the database holds private chat data.
func ensureDBDir(cfg config.Config) error {
	path := strings.TrimPrefix(cfg.DB.DSN, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == ":memory:" {
		return nil
	}
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// localScope returns a context scoped to the implicit local tenant. CLI data
// commands are local-mode only until hosted tenancy lands in M5.
func (a *app) localScope(ctx context.Context) (context.Context, error) {
	if a.cfg.IsHosted() {
		return nil, errors.New("CLI data commands support local mode only (hosted tenancy arrives in M5)")
	}
	t, u, err := a.store.EnsureLocalTenant(ctx)
	if err != nil {
		return nil, err
	}
	return tenancy.WithScope(ctx, tenancy.Scope{TenantID: t.ID, UserID: u.ID}), nil
}
