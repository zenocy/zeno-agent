# syntax=docker/dockerfile:1.7

# ---- Stage 1: build the UI ---------------------------------------------------
FROM node:22-alpine AS ui
WORKDIR /src
COPY ui/package.json ui/package-lock.json* ./
RUN npm ci
COPY ui/ ./
# Vite outputs to ../cmd/zeno/ui-dist/ — mirror that path.
RUN mkdir -p /out/cmd/zeno && cp -r . /out/ui && \
    cd /out/ui && npm run build

# ---- Stage 2: build the Go binary -------------------------------------------
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# UI dist from stage 1 lands at the embed path.
COPY --from=ui /out/cmd/zeno/ui-dist ./cmd/zeno/ui-dist
ARG VERSION=dev
RUN CGO_ENABLED=1 go build \
    -ldflags="-s -w -X github.com/zenocy/zeno-v2/internal/http/api.Version=${VERSION}" \
    -o /out/zeno ./cmd/zeno

# ---- Stage 3: runtime --------------------------------------------------------
FROM gcr.io/distroless/cc-debian12 AS runtime
WORKDIR /app
COPY --from=build /out/zeno /app/zeno
ENV ZENO_DB_PATH=/data/zeno.db
EXPOSE 7777
VOLUME ["/data"]
ENTRYPOINT ["/app/zeno", "-config", "/config.yaml"]
