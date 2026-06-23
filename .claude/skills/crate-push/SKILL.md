---
name: crate-push
description: Publish an HTML directory to a crate-html daemon and return a shareable URL. Use this whenever the user wants to view rendered HTML (a plan, an explainer doc, a design artifact) in a browser instead of as markdown.
---

# crate-push

Wraps the `crate push` CLI: streams a tar of static HTML to a running `crated` daemon over HTTP and returns a URL the user can open. The daemon may be on the host, in Docker, or behind tsdproxy on a Tailscale tailnet — same CLI surface in every case.

## When to use this skill

- The user asks to "view this as HTML", "open this in a browser", "render this", or similar.
- You generated multi-page documentation, a status dashboard, a diagram-heavy explainer, or any output where styling and side-by-side reading matter.
- The user references a previously deployed site by name (use `crate ls` to enumerate).

## Preconditions

- `crated` is running somewhere reachable. Verify with `crate status` (or `docker exec crated crate status`). Non-zero exit means the daemon isn't up — ask the user to start it.
- For Docker deployments, the `crate` CLI is inside the container; invoke it via `docker exec`. For local laptop runs, `crate` is on the host PATH.
- Site name must match `^[a-z0-9][a-z0-9._-]{0,62}$`: lowercase letter or digit first, then letters / digits / dot / hyphen / underscore. Max 63 chars.

## Push variants

`crate push <src> <name>` accepts three source shapes — pick whichever fits your environment:

```bash
# 1. Directory walk (most common — local or `docker cp` followed by push).
crate push ./my-site mysite
docker exec crated crate push /tmp/my-site mysite

# 2. Stdin tar — the cleanest path when the agent runs on the Docker host.
#    No intermediate filesystem mutation in the container.
tar -C ./my-site -cf - . | docker exec -i crated crate push - mysite

# 3. Pre-built tar file on disk.
tar -C ./my-site -cf /tmp/mysite.tar .
crate push /tmp/mysite.tar mysite
```

All three result in the same atomic site replacement on the daemon side; symlinks, hardlinks, and special files are silently dropped.

## Other useful commands

```bash
crate ls                  # list deployed sites
crate rm <name>           # remove a site
crate open <name>         # open the site URL in the default browser
crate status              # daemon version + site count
crate token               # print the bearer token from the loaded config
crate push --open <src> <name>   # push + open in one step
```

For Docker deployments, prefix any of those with `docker exec crated`.

## Flags worth knowing

- `--open` / `-o` on `crate push` — opens the URL in the default browser after success. Honors `$BROWSER` for headless / scripted environments (set it to `/usr/bin/true` to suppress the open).
- `--config <path>` (global, on both `crate` and `crated`) — override the XDG config-file location. Useful for running multiple isolated daemons.
- Env vars on the CLI side: `CRATE_TOKEN` and `CRATE_BASE_URL` override the loaded config process-locally. Use `eval "$(task docker:env)"` to point a host CLI at a dockerized daemon.

## What to give the user

After a successful push, the CLI prints two lines:
```
pushed <name> (N files, M bytes)
http://localhost:7777/<name>/
```

The second line is the URL the user should open. Present it as your primary deliverable; the file/byte count is just confirmation.

## Common failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| `connection refused` | Daemon isn't running | Start `crated` (local) or `task docker:up` (Docker) |
| `401 Unauthorized: invalid token` | CLI and daemon have different tokens | Run inside the container, or `eval "$(task docker:env)"` first |
| `invalid site name` | Name violated the regex | Suggest a valid alternative (lowercase, no spaces, ≤63 chars) |
| `unsafe path in archive` | Tar entry contained `../` traversal | Re-create the tar from a clean directory |
| Missing index.html → 404 in browser | Site dir lacks `index.html` | Ensure every site has an `index.html` at the root |
