# Changelog

## 1.0.0 (2026-06-25)


### Features

* embed cratesplainer docs into crated; add testdata fixtures ([a4533c6](https://github.com/twistedgrim/crate-html/commit/a4533c665bf81cd254f6a6a01a7a1f37a58aac51))
* initial v0 scaffold — crate CLI + crated daemon ([d72d823](https://github.com/twistedgrim/crate-html/commit/d72d8232666ff1036c28f5fadb68e9500a8db379))
* smoke harness + Docker support ([c6aba08](https://github.com/twistedgrim/crate-html/commit/c6aba081378ba1391226f9ce191dee70afc5807d))
* task docker:env for host-CLI -&gt; dockerized-daemon bridge ([9fb4a6d](https://github.com/twistedgrim/crate-html/commit/9fb4a6d646b94b4a66bc098ee251eee48197aa77))


### Bug Fixes

* bugs surfaced by repo audit ([9cb402d](https://github.com/twistedgrim/crate-html/commit/9cb402d74a4718d8a6862f4d2cd4f55811f5d25c))

## Changelog

All notable changes to crate-html are documented here.

This file is maintained by [release-please](https://github.com/googleapis/release-please). Conventional-commit messages on `main` drive version bumps and changelog entries; merging the release-please PR cuts a new tag + GitHub release with binaries attached.

Pre-1.0 bump rules (set in `release-please-config.json`):

- `feat:` → patch bump (`0.1.0` → `0.1.1`)
- `fix:` → patch bump
- `feat!:` (breaking) → minor bump (`0.1.0` → `0.2.0`)

After v1.0.0 these revert to standard SemVer.
