// Package store is the persistence boundary (ADR-0003). It defines the domain
// records and the Store interface; dialect packages implement it:
//
//	store/sqlite    local profile (M1)
//	store/postgres  hosted profiles with Row-Level Security (M5)
//
// Tenant isolation (MT-001): every method except the explicitly global ones
// (Migrate, Ping, Close, EnsureLocalTenant) reads tenancy.Scope from ctx and
// confines reads and writes to that tenant. A missing scope returns
// tenancy.ErrNoScope. Rows of another tenant behave as if they did not exist
// (ErrNotFound), so IDs cannot be probed.
//
// Schema reference: docs/03-data-model.md. M1 creates tenants, users,
// memberships, chats, participants, imports and messages (+ full-text index);
// later milestones add tables through new migrations.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. Implementations wrap driver errors with these so callers
// can use errors.Is without knowing the dialect.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflict")
)

// Tenant is the isolation boundary (a workspace).
type Tenant struct {
	ID        uuid.UUID
	Name      string
	Slug      string
	Plan      string
	Status    string // active | suspended | pending_deletion
	CreatedAt time.Time
}

// User is a person who can sign in; global across tenants.
type User struct {
	ID           uuid.UUID
	Email        string
	DisplayName  string
	AuthProvider string // local | supabase | oidc
	CreatedAt    time.Time
}

// Chat is one imported two-person conversation.
type Chat struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Title    string
	Platform string // whatsapp | telegram
	// AnalysisTZ is the IANA timezone used for local dates (FR-ING-009).
	AnalysisTZ string
	// Settings holds analysis settings as JSON (docs/04-metrics-catalog.md).
	Settings json.RawMessage
	// Maintained by CompleteImport.
	MessageCount   int64
	FirstMessageAt *time.Time
	LastMessageAt  *time.Time
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
	ArchivedAt     *time.Time
}

// Participant is a sender in a chat (FR-ING-008).
type Participant struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ChatID      uuid.UUID
	DisplayName string
	Alias       string
	IsMe        bool
	// PlatformKeys are every raw sender key mapped to this participant
	// (ingest.Message.SenderKey); merging senders appends keys.
	PlatformKeys []string
}

// Import statuses (docs/03-data-model.md §4.2).
const (
	ImportProcessing = "processing"
	ImportCompleted  = "completed"
	ImportFailed     = "failed"
)

// Import records one upload and its outcome (FR-ING-011).
type Import struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	ChatID        uuid.UUID
	CreatedBy     uuid.UUID
	Platform      string
	Variant       string
	Status        string
	Timezone      string
	TimePrecision string
	FileName      string // base name only, a display hint
	FileSize      int64
	FileSHA256    []byte
	// Report is the ingest.Report as JSON.
	Report         json.RawMessage
	InsertedCount  int
	DuplicateCount int
	Error          string // user-safe message
	StartedAt      time.Time
	CompletedAt    *time.Time
}

// Message is a stored canonical message (docs/03-data-model.md §4.2).
type Message struct {
	ID            int64
	TenantID      uuid.UUID
	ChatID        uuid.UUID
	ImportID      uuid.UUID
	ParticipantID *uuid.UUID // nil for system events
	// Ordinal orders messages within a chat; ties on SentAt break by it.
	Ordinal int64
	SentAt  time.Time // UTC
	// LocalDate is YYYY-MM-DD in the chat's AnalysisTZ.
	LocalDate  string
	Kind       string
	MediaType  string
	SystemType string
	Body       string
	// ReplyToID is resolved from ReplyToPlatformID by ResolveReplies.
	ReplyToID         *int64
	ReplyToPlatformID string
	IsEdited          bool
	IsForwarded       bool
	IsDeleted         bool
	PlatformMsgID     string
	Fingerprint       []byte
	// Meta holds platform extras (media metadata, call info, reactions) as JSON.
	Meta json.RawMessage
}

// MessageQuery selects messages for browsing (FR-LIB-003) and verification.
type MessageQuery struct {
	ChatID uuid.UUID
	// From/To bound SentAt as a half-open interval [From, To); zero = unbounded.
	From, To time.Time
	// AfterOrdinal is a keyset cursor: return messages with Ordinal > it.
	AfterOrdinal int64
	Limit        int
}

// Store is implemented by each SQL dialect. All methods are safe for
// concurrent use.
type Store interface {
	// Migrate applies pending schema migrations (NFR-REL-004).
	Migrate(ctx context.Context) error
	// Ping checks connectivity; used by GET /readyz.
	Ping(ctx context.Context) error
	Close() error

	// EnsureLocalTenant returns the single implicit tenant and user of local
	// mode (FR-ADM-001), creating them on first use. Not tenant-scoped.
	EnsureLocalTenant(ctx context.Context) (Tenant, User, error)

	CreateChat(ctx context.Context, c Chat) (Chat, error)
	GetChat(ctx context.Context, id uuid.UUID) (Chat, error)
	ListChats(ctx context.Context) ([]Chat, error)
	// DeleteChat hard-deletes the chat and everything derived from it
	// (FR-LIB-002, PRV-006).
	DeleteChat(ctx context.Context, id uuid.UUID) error

	// ListParticipants returns a chat's participants in creation order.
	ListParticipants(ctx context.Context, chatID uuid.UUID) ([]Participant, error)
	// UpsertParticipant creates p, or updates it when p.ID exists.
	UpsertParticipant(ctx context.Context, p Participant) (Participant, error)

	CreateImport(ctx context.Context, im Import) (Import, error)
	GetImport(ctx context.Context, id uuid.UUID) (Import, error)
	// ListImports returns a chat's imports, newest first.
	ListImports(ctx context.Context, chatID uuid.UUID) ([]Import, error)
	// FindCompletedImportBySHA256 finds an identical earlier upload into the
	// chat, so the importer can short-circuit (ErrNotFound when none).
	FindCompletedImportBySHA256(ctx context.Context, chatID uuid.UUID, sum []byte) (Import, error)

	// InsertMessages inserts msgs in one transaction, silently skipping rows
	// whose (chat_id, fingerprint) already exists. It returns how many rows
	// were actually inserted.
	InsertMessages(ctx context.Context, msgs []Message) (inserted int, err error)
	// MaxOrdinal returns the highest ordinal in the chat (0 when empty).
	MaxOrdinal(ctx context.Context, chatID uuid.UUID) (int64, error)

	// CompleteImport atomically marks the import completed with its final
	// report and counts, resolves reply links, and refreshes the chat's
	// counters. Messages of an import are invisible to ListMessages and
	// CountMessages until this succeeds (NFR-REL-002).
	CompleteImport(ctx context.Context, im Import) error
	// FailImport marks the import failed and deletes the messages it inserted.
	FailImport(ctx context.Context, importID uuid.UUID, userError string, report json.RawMessage) error

	// ListMessages returns visible messages ordered by (SentAt, Ordinal).
	ListMessages(ctx context.Context, q MessageQuery) ([]Message, error)
	// CountMessages counts visible messages in the chat.
	CountMessages(ctx context.Context, chatID uuid.UUID) (int64, error)
	// SearchMessages runs a full-text query over visible message bodies in the
	// chat, best matches first (FR-LIB-003; SQLite FTS5 / Postgres tsvector).
	SearchMessages(ctx context.Context, chatID uuid.UUID, query string, limit int) ([]Message, error)
}
