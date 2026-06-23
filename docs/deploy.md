# Deploying crate-html

Three deployment shapes, in order of escalation:

1. [Local foreground](#1-local-foreground-laptop) — `./bin/crated` in a terminal.
2. [Docker](#2-docker-persistent-daemon) — `task docker:up`, persistent across terminal sessions.
3. [Docker + tsdproxy on Tailscale](#3-docker--tsdproxy-on-tailscale-https-on-your-tailnet) — HTTPS, accessible from any tailnet device.

All three serve the same HTTP API; the differences are *who can reach the daemon* and *who owns the lifecycle*. crate-html itself stays HTTP-only — TLS, hostnames, and remote access are layered above it.

## 1. Local foreground (laptop)

The default for casual use and development. The daemon binds `127.0.0.1:7777` only.

```bash
task build
./bin/crated &
./bin/crate status
./bin/crate push ./my-site my-site
./bin/crate open my-site
```

When you close the terminal `crated` exits with it. Config + sites live under `$XDG_CONFIG_HOME/crate/` and `$XDG_DATA_HOME/crate/sites/` (see [`architecture.md`](architecture.md#storage-model) for the macOS-vs-Linux paths).

## 2. Docker (persistent daemon)

For a daemon that survives terminal sessions and reboots, use the bundled compose stack.

```bash
task docker:build
task docker:up
```

The image is a multi-stage build (~34 MB on `alpine:3.22`). Inside the container:

- `crated` binds `0.0.0.0:7777` (`CRATE_LISTEN_ADDR` env override in the Dockerfile).
- Config lives in the `crate-config` named volume mounted at `/config`. Token: `/config/crate/config.yaml` (XDG layout — `crate/` subdir is part of the app's path).
- Sites live in the `crate-data` named volume mounted at `/data`. Site files: `/data/crate/sites/<name>/`.
- Healthcheck runs `crate status` every 10s.

`task docker:down` preserves both volumes; `task docker:nuke` deletes them (calls `docker compose down -v`).

### Pushing from the host CLI (development)

The host `crate` CLI talks to the dockerized daemon via env vars:

```bash
eval "$(task docker:env)"      # exports CRATE_TOKEN and CRATE_BASE_URL
./bin/crate ls                 # now talks to the dockerized daemon
./bin/crate push ./my-site demo
```

Unset the vars or open a new terminal to go back to a host-side daemon.

### Pushing from inside the container (production agent path)

In production, the supported path is to pipe a tar of your site through `docker exec` — the CLI's stdin mode handles the upload in one command, the token never leaves the container, and no host binary is required:

```bash
tar -C ./my-site -cf - . | docker exec -i crated crate push - my-site
```

Other operations follow the same pattern:

```bash
docker exec crated crate ls
docker exec crated crate status
docker exec crated crate token          # print the bearer token to stdout
docker exec crated crate rm my-site
```

The container's `crate` CLI reads `/config/crate/config.yaml` automatically — no `CRATE_TOKEN` is needed inside the container.

If you'd rather stage files on the container's filesystem first (less common; useful if you already have a directory there), use `docker cp` followed by `crate push <dir> <name>`. The container runs as the `crate` user (uid 100), so files copied in with `docker cp` retain the host's ownership and may not be readable to `crated` directly — staging via `docker cp` then re-tarring through `crate push` is what makes the contents available, since the push extracts under the right ownership.

## 3. Docker + tsdproxy on Tailscale (HTTPS on your tailnet)

This is the production setup: `crated` exposed at `https://crate.<your-tailnet>.ts.net/` and reachable from any device on your tailnet.

[tsdproxy](https://github.com/almeidapaulopt/tsdproxy) is a Docker-label-driven Tailscale ingress controller. One tsdproxy container watches the Docker socket and auto-provisions a Tailscale node identity + LetsEncrypt TLS certificate for each container that opts in via labels.

### Prerequisites

- A Tailscale tailnet you're an owner of.
- A Tailscale auth key — generate at [Tailscale → Settings → Keys](https://login.tailscale.com/admin/settings/keys). Reusable + non-ephemeral keys work fine.
- `tsdproxy` running on the same Docker host as `crated`. See the [tsdproxy quickstart](https://github.com/almeidapaulopt/tsdproxy#getting-started). Four configuration points that meaningfully affect reliability:
  - Run with `network_mode: host` so it can register cleanly with Tailscale.
  - Mount `/var/run/docker.sock` (read-only) so it can discover labeled containers.
  - Give it a **persistent `/data` volume** (e.g. a named `tsdproxy-data` volume) so the Tailscale node identity and TLS certs survive container restarts. Without it, every restart creates a fresh node and your tailnet hostname can collide with the old identity, registering as `crate-1.<tailnet>.ts.net` instead.
  - Set the auth key via **`authKeyFile:` in `config.yaml`**, pointing to the mounted key file. The `TSPROXY_AUTH_KEY` env var is silently ignored when `authKeyFile` is present — even if the file is empty — so the env-var approach can leave you debugging a broken auth path.

### Add tsdproxy labels to the `crated` service

Edit `docker-compose.yml` (or copy [`examples/docker-compose.tsdproxy.yml`](../examples/docker-compose.tsdproxy.yml) in as a `docker-compose.override.yml`) and add three labels to the `crated` service:

```yaml
services:
  crated:
    # ... existing fields ...
    labels:
      tsdproxy.enable: "true"
      tsdproxy.name: "crate"
      tsdproxy.port.1: "443/https:7777/http"
```

What each label does:

| Label | Meaning |
|---|---|
| `tsdproxy.enable: "true"` | Opt this container in to tsdproxy discovery. |
| `tsdproxy.name: "crate"` | Hostname under your tailnet: `crate.<your-tailnet>.ts.net`. |
| `tsdproxy.port.1: "443/https:7777/http"` | Expose port 443 over HTTPS, proxy to container port 7777 over HTTP. |

Then restart:

```bash
task docker:down
task docker:up
```

Within ~30 seconds, tsdproxy registers the new Tailscale node, obtains a TLS cert, and `https://crate.<your-tailnet>.ts.net/` is live.

### First-time setup tip

If you have to delete and re-create the Tailscale node (e.g. after a misconfigured first attempt), LetsEncrypt rate-limits cert issuance for the affected hostname and tsdproxy enters exponential backoff. New certs can take up to ~30 minutes to provision. The reliable shakedown is to prove the tsdproxy pipeline end-to-end with a throwaway nginx container carrying the same label pattern first, then swap in `crated` once you've seen a clean cert handshake.

### Agent integration

Agents running anywhere on the tailnet can hit the same URL the human uses:

```
https://crate.<your-tailnet>.ts.net/<site>/
```

Pushes still go via `docker exec crated crate push -` on the Docker host (the API token never leaves the container).

## When you'd use Caddy or nginx instead

If you don't want Tailscale in the mix (or want a public-internet hostname rather than a tailnet one), put Caddy or nginx in front of `crated`:

```caddy
crate.example.com {
  reverse_proxy 127.0.0.1:7777
}
```

This gives you TLS, vhost routing, and optional IP-allowlisting without changing any crate-html code. Caddy/nginx and tsdproxy aren't exclusive — you can mix them (e.g. tsdproxy for the tailnet, Caddy for the public side).

## Kubernetes

Out of scope for v0, but the shape is clean: package the existing image, mount the same `/config` and `/data` volumes as `PersistentVolumeClaim`s, expose with a Gateway-API `HTTPRoute`. The Gateway controller handles TLS the same way tsdproxy does. A reference manifest will land alongside the Helm/kustomize work in [`roadmap.md`](roadmap.md).
