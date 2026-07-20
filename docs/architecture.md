# Architecture

How crate-html works end-to-end. This document describes *what* the system does and *how the pieces fit*; for the rationale behind these choices see [`design.md`](design.md).

## Logical view

```
┌──────────────┐     HTTP (tar body)     ┌──────────────────────┐
│  crate CLI   │ ──────────────────────▶ │      crated          │
│  (agent or   │     Bearer token        │   (HTTP daemon)      │
│   human)     │ ◀────────────────────── │                      │
└──────────────┘     JSON response       │   sites/             │
                                         │   ├── foo/           │
                                         │   ├── bar/           │
┌──────────────┐                         │   └── ...            │
│   Browser    │ ◀── GET /<site>/ ─────  │                      │
└──────────────┘     no auth             └──────────────────────┘
```

Two binaries, one Go module:

- **`crate`** — short-lived CLI invoked once per push/list/rm/open. Reads `config.yaml`, talks HTTP to `crated`.
- **`crated`** — long-lived HTTP daemon. Outlives any single agent invocation. Owns the filesystem under `$XDG_DATA_HOME/crate/sites/`.

`internal/wire` defines the HTTP request/response types. Both binaries import it; they don't import each other.

## HTTP API surface

### Authenticated (bearer token required)

| Method | Path | Purpose |
|---|---|---|
| `PUT` | `/api/sites/{name}` | Push a tar stream as a site (replaces existing) |
| `DELETE` | `/api/sites/{name}` | Remove a deployed site |
| `GET` | `/api/sites` | List all sites with metadata |

### Root-token only

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/tokens` | Mint a named API token (plaintext returned once) |
| `GET` | `/api/tokens` | List minted tokens (metadata only, never secrets) |
| `DELETE` | `/api/tokens/{id}` | Revoke a token by id or name, effective immediately |

### Public (no auth)

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/status` | Liveness probe — version + site count |
| `GET` | `/` | HTML index linking to all sites (disk + built-in) |
| `GET` | `/<site>/` | Serve `<site>/index.html` |
| `GET` | `/<site>/<path>` | Serve a file under the site |

`/api/status` is deliberately unauthenticated so external healthchecks (Docker, reverse proxies, monitoring) can probe the daemon without holding a token.

## Push protocol

```
crate push ./my-dir my-site
```

**Client side** (`internal/cliclient` + `internal/storage`):

1. Read `config.yaml`, apply `CRATE_*` env overrides.
2. Open an `io.Pipe`. One goroutine walks `./my-dir` writing tar entries to the write end. The HTTP client reads from the read end and PUTs it as the request body.
3. No temp file on either side — first byte goes to the wire before the last file is stat'd.

**Server side** (`internal/server` + `internal/storage`):

1. Authenticate via `Bearer <token>`, constant-time compare (`crypto/subtle`).
2. Validate site name against `^[a-z0-9][a-z0-9._-]{0,62}$`.
3. Create staging dir `sites/.my-site.stage-<rand>/`.
4. Extract tar entries. Regular files and directories only — symlinks, hardlinks, device nodes, and FIFOs are silently skipped (sandbox hardening; a symlink to `/etc/passwd` in an uploaded tar would otherwise be an escape vector).
5. If extraction fails, remove the stage dir and bail. The live site is untouched.
6. If `sites/my-site/` already exists, rename it aside to `sites/.my-site.old-<unix-nano>/`.
7. Rename `sites/.my-site.stage-<rand>/` → `sites/my-site/`. Both renames are within one directory and atomic on POSIX filesystems.
8. Remove the old copy and respond with `{site, url}`.

The window where neither `my-site/` nor a stage/old equivalent exists is bounded by a single syscall.

## Serve protocol

```
GET /foo/about.html
```

1. Strip the leading slash; first path segment (`foo`) is the site name.
2. Validate against the same name regex; reject invalid names with 404.
3. Check disk first: `sites/foo/` exists? Serve `about.html` from there via `http.ServeFile`.
4. Otherwise check the built-in list (`internal/builtin.Sites()`); if `foo` matches, serve from the embedded FS via `http.ServeFileFS`.
5. Otherwise 404.

A request to `/foo` (no trailing slash) 302s to `/foo/` so relative links resolve. `path.Clean("/" + rest)` defangs traversal attempts — `/foo/../../etc/passwd` cleans to `/etc/passwd`, which is then joined with the site root and resolves to a nonexistent file.

## Auth model

Two credential tiers, both sent as `Authorization: Bearer <value>`:

### Root token

- 32 random bytes (hex-encoded → 64 chars) generated on first daemon startup.
- Stored at `$XDG_CONFIG_HOME/crate/config.yaml` with mode `0600`.
- Persists across daemon restarts. Only deleting the config file regenerates it.
- Authorizes everything, and is the *only* credential accepted by `/api/tokens` — minted tokens can manage sites but can never mint, list, or revoke tokens. That keeps privilege escalation impossible without introducing scopes.

