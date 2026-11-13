-- M2 analytics schema (docs/04-metrics-catalog.md §1, §6; docs/03-data-model.md §4.2).
--
-- Per-message derived features live on messages so the exact path (§5.5) and
-- drill-downs can recompute any metric from the window's rows. Sessions and
-- daily rollups are fully derived: an analysis run replaces them inside one
-- write transaction, so they never need to be migrated, only rebuilt.

-- +goose Up

-- Derived per-message columns (analytics.MessageFeatures). Defaults describe
-- "not analysed yet": no session, not a turn start, no features. None of them
-- is the body, so the FTS update trigger (AFTER UPDATE OF body) never fires
-- for feature writes.
ALTER TABLE messages ADD COLUMN session_index  INTEGER NOT NULL DEFAULT -1;
ALTER TABLE messages ADD COLUMN is_turn_start  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN response_s     INTEGER;
ALTER TABLE messages ADD COLUMN word_count     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN char_count     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN emoji_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN link_count     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN is_question    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN sentiment      REAL;
ALTER TABLE messages ADD COLUMN affection_hits INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN conflict       INTEGER NOT NULL DEFAULT 0;

-- Bookkeeping for the last analysis run. chats.rollup_version already exists
-- from 00001 (0 = never analysed; analytics.RollupVersion starts at 1).
ALTER TABLE chats ADD COLUMN analyzed_at    INTEGER;

-- Conversations (§4.1), one row per session, numbered per chat.
CREATE TABLE sessions (
    tenant_id         TEXT    NOT NULL,
    chat_id           TEXT    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    idx               INTEGER NOT NULL,
    started_at        INTEGER NOT NULL,
    ended_at          INTEGER NOT NULL,
    -- Participant IDs without foreign keys: sessions are rebuilt wholesale,
    -- and a participant merge must not cascade into derived rows.
    initiator_id      TEXT,
    ender_id          TEXT,
    msg_count         INTEGER NOT NULL DEFAULT 0,
    turn_count        INTEGER NOT NULL DEFAULT 0,
    first_message_id  INTEGER NOT NULL DEFAULT 0,
    last_message_id   INTEGER NOT NULL DEFAULT 0,
    start_date        TEXT    NOT NULL,
    conflict_msg      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, idx)
);
-- ListSessions filters by start time.
CREATE INDEX sessions_chat_started ON sessions (chat_id, started_at);

-- Daily rollups (§6): one row per participant per local date per timezone.
-- The full analytics.Rollup is stored as JSON in payload: the row is always
-- read and written whole, a year is at most ~730 rows, and a JSON document
-- carries histograms and term maps without a column per bucket. msg_count is
-- duplicated as a real column for quick sanity queries.
CREATE TABLE daily_rollups (
    tenant_id       TEXT    NOT NULL,
    chat_id         TEXT    NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    tz              TEXT    NOT NULL,
    local_date      TEXT    NOT NULL,
    participant_id  TEXT    NOT NULL,
    rollup_version  INTEGER NOT NULL,
    computed_at     INTEGER NOT NULL,
    msg_count       INTEGER NOT NULL DEFAULT 0,
    -- Light facts as columns so RollupIndex never parses JSON.
    last_msg_at     INTEGER NULL,
    -- The rollup without its term maps, and the maps separately: they are
    -- most of the bytes and only long windows need them.
    payload         TEXT    NOT NULL CHECK (json_valid(payload)),
    terms           TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(terms)),
    PRIMARY KEY (chat_id, tz, local_date, participant_id)
);

-- +goose Down
-- Development only: production migrations are forward-only (NFR-REL-004).
DROP TABLE IF EXISTS daily_rollups;
DROP INDEX IF EXISTS sessions_chat_started;
DROP TABLE IF EXISTS sessions;
ALTER TABLE chats DROP COLUMN analyzed_at;
ALTER TABLE messages DROP COLUMN conflict;
ALTER TABLE messages DROP COLUMN affection_hits;
ALTER TABLE messages DROP COLUMN sentiment;
ALTER TABLE messages DROP COLUMN is_question;
ALTER TABLE messages DROP COLUMN link_count;
ALTER TABLE messages DROP COLUMN emoji_count;
ALTER TABLE messages DROP COLUMN char_count;
ALTER TABLE messages DROP COLUMN word_count;
ALTER TABLE messages DROP COLUMN response_s;
ALTER TABLE messages DROP COLUMN is_turn_start;
ALTER TABLE messages DROP COLUMN session_index;
