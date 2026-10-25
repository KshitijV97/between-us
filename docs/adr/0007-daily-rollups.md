# ADR-0007: Per-participant daily rollups as the analytics foundation

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | FR-ANA-001..003, NFR-PERF-002, NFR-PERF-003, §7.1 |
| Related | [ADR index](README.md) · [Data model §4.2](../03-data-model.md#42-chat-domain) · [Metrics catalog](../04-metrics-catalog.md) |

## Context

Users switch between day, week, month, year, all-time and custom windows and compare with the previous window.
Scanning up to 1M messages per request would miss the p95 targets on free-tier databases.

## Decision

After each import, compute one `daily_rollups` row per participant per local calendar day (in the chat's
analysis timezone) with additive counters, an hour histogram, mergeable log-bucket response-time histograms
and bounded top-K lists. Sessions are materialised in `sessions`. Every window query aggregates rollups;
metrics that are not mergeable are computed on demand from messages and cached. Rollups carry a
`rollup_version` and are rebuilt for affected days on new imports, timezone or settings changes.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Compute from raw messages every time | Exact, simple | Slow at scale | NFR-PERF-003 |
| Hourly rollups | Finer detail | 24x rows | Daily + hour histogram is sufficient |
| OLAP engine (DuckDB, ClickHouse) | Fast analytics | Extra component / CGO | Leanness |

## Consequences

- **Positive:** any window is O(days); tiny storage; deterministic and testable.
- **Negative:** percentiles are approximate (bucketed); per-view timezone overrides need on-demand paths.
- **Follow-ups:** document bucket boundaries and error bounds in the metrics catalog.
