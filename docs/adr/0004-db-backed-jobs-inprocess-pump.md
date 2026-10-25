# ADR-0004: Database-backed jobs with `inprocess` and `pump` execution modes

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | NFR-REL-002, FR-NOT-004, NFR-COST-001, FR-ING-013 |
| Related | [ADR index](README.md) · [Architecture §10](../02-architecture.md#10-jobs-in-process-and-pump-modes) |

## Context

Imports, rollups, insights, embeddings and notifications are long-running. Free serverless hosts (Vercel
Hobby) cap function duration and run no background workers; free Redis/queues add another service. Locally
there is only one process.

## Decision

Store jobs in a `jobs` table in the main database (leases, checkpoints, retries, idempotency keys). Two
runners share the same handlers:

- `inprocess`: a goroutine pool polls the table (and `LISTEN/NOTIFY` on Postgres). Used locally and on
  container hosts.
- `pump`: `POST /api/v1/jobs/pump` executes job steps until a time budget (default 8 s) is spent. Triggered by
  the SPA while a user waits on an import, and by the synthetic uptime check plus a daily cron for background work (Vercel Hobby cron runs at most once a day, so it cannot drive interactive jobs).

Handlers are written as resumable steps that persist a checkpoint.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Redis + asynq / BullMQ | Mature | Extra service; free tiers limited | Cost, leanness |
| River (Postgres queue for Go) | Good Go library | Postgres-only; no SQLite | Dual-dialect requirement |
| Supabase Edge Functions / pg_cron | Managed | Vendor lock-in, other runtime | NFR-COST-004 |

## Consequences

- **Positive:** no broker; transactional enqueue with business data (outbox pattern); works on any host.
- **Negative:** pump mode makes background latency depend on triggers; throughput bounded by DB.
- **Follow-ups:** fairness per tenant; metrics for queue depth and age; alert on stuck jobs.
