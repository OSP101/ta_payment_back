# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/seed ./cmd/seed

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Bangkok
WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/seed /app/seed
COPY migrations /app/migrations
COPY assets /app/assets
RUN mkdir -p /data/uploads && chown -R nobody /data
USER nobody
EXPOSE 8080
CMD ["/app/api"]
