# syntax=docker/dockerfile:1
FROM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml* ./
RUN pnpm install --frozen-lockfile || pnpm install
COPY web/ ./
RUN pnpm build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./internal/webdist/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/vm2api-portal ./cmd/portal

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 portal \
 && mkdir -p /data && chown -R portal:portal /data
USER portal
WORKDIR /app
COPY --from=build /out/vm2api-portal /app/vm2api-portal
ENV PORTAL_LISTEN=:8790 PORTAL_DB_PATH=/data/portal.db
VOLUME ["/data"]
EXPOSE 8790
ENTRYPOINT ["/app/vm2api-portal"]
