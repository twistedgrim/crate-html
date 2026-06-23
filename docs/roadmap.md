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

- **Docker.** Multi-stage build, alpine runtime, named volumes for `/config` and `/data`, `task docker:{build,up,down,nuke,logs,token,env,shell}`, env-var overrides for in-container binding.
- **Go integration suite (`task smoke`).** ~30 tests under `tests/smoke/` (build tag `smoke`) covering lifecycle (status/ls/rm/push), bearer-token enforcement on each `/api/sites/*` verb, path-traversal rejection in URLs and tarballs, built-in cratesplainer serving + disk-shadowing, push variants (dir / stdin / pre-built tar / `--open`), `--config` flag, and `CRATE_TOKEN` env override. Replaces the original bash harness.
- **Unit-test coverage** across `internal/`:
  - `storage` (existing) — ValidateName, atomic replace, traversal, symlinks, write→read round-trip.
  - `config` — applyDefaults/applyEnv, fresh init, env override doesn't rewrite file, Save MkdirAlls parent.
  - `cliclient` — Push/PushReader/List/Delete/Status against `httptest.NewServer`, bearer attachment, error decoding.
  - `server` — disk-vs-builtin routing, requireAuth on each verb, trailing-slash redirect, invalid name 404, builtin shadowing.
  - `builtin` — every embedded site has `index.html`; cratesplainer assets reachable; broken-HTML regression guard.
- **Tier-1 CLI ergonomics:** `crate token`, `crate push --open`, `crate push -` (stdin), `crate push <file.tar>`, `--config <path>` for both binaries, `examples/docker-compose.tsdproxy.yml`.

## Near-term

### Pi coding agent skill

The Claude Code skill at `.claude/skills/crate-push/SKILL.md` is the template. The Pi-side equivalent is the same shape — a manifest that wraps `crate push`. Same API, different agent.

> "Pi" here means the Pi coding agent — a peer to Claude Code. There is no Raspberry Pi or embedded-hardware story.

### GitHub Actions CI

Run `go test ./...`, `task smoke`, and `task docker:build` on every push so `main` stays green without needing James's laptop. Tests already exist; CI is the wiring.

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

## Far future

Open shape, not committed:

- **Diff views.** When `crate push` overwrites an existing site, show what changed file-by-file.
- **TTLs.** `crate push --expires 24h` for ephemeral drafts that self-clean.
- **Templates.** `crate new plan` scaffolds a starter site so agents don't have to generate boring chrome each time.
- **Search across sites.** Full-text index of all deployed HTML so you can find that one page you pushed last week.

## Explicit non-goals

| Not doing | Why |
|---|---|
| Database | Filesystem is enough. `readdir` is the index. |
| Multi-tenant auth | One human, one machine. Single bearer token. |
| Server-side rendering | Sites are static. If you want SSR, render to static first, then push. |
| Vhosts in `crated` | Path routing is enough. Caddy can rewrite if you ever want vhosts. |
| Embedded hardware | v0 is laptop-only. Agents call into it, not the other way around. |
