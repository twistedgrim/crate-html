package server

import (
	"io"
	"io/fs"
	"time"

	"github.com/Twistedgrim/crate-html/internal/wire"
)

// Backend is the site storage the daemon serves from. It is defined here, at
// the consumer, so alternative implementations (local disk, object storage)
// only have to satisfy what the HTTP layer actually uses.
//
// The read path is deliberately expressed as an fs.FS rather than a filesystem
// path: that is the whole seam that lets a backend keep site content somewhere
// other than a local directory. Embedded built-in sites already serve through
// the same fs.FS path, so disk and non-disk content take identical code.
//
// Implementations are expected to be safe for concurrent use.
type Backend interface {
	// List returns metadata for every site, sorted by name.
	List() ([]wire.Site, error)

	// Names returns just the site names. It is the cheap counterpart to List
	// for callers that only need to know which sites exist — it runs on the
	// 404 path, so implementations should avoid per-site metadata lookups.
	Names() ([]string, error)

	// Exists reports whether a site is present.
	Exists(name string) (bool, error)

	// Open returns the site's content rooted at the site itself, so that
	// "index.html" addresses the site's own index. Returns ErrNotFound if the
	// site does not exist.
	Open(name string) (fs.FS, error)

	// ReplaceFromTarWithExpiry replaces a site's content from a tar stream and
	// records its expiry, where a nil duration retains the site indefinitely.
	// The replacement must be atomic from a reader's point of view: a failed
	// or partial upload leaves the previous content in place.
	ReplaceFromTarWithExpiry(name string, r io.Reader, expiry *time.Duration) (wire.Site, error)

	// Delete removes a site, returning ErrNotFound if it is not present.
	Delete(name string) error

	// DeleteExpired removes every site whose deadline is at or before now and
	// returns the names it removed.
	DeleteExpired(now time.Time) ([]string, error)
}
