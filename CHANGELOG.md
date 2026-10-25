# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Once the repository is on GitHub, release-please adds a section for
each release above "Unreleased" from Conventional Commit messages.

For a feature-by-feature view, including what is planned, see [FEATURES.md](FEATURES.md).

## [Unreleased]

### Rename (2026-10-25)

#### Changed

- The project is now **Between Us** (previously Chat Analyzer): repository `between-us`, Go module
  `github.com/kshitijv97/between-us/backend`, command `betweenus`, settings prefix `BU_` (was `CA_`), data
  folder `betweenus`, database file `betweenus.db`, telemetry service name `between-us`, metric prefix `bu_`.

### M1 — Import (2026-10-25)

#### Added

- WhatsApp parser: Android and iOS header variants, 12/24-hour clocks (including the iOS narrow no-break space
  before AM/PM), date-order inference with recorded evidence, multi-line messages, system-message and placeholder
  catalogue, edited/deleted markers, calls, DST gap and overlap handling, bounded memory.
- Telegram parser: streaming `result.json` decoder, text entities, replies, edits, reactions, media, calls,
  polls, locations and contacts (coordinates and vCards are not kept), chat selection in full-account exports.
- Zip handling without extraction: entry, size and compression-ratio limits checked before reading, path-traversal
  and symlink guards, media enumerated and counted.
- SQLite store: migrations, WAL with a single-writer pool and reader pool, FTS5 full-text index, tenant scoping on
  every query, imports visible only once complete, batched deletes; reusable store contract tests including
  tenant isolation.
- Import pipeline: preview then import, participant mapping, timezone-independent de-duplication, incremental
  re-import, rollback of failed or cancelled imports.
- CLI: `betweenus import` (`-dry-run`, `-tz`, `-me`, `-participants`, `-date-order`, `-chat`,
  `-telegram-chat`, `-entry`, `-json`), `betweenus chats [delete <id>]`, `betweenus migrate`.
- `serve` opens and migrates the database and reports it in `/readyz`.
- Synthetic chat generator (`cmd/synthchat`), `make demo`, CI import smoke test, golden fixtures and fuzz targets.

#### Fixed (found while verifying M1)

- Header-shaped lines with impossible dates were silently folded into the previous message; the import report
  now counts them and warns.
- Two zip entries could declare sizes whose sum overflowed and bypassed the total-size limit.
- A 64 MiB SQLite page cache pushed large imports over the 150 MB memory ceiling; reduced to 16 MiB.
- Fuzzing found two cases that produced a sender name without a sender key (punctuation-only WhatsApp names,
  Telegram calls without an actor).
- Re-uploading an identical file is now reported as "nothing to do" instead of an error.

#### Known limitations

- A WhatsApp message edited between two exports is imported twice (exports carry no message IDs).
- Importing 1M messages takes about 2 minutes; storage is about 640 bytes per message.
- Selecting two participants from a Telegram group export is not supported yet.

### M0 — Foundation (2026-10-24)

#### Added

- Go backend skeleton: `betweenus serve | version`, `CA_*` configuration with deployment profiles, structured
  logging with request IDs, health/readiness/version endpoints, RFC 9457 problem errors, security headers,
  panic recovery, graceful shutdown, embedded single-page app.
- React + TypeScript + Vite web shell with typed API client, strict linting, tests and a bundle-size budget.
- Makefile (`dev`, `build`, `run`, `test`, `lint`, `vuln`, `check`, `docker`), distroless Dockerfile,
  `.env.example`, `SECURITY.md`.
- GitHub Actions: CI with smoke tests and security gates, CodeQL, PR-title check, release-please, Dependabot.

### Documentation (2026-10-24)

#### Added

- Requirements specification, architecture, data model, metrics catalog, AI insights, observability,
  deployment (verified free-tier matrix), security and privacy, notifications, roadmap, WhatsApp and Telegram
  format specifications, ADRs 0001–0010, Apache-2.0 license.
