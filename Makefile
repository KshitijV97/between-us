# Between Us — developer entry points. Run `make help` for the list.
#
# Requirements: Go (any recent version; go.mod's toolchain line downloads the
# pinned one automatically), Node.js 24 with npm, and optionally Docker.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BACKEND := backend
WEB     := web
BIN     := bin/betweenus
UI_DIST := $(BACKEND)/internal/webui/dist

# Build metadata injected into internal/buildinfo (see its package docs).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/kshitijv97/between-us/backend/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## ---------------------------------------------------------------- setup

.PHONY: deps
deps: ## Install web dependencies and download Go modules
	cd $(WEB) && npm ci
	cd $(BACKEND) && go mod download

$(WEB)/node_modules: $(WEB)/package-lock.json
	cd $(WEB) && npm ci
	@touch $@

## ---------------------------------------------------------------- develop

.PHONY: dev
dev: $(WEB)/node_modules ## Run the Go API (:7420) and Vite dev server (:5173) together
	@echo "API → http://127.0.0.1:7420   UI → http://127.0.0.1:5173 (Ctrl-C stops both)"
	@trap 'kill 0' INT TERM EXIT; \
	  (cd $(BACKEND) && BU_LOG_LEVEL=debug go run ./cmd/betweenus serve) & \
	  (cd $(WEB) && npm run dev -- --host 127.0.0.1) & \
	  wait

.PHONY: run
run: build ## Build everything and run the single binary (embedded UI)
	./$(BIN) serve

## ---------------------------------------------------------------- build

.PHONY: build
build: web-build embed-ui ## Build the web UI and the Go binary with the UI embedded
	cd $(BACKEND) && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o ../$(BIN) ./cmd/betweenus
	@ls -lh $(BIN) | awk '{print "built " $$9 " (" $$5 ")"}'

.PHONY: web-build
web-build: $(WEB)/node_modules ## Production build of the web UI into web/dist
	cd $(WEB) && npm run build && npm run -s size

# Copy the UI into the Go embed directory, keeping the committed placeholder.
.PHONY: embed-ui
embed-ui:
	@find $(UI_DIST) -mindepth 1 ! -name .gitkeep -delete
	cp -R $(WEB)/dist/. $(UI_DIST)/

.PHONY: docker
docker: ## Build the container image (cloud-free-container profile)
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t betweenus:$(VERSION) .

.PHONY: demo
demo: build ## Import a fictional 5,000-message chat into a throwaway data dir
	@mkdir -p .demo
	cd $(BACKEND) && go run ./cmd/synthchat -format whatsapp -n 5000 > "../.demo/WhatsApp Chat with Bob.txt"
	BU_DATA_DIR=.demo ./$(BIN) import -tz Asia/Kolkata ".demo/WhatsApp Chat with Bob.txt"
	@echo "Inspect with: BU_DATA_DIR=.demo ./$(BIN) chats"

## ---------------------------------------------------------------- quality

.PHONY: test
test: test-go test-web ## Run all tests

.PHONY: test-go
test-go:
	cd $(BACKEND) && go test -race -count=1 ./...

.PHONY: test-web
test-web: $(WEB)/node_modules
	cd $(WEB) && npm test

.PHONY: lint
lint: lint-go lint-web ## Run all linters and format checks

.PHONY: lint-go
lint-go:
	@cd $(BACKEND) && unformatted="$$(gofmt -l .)" && \
	  if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	cd $(BACKEND) && go vet ./... && go tool staticcheck ./...

.PHONY: lint-web
lint-web: $(WEB)/node_modules
	cd $(WEB) && npm run -s format:check && npm run -s lint && npm run -s typecheck

.PHONY: vuln
vuln: $(WEB)/node_modules ## Scan dependencies for known vulnerabilities
	cd $(BACKEND) && go tool govulncheck ./...
	cd $(WEB) && npm audit --omit=dev --audit-level=high

.PHONY: fmt
fmt: $(WEB)/node_modules ## Format Go and web sources in place
	cd $(BACKEND) && gofmt -w .
	cd $(WEB) && npm run -s format

.PHONY: check
check: lint test vuln ## Everything CI runs, locally

## ---------------------------------------------------------------- housekeeping

.PHONY: clean
clean: ## Remove build outputs
	rm -rf bin $(WEB)/dist .demo
	@find $(UI_DIST) -mindepth 1 ! -name .gitkeep -delete
