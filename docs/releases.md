# Releases

crate-html uses [Release Please](https://github.com/googleapis/release-please)
for versioning, changelogs, GitHub Releases, binaries, and container images.

## Release-bearing changes

Release Please reads Conventional Commit subjects that land on `main`:

- `feat:` and `fix:` create a release.
- `feat!:` / `fix!:` and a `BREAKING CHANGE:` footer signal a breaking release.
- `docs:`, `chore:`, `ci:`, `refactor:`, `test:`, `build:`, `perf:`, and
  `revert:` are valid PR titles but do not create a release by themselves.

While the project is pre-1.0, features and fixes bump the patch version;
breaking changes bump the minor version. The initial release was explicitly
bootstrapped as `v0.1.0` rather than Release Please's default `v1.0.0`.

## Pull-request and merge rules

PR titles must use Conventional Commit syntax. The `Conventional PR Title`
workflow validates this before merge:

```text
feat: add default site expiry
fix(server): make expiry publishing atomic
docs: refresh deployment guide
```

Use squash merge for feature work and keep the PR title unchanged as the
squash-commit subject. GitHub uses that title for the commit on `main`; losing
the `feat:` or `fix:` prefix means Release Please will not include the change.

## Local feedback

Install the optional local safeguards after cloning:

```bash
task hooks:install
```

Lefthook validates commit subjects with the same Conventional Commit script
that CI uses for both PR titles and every PR commit. It also runs Go
formatting/vetting plus Markdown, Dockerfile, and GitHub Actions linting for
affected files. Install `hadolint`, `markdownlint`, and `actionlint` when
working on those file types; CI remains the merge authority.

## Flow

1. A release-bearing squash commit lands on `main`.
2. Release Please opens or updates a release PR with a version bump and
   changelog entries.
3. Merge that generated release PR to create the tag and GitHub Release.
4. The release workflow builds and attaches binaries and publishes the tagged
   container image.
