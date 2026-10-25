-- M1 schema for the local (SQLite) profile. Logical model and column notes:
-- docs/03-data-model.md §4. Type mapping (§1.1): UUIDs are TEXT, instants are
-- INTEGER Unix milliseconds (UTC), calendar days are TEXT 'YYYY-MM-DD', JSON is
-- TEXT guarded by json_valid(). Enums are TEXT with CHECK constraints so new
-- values only need a migration, not a type change.
--
-- Analytics columns (per-message features, rollups, sessions) arrive with M2
-- in later migrations; this file only covers ingestion.

-- +goose Up

-- Tenants are the isolation boundary (MT-001). Local mode has exactly one.
CREATE TABLE tenants (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    slug        TEXT    NOT NULL UNIQUE,
    plan        TEXT    NOT NULL DEFAULT 'free',
    status      TEXT    NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'suspended', 'pending_deletion')),
    settings    TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(settings)),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER
);

-- Users are global; a user may belong to many tenants (MT-003).
CREATE TABLE users (
    id             TEXT    PRIMARY KEY,
    email          TEXT    NOT NULL,
    display_name   TEXT    NOT NULL DEFAULT '',
    auth_provider  TEXT    NOT NULL CHECK (auth_provider IN ('local', 'supabase', 'oidc')),
    auth_subject   TEXT    NOT NULL DEFAULT '',
    password_hash  TEXT,
    last_login_at  INTEGER,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
-- Case-insensitive uniqueness without a citext type.
CREATE UNIQUE INDEX users_email_lower ON users (lower(email));
CREATE UNIQUE INDEX users_auth_subject ON users (auth_provider, auth_subject) WHERE auth_subject <> '';

CREATE TABLE memberships (
    tenant_id   TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id     TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        TEXT    NOT NULL CHECK (role IN ('owner', 'admin', 'analyst', 'viewer')),
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE chats (
    id                TEXT    PRIMARY KEY,
    tenant_id         TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    title             TEXT    NOT NULL,
    platform          TEXT    NOT NULL CHECK (platform IN ('whatsapp', 'telegram')),
    analysis_tz       TEXT    NOT NULL,
    settings          TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(settings)),
    cover             TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(cover)),
    -- Counters over visible messages, refreshed when an import completes.
    message_count     INTEGER NOT NULL DEFAULT 0,
    first_message_at  INTEGER,
    last_message_at   INTEGER,
    rollup_version    INTEGER NOT NULL DEFAULT 0,
    created_by        TEXT    REFERENCES users (id) ON DELETE SET NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    archived_at       INTEGER,
    deleted_at        INTEGER
);
CREATE INDEX chats_tenant_created ON chats (tenant_id, created_at DESC);

CREATE TABLE participants (
    id              TEXT    PRIMARY KEY,
    tenant_id       TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    chat_id         TEXT    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    display_name    TEXT    NOT NULL,
    alias           TEXT    NOT NULL DEFAULT '',
    is_me           INTEGER NOT NULL DEFAULT 0 CHECK (is_me IN (0, 1)),
    -- Every raw sender key mapped to this participant (FR-ING-008 merges).
    platform_keys   TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(platform_keys)),
    merged_into_id  TEXT    REFERENCES participants (id) ON DELETE SET NULL,
    color           TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX participants_chat ON participants (chat_id, created_at);
-- At most one participant per chat is "me".
CREATE UNIQUE INDEX participants_one_me ON participants (chat_id) WHERE is_me = 1;

CREATE TABLE imports (
    id               TEXT    PRIMARY KEY,
    tenant_id        TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    chat_id          TEXT    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    created_by       TEXT    REFERENCES users (id) ON DELETE SET NULL,
    platform         TEXT    NOT NULL,
    variant          TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL
                             CHECK (status IN ('uploaded', 'detected', 'confirmed', 'processing',
                                               'completed', 'failed', 'cancelled')),
    timezone         TEXT    NOT NULL DEFAULT '',
    time_precision   TEXT    NOT NULL DEFAULT '' CHECK (time_precision IN ('', 'second', 'minute')),
    file_name        TEXT    NOT NULL DEFAULT '',
    file_size        INTEGER NOT NULL DEFAULT 0,
    -- Detects identical re-uploads; the file itself is never stored (FR-ING-014).
    file_sha256      BLOB,
    report           TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(report)),
    inserted_count   INTEGER NOT NULL DEFAULT 0,
    duplicate_count  INTEGER NOT NULL DEFAULT 0,
    error            TEXT    NOT NULL DEFAULT '',
    started_at       INTEGER NOT NULL,
    completed_at     INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX imports_chat_started ON imports (chat_id, started_at DESC);
