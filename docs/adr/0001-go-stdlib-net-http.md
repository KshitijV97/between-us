# ADR-0001: Go with the standard library `net/http` for the backend

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | NFR-PERF-001, NFR-COST-001, FR-API-001, FR-API-003 |
| Related | [ADR index](README.md) · [Architecture §4](../02-architecture.md#4-level-3--backend-components) |

## Context

The backend must be lean (≤ 40 MB binary, ≤ 60 MB idle RSS, ≤ 1 s cold start), run as a single local binary,
as a container, and as a Vercel Go Function. The owner prefers Go. Go 1.22 added method and wildcard
patterns to `http.ServeMux`, removing the main reason to adopt a router framework.

## Decision

Write the backend in Go (≥ 1.22) using only `net/http` for routing and middleware, `log/slog` for logging,
and small, well-maintained libraries where the standard library has no answer (pgx, modernc SQLite, goose,
OpenTelemetry). One binary `chatanalyzer` exposes subcommands `serve`, `migrate`, `import`, `worker`, `version`.
`httpapi.Handler()` returns an `http.Handler` used by both `serve` and the Vercel adapter.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| chi / echo / gin | Middleware ecosystem, familiar | Extra dependency; features now in stdlib | Not needed since Go 1.22 |
| Node.js (Express/Fastify) | One language with frontend | Higher memory, slower cold start | Leanness and owner preference |
| Rust (axum) | Even leaner | Slower iteration, smaller talent pool | Productivity |

## Consequences

- **Positive:** minimal dependencies and attack surface; trivial cross-compilation; stable APIs.
- **Negative:** a few helpers (problem+json, request decoding, SSE) are written in-house.
- **Follow-ups:** keep middleware in `internal/httpapi/middleware`; OpenAPI conformance tests guard handlers.
