# Deploying crate-html

Four deployment shapes, in order of escalation:

1. [Local foreground](#1-local-foreground-laptop) — `./bin/crated` in a terminal.
2. [Docker](#2-docker-persistent-daemon) — `task docker:up`, persistent across terminal sessions.
3. [Docker + tsdproxy on Tailscale](#3-docker--tsdproxy-on-tailscale-https-on-your-tailnet) — HTTPS, accessible from any tailnet device.
4. [Object storage](#4-object-storage-no-writable-volume) — sites in an S3-compatible bucket, for hosts with no writable volume.

All four serve the same HTTP API; the first three differ in *who can reach the daemon* and *who owns the lifecycle*, while the fourth changes *where sites live*. crate-html itself stays HTTP-only — TLS, hostnames, and remote access are layered above it.

Every shape may use the default combined daemon or the optional
[split broker + web topology](#optional-split-broker--web). The split still
uses one image.

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

## Optional split broker + web

Use this when the human-facing crate URLs should be isolated from the
write-capable API:

```bash
task docker:split:up
eval "$(task docker:split:env)"
./bin/crate push ./my-site demo
```

The example exposes two independent endpoints:

| Endpoint | Audience | Service |
|---|---|---|
| `http://localhost:7777` | Humans and browsers | Read-only web |
| `http://localhost:7778` | Local CLI and agents | Broker API |

No reverse proxy is required. `CRATE_API_URL` sends CLI operations to the
broker; `CRATE_PUBLIC_URL` tells the broker and CLI which URL humans should
open. The pushed site response therefore points at port 7777 even though the
upload went to port 7778.

Both containers come from the same image:

```yaml
services:
  broker:
    image: crate-html:dev
    command: ["--role=broker"]
    volumes:
      - crate-config:/config
      - crate-data:/data

  web:
    image: crate-html:dev
    command: ["--role=web"]
    volumes:
      - crate-data:/data:ro
```

The broker exclusively owns uploads, manual deletes, token state, and expiry
cleanup. Run one broker replica. The web role neither loads the broker token
store nor runs cleanup, and it can be replicated freely.

For a shared hostname, route `/api/*` to broker and everything else to web.
For separate hostnames, expose the broker only on a private network or
Tailscale and publish the web hostname normally. The CLI only needs network
access to the broker; humans only need network access to web.

With local storage, both containers must see the same volume on one Docker
host. To place them on different hosts, use the S3 backend.

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

## 4. Object storage (no writable volume)

Where there is no PVC to mount — an ephemeral container filesystem, a serverless
runtime, a node pool without persistent storage — sites can live in an
S3-compatible bucket instead of on disk:

```bash
CRATE_STORAGE_BACKEND=s3 \
CRATE_S3_ENDPOINT=https://s3.example.com \
CRATE_S3_BUCKET=crate \
CRATE_S3_ACCESS_KEY=... \
CRATE_S3_SECRET_KEY=... \
crated
```

Everything is settable by environment variable because the whole point is
running without a durable config file. Leaving the credentials empty falls back
to the standard AWS credential chain, so under an IAM role there are no secrets
to supply at all. Works with AWS S3, Cloudflare R2, MinIO, rustfs, or anything
else speaking the S3 API.

The bucket must already exist — crated never creates it, so a typo fails loudly
at startup instead of silently starting with an empty one.

For a local stack (rustfs + a crated with **no `/data` volume**, which is the
point) see [`../examples/docker-compose.rustfs.yml`](../examples/docker-compose.rustfs.yml)
or run `task rustfs:up`.

Sites *and* named API tokens both move to the bucket, so a restart keeps serving
the same content and keeps accepting the same credentials.

### Hybrid: bucket for content, small volume for config

Selecting the S3 backend does not force you to give up a volume entirely. What
moves to the bucket and what stays local are separate questions:

| | Sites | Named tokens | Root token | Volumes |
|---|---|---|---|---|
| Local (default) | disk | disk | `config.yaml` | `/config` + `/data` |
| **Hybrid** | bucket | bucket | `config.yaml` | `/config` only |
| Stateless | bucket | bucket | `CRATE_TOKEN` | none |

**Hybrid is what most deployments want.** Mount a small `/config` volume, omit
`CRATE_TOKEN`, and the root token is generated once and persists exactly as it
does today — bucket-backed content without a secret to manage. Go fully
stateless only when you cannot mount anything at all.

Confirm which backend is live from the startup log, which prints the bucket
instead of a directory:

```
crated sites:  s3://crate/ (https://s3.example.com)
```

### Before running this in anger

- **Set `CRATE_TOKEN` if `/config` is not persisted.** The root token still
  comes from `config.yaml`; without a durable `/config` and without this env var
  every restart mints a new random one and invalidates whatever your clients
  were using. Named tokens are unaffected — they live in the bucket.
- **Combined (`role=all`) replicas go eventually consistent.** A push handled
  by one replica becomes visible to the others within about ten seconds.
  Expiry also runs independently in every combined replica. Token writes are
  compare-and-swap, so a concurrent mint fails loudly rather than silently
  dropping the other replica's tokens. Pin to one combined replica if you want
  the same behavior as the local backend.

For a role-split S3 deployment, run one broker with read/write/delete
credentials and any number of web processes with list/read-only credentials.
Each web process refreshes metadata within about ten seconds, then fetches the
new version-keyed content.

Full operator reference — permissions, every setting, bucket layout, and the
rest of the gotchas — is in [`s3-storage.md`](./s3-storage.md).

The design and its tradeoffs are written up in
[`plans/object-storage-backend.md`](./plans/object-storage-backend.md).
