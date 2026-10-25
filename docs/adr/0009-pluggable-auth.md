# ADR-0009: Pluggable authentication — `local`, `supabase`, `oidc`

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | MT-007, MT-008, FR-ADM-001, FR-ADM-002, SEC-001, SEC-008 |
| Related | [ADR index](README.md) · [Architecture §9](../02-architecture.md#9-key-interfaces) · [Security & privacy](../08-security-privacy.md) |

## Context

Local mode needs no or minimal auth. The free hosted profile needs a free identity service. Enterprise tenants
will want SSO with their own IdP. Avoid lock-in.

## Decision

Define `auth.Provider` with three implementations: `local` (built-in accounts, Argon2id, server-side sessions
in HttpOnly cookies, optional passphrase lock in local mode), `supabase` (verify Supabase Auth JWTs via JWKS;
the free tier covers hosted sign-up/sign-in), and generic `oidc` (Keycloak, Zitadel, Authentik, Google…).
All providers yield the same `Principal`; users are linked by (`auth_provider`, `auth_subject`). Authorization
is independent of the provider (policy module + memberships).

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Supabase Auth only | Least code | No offline local mode; lock-in | Local-first |
| Self-hosted Keycloak/Zitadel always | Full-featured | Heavy for free/local | Leanness |
| Clerk / Auth0 | Polished | Proprietary, limits | FOSS preference |

## Consequences

- **Positive:** works offline, free hosted, enterprise-ready via OIDC.
- **Negative:** local provider means owning password security (rate limits, breached-password check, MFA in P2).
- **Follow-ups:** SAML (P3) through an OIDC broker; account-linking UX.
