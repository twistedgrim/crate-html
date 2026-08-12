# syntax=docker/dockerfile:1.7

# --- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

ARG VERSION=0.1.0-dev
ARG UPDATE_REPOSITORY=Twistedgrim/crate-html

# Cache the module download as its own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binaries — no glibc dependency, run anywhere.
RUN LDFLAGS="-s -w -X github.com/Twistedgrim/crate-html/internal/buildinfo.Version=${VERSION} -X github.com/Twistedgrim/crate-html/internal/updater.Repository=${UPDATE_REPOSITORY}" && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="$LDFLAGS" -o /out/crated ./cmd/crated && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="$LDFLAGS" -o /out/crate  ./cmd/crate

# --- runtime ----------------------------------------------------------------
FROM alpine:3.22

RUN addgroup -S crate && adduser -S -G crate -H -D crate

COPY --from=build /out/crated /usr/local/bin/crated
COPY --from=build /out/crate  /usr/local/bin/crate

# XDG dirs live in /config (config + token) and /data (sites). Both are
# declared as volumes so they survive container rebuilds.
ENV XDG_CONFIG_HOME=/config \
    XDG_DATA_HOME=/data \
    XDG_STATE_HOME=/state \
    CRATE_LISTEN_ADDR=0.0.0.0:7777

RUN mkdir -p /config /data /state && \
    chown -R crate:crate /config /data /state

# hadolint ignore=DL3066
USER crate
EXPOSE 7777
VOLUME ["/config", "/data"]

ENTRYPOINT ["/usr/local/bin/crated"]
