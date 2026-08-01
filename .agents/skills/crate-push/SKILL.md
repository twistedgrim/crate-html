---
name: crate-push
description: Publish a static HTML directory to a crate-html daemon and return a shareable URL. Use when a user wants generated HTML, a plan, an explainer, a dashboard, or another browser-friendly artifact published instead of returned only as Markdown.
---

# crate-push

Publish with the `crate push` CLI. The daemon may run locally, in Docker, or
behind tsdproxy on a Tailscale tailnet; the publish workflow is the same.

## Workflow

1. Verify the daemon is reachable with `crate status`; for a Docker deployment,
   use `docker exec crated crate status`.
2. Choose a lowercase site name matching
   `^[a-z0-9][a-z0-9._-]{0,62}$`.
3. Push the directory, then return the resulting URL as the primary deliverable.

```bash
# Local daemon or host CLI configured for a remote daemon.
crate push ./my-site mysite

# Docker daemon; avoids copying a directory into the container.
tar -C ./my-site -cf - . | docker exec -i crated crate push - mysite
```

Use `crate push --open <src> <name>` only when opening a local browser is useful.
The successful command prints the published URL; present that URL to the user.

## Configuration

For a Docker deployment, run commands inside the container or initialize a host
CLI with `eval "$(task docker:env)"`. For a split broker/web deployment, use
`eval "$(task docker:split:env)"`; mutations go to the broker and returned
links use the public web URL.

## Useful commands

```bash
crate ls
crate rm <name>
crate open <name>
crate status
crate push --open <src> <name>
```

## Failure handling

| Symptom | Resolution |
|---|---|
| Connection refused | Start the local daemon or Docker stack, then retry `crate status`. |
| `401 Unauthorized` | Run the CLI in the Docker container or load the matching `task docker:env` exports. |
| Invalid site name | Use a lowercase name without spaces, up to 63 characters. |
| Missing `index.html` | Add an `index.html` at the root of the pushed directory. |
