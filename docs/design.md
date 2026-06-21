# Design

Why crate-html looks the way it does. Each section is a decision we made and the rationale, so future changes can either honor it or argue with it.

## Two binaries, one module

`crate` (CLI) and `crated` (daemon) ship as separate binaries in one Go module.

The agent calls `crate push`; the human opens a URL in a browser. Those are separate processes with different lifetimes. The daemon outlives any single agent invocation, so splitting them maps onto reality. The cost is one extra binary to ship and one extra concept ("is the daemon running?") to keep in your head.

`internal/wire` defines request/response types in one place. Both binaries import it; they don't import each other.

## HTTP push, not direct disk write

The CLI could write directly to `$XDG_DATA_HOME/crate/sites/<name>/` since it shares a host with the daemon. We don't.

- Same code path works if the daemon ever moves off the laptop (Caddy/Tailscale path).
- Auth is meaningful from day 1 — the CLI proves it has the token.
- Concurrent writes are serialized by the daemon, not by ad-hoc file locks.
- The push API is the same thing other agents will integrate against. Direct-disk writes would create two contracts.

## Streaming tar body

`PUT /api/sites/{name}` body is a raw tar stream — no JSON envelope, no multipart.

The CLI tars on the fly via `io.Pipe`: one goroutine walks the directory and writes tar entries to the write end; the HTTP client reads from the read end. The first byte goes over the wire before the last file is even opened. No temp file on either side.

JSON wrapping would force buffering. Multipart would add a parser layer for no gain. A bare tar body is what `archive/tar` already speaks.

## Atomic site replacement

A failed or interrupted upload must not leave a half-replaced site. The daemon:

1. Extracts the tar into `sites/.<name>.stage-<rand>/`. If extraction fails, removes the stage dir and bails — the live site is untouched.
2. Renames the existing site aside (`sites/.<name>.old-<ts>`).
3. Renames the stage dir into place.
4. Removes the old copy.

Both renames are within the same directory, so they're atomic on POSIX filesystems. The window where neither `<name>/` nor anything-named-`<name>` exists is bounded by one syscall.

## Bearer token from day 1

Even though v0 binds `127.0.0.1` only, every mutating `/api/sites/*` endpoint requires a bearer token. On first run the daemon generates 32 random bytes (hex-encoded) and writes them to `config.yaml` with mode `0600`. The server checks tokens with `subtle.ConstantTimeCompare`.

Reason: if you ever expose the daemon (Caddy, tailnet, anything), retrofitting auth is a code change. Adding it now is a few lines and means the deployment story is "change one config field," not "rewrite the security model."

`/api/status` is intentionally unauthenticated — `crate status` uses it as a liveness probe, and the Docker healthcheck does the same. Static serve paths (`/`, `/<site>/...`) are also public; that's the whole point.

## Env-var overrides on config

`crated` reads `config.yaml` from XDG_CONFIG_HOME, then applies these env vars on top:

| Env var | Overrides |
|---|---|
| `CRATE_LISTEN_ADDR` | `listen_addr` |
| `CRATE_BASE_URL` | `base_url` |
| `CRATE_TOKEN` | `token` |

Overrides stay process-local — the on-disk config isn't rewritten. The mechanism exists for one specific case: containers. Inside Docker, `crated` must bind `0.0.0.0:7777` for the port mapping to work; outside, it must bind `127.0.0.1`. An env-var override in the Dockerfile (`ENV CRATE_LISTEN_ADDR=0.0.0.0:7777`) handles this without needing two different config files.

There's intentionally no `CRATE_PORT` — `port` only exists to compose the default `listen_addr`/`base_url` strings. Overriding it after defaults are applied wouldn't change anything reachable. To bind a different port, set `CRATE_LISTEN_ADDR` directly.

## Localhost-only binding

`crated` binds `127.0.0.1:7777` by default, not `:7777`. v0 is laptop-only. Binding all interfaces would make the daemon reachable from anything on the same network without HTTPS or a real auth story, and the token-in-yaml setup isn't built for that.

If you want remote access later, put Caddy in front of `crated` and let Caddy handle TLS and (optionally) IP-allowlisting. The daemon itself stays HTTP-only.

## Path routing, not vhosts

The daemon routes by URL path: `GET /foo/bar.html` serves `sites/foo/bar.html`. There's no virtual-host configuration.

Path routing works on `localhost` with zero setup. Vhost routing would force you into `/etc/hosts` or a reverse proxy immediately. If you ever want vhosts, Caddy can rewrite `foo.crate.local` → `localhost:7777/foo/` without `crated` changing.

## Site name validation

```
^[a-z0-9][a-z0-9._-]{0,62}$
```

Lowercase letter or digit first, then up to 62 letters/digits/dot/hyphen/underscore. Total 1–63 chars.

URL-safe, filesystem-safe on every OS, no surprises like a site called `.git` or `../etc`.

## Symlinks dropped during extraction

Tar entries that aren't regular files or directories are silently skipped: symlinks, hardlinks, device nodes, FIFOs. Sites are pure static content. Honoring symlinks would mean every upload is a potential sandbox escape — `/etc/passwd`, the user's SSH key, the next file you forget about.

Anyone needing dynamic links can render them at push time.

## Filesystem storage, no database

Sites are plain directories on disk. `readdir` is the index; `os.Rename` is the transaction.

A database would let us track upload history, TTLs, access counts, owners. v0 has none of those. Adding them later costs a database; running them now costs a database. The shape we have (one file per file, name = dir name) makes every `crate ls` and every browser `GET` exactly one filesystem op.

## XDG paths via `adrg/xdg`

Config under `XDG_CONFIG_HOME`, site data under `XDG_DATA_HOME`, logs under `XDG_STATE_HOME`. On macOS the library defers to `~/Library/Application Support/` by default; setting the XDG env vars overrides that.

Putting site data under `XDG_DATA_HOME` (not `XDG_CACHE_HOME`) is deliberate — these are user artifacts, not regenerable cache. They should survive a `~/.cache` wipe.

## What's deliberately not in v0

| Not in v0 | Why | Where it slots in |
|---|---|---|
| Caddy / nginx | Localhost HTTP is enough today. | A reverse proxy in front of `crated` (host or Docker) handles TLS, vhosts, IP-allowlisting. |
| Tailscale | Laptop-only. | Bind `crated` to a tailscale interface; same binary. |
| Database | Filesystem is enough. | Never, ideally. |
| Multi-tenant auth | One human, one machine. | Per-token-scoped sites, if multi-user ever happens. |
| Server-side rendering | Sites are static. | Render to static first, then push. |

Docker support **is** shipped (Dockerfile + `task docker:*`) — see the README quickstart.
