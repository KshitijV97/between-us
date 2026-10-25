# ADR-0006: OpenTelemetry with Grafana LGTM (local) and Grafana Cloud free (hosted)

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | NFR-OBS-001..008, G-7, K-5 |
| Related | [ADR index](README.md) · [Observability](../06-observability.md) |

## Context

Observability, monitoring and alerting must be built in from day one, vendor-neutral, free, and fully local
when desired. Message content must never reach telemetry.

## Decision

Instrument with the OpenTelemetry Go SDK (traces, metrics, logs via an `slog` bridge) exporting OTLP.
Locally, `deploy/compose` runs `grafana/otel-lgtm` (Grafana, Loki, Tempo, Prometheus) as one container.
Hosted, export to Grafana Cloud's free tier or any OTLP backend. A Prometheus `/metrics` endpoint is also
available. Dashboards and alert rules are versioned in `deploy/grafana/`. Attribute redaction is enforced in
`internal/telemetry`.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Vendor SDKs (Datadog, New Relic) | Turnkey | Lock-in, cost | NFR-COST-004 |
| Prometheus + plain logs only | Simple | No tracing | NFR-OBS-001 |
| SigNoz / Uptrace self-hosted | FOSS, OTel-native | Heavier locally (ClickHouse) | LGTM image is lighter; both stay possible via OTLP |

## Consequences

- **Positive:** one standard; backend swappable by env vars; local and hosted parity.
- **Negative:** free-tier quotas on series/logs require sampling and cardinality discipline.
- **Follow-ups:** cardinality budget (no IDs as metric labels); synthetic uptime checks; SLO burn-rate alerts.
