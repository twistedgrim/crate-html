# Repository agent guidance

## Workflow

- Work on a topic branch; never commit directly to `main`.
- Name branches `<type>/<short-name>` using a Conventional Commit type, such as
  `feat/broker-logging`, `fix/expiry-race`, or `docs/deployment-guide`. Do not
  add agent or vendor prefixes.
- Install the repository hooks after cloning with `task hooks:install`.
- Before committing, run `task hooks:run`. Do not bypass hooks with
  `--no-verify`.
- Use Conventional Commit subjects and PR titles in the form
  `<type>(optional-scope)!: description`. Allowed types are `feat`, `fix`,
  `deps`, `docs`, `chore`, `ci`, `refactor`, `perf`, `test`, `build`, and
  `revert`.
- Before pushing, validate every commit in the PR range with
  `task commits:check`.
- Open PRs against `main`. Use squash merge and retain the Conventional PR
  title as the squash-commit subject.

## Validation

- Run validation proportional to the change.
- For Go changes, run `task test`, `task vet`, and `task smoke`.
- For Docker or runtime changes, run the relevant `task docker:*` E2E checks.
- Always run `git diff --check`.
- CI is authoritative.

## References

- `docs/releases.md`
- `lefthook.yml`
- `scripts/hooks/commit-msg.sh`
- `scripts/hooks/check-pr-commits.sh`
