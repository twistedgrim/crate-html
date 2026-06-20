# Roadmap

v0 is intentionally small. Every item below is a clean layer over the current code — not a rewrite.

## How the daemon is meant to run

`crated` is a simple HTTP daemon. Operational modes, in order of escalation:

1. **Terminal session** — `./bin/crated` in a foreground shell. Default for casual use.
2. **Taskfile** — `task run` (build + foreground). Convenient during development.
3. **Docker** — for persistent operation. The planned next step (see below).
4. **Reverse proxy in front** — Caddy or nginx for TLS, vhosts, IP-allowlisting.
5. **Kubernetes with Gateway API** — if you want to go nuts.

No host-level service manager (launchd, systemd, `brew services`) is in scope. The daemon stays portable; persistence is a layer above it.

## Near-term

### Docker

Dockerfile + docker-compose for running `crated` as a persistent container. Taskfile gains `docker:build`, `docker:up`, `docker:down`. Site data and config mount in as volumes so they survive container rebuilds.

This is the next chunk of work after v0.

### Pi coding agent skill

The Claude Code skill at `.claude/skills/crate-push/SKILL.md` is the template. The Pi-side equivalent is the same shape — a manifest that wraps `crate push`. Same API, different agent.

> "Pi" here means the Pi coding agent — a peer to Claude Code. There is no Raspberry Pi or embedded-hardware story.

### `crate push --open`

One flag to open the URL in a browser after a successful push. Same as `crate push` then `crate open`, with one fewer step.

### Smoke-test suite

The current smoke test is an inline bash script. Wrap it as `task smoke` so it runs in CI and during local refactors. The site fixtures already live in `testdata/sites/` — the harness pushes each one against a freshly-started `crated`, verifies the public URL responds, and tears the daemon down.

### Unit tests for `internal/storage`

The atomic-replacement and tar-extraction paths are the highest-risk code in the repo. Cover them with table-driven tests: clean push, partial-extract failure, path-traversal rejection, symlink stripping.

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
