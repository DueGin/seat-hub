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

mock:
	go run ./cmd/mock-vm2api

test:
	go test ./...

fmt:
	gofmt -w cmd internal
