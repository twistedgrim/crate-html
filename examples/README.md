# Examples

Optional compose overrides and reference configurations. Nothing here is required to run crate-html — the root `docker-compose.yml` is self-contained.

## `docker-compose.tsdproxy.yml`

Adds tsdproxy labels to the `crated` service so the daemon is reachable at `https://crate.<your-tailnet>.ts.net/` on your Tailscale tailnet. Requires [tsdproxy](https://github.com/almeidapaulopt/tsdproxy) running on the same Docker host.

```bash
docker compose -f docker-compose.yml -f examples/docker-compose.tsdproxy.yml up -d
```

Or copy it in as a `docker-compose.override.yml` and Compose will auto-merge it:

```bash
cp examples/docker-compose.tsdproxy.yml docker-compose.override.yml
docker compose up -d
```

See [`docs/deploy.md`](../docs/deploy.md#3-docker--tsdproxy-on-tailscale-https-on-your-tailnet) for the full deployment recipe, including the `authKeyFile` vs env var configuration detail and the persistent-`/data`-volume requirement.
