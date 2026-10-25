package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/chat-analyzer/backend/internal/store"
)

// Local-mode identity (FR-ADM-001). The slug is fixed so EnsureLocalTenant is
// idempotent; the e-mail is a placeholder that never receives mail.
const (
	localSlug  = "local"
	localEmail = "local@localhost"
)

// EnsureLocalTenant implements store.Store. It runs in one write transaction,
// so concurrent first starts cannot create two local tenants.
func (s *Store) EnsureLocalTenant(ctx context.Context) (store.Tenant, store.User, error) {
	var (
		t store.Tenant
		u store.User
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = getTenantBySlug(ctx, tx, localSlug)
		switch {
		case errors.Is(err, store.ErrNotFound):
			t, err = insertTenant(ctx, tx, "Local workspace", localSlug)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		}

		u, err = getLocalOwner(ctx, tx, t.ID)
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		now := time.Now()
		u = store.User{ID: uuid.Must(uuid.NewV7()), Email: localEmail, DisplayName: "You", AuthProvider: "local", CreatedAt: now.UTC().Truncate(time.Millisecond)}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, email, display_name, auth_provider, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			u.ID.String(), u.Email, u.DisplayName, u.AuthProvider, toMS(now), toMS(now)); err != nil {
			return mapErr(err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)`,
			t.ID.String(), u.ID.String(), toMS(now))
		return mapErr(err)
	})
	if err != nil {
		return store.Tenant{}, store.User{}, err
	}
	return t, u, nil
}

// CreateTenant creates an additional tenant. It is not part of store.Store yet
// (hosted sign-up arrives in M5); the contract test suite uses it through an
// optional interface to exercise tenant isolation.
func (s *Store) CreateTenant(ctx context.Context, name, slug string) (store.Tenant, error) {
	var t store.Tenant
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = insertTenant(ctx, tx, name, slug)
		return err
	})
	return t, err
}

func insertTenant(ctx context.Context, tx *sql.Tx, name, slug string) (store.Tenant, error) {
	now := time.Now()
	t := store.Tenant{ID: uuid.Must(uuid.NewV7()), Name: name, Slug: slug, Plan: "free", Status: "active", CreatedAt: now.UTC().Truncate(time.Millisecond)}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, slug, plan, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID.String(), t.Name, t.Slug, t.Plan, t.Status, toMS(now), toMS(now))
	return t, mapErr(err)
}

func getTenantBySlug(ctx context.Context, tx *sql.Tx, slug string) (store.Tenant, error) {
	var (
		t       store.Tenant
		created int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, slug, plan, status, created_at FROM tenants WHERE slug = ?`, slug,
	).Scan(&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &created)
	t.CreatedAt = fromMS(created)
	return t, mapErr(err)
}

// getLocalOwner returns the owner of the local tenant.
func getLocalOwner(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID) (store.User, error) {
	var (
		u       store.User
		created int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.display_name, u.auth_provider, u.created_at
		FROM users u JOIN memberships m ON m.user_id = u.id
		WHERE m.tenant_id = ? AND m.role = 'owner'
		ORDER BY m.created_at LIMIT 1`, tenantID.String(),
	).Scan(&u.ID, &u.Email, &u.DisplayName, &u.AuthProvider, &created)
	u.CreatedAt = fromMS(created)
	return u, mapErr(err)
}
