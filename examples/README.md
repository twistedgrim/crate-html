# Examples

Optional compose overrides and reference configurations. Nothing here is required to run crate-html — the root `docker-compose.yml` is self-contained.

## `index.tmpl`

A custom `html/template` for the `/` and `/<prefix>/` index pages, replacing the one embedded in the binary. The view model it receives is documented in a comment at the top of the file.

```bash
CRATE_INDEX_TEMPLATE=/path/to/index.tmpl crated
```

Or set it in `config.yaml`:

```yaml
index_template: /config/index.tmpl
```

The template is parsed once at startup — a syntax error stops the daemon rather than surfacing as a 500 later. It is operator-supplied only: crated never renders a template that arrived over the public API, so pushed sites stay pure static content.

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
