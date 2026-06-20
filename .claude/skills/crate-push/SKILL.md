---
name: crate-push
description: Publish an HTML directory to the local crate-html daemon and return a shareable URL. Use this whenever the user wants to view rendered HTML (a plan, an explainer doc, a design artifact) in a browser instead of as markdown.
---

# crate-push

Wraps the `crate push` CLI: tars a local directory and uploads it to a running `crated` daemon over HTTP. On success, returns a `http://localhost:7777/<name>/` URL the user can open.

## When to use this skill

- The user asks to "view this as HTML", "open this in a browser", "render this", or similar.
- You have built or generated a directory of static HTML/CSS/JS and want the user to see it as a real page.
- The user references a previous deployed site by name (use `crate ls` to enumerate).

## Preconditions

- `crate` and `crated` are installed and on `PATH` (build with `task build` from this repo and add `./bin` to `PATH`, or `go install ./cmd/...`).
- `crated` is running. If not, start it in a separate terminal: `crated`. If you're unsure, run `crate status` — non-zero exit means the daemon isn't up.

## How to use

1. Ensure you have a directory of static files (must contain `index.html`).
2. Pick a site name: lowercase letters, digits, dot, hyphen, underscore. No leading dot/dash.
3. Run:

   ```bash
   crate push <directory> <name>
   ```

4. The command prints the file count, byte size, and the URL. Show that URL to the user.

## Examples

```bash
# Publish the rendered plan
crate push ./plan-html plan-2026-06-20

# List what's deployed
crate ls

# Remove an old draft
crate rm old-plan
```

## Common failures

- **"connection refused"** — `crated` isn't running. Tell the user to start it (`crated` in another terminal) and re-try.
- **"invalid site name"** — name violated the pattern. Suggest a valid alternative.
- **"unsafe path in archive"** — the source directory contained a symlink or a path that escapes the tree. Re-run from a clean directory.
