// Package tenancy carries the active tenant through a request or job.
//
// Every store method reads the scope from its context and filters by it
// (MT-001). A context without a scope is a programming error: the store
// returns ErrNoScope rather than silently reading across tenants.
package tenancy

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNoScope is returned when a tenant-scoped operation runs without a scope.
var ErrNoScope = errors.New("tenancy: no tenant scope in context")

// Scope identifies who is acting and on behalf of which tenant.
type Scope struct {
	TenantID uuid.UUID
	// UserID is uuid.Nil for system jobs.
	UserID uuid.UUID
}

type ctxKey struct{}

// WithScope returns a context carrying s.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the scope in ctx, or ErrNoScope.
func FromContext(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(ctxKey{}).(Scope)
	if !ok || s.TenantID == uuid.Nil {
		return Scope{}, ErrNoScope
	}
	return s, nil
}
