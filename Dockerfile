# syntax=docker/dockerfile:1

# ---- web console ----------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web ./web
RUN cd web && npm run build

# ---- Go binary ------------------------------------------------------------
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/wb2api ./cmd/hub

# ---- runtime --------------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 wb2api \
 && adduser -S -D -H -u 10001 -G wb2api wb2api \
 && mkdir -p /data \
 && chown -R wb2api:wb2api /data
COPY --from=build /out/wb2api /usr/local/bin/wb2api
USER wb2api
EXPOSE 8788
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/wb2api"]
CMD ["-lan", "-port", "8788", "-accounts-dir", "/data/accounts", "-usage-dir", "/data/usage", "-catalog-dir", "/data/catalog"]
