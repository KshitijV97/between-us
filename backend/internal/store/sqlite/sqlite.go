// Package sqlite implements store.Store on SQLite for the local profile
// (ADR-0003). It uses the pure-Go modernc.org/sqlite driver, so the binary
// stays CGO-free and cross-compiles everywhere.
//
// # Connection pools
//
// SQLite allows many concurrent readers but only one writer. Instead of one
// pool where writers would contend and hit SQLITE_BUSY, the store keeps two:
//
//   - a writer pool with exactly one connection, whose transactions start with
//     BEGIN IMMEDIATE (_txlock=immediate) so a write transaction takes the
//     write lock up front and never has to upgrade a read lock mid-way (the
//     classic source of "database is locked" deadlocks);
//   - a reader pool of several query_only connections that read concurrently
//     thanks to WAL mode.
//
// In-memory databases (":memory:" DSNs) are private to one connection, so they
// use the single writer connection for everything.
//
// # Visibility
//
// Messages become visible only when their import completes (NFR-REL-002).
// Instead of flipping a flag on every row, read queries join imports and
// require status = 'completed'; imports per chat are few, so the join is a
// primary-key lookup.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kshitijv/chat-analyzer/backend/internal/store"
	"github.com/kshitijv/chat-analyzer/backend/internal/tenancy"
)

//go:embed migrations/*.sql
var migrations embed.FS

// pragmas are applied to every connection the driver opens:
//   - foreign_keys: SQLite ships with FK enforcement off for compatibility.
//   - journal_mode=WAL: readers do not block the writer and vice versa.
//   - busy_timeout: wait up to 5 s for a lock instead of failing immediately.
//   - synchronous=NORMAL: safe with WAL (a power loss can lose the last
//     transaction but never corrupts the file) and much faster than FULL.
var pragmas = []string{
	"foreign_keys(1)",
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
}

// Store is the SQLite implementation of store.Store.
type Store struct {
	w *sql.DB // single-connection writer pool
	r *sql.DB // reader pool (== w for in-memory databases)
}

var _ store.Store = (*Store)(nil)

type options struct {
	readers int
}

// Option customises Open.
type Option func(*options)

// WithReaders sets the size of the reader pool (default 4).
func WithReaders(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.readers = n
		}
	}
}

// Open opens (creating if needed) the database at dsn. dsn may be a plain path
// ("chatanalyzer.db"), a "file:" URI, or ":memory:". It does not migrate; call
// Migrate (or rely on CA_MIGRATE_ON_START in the caller).
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	o := options{readers: 4}
	for _, opt := range opts {
		opt(&o)
	}

	memory := strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
	// The writer gets a 16 MiB page cache: bulk imports insert into several
	// indexes keyed by random fingerprints, and with the 2 MiB default the
	// cache spills to the WAL constantly (profiling showed ~30% of insert time
	// in page-cache stress). 64 MiB was faster still but pushed a large import
	// past the 150 MB RSS ceiling (FR-ING-013); 16 MiB keeps most of the gain.
	w, err := openPool(ctx, dsn, 1, "_txlock=immediate", "_pragma=cache_size(-16384)", "_pragma=temp_store(MEMORY)")
	if err != nil {
		return nil, err
	}
	s := &Store{w: w, r: w}
	if memory {
		// Keep the only connection alive forever: closing it drops the database.
		w.SetConnMaxLifetime(0)
		w.SetConnMaxIdleTime(0)
		return s, nil
	}
	// query_only comes after the shared pragmas so journal_mode is already
	// settled when the connection becomes read-only.
	r, err := openPool(ctx, dsn, o.readers, "_pragma=query_only(1)")
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	s.r = r
	return s, nil
}

// openPool opens a *sql.DB with the shared pragmas plus extra DSN parameters
// and verifies the connection.
func openPool(ctx context.Context, dsn string, maxConns int, extra ...string) (*sql.DB, error) {
	params := make([]string, 0, len(pragmas)+len(extra))
	for _, p := range pragmas {
		params = append(params, "_pragma="+p)
	}
	params = append(params, extra...)
	db, err := sql.Open("sqlite", withParams(dsn, params...))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: connect: %w", err)
	}
	return db, nil
}

// withParams appends query parameters to a DSN, turning a bare path into a
// "file:" URI so the driver parses the parameters.
func withParams(dsn string, params ...string) string {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(params, "&")
}

// migrationsFS roots the embedded files so goose sees them at the top level.
func migrationsFS() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // unreachable: the directory is embedded at compile time
	}
	return sub
}

// Migrate applies all pending embedded migrations with goose.
func (s *Store) Migrate(ctx context.Context) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, s.w, migrationsFS())
	if err != nil {
		return fmt.Errorf("sqlite: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	return s.r.PingContext(ctx)
}

// Close closes both pools.
func (s *Store) Close() error {
	err := s.w.Close()
	if s.r != s.w {
		err = errors.Join(err, s.r.Close())
	}
	return err
}

// inTx runs fn in a write transaction, committing on success.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return mapErr(tx.Commit())
}

// scope returns the tenant scope or tenancy.ErrNoScope.
func scope(ctx context.Context) (tenancy.Scope, error) {
	return tenancy.FromContext(ctx)
}

// mapErr translates driver errors into store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
	}
	return err
}

// Time helpers: instants are stored as Unix milliseconds in UTC.

func toMS(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toNullMS(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toMS(*t), Valid: true}
}

func fromNullMS(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMS(n.Int64)
	return &t
}

// nullUUID stores uuid.Nil as SQL NULL (optional foreign keys).
func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// jsonOr returns raw, or def when raw is empty, so JSON columns never receive
// an empty string (which json_valid rejects).
func jsonOr(raw []byte, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}
