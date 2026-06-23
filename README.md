# crate-html

Local HTML hosting for AI coding agents.

An agent (Claude Code, [Pi the coding agent](https://pi.ai/), anything that can shell out) generates a directory of static HTML, runs `crate push ./dir name`, and gets back `http://localhost:7777/name/`. The human opens that URL and sees the rendered artifact — a plan, an explainer, a code review — instead of skimming raw markdown in a chat window.

> "Pi" throughout this repo means the Pi coding agent, not Raspberry Pi hardware.

Status: **v0.1.0-dev** — laptop-only out of the box, optional Docker for persistence, optional Tailscale exposure via tsdproxy. Caddy / nginx in front of the daemon is roadmap.

## Quickstart

Requires Go 1.26+ and [Task](https://taskfile.dev) (`brew install go-task`).

```bash
task build              # produces ./bin/crate and ./bin/crated
./bin/crated &          # starts the daemon, generates config + token on first run of either binary
./bin/crate status      # confirm the daemon is up
./bin/crate push ./some-html-dir my-site
./bin/crate open my-site
```

Default URL is `http://localhost:7777/`. The daemon binds to `127.0.0.1` only (override with `CRATE_LISTEN_ADDR`).

### Docker

For a persistent daemon that survives terminal sessions:

```bash
task docker:build       # build the crate-html image
task docker:up          # start crated on :7777 with persistent volumes
task docker:token       # print just the bearer token (via `crate token` inside the container)
task docker:logs        # tail the container's logs
task docker:down        # stop the container (volumes preserved)
```

The host `crate` CLI talks to the dockerized daemon via env vars — no config-file editing required:

```bash
eval "$(task docker:env)"          # exports CRATE_TOKEN and CRATE_BASE_URL
./bin/crate ls                     # now talks to the dockerized daemon
./bin/crate push ./my-site demo
```

The env vars override anything in `config.yaml` for the lifetime of the shell. Unset them or open a new terminal to go back to the host-side daemon.

`task docker:nuke` deletes the volumes too — use when you want a clean slate.

### Tailscale (HTTPS on your tailnet)

For a real hostname like `https://crate.<your-tailnet>.ts.net/`, add three [tsdproxy](https://github.com/almeidapaulopt/tsdproxy) labels to the `crated` service in `docker-compose.yml`:

```yaml
labels:
  tsdproxy.enable: "true"
  tsdproxy.name: "crate"
  tsdproxy.port.1: "443/https:7777/http"
```

tsdproxy auto-provisions the Tailscale node and TLS cert. Full setup including prerequisites: [`docs/deploy.md`](docs/deploy.md).

## CLI

```
crate push <src> <name>     upload a directory, a pre-built tar file, or '-' (stdin tar)
crate push <src> <name> -o  same as push, then open the URL in a browser
crate ls                    list deployed sites
crate rm <name>             remove a site
crate open <name>           open the site in your default browser
crate status                show daemon version and site count
crate token                 print the bearer token from the loaded config
```

Site names must match `^[a-z0-9][a-z0-9._-]{0,62}$`. A global `--config <path>` flag (on both `crate` and `crated`) overrides the XDG config-file location.

## Layout

```
cmd/crate/          CLI (kong) — push, ls, rm, open, status, token
cmd/crated/         HTTP daemon
internal/wire/      request/response types — the API contract
internal/config/    XDG config loader, first-run token generation
internal/storage/   filesystem ops, tar in/out, atomic site replacement
internal/server/    net/http handlers — bearer auth on /api/sites (status is public), static serve
internal/cliclient/ HTTP client used by `crate`
internal/builtin/   sites embedded in the binary (e.g. cratesplainer)
testdata/sites/     site fixtures used by smoke tests
.claude/skills/     Claude Code skill wrapping `crate push`
```

One Go module. `internal/wire` is the seam — both binaries import it; they don't import each other.

## Built-in sites

`crated` ships with one site embedded in the binary via `go:embed`:

- **`/cratesplainer/`** — a deliberately-overexplained guide to using crate, useful when a new user lands on the daemon for the first time.

Disk sites win conflicts: if you `crate push ./my-version cratesplainer`, your copy is served instead. `crate rm cratesplainer` removes your override and the built-in resurfaces.

## Where data lives

Paths resolve via [`github.com/adrg/xdg`](https://github.com/adrg/xdg), which follows the XDG Base Directory Spec on Linux and the platform-native location on macOS.

| Purpose | Linux | macOS |
|---|---|---|
| Config | `~/.config/crate/config.yaml` | `~/Library/Application Support/crate/config.yaml` |
| Sites | `~/.local/share/crate/sites/` | `~/Library/Application Support/crate/sites/` |
| Logs | `~/.local/state/crate/log/` | `~/Library/Application Support/crate/log/` |

To force pure-XDG layout on macOS, set `XDG_CONFIG_HOME` / `XDG_DATA_HOME` / `XDG_STATE_HOME` in your shell.

## Build & test

```bash
task build      # go build ./cmd/...
task test       # go test ./...
task vet        # go vet ./...
task tidy       # go mod tidy
```

## Docs

- [`docs/architecture.md`](docs/architecture.md) — how it works end-to-end (logical view, API, push/serve protocols, deployment topology)
- [`docs/deploy.md`](docs/deploy.md) — three deployment shapes (local, Docker, Docker + tsdproxy on Tailscale)
- [`docs/design.md`](docs/design.md) — *why* the architecture is shaped the way it is
- [`docs/naming.md`](docs/naming.md) — naming rationale and availability check
- [`docs/roadmap.md`](docs/roadmap.md) — near-term work and explicit non-goals
- [`examples/`](examples/) — optional compose overrides (currently: tsdproxy on Tailscale)

## Inspiration

ClawBox from OpenClaw — a plug-and-play appliance hosting an agent-facing service. crate-html is the same shape, specialized for HTML, and runs locally rather than as an appliance.

## License

[MIT](LICENSE). Share, modify, ship it in your product — just keep the license notice with any substantial copy.
