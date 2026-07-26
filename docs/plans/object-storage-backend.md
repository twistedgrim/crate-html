# Object storage backend

Running crated where there is no writable volume — a container with an
ephemeral filesystem, a serverless runtime, a node pool with no PVC — means
sites cannot live on local disk. This is the design for keeping them in
S3-compatible object storage instead.

## Why not a database

The metadata a site carries is five scalars: name, file count, size, updated
time, and an optional expiry. There are no joins, no queries across sites, and
no transactions spanning more than one site. A database would add a second
stateful system to operate, which is the exact cost this feature exists to
remove — anyone willing to run Postgres alongside crated was already willing to
mount a volume.

The one argument for a database was the token store's read-modify-write cycle.
S3 conditional writes (`If-Match` / `If-None-Match`) provide compare-and-swap,
so that gap closes without one.

A database earns its place later if crate grows per-user quotas, audit history,
or expiry indexing across tens of thousands of sites. None of those exist today.

## The seam

The HTTP layer previously asked the store for a filesystem path and handed it to
`http.ServeFile`, which hardcoded local disk into the read path. Embedded
built-in sites, meanwhile, already served through `http.ServeFileFS` from an
`fs.FS`.

`server.Backend` unifies those: the read path is `Open(name) (fs.FS, error)`,
and one `serveSite` handles stored sites and built-ins alike. A site is a name
plus an `fs.FS`, wherever its bytes came from.

## Layout

A site is one tar object under a version-scoped key, plus a metadata object
naming the live version:

```
<prefix>meta/<name>.json            {version, file_count, size_bytes, ...}
<prefix>sites/<name>/<version>.tar  the archive exactly as uploaded
```

Storing the archive whole, rather than exploding it into per-file objects,
keeps a push to a single content write and makes the read path one GET.

## Publishing is a pointer flip

Object storage has no rename, so the local backend's stage-and-rename does not
translate. A push instead:

1. writes a new `sites/<name>/<version>.tar` — nothing references it, so it is
   invisible to readers;
2. overwrites `meta/<name>.json` to name the new version. This single PUT is
   atomic, which makes it the commit point;
3. deletes the superseded version object.

Readers therefore see the old site or the new one, never a partial one. A
failure before step 2 leaves an orphaned object, which is garbage rather than
corruption. A failure at step 3 leaks an unreferenced object, never live data.

Metadata is also the existence record: a site with no metadata object does not
exist, whatever content objects remain. That is what stops a half-finished push
or a failed delete from resurrecting a site.

## Caching

Content is unpacked into an in-memory `fs.FS` on first read and served from
there. The cache is bounded by a byte budget with LRU eviction.

Cache keys include the content version, so a push makes previous entries
unreachable — there is no invalidation step to forget. A site larger than the
entire budget is served but never retained, so one oversized site cannot flush
the cache on every request.

Site metadata is cached separately and re-read from the bucket when it ages past
`MetaTTL`. A process always sees its own writes immediately; the TTL only bounds
how long it can be stale about *another* replica's writes.

## What this changes operationally

- **Multi-replica staleness.** With more than one replica, a push handled by one
  replica is visible to others only after `MetaTTL`. The local backend never had
  this property because it never had more than one view of the data.
- **Expiry stays an in-process ticker.** Every replica reaps independently.
  Deletes are idempotent so concurrent reaping is benign, but it is duplicated
  work, and at zero replicas nothing reaps at all.
- **Uploads are buffered in memory.** The archive must be validated before
  anything is written, and there is no scratch disk to stage it on. The existing
  `max_upload_bytes` cap bounds this.
- **Tokens still live on local disk.** Until the token store moves behind the
  same backend, "no local storage" is not yet literally true — `/config` remains
  stateful.

## Configuration

`storage_backend: local | s3`, defaulting to `local` so existing deployments are
untouched. Every S3 setting is also an environment variable (`CRATE_S3_*`),
because a deployment with no durable config file has to be configurable from the
environment alone. Credentials left empty fall back to the standard AWS chain,
which is what allows running under an IAM role with no secrets in config.

crated validates storage settings and contacts the bucket during startup, so a
bad endpoint or a missing bucket stops the daemon with a clear message instead
of failing the first push.

## Tenancy

One bucket with key prefixes, not a bucket per tenant. Buckets are a poor
tenancy unit — global namespace, slow to create, and a low per-account cap — and
crate has no user or project concept to map them onto today. Dot-namespacing
(`myproject.docs`) is presentation-only; site names remain a flat namespace. The
`prefix` setting already scopes a deployment within a bucket, so splitting later
is mechanical.

## Status

Implemented: the `Backend` seam, the S3 backend, the bounded cache, config and
env wiring, and rustfs-backed end-to-end tests.

Not done: moving the token store off local disk (see above), and any
cross-replica coordination for expiry.
