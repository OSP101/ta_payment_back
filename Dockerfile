# syntax=docker/dockerfile:1.7
#
# Built by deploy/docker-compose.yml with context = this directory.
# Cache mounts keep the module download and build cache across rebuilds, so a
# redeploy after a small code change compiles in seconds instead of minutes.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/api ./ && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/seed ./cmd/seed

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Bangkok
WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/seed /app/seed
COPY migrations /app/migrations
COPY assets /app/assets
# UPLOAD_DIR in compose points here; the named volume inherits this ownership
# on first creation. Nothing under /app is writable at runtime on purpose.
RUN mkdir -p /data/uploads && chown -R nobody:nobody /data
USER nobody
EXPOSE 8080
CMD ["/app/api"]
