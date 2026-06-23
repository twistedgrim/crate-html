# Smoke test fixtures

Static-site fixtures used by the Go integration tests under `tests/smoke/` (build tag `smoke`; run via `task smoke`). Each subdirectory is a self-contained site that should round-trip cleanly through `crate push` → daemon storage → HTTP GET → expected response.

| Site | Why it's here |
|---|---|
| `welcome/` | Minimal single-page site — shortest happy path. |
| `overview/` | Multi-page (6 files) with shared CSS — exercises cross-file linking and asset serving. |
| `architecture/` | Inline SVG with `currentColor` — exercises content-type sniffing and dark-mode CSS variables. |
| `plan-q3-roadmap/` | Multi-page with relative-link navigation — exercises path routing and the no-trailing-slash redirect. |
| `explainer-auth-flow/` | Single page heavy on CSS — exercises content-type for `text/css` inline. |
| `review-2026-06-20/` | Long-form HTML in a single file — exercises larger payload sizes. |
| `status/` | Tiny single-file site — fast path. |

These are not built into the binary. `cratesplainer/` lives in `internal/builtin/` because it ships as part of `crated`; everything here is purely test data.

## Manual push

```bash
for d in testdata/sites/*/; do
  name=$(basename "$d")
  ./bin/crate push "$d" "$name"
done
```
