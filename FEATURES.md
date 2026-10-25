# Feature log

What Chat Analyzer can do today, what is planned next, and ideas being considered. For the dated, commit-level
history see the [changelog](CHANGELOG.md); for scope and requirement IDs see the
[requirements](docs/01-requirements.md) and [roadmap](docs/10-roadmap.md).

**Legend:** ✅ shipped · 🔜 planned (has requirement IDs and a milestone) · 💡 idea (not yet in the requirements)

---

## ✅ Shipped

### M1 — Import (2026-10-25)

| Feature | How to use | Requirements |
|---|---|---|
| **WhatsApp import:** `.txt` and `.zip`, Android and iOS, 12/24-hour clocks, automatic date-order detection, multi-line messages, system messages, edited/deleted markers, calls, media placeholders, DST-safe times | `chatanalyzer import <file>` | FR-ING-002…005, FR-ING-009 |
| **Telegram import:** Desktop JSON export (streamed, so very large files are fine), replies, edits, reactions, calls, polls; picks one chat from a full-account export | `chatanalyzer import result.json` (`-telegram-chat <id>` for full exports) | FR-ING-006 |
| **Preview before importing:** detected format, participants, period, date order and warnings; nothing is written | `-dry-run` | FR-ING-011 |
| **Safe zip handling:** nothing is extracted to disk; size, count and compression limits; path-traversal and symlink guards; media counted, not stored | automatic | FR-ING-005, SEC-003 |
| **Incremental re-import:** importing a newer export of the same chat adds only new messages, even with a different timezone | `-chat <id>` | FR-ING-010 |
| **Mark yourself and pick participants:** "this is me", and choose two senders from a group export | `-me <key>`, `-participants a,b` | FR-ING-008, FR-ING-012 |
| **Chat library (CLI):** list chats and permanently delete one with all its messages | `chatanalyzer chats`, `chats delete <id>` | FR-LIB-001, FR-LIB-002 |
| **Local database:** a single SQLite file in your OS app-data folder, full-text index, tenant isolation built in from day one | `CA_DATA_DIR` to relocate | ADR-0003, MT-001 |
| **Privacy by default:** the export file is never kept; no network calls; message text never appears in logs | automatic | FR-ING-014, PRV-001, PRV-007 |
| **Fictional demo data:** generate realistic synthetic chats of any size | `make demo`, `go run ./cmd/synthchat` | NFR-MNT-002 |

### M0 — Foundation (2026-10-24)

| Feature | Notes |
|---|---|
| Single binary `chatanalyzer` with the web UI embedded | ~12 MB; ~11 MB RAM when idle |
| Health, readiness and version endpoints | `/healthz`, `/readyz`, `/version` |
| Hardened HTTP layer | Security headers, request IDs, JSON problem errors, panic recovery, graceful shutdown |
| Web app shell (React + TypeScript) | Shows API status; light/dark theme |
| Configuration profiles | `CA_PROFILE`: local, local-full, cloud-free or cloud-free-container ([.env.example](.env.example)) |
| Container image | ~9 MB distroless image (`make docker`) |
| CI and security gates | Lint, tests, smoke tests, gosec, gitleaks, OSV, govulncheck, CodeQL, Dependabot, release-please |
| Documentation set | Requirements, architecture, data model, metrics, AI, observability, deployment, security, notifications, roadmap, ADRs |

---

## 🔜 Planned

| Milestone | Features | Requirements |
|---|---|---|
| **M2 — Analytics engine and API** | Conversations (sessions), who initiates and ends, response times, reply rate, double-texting, streaks and silences, hour × weekday heatmap, late-night activity, emoji and top words (incl. Hinglish), questions, links, media counts, calls, lexicon sentiment, affection and conflict markers, balance and engagement scores; for any **day, week, month, year or custom range**, compared with the previous period; versioned REST API | FR-ANA-001…006, M-001…M-027, FR-API-001/002 |
| **M3 — Web dashboard** | Import in the browser (drag and drop, preview, "this is me", timezone, consent), chat library, charts with drill-down to messages, message browser with search and permalinks, PWA for phones, accessibility pass | FR-ING-001, FR-LIB-001…004, FR-ANA-004/005, NFR-UX-* |
| **M4 — AI insights and Q&A** | Relationship profile, mood and mind per person and period, period summaries, topic timeline, questions over a period answered with message citations; local Ollama by default, works without any AI | FR-AI-001…007 |
| **M5 — Hosted and multi-tenant** | PostgreSQL with row-level security, sign-in (local, Supabase, OIDC), workspaces, quotas, audit log, data export and account deletion, free deployment on Vercel + Supabase/Aiven | MT-*, FR-ADM-*, PRV-004…006 |
| **M6 — Observability and notifications** | OpenTelemetry traces and metrics, Grafana dashboards and alerts as code, email notifications (SMTP or HTTP API), alert routing | NFR-OBS-*, FR-NOT-001…004 |
| **M7 — MVP release** | Release binaries for macOS, Linux and Windows; SBOM; hosted demo with synthetic data | §14 acceptance criteria |
| **P2 — Team and polish** | Roles and invites, encryption at rest, MFA, Telegram HTML import, media inventory, bookmarks and tags, CSV/PDF exports, side-by-side period comparison, notable-event detection, weekly digests, ntfy and Telegram-bot notifications, admin console, API keys | see [roadmap §3](docs/10-roadmap.md#3-post-mvp-phases) |
| **P3 — Breadth** | Photo analysis with local vision models, voice-note transcription (whisper.cpp), group chats, more platforms (Signal, Instagram, Messenger, iMessage, Discord), SMS notifications, SSO/SAML, webhooks | FR-ING-021, FR-NOT-006 |
| **Future** | Mobile apps, live Telegram sync, shareable reports, comparing several chats | FR-ING-030 |

---

## 💡 Ideas under consideration

Not yet in the requirements. Each needs a requirement ID, priority and milestone before work starts; open an
issue to discuss.

| Idea | Why it might be worth it |
|---|---|
| **Browser import before M3** | Use your own export from the web UI without waiting for the dashboard (pulls the M3 import flow forward). |
| **Parse in the browser (WebAssembly)** | Compile the Go parsers to WASM so an export never leaves the device, even on a hosted instance. |
| **"Year in review"** | A shareable-to-yourself recap of a year: busiest day, longest streak, top emoji, mood arc. |
| **"On this day"** | Resurface what the two of you talked about on the same date in earlier years. |
| **Drift alerts (opt-in)** | Notice when response times, initiations or sentiment shift noticeably compared with your own baseline. |
| **Period report with citations** | Export a period summary as Markdown/PDF with linked evidence messages. |
| **Compare two chats** | How you write to one person versus another (tone, timing, initiations). |
| **Screenshot-safe view** | Blur names, numbers and message text so charts can be shared without exposing content. |
| **Wider language support** | Per-message language detection and sentiment lexicons beyond English and Hinglish. |
