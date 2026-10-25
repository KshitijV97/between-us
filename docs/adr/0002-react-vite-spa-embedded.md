# ADR-0002: React + Vite SPA, embedded in the Go binary for local mode

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | NFR-PERF-004, NFR-UX-001, FR-API-001, FR-API-002, G-5 |
| Related | [ADR index](README.md) · [Architecture §5](../02-architecture.md#5-frontend-structure) |

## Context

The owner prefers TypeScript + React. Local mode must be a single download; hosted mode must be served from a
free CDN (Vercel/Cloudflare Pages). Server-side rendering would require a Node runtime and adds cost.

## Decision

Build a client-side SPA with React, TypeScript and Vite (TanStack Query, Tailwind + shadcn/ui, Recharts). The
production build is copied into `backend/webdist/` and embedded with `//go:embed` so `chatanalyzer serve`
serves UI and API from one origin. Hosted mode deploys the same build as static files. The SPA talks only to
`/api/v1` through a client generated from `openapi.yaml`.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Next.js | SSR, Vercel-native | Needs Node runtime; heavier; can't embed in Go binary | Leanness, local-first |
| SvelteKit / Solid | Smaller bundles | Not the preferred stack | Owner preference |
| Go templates + htmx | No JS build | Weak for rich interactive charts | Chart-heavy UI |

## Consequences

- **Positive:** one origin locally (no CORS), static hosting is free, same artefact everywhere.
- **Negative:** no SEO (irrelevant for a private app); initial load must be kept small via code splitting.
- **Follow-ups:** bundle-size budget check in CI; route-level lazy loading; PWA manifest.
