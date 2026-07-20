# Roadmap

v0 is intentionally small. Every item below is a clean layer over the current code — not a rewrite.

## How the daemon is meant to run

`crated` is a simple HTTP daemon. Operational modes, in order of escalation:

1. **Terminal session** — `./bin/crated` in a foreground shell. Default for casual use.
2. **Taskfile** — `task run` (build + foreground). Convenient during development.
3. **Docker** — for persistent operation. *Shipped.* See README + `task docker:*`.
4. **Reverse proxy in front** — Caddy or nginx for TLS, vhosts, IP-allowlisting.
5. **Kubernetes with Gateway API** — if you want to go nuts.

No host-level service manager (launchd, systemd, `brew services`) is in scope. The daemon stays portable; persistence is a layer above it.

## Recently shipped

- **Named API tokens.** `crate token create/ls/revoke` backed by root-only `/api/tokens`. Tokens are `crate_<id>_<secret>`, stored as SHA-256 hashes in `tokens.yaml`, with optional expiry, `last_used_at` tracking, and instant revocation. The config-file token is now the root credential; per-client tokens mean revoking one agent doesn't re-key the rest. Also added `max_upload_bytes` (default 100 MiB) so a runaway push can't fill the disk.
- **Docker.** Multi-stage build, alpine runtime, named volumes for `/config` and `/data`, `task docker:{build,up,down,nuke,logs,token,env,shell}`, env-var overrides for in-container binding.
- **Go integration suite (`task smoke`).** ~30 tests under `tests/smoke/` (build tag `smoke`) covering lifecycle (status/ls/rm/push), bearer-token enforcement on each `/api/sites/*` verb, path-traversal rejection in URLs and tarballs, built-in cratesplainer serving + disk-shadowing, push variants (dir / stdin / pre-built tar / `--open`), `--config` flag, and `CRATE_TOKEN` env override. Replaces the original bash harness.
- **Unit-test coverage** across `internal/`:
  - `storage` (existing) — ValidateName, atomic replace, traversal, symlinks, write→read round-trip.
  - `config` — applyDefaults/applyEnv, fresh init, env override doesn't rewrite file, Save MkdirAlls parent.
  - `cliclient` — Push/PushReader/List/Delete/Status against `httptest.NewServer`, bearer attachment, error decoding.
  - `server` — disk-vs-builtin routing, requireAuth on each verb, trailing-slash redirect, invalid name 404, builtin shadowing.
  - `builtin` — every embedded site has `index.html`; cratesplainer assets reachable; broken-HTML regression guard.
- **Tier-1 CLI ergonomics:** `crate token`, `crate push --open`, `crate push -` (stdin), `crate push <file.tar>`, `--config <path>` for both binaries, `examples/docker-compose.tsdproxy.yml`.
- **GitHub Actions CI + release-please.** `.github/workflows/ci.yml` runs `go test -race ./...`, `task smoke`, `task docker:build`, and hadolint on every push + PR. `.github/workflows/release-please.yml` opens the release PR from conventional commits; merging it cuts the tag, cross-builds `crate` + `crated` for darwin/{arm64,amd64} + linux/{amd64,arm64}, attaches archives to the GitHub Release, and pushes a multi-arch image to `ghcr.io/twistedgrim/crate-html`.
- **Site expiry** (PR #7). `crate push` defaults to 24h; `--expires <duration>` or `--expires never` overrides. Wire header `X-Crate-Expires`; server persists deadlines under a private `.expiries/` metadata dir and reaps elapsed sites once per minute.

## Near-term

### Pi coding agent skill

The Claude Code skill at `.claude/skills/crate-push/SKILL.md` is the template. The Pi-side equivalent is the same shape — a manifest that wraps `crate push`. Same API, different agent.

> "Pi" here means the Pi coding agent — a peer to Claude Code. There is no Raspberry Pi or embedded-hardware story.

## Medium-term

### Reverse-proxy recipes

Reference configs for sticking Caddy or nginx in front of `crated` (running locally or in Docker):

```caddy
crate.local {
  reverse_proxy 127.0.0.1:7777
}
```

No code change in `crated`. The proxy handles certs, vhosts, HTTP/2.

### Tailnet exposure

Run `crated` (or the Docker container) on the laptop and reach it from any tailnet device via the Mac's `100.x.y.z` address. Combined with Caddy + a real hostname, this is the "share a draft with your phone" loop.

### `crate logs <site>`

Per-site access log tail — what an agent (or a human) actually fetched after a push. Useful for debugging "I pushed it but the page is blank."

### Homebrew tap

Ship `crate` via a personal Homebrew tap before submitting to homebrew-core. Lower friction for early users.

### Kubernetes example

A bare-bones Helm chart or kustomize overlay that runs `crated` behind a Gateway-API HTTPRoute. Far-end option for anyone who wants to host crate-html as real infrastructure.

## Aspirational

**Nothing in this section is on a schedule.** These are ideas we like and might pursue as time and appetite allow. Track record: some aspirational items graduate to shipped (site expiry, Docker), some sit for a long time, some quietly get dropped. Don't wait for any of them.

### CLI ergonomics

- **`crate ls --urls`** — print each site's full URL (`BaseURL` + `/name/`) so the output is one copy-paste away from a browser tab. Especially useful on tsdproxy where the tailnet hostname isn't in local muscle memory.
- **`crate stat <name>`** — single-site metadata (size, files, `expires_at`, `updated_at`). The wire type already carries everything; this is just a per-site CLI accessor rather than filtering `crate ls`.
- **`crate mv old new`** — atomic rename via `os.Rename` in the sites root. Cheap; replaces the current `push` + `rm` dance for "I typo'd the name."
- **`crate watch <dir> <name>`** — filesystem watcher that auto-pushes on change. Debounced. Useful for the "human edits HTML in an editor while an agent watches" and "agent iterates on generated HTML" loops.

### Daemon / observability

- **Structured JSON logs** (`--log-format=json` and `CRATE_LOG_FORMAT`). Switch `crated`'s logger to `log/slog` and add request-log middleware. Feeds cleanly into any log aggregator. A local WIP branch exists.
- **`/metrics` Prometheus endpoint** — site count, push success/fail counters, request-latency histogram. Public like `/api/status`. Gate behind `--metrics` if we want to keep the base binary lean.

### Speculative

- **`crate import --from-url`** — pull a remote tarball directly and push it, skipping the local stage. CLI-side implementation to avoid the SSRF surface a server-side fetch would open.
- **`crate doctor`** — sanity check for first-run setup (daemon up, token retrievable, XDG paths writable, tsdproxy hostname resolves). Shortens the "why doesn't this work" loop for new users.

## Far future

Not yet aspirational — closer to notebook margins. Open shape, not committed:

- **Diff views.** When `crate push` overwrites an existing site, show what changed file-by-file.
- **Templates.** `crate new plan` scaffolds a starter site so agents don't have to generate boring chrome each time.
- **Search across sites.** Full-text index of all deployed HTML so you can find that one page you pushed last week.

## Explicit non-goals

| Not doing | Why |
|---|---|
| Database | Filesystem is enough. `readdir` is the index. |
| Multi-tenant auth | One human, several clients. Named tokens (shipped) cover per-client credentials; there are no user accounts, scopes, or per-site ACLs. |
| Server-side rendering | Sites are static. If you want SSR, render to static first, then push. |
| Vhosts in `crated` | Path routing is enough. Caddy can rewrite if you ever want vhosts. |
| Embedded hardware | v0 is laptop-only. Agents call into it, not the other way around. |