### Named API tokens (`internal/token`)

- Shape: `crate_<id>_<secret>` — 8-hex-char public id + 64-hex-char secret. The id makes verification an O(1) lookup and shows up in listings/logs without exposing the credential.
- Stored in `$XDG_CONFIG_HOME/crate/tokens.yaml` (mode `0600`, written atomically via temp-file + rename) as SHA-256 hashes only. Plain SHA-256 is appropriate because secrets are high-entropy random, not passwords.
- The plaintext is returned exactly once, by `POST /api/tokens`.
- Optional expiry; `last_used_at` is tracked per token and persisted at most once a minute so busy clients don't hammer the file.
- Revocation deletes the record — effective on the next request, no restart.

All bearer comparisons are constant-time (`crypto/subtle`). `/api/status` and all static-serve paths are public.

The CLI reads the same `config.yaml` by default, so local pushes work without any extra setup. For agents running outside the daemon's host (e.g. host CLI → dockerized daemon), the `CRATE_TOKEN` env var overrides the file-loaded token process-locally — it may hold the root token or a minted token.

Site uploads are capped by `max_upload_bytes` (default 100 MiB); oversized pushes get `413` and never touch the live site.

## Storage model

Sites are plain directories on disk. No database. `readdir` is the site index; `os.Rename` is the transaction.

New pushes expire after 24 hours by default. `crate push --expires <duration>`
sets another lifetime and `--expires never` opts out using the same flag. The
CLI sends the policy in `X-Crate-Expires`; the daemon persists the absolute
deadline under the private `.expiries` metadata directory and checks for elapsed
sites once a minute. Sites without metadata, including sites from older
versions, are retained indefinitely.

| XDG var | Linux default | macOS default | Purpose |
|---|---|---|---|
| `XDG_CONFIG_HOME` | `~/.config` | `~/Library/Application Support` | `crate/config.yaml` (token, listen addr) |
| `XDG_DATA_HOME` | `~/.local/share` | `~/Library/Application Support` | `crate/sites/<name>/` |
| `XDG_STATE_HOME` | `~/.local/state` | `~/Library/Application Support` | `crate/log/` (reserved) |

Site data lives under `XDG_DATA_HOME` (not `XDG_CACHE_HOME`) deliberately — these are user artifacts, not regenerable cache. They should survive a `~/.cache` wipe.

## Env-var overrides

`crated` and `crate` both apply these on top of `config.yaml`:

| Env var | Overrides | Use case |
|---|---|---|
| `CRATE_LISTEN_ADDR` | `listen_addr` | Bind `0.0.0.0:7777` inside Docker; `127.0.0.1:7777` outside |
| `CRATE_BASE_URL` | `base_url` | Self-URL returned in `crate push` responses |
| `CRATE_TOKEN` | `token` | Runtime token override (Docker secrets, host CLI talking to a container) |

Overrides stay process-local — the on-disk config isn't rewritten. There's no `CRATE_PORT` env var; `port` only exists to compose default address strings, and overriding it after defaults are applied silently no-ops.

## Built-in sites

Some sites ship inside the binary via `go:embed` (`internal/builtin/`). Today there's one:

- **`/cratesplainer/`** — a deliberately-overexplained guide to using crate, present on every fresh install so first-time users have something to read.

The lookup order is **disk first, builtin second**. If you `crate push ./my-version cratesplainer`, your version is served instead. `crate rm cratesplainer` removes your override and the built-in resurfaces. Built-in sites are listed on the `/` index with a `built-in` tag.

## Deployment topology — the user's setup

```
Tailscale tailnet (e.g. <your-tailnet>.ts.net)
 │
 ├─ crate.<tailnet>.ts.net  ─── tsdproxy ─── crated:7777
 │                                              │
 │                                              ├── /config  (config.yaml, token)
 │                                              └── /data    (sites/<name>/...)
 │
 └─ tsdproxy (Docker, host networking)
    ├─ watches /var/run/docker.sock for tsdproxy.* labels
    ├─ authKeyFile: /opt/tsdproxy/authkey
    └─ persistent /data volume (Tailscale node identity + TLS certs)
```

- `crated` runs as a Docker container, exposes port 7777 internally.
- `tsdproxy` is a label-driven Tailscale ingress that auto-provisions tailnet hostnames + LetsEncrypt TLS for any container with `tsdproxy.enable: "true"`.
- Adding crate-html to an existing tsdproxy is three labels on the `crated` service — see [`deploy.md`](deploy.md) for the full recipe.

The crate-html daemon itself stays HTTP-only. TLS, vhosts, and tailnet exposure are all layered above it — replace tsdproxy with Caddy, nginx, or a Kubernetes Gateway-API HTTPRoute without changing a line of Go.
