# Build stages run on the build host's platform and cross-compile, so a
# multi-arch image does not emulate the Go and Node builds.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml* ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG BUILD_TIME=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN rm -rf internal/webdist/dist
COPY --from=web /src/web/dist ./internal/webdist/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/DueGin/seat-hub/internal/buildinfo.Version=${VERSION} -X github.com/DueGin/seat-hub/internal/buildinfo.Commit=${COMMIT} -X github.com/DueGin/seat-hub/internal/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/seat-hub ./cmd/seat-hub

FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/DueGin/seat-hub" \
      org.opencontainers.image.description="Claude 账号托管 / 租赁门面"
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 portal \
 && mkdir -p /data/app && chown -R portal:portal /data
USER portal
WORKDIR /app
COPY --from=build /out/seat-hub /app/seat-hub
# SEAT_HUB_SUPERVISOR=docker enables in-site update: the process exits after
# installing a new version and the container restart policy starts it again.
# Downloaded versions live in /data/app, on the same volume as the database.
ENV PORTAL_LISTEN=:8790 PORTAL_DB_PATH=/data/portal.db SEAT_HUB_SUPERVISOR=docker TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8790
ENTRYPOINT ["/app/seat-hub"]
