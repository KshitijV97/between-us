# ADR-0010: Raw chat exports are not persisted by default

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | FR-ING-014, PRV-004, PRV-006, NFR-COST-001 |
| Related | [ADR index](README.md) · [Architecture §8.1](../02-architecture.md#81-import-fr-ing-001014) · [Security & privacy](../08-security-privacy.md) |

## Context

Exports contain the full conversation (and in `.zip` files, media). Keeping them doubles the sensitive data
footprint, consumes scarce free-tier storage and complicates erasure.

## Decision

Stage uploads only for the duration of processing (temp file locally; short-TTL staging hosted), record a
SHA-256 of the file in `imports.file_sha256`, and delete the staged file when the import completes, fails or
is cancelled. Media files inside zips are enumerated for counts and never stored in MVP. Users may opt in with
`BU_KEEP_RAW_EXPORTS=true` (local) or a workspace setting (hosted) to allow re-processing with newer parsers.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Always keep originals | Easy re-parse | Privacy, storage cost | Data minimisation |
| Parse fully in the browser, upload canonical JSON | Server never sees raw file | Duplicate parser in TS/WASM | Possible later optimisation |

## Consequences

- **Positive:** smaller footprint, simpler erasure, fits free storage.
- **Negative:** parser improvements need the user to re-upload unless they opted in.
- **Follow-ups:** a sweeper job deletes orphaned staged files older than 1 h; consider WASM parsing in the browser.
