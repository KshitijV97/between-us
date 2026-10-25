# Container image for the cloud-free-container and local-full profiles
# (docs/07-deployment.md). Three stages keep the runtime image tiny: the final
# image holds only the static Go binary, which embeds the web UI.

# --- 1. Web UI -----------------------------------------------------------------
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build && npm run -s size

# --- 2. Go binary --------------------------------------------------------------
FROM golang:1.27-alpine AS api
# The base image already provides the toolchain pinned in go.mod.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/web/dist/ ./internal/webui/dist/
ARG VERSION=dev
ARG COMMIT=""
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/kshitijv97/chat-analyzer/backend/internal/buildinfo.Version=${VERSION} -X github.com/kshitijv97/chat-analyzer/backend/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/chatanalyzer ./cmd/chatanalyzer

# --- 3. Runtime ----------------------------------------------------------------
# Distroless "static" has no shell or package manager (small attack surface,
# SEC-007) and runs as an unprivileged user.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=api /out/chatanalyzer /usr/local/bin/chatanalyzer
ENV CA_PROFILE=cloud-free-container
EXPOSE 7420
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/chatanalyzer"]
CMD ["serve"]
