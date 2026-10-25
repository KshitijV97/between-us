# ADR-0003: SQLite for local, PostgreSQL for hosted, behind a `Store` interface

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | G-5, G-6, NFR-PERF-001, NFR-PERF-002, MT-002, NFR-COST-004 |
| Related | [ADR index](README.md) · [Data model](../03-data-model.md) |

## Context

Local mode must need no database server. Hosted mode must use a free managed database (Supabase or Aiven
Postgres) and needs RLS, concurrent writers, `tsvector` and `pgvector`.

## Decision

Implement `store.Store` twice: `store/sqlite` using the pure-Go `modernc.org/sqlite` driver (no CGO, FTS5
built in, WAL mode) and `store/postgres` using `pgx` (≥ PG 15, RLS, `tsvector`, `pgvector`). Migrations are
maintained per dialect with matching version numbers (goose). A shared contract test suite runs against both.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Postgres everywhere (Docker locally) | One dialect | Local needs Docker; not a single binary | Leanness, local-first |
| SQLite everywhere (Turso/libSQL hosted) | One dialect | No RLS; free tiers and FOSS story weaker | Multi-tenancy guarantees |
| ORM (GORM/ent) to hide dialects | Less SQL | Heavier, hides query plans | Explicit SQL preferred |

## Consequences

- **Positive:** zero-dependency local install; managed Postgres in cloud; no lock-in.
- **Negative:** two sets of SQL and migrations to maintain; some features differ (vectors brute-forced locally).
- **Follow-ups:** CI runs store tests on both dialects; schema-snapshot comparison.
