.PHONY: dev build web test fmt mock

web:
	pnpm -C web install
	pnpm -C web build
	rm -rf internal/webdist/dist
	cp -R web/dist internal/webdist/dist

VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w \
	-X github.com/DueGin/seat-hub/internal/buildinfo.Version=$(VERSION) \
	-X github.com/DueGin/seat-hub/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/DueGin/seat-hub/internal/buildinfo.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# A release build: `make build VERSION=v1.2.3`. Untagged builds report "dev"
# and never offer an in-site update.
build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/seat-hub ./cmd/seat-hub

dev:
	PORTAL_DEV=1 go run ./cmd/seat-hub

# Loads .env so the mock and the portal agree on the master key
# (PORTAL_MOCK_VM2API_MASTER_KEY defaults to mock-master-key).
mock:
	@set -a; [ -f .env ] && . ./.env; set +a; \
		PORTAL_MOCK_VM2API_MASTER_KEY="$${VM2API_MASTER_KEY:-mock-master-key}" \
		go run ./cmd/mock-vm2api

test:
	go test ./...

fmt:
	gofmt -w cmd internal
