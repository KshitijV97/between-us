# ADR-0005: Pluggable LLM provider with a local-first default

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-10-24 |
| Deciders | Product owner |
| Requirements | FR-AI-001..007, PRV-001, PRV-003, NFR-COST-003, SEC-004 |
| Related | [ADR index](README.md) · [AI insights](../05-ai-insights.md) |

## Context

Chat content is Restricted data. AI features must work offline, at no cost, and optionally with hosted
providers whose free tiers may use inputs for training. Providers differ in tool calling and JSON modes.

## Decision

Define `llm.Provider` and `llm.Embedder` interfaces with adapters `none` (deterministic only, default),
`ollama` (recommended local), `openai-compatible` (Groq, OpenRouter, Gemini compat endpoint, LM Studio, vLLM,
llama.cpp server) and `anthropic`. All prompts treat chat text as untrusted data; outputs are schema-validated
and citation-checked. Hosted providers require explicit per-workspace opt-in, pass through PII redaction, and
are restricted by an egress allow-list.

## Alternatives considered

| Option | Pros | Cons | Why not chosen |
|---|---|---|---|
| Single hosted provider SDK | Simplest | Privacy, cost, lock-in | Violates local-first |
| LangChain-style framework | Many integrations | Heavy, Python/JS-centric | Leanness |
| Embedded inference in Go | No extra process | Large binary, slow on CPU | Ollama does this better |

## Consequences

- **Positive:** private by default; swap providers by config; zero cost possible.
- **Negative:** quality varies by model; capability detection and fallbacks needed.
- **Follow-ups:** evaluation set of synthetic chats to compare providers; per-tenant token quotas.
