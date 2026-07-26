# Running crate-html on object storage

crated normally keeps sites as directories on a local filesystem. Setting
`storage_backend: s3` moves them into an S3-compatible bucket instead, so the
daemon can run somewhere with no writable volume — an ephemeral container
filesystem, a serverless runtime, a node pool with no PersistentVolumeClaim.

The design rationale is in [`plans/object-storage-backend.md`](./plans/object-storage-backend.md).
This page is the operator's view: what to set, what you get, and what to watch
out for.

## Three modes

The choice is not binary. What moves to the bucket and what stays local are
separate questions:

| | Sites | Named API tokens | Root token / config | Needs a volume? |
|---|---|---|---|---|
| **Local** (default) | disk | disk | `config.yaml` on disk | yes — `/config` + `/data` |
| **Hybrid** — s3 + config volume | **bucket** | **bucket** | `config.yaml` on disk | yes — `/config` only |
| **Stateless** — s3, no volume | **bucket** | **bucket** | `CRATE_TOKEN` env | **no** |

**Hybrid is the one most deployments want.** Selecting the S3 backend already
moves sites *and* named tokens to the bucket; whether you also need `CRATE_TOKEN`
depends only on whether `/config` survives a restart. Mount a small `/config`
volume and the root token is generated once and persists, exactly as it does
today — you get bucket-backed content without having to manage a secret.

Go stateless only when you genuinely cannot mount anything. Then the root token
*must* come from the environment (see [Gotchas](#gotchas)).

## Requirements

- **A bucket that already exists.** crated never creates one. A typo therefore
  fails loudly at startup instead of silently starting against an empty bucket.
- **An S3-compatible endpoint.** Tested against [rustfs](https://rustfs.com);
  AWS S3, Cloudflare R2, MinIO, and Backblaze B2 speak the same API.
- **Credentials**, either explicit or via the standard AWS chain (env vars, IAM
  role, instance profile). Under an IAM role there are no secrets to supply.
- **Permissions** on the bucket:

  | Action | Used for |
  |---|---|
  | `s3:ListBucket` | startup bucket check, listing sites |
  | `s3:GetObject` | serving sites, reading metadata and tokens |
  | `s3:PutObject` | pushing sites, writing metadata and tokens |
  | `s3:DeleteObject` | `crate rm`, expiry, superseded versions |

  Scope them to the bucket and, if you use `prefix`, to that key prefix.

## Configuration

Every setting is available as an environment variable, because a host with no
durable config file has to be configurable from the environment alone.

| Env var | `config.yaml` | Notes |
|---|---|---|
| `CRATE_STORAGE_BACKEND` | `storage_backend` | `local` (default) or `s3` |
| `CRATE_S3_ENDPOINT` | `s3.endpoint` | **Required.** Bare host implies `https`; prefix `http://` for a plaintext dev server |
| `CRATE_S3_BUCKET` | `s3.bucket` | **Required.** Must already exist |
| `CRATE_S3_REGION` | `s3.region` | Optional for most S3-compatible servers |
| `CRATE_S3_ACCESS_KEY` | `s3.access_key` | Empty → AWS credential chain |
| `CRATE_S3_SECRET_KEY` | `s3.secret_key` | Empty → AWS credential chain |
| `CRATE_S3_PREFIX` | `s3.prefix` | Scopes all keys, so one bucket can host several deployments |
| `CRATE_S3_CACHE_BYTES` | `s3.cache_bytes` | In-memory site cache budget. `0` → 256 MiB default; negative disables caching |

Minimal stateless invocation:

```bash
CRATE_STORAGE_BACKEND=s3 \
CRATE_S3_ENDPOINT=https://s3.example.com \
CRATE_S3_BUCKET=crate \
CRATE_S3_ACCESS_KEY=... \
CRATE_S3_SECRET_KEY=... \
CRATE_TOKEN=<your-root-token> \
crated
```

Confirm which backend is live by reading the startup log — it prints the bucket
rather than a directory:

```
crated sites:  s3://crate/ (https://s3.example.com)
```

If that line shows a filesystem path, the S3 backend is **not** active.

## What lives in the bucket

```
<prefix>meta/<name>.json            per-site metadata; also the existence record
<prefix>sites/<name>/<version>.tar  site content, one object per published version
<prefix>state/tokens.yaml           named API tokens
```

A push writes a new version object, then overwrites the metadata object to point
at it. That second write is the atomic commit, so readers see the old site or
the new one and never a partial one.

## Gotchas

**Set `CRATE_TOKEN` if `/config` is not persisted.** The root token still lives
in `config.yaml`. Without a durable `/config` *and* without `CRATE_TOKEN`, every
restart generates a new random root token and silently invalidates whatever your
clients were using. Named tokens minted through `/api/tokens` are unaffected —
they are in the bucket. This is the single most common way to get this wrong.

**A bare endpoint means HTTPS.** `CRATE_S3_ENDPOINT=localhost:9000` is treated
as `https://localhost:9000` and will fail against a plaintext dev server. Write
`http://localhost:9000` explicitly.

**Multiple replicas are eventually consistent.** A push handled by one replica
becomes visible to others within about ten seconds (the metadata cache TTL). The
local backend never had this property because it never had more than one view of
the data. Pin to one replica if you need read-your-writes across the fleet.

**Expiry runs independently in every replica.** Deletes are idempotent so this is
harmless, but it is duplicated work — and at zero replicas nothing reaps at all,
so sites outlive their deadline until something is running again.

**Token writes are compare-and-swap.** Two replicas minting at once would
otherwise clobber each other's token set, so a stale writer is refused rather
than winning. Servers that ignore conditional write headers degrade to
last-write-wins. rustfs, MinIO, AWS S3, and R2 all enforce them.

**Uploads are buffered in memory.** The archive is validated before anything is
written and there is no scratch disk to stage it on, so peak memory tracks
`max_upload_bytes` (default 100 MiB) per concurrent upload.

**Cache sizing.** Sites are unpacked into memory on first request and held under
an LRU budget (`CRATE_S3_CACHE_BYTES`, default 256 MiB). A site larger than the
whole budget is served but never cached, so it re-fetches on every request —
raise the budget if you host something big and hot.

## Local development

`examples/docker-compose.rustfs.yml` brings up rustfs, creates the bucket, and
runs a crated with **no volumes at all** — the stateless mode, so the container
can be destroyed and recreated with nothing lost:

```bash
task rustfs:up
```

For the hybrid mode, add a `/config` volume to the `crated` service and drop
`CRATE_TOKEN`.

## Tests

```bash
task smoke:s3
```

Starts rustfs in Docker on demand and runs the object-storage end-to-end suite:
push/serve/replace/delete, tar traversal rejection, expiry reaping, restart
against a fresh home, token survival across a restart, and the token
compare-and-swap conflict path. Set `CRATE_TEST_S3_ENDPOINT` to run against an
existing server instead. With neither available the suite skips rather than
fails.
