# crate-html

Local HTML hosting for AI coding agents.

An agent (Claude Code, Pi, anything that can shell out) generates a directory of static HTML, runs `crate push ./dir name`, and gets back `http://localhost:7777/name/`. The human opens that URL and sees the rendered artifact — a plan, an explainer, a code review — instead of skimming raw markdown in a chat window.

Status: **v0.1.0-dev** — laptop-only, no Docker, no Caddy, no Tailscale.

## Quickstart

```bash
task build              # produces ./bin/crate and ./bin/crated
./bin/crated &          # starts the daemon, generates config + token on first run
./bin/crate status      # confirm the daemon is up
./bin/crate push ./some-html-dir my-site
./bin/crate open my-site
```

Default URL is `http://localhost:7777/`. The daemon binds to `127.0.0.1` only.

### Docker

For a persistent daemon that survives terminal sessions:

```bash
task docker:build       # build the crate-html image
task docker:up          # start crated on :7777 with persistent volumes
task docker:token       # read the auto-generated bearer token from the volume
task docker:logs        # tail the container's logs
task docker:down        # stop the container (volumes preserved)
```

The host `crate` CLI works against the dockerized daemon if both use the same token. Copy the token from `task docker:token` into `~/.config/crate/config.yaml` (or `~/Library/Application Support/crate/config.yaml` on macOS), and the CLI will reach the container at `localhost:7777`.

`task docker:nuke` deletes the volumes too — use when you want a clean slate.

## CLI

```
crate push <dir> <name>     upload a directory as a site
crate ls                    list deployed sites
crate rm <name>             remove a site
crate open <name>           open the site in your default browser
crate status                show daemon version and site count
```

Site names must match `^[a-z0-9][a-z0-9._-]{0,62}$`.

## Layout

```
cmd/crate/          CLI (kong) — push, ls, rm, open, status
cmd/crated/         HTTP daemon
internal/wire/      request/response types — the API contract
internal/config/    XDG config loader, first-run token generation
internal/storage/   filesystem ops, tar in/out, atomic site replacement
internal/server/    net/http handlers — bearer auth on /api, public static serve
internal/cliclient/ HTTP client used by `crate`
internal/builtin/   sites embedded in the binary (e.g. cratesplainer)
testdata/sites/     site fixtures used by smoke tests
.claude/skills/     Claude Code skill wrapping `crate push`
```

One Go module. `internal/wire` is the seam — both binaries import it; they don't import each other.

## Built-in sites

`crated` ships with one site embedded in the binary via `go:embed`:

- **`/cratesplainer/`** — a deliberately-overexplained guide to using crate, useful when a new user lands on the daemon for the first time.

Disk sites win conflicts: if you `crate push cratesplainer ./my-version`, your copy is served instead. `crate rm cratesplainer` removes your override and the built-in resurfaces.

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

- [`docs/design.md`](docs/design.md) — design decisions and what's deliberately out of v0
- [`docs/naming.md`](docs/naming.md) — naming rationale and availability check
- [`docs/roadmap.md`](docs/roadmap.md) — near-term work and explicit non-goals

## Inspiration

[ClawBox](https://github.com/openclaw/clawbox) — a plug-and-play appliance hosting an agent-facing service. crate-html is the same shape, specialized for HTML, and runs locally rather than as an appliance.

## License

[MIT](LICENSE). Share, modify, ship it in your product — just keep the license notice with any substantial copy.
