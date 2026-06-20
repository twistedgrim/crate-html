# syntax=docker/dockerfile:1.7

# --- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache the module download as its own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binaries — no glibc dependency, run anywhere.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/crated ./cmd/crated && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/crate  ./cmd/crate

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

USER crate
EXPOSE 7777
VOLUME ["/config", "/data"]

ENTRYPOINT ["/usr/local/bin/crated"]
