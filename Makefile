.PHONY: dev build web test fmt

web:
	pnpm -C web install
	pnpm -C web build
	rm -rf internal/webdist/dist
	cp -R web/dist internal/webdist/dist

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/vm2api-portal ./cmd/portal

dev:
	PORTAL_DEV=1 go run ./cmd/portal

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
