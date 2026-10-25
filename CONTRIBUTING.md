# Contributing

These conventions apply to all code and docs in this repository. They implement NFR-MNT-001…006 from the
[requirements](docs/01-requirements.md#93-maintainability-and-quality-nfr-mnt).

## Golden rules

1. **Never commit real chat exports**, screenshots of real chats, or secrets. Test fixtures are synthetic
   (fictional participants such as *Alice* and *Bob*). `.gitignore` blocks common export file names, but you
   are still responsible.
2. **Never log message content**, participant names or phone numbers. Use IDs.
3. **Every requirement-driven change references its ID** (`FR-ING-003`, `NFR-OBS-002` …) in the PR description
   and, where useful, in a code comment.

## Branches and commits

- Branch from `main`: `feat/<short-name>`, `fix/<short-name>`, `docs/<short-name>`.
- [Conventional Commits](https://www.conventionalcommits.org/): `feat(ingest): detect iOS WhatsApp headers`.
- Squash-merge PRs; the PR title becomes the commit message and feeds the changelog.

## Code comments (granular by design)

- **Go:** every exported identifier has a doc comment starting with its name. Every package has a `doc.go`
  explaining its responsibility and its boundaries.
- **TypeScript:** every module starts with a short header comment; exported functions/components use TSDoc.
- **Non-obvious logic gets an inline "why" comment:** date-order inference, sessionisation thresholds,
  timezone/DST handling, sketch merging, prompt construction, tenant scoping.
- Link back to the docs where an algorithm is specified, e.g.
  `// Algorithm: docs/04-metrics-catalog.md#m-010-response-time`.

## Tests

| Area | Expectation |
|---|---|
| Parsers | Golden-file tests per export variant in `backend/internal/ingest/testdata/` |
| Analytics | Table-driven tests with hand-computed expectations, including a DST boundary |
| Tenancy | Isolation tests: tenant A can never read tenant B (repository and RLS layers) |
| AI | Grounding validator unit tests; offline evaluation set on synthetic chats |
| Web | Component tests (Vitest + Testing Library); critical flows with Playwright (M3) |

## Docs

- Docs live in `docs/` and **must be linked from the [README](README.md)**.
- Diagrams use Mermaid in fenced ` ```mermaid ` blocks so they render on GitHub. Quote node labels that
  contain punctuation, and use `<br/>` for line breaks.
- Significant decisions get an ADR in `docs/adr/` (copy `0000-template.md`).

## Local checks

```bash
make check
```

This runs what CI runs: `gofmt`, `go vet`, `staticcheck`, `go test -race`, `govulncheck`, then Prettier,
ESLint, `tsc`, Vitest and `npm audit` for the web app. Go tools (`staticcheck`, `govulncheck`) are pinned in
`backend/go.mod` with the `tool` directive and run via `go tool`, so there is nothing extra to install.

The web app uses **npm** (bundled with Node.js); commit `web/package-lock.json` with dependency changes.
