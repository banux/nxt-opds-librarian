# syntax=docker/dockerfile:1

# ──────────────────────────────────────────────────────────────────────────────
# Stage 1 – Build
# Fully-static Go binary; modernc.org/sqlite is unused here so CGO_ENABLED=0
# is uncontroversial.
# ──────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /librarian \
    ./cmd/librarian

# ──────────────────────────────────────────────────────────────────────────────
# Stage 2 – Runtime
# Distroless static: no shell, no package manager, just CA bundle + tzdata.
# Runs as uid 65532 (nonroot). /config is mounted as a writable volume so
# `librarian pair` can persist the YAML.
# ──────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=builder /librarian /app/librarian

VOLUME ["/config"]
WORKDIR /app

EXPOSE 8080

# Default config path resolves to /config/config.yaml. Override with the
# LIBRARIAN_CONFIG env var if the operator wants a custom layout.
ENV LIBRARIAN_CONFIG=/config/config.yaml

ENTRYPOINT ["/app/librarian"]
CMD ["serve", "--listen", ":8080"]