CREATE INDEX imports_chat_sha ON imports (chat_id, file_sha256) WHERE file_sha256 IS NOT NULL;

-- The largest table. INTEGER PRIMARY KEY aliases the rowid, which the FTS5
-- external-content table below uses as its key.
CREATE TABLE messages (
    id                    INTEGER PRIMARY KEY,
    tenant_id             TEXT    NOT NULL,
    chat_id               TEXT    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    import_id             TEXT    NOT NULL REFERENCES imports (id) ON DELETE CASCADE,
    participant_id        TEXT    REFERENCES participants (id) ON DELETE SET NULL,
    ordinal               INTEGER NOT NULL,
    sent_at               INTEGER NOT NULL,
    local_date            TEXT    NOT NULL,
    kind                  TEXT    NOT NULL
                                  CHECK (kind IN ('text', 'media', 'call', 'location', 'contact',
                                                  'poll', 'deleted', 'system')),
    media_type            TEXT    NOT NULL DEFAULT '',
    event_type            TEXT    NOT NULL DEFAULT '',
    body                  TEXT    NOT NULL DEFAULT '',
    -- Resolved from reply_to_platform_id when the import completes. There is
    -- deliberately no self-referencing foreign key: replies are only ever
    -- deleted together with their chat or import, and the extra FK check on
    -- every insert and delete would slow bulk imports for no integrity gain.
    reply_to_id           INTEGER,
    reply_to_platform_id  TEXT    NOT NULL DEFAULT '',
    is_edited             INTEGER NOT NULL DEFAULT 0,
    is_forwarded          INTEGER NOT NULL DEFAULT 0,
    is_deleted_marker     INTEGER NOT NULL DEFAULT 0,
    platform_msg_id       TEXT    NOT NULL DEFAULT '',
    fingerprint           BLOB    NOT NULL,
    meta                  TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(meta))
);
-- De-duplication across re-imports (FR-ING-010).
CREATE UNIQUE INDEX messages_chat_fingerprint ON messages (chat_id, fingerprint);
-- Window range scans and the message browser cursor.
CREATE INDEX messages_chat_time ON messages (chat_id, sent_at, ordinal);
-- Rollup rebuilds for affected days (M2).
CREATE INDEX messages_chat_local_date ON messages (chat_id, local_date);
-- Per-person queries and Q&A tools.
CREATE INDEX messages_chat_participant ON messages (chat_id, participant_id, sent_at);
-- FailImport deletes and visibility joins by import.
CREATE INDEX messages_import ON messages (import_id);
-- Reply resolution looks targets up by platform ID within the chat.
CREATE INDEX messages_chat_platform_id ON messages (chat_id, platform_msg_id) WHERE platform_msg_id <> '';
CREATE INDEX messages_reply_to ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;

-- Full-text index over bodies (FR-LIB-003, docs/03-data-model.md §5.1).
-- External content: the text lives only in messages, the index stores tokens.
-- unicode61 with remove_diacritics 2 folds accents so "cafe" finds "café",
-- and tokenises Devanagari and romanised Hindi alike.
CREATE VIRTUAL TABLE messages_fts USING fts5 (
    body,
    content = 'messages',
    content_rowid = 'id',
    tokenize = 'unicode61 remove_diacritics 2'
);

-- Triggers keep the index in sync. Empty bodies (pure media, calls, system
-- events) are never indexed, so the delete trigger skips them too: an FTS5
-- external-content 'delete' must exactly mirror what was inserted.
-- +goose StatementBegin
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages WHEN new.body <> '' BEGIN
    INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages WHEN old.body <> '' BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER messages_fts_update AFTER UPDATE OF body ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body)
        SELECT 'delete', old.id, old.body WHERE old.body <> '';
    INSERT INTO messages_fts (rowid, body)
        SELECT new.id, new.body WHERE new.body <> '';
END;
-- +goose StatementEnd

-- +goose Down
-- Development only: production migrations are forward-only (NFR-REL-004).
DROP TRIGGER IF EXISTS messages_fts_update;
DROP TRIGGER IF EXISTS messages_fts_delete;
DROP TRIGGER IF EXISTS messages_fts_insert;
DROP TABLE IF EXISTS messages_fts;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS imports;
DROP TABLE IF EXISTS participants;
DROP TABLE IF EXISTS chats;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
