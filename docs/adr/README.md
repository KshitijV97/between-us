# Architecture Decision Records

| Field | Value |
|---|---|
| Document | ADR index |
| Status | Living document |
| Date | 2026-10-24 |
| Related | [Requirements](../01-requirements.md) · [Architecture](../02-architecture.md) · [Data model](../03-data-model.md) · [Metrics catalog](../04-metrics-catalog.md) · [AI insights](../05-ai-insights.md) · [Observability](../06-observability.md) · [Deployment](../07-deployment.md) · [Security & privacy](../08-security-privacy.md) · [Notifications](../09-notifications.md) · [Roadmap](../10-roadmap.md) · [WhatsApp format](../parsers/whatsapp.md) · [Telegram format](../parsers/telegram.md) |

ADRs use the [MADR](https://adr.github.io/madr/)-style [template](0000-template.md). Copy it to the next
number, keep it short, and never edit an accepted ADR's decision — supersede it with a new one.

| # | Title | Status |
|---|---|---|
| [0001](0001-go-stdlib-net-http.md) | Go with the standard library `net/http` | Accepted |
| [0002](0002-react-vite-spa-embedded.md) | React + Vite SPA embedded in the Go binary | Accepted |
| [0003](0003-sqlite-local-postgres-hosted.md) | SQLite local, PostgreSQL hosted, behind `Store` | Accepted |
| [0004](0004-db-backed-jobs-inprocess-pump.md) | DB-backed jobs with `inprocess` and `pump` modes | Accepted |
| [0005](0005-pluggable-llm-local-first.md) | Pluggable LLM provider, local-first default | Accepted |
| [0006](0006-opentelemetry-grafana-lgtm.md) | OpenTelemetry + Grafana LGTM / Grafana Cloud | Accepted |
| [0007](0007-daily-rollups.md) | Daily rollups as the analytics foundation | Accepted |
| [0008](0008-multi-tenancy-tenant-id-rls.md) | Multi-tenancy via `tenant_id` + Postgres RLS | Accepted |
| [0009](0009-pluggable-auth.md) | Pluggable auth: local, supabase, oidc | Accepted |
| [0010](0010-raw-exports-not-persisted.md) | Raw exports not persisted by default | Accepted |
