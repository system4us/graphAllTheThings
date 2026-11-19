# syntax=docker/dockerfile:1

# Stage 1: build the CGO-enabled gatt binary (go-tree-sitter requires CGO).
FROM golang:1.25-bookworm AS go-builder

ARG VERSION=docker
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gatt ./cmd/gatt

# Stage 2: install and prune the local ONNX embedding worker (CPU-only for target arch).
FROM node:22-bookworm-slim AS embed-builder

WORKDIR /opt/gatt-embed
COPY tools/gatt-embed/package.json tools/gatt-embed/package-lock.json tools/gatt-embed/prune.mjs ./
RUN npm ci --omit=dev
COPY tools/gatt-embed/server.mjs ./

# Stage 3: extract the Qdrant binary (runs strictly on loopback inside the container).
FROM qdrant/qdrant:v1.13.6 AS qdrant-bin

# Stage 4: minimal runtime with gatt + internal ONNX worker + internal Qdrant + git.
FROM node:22-bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git libunwind8 \
    && rm -rf /var/lib/apt/lists/* \
    && git config --global --add safe.directory '*' \
    && mkdir -p /data /cache/huggingface

COPY --from=go-builder /out/gatt /usr/local/bin/gatt
COPY --from=embed-builder /opt/gatt-embed /opt/gatt-embed
COPY --from=qdrant-bin /qdrant/qdrant /usr/local/bin/qdrant

ENV GATT_DATA_DIR=/data \
    GATT_EMBED_WORKER=/opt/gatt-embed \
    GATT_EMBED_CACHE=/cache/huggingface \
    GATT_QDRANT_BIN=/usr/local/bin/qdrant

EXPOSE 7788

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["node", "-e", "fetch('http://127.0.0.1:7788/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]

ENTRYPOINT ["gatt"]
CMD ["serve", "--addr", "0.0.0.0:7788", "--bind-any", "--mcp", "--data-dir", "/data", "--onnx", "/opt/gatt-embed", "--qdrant-bin", "/usr/local/bin/qdrant"]
