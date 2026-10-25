# ADR-0008: Multi-tenancy via `tenant_id` scoping plus Postgres Row-Level Security

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | MT-001..005, MT-009, SEC-006, acceptance criterion 7 |
| Related | [ADR index](README.md) · [Data model §6](../03-data-model.md#6-tenant-isolation) |

## Context

Hosted mode serves many workspaces from one database on a free tier. A tenant must never see another's data,
even through a bug in a single query. Local mode has one tenant.

## Decision

Use a shared schema with a `tenant_id` column on every tenant-owned table. Isolation is layered:
(1) repository methods always filter by the tenant from `context.Context`; (2) Postgres RLS with `FORCE ROW
LEVEL SECURITY` and policies on `app.current_tenant()` set per transaction via `set_config('app.tenant_id',
…, true)`; (3) the app role has no `BYPASSRLS`; cross-tenant job claiming uses a narrow `SECURITY DEFINER`
function; (4) an automated cross-tenant test suite. Tables live in schema `app`, not exposed by Supabase's Data API.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Database per tenant | Strongest isolation | Impossible on free tiers | Cost |
| Schema per tenant | Good isolation | Migration fan-out, connection limits | Operational cost |
| App-level filtering only | Simple, portable | One bug leaks data | Insufficient for enterprise grade |

## Consequences

- **Positive:** cheap, scalable, defence in depth.
- **Negative:** every query and index must include tenant context; RLS adds slight overhead.
- **Follow-ups:** lint for unscoped queries; RLS tests in CI against real Postgres.
