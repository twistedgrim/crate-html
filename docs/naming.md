# Naming

Decisions on the binary name, the package/repo name, and the marketing domain. Kept here so the next time someone questions the split, they can read the evidence instead of re-running the searches.

## Surfaces

| Surface | Name |
|---|---|
| CLI binary | `crate` |
| Package name (npm / PyPI / GitHub repo) | `crate-html` |
| Marketing domain | `getcratehtml.com` |

Pattern: short binary + qualified package name. Mirrors `gh` binary / `cli` repo, `kubectl` binary / `kubernetes` package, `aws` binary / `awscli` package.

## Availability check

Point-in-time snapshot as of **2026-06-20**. Re-verify before any public release if more than a couple of weeks have passed.

| Slot | Status |
|---|---|
| `brew install crate` | No homebrew-core formula — available |
| `npm install crate-html` | 404 — available |
| `pip install crate-html` | PyPI JSON returns not found — available |
| `github.com/crate-html` | 404 — available |
| `getcratehtml.com` | DNS does not resolve — ~$10–15/yr at any registrar |

## Why not bare `crate`

CrateDB owns the `crate` namespace where it matters for dev tools:

- PyPI `crate` = CrateDB Python client (active, shipped v2.2.1 on 2026-06-17)
- `github.com/crate` = CrateDB org (4.4k stars, 136+ repos)
- `crate.io` = CrateDB website
- npm `crate` = 2011 squat by user `ecto`, version 0.0.0
- `crate.com` = unrelated AI consulting company

Homebrew `crate` is the only clean slot. Hence the split: short binary on brew, qualified name everywhere else.

## Why not `slab`

`slab.com` is an established team knowledge-base SaaS. Same dev-adjacent audience, real SEO and conversational collision risk.

## CLI grammar

Verb-style subcommands (`git`/`docker` style):

```
crate push <src> <name>   # src = dir | tar file | '-' (stdin tar)
crate ls
crate rm <name>
crate open <name>
crate status
crate token
```

Reads naturally as commands. The noun-then-verb style (`crate site push name`) was on the table but adds a word per invocation with no payoff for a six-command surface.
