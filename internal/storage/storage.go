// Package storage manages site directories on disk. Sites live as plain
// directories under a root (typically $XDG_DATA_HOME/crate/sites). Writes
// stage into a tmp directory and atomically rename into place so a partial
// upload never leaves a half-replaced site.
package storage

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Twistedgrim/crate-html/internal/wire"
)

const expiryDir = ".expiries"

// ErrInvalidName is returned when a site name fails validation.
var ErrInvalidName = errors.New("invalid site name")

// ErrNotFound is returned when a site does not exist.
var ErrNotFound = errors.New("site not found")

// ErrUnsafePath is returned when a tar entry would escape the site directory.
var ErrUnsafePath = errors.New("unsafe path in archive")

// ErrSiteTooLarge is returned when extracting an archive would exceed the
// store's max site size. This guards the *logical* size: a sparse tar can
// encode gigabytes of zeros in a few kilobytes on the wire, so an HTTP
// body-size cap alone does not bound what lands on disk.
var ErrSiteTooLarge = errors.New("site exceeds maximum size")

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Store owns the sites root directory.
type Store struct {
	root         string
	maxSiteBytes int64 // 0 = unlimited
}

// New returns a Store rooted at sitesDir. The directory must already exist.
func New(sitesDir string) *Store {
	return &Store{root: sitesDir}
}

// SetMaxSiteBytes caps the total extracted size of a site. 0 disables the cap.
func (s *Store) SetMaxSiteBytes(n int64) { s.maxSiteBytes = n }

// Root returns the sites root directory.
func (s *Store) Root() string { return s.root }

// ValidateName enforces a conservative DNS-ish name policy.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q (must match %s)", ErrInvalidName, name, nameRE.String())
	}
	return nil
}

// Path returns the on-disk directory for a site (whether it exists or not).
func (s *Store) Path(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root, name), nil
}

// Exists reports whether the site directory exists.
func (s *Store) Exists(name string) (bool, error) {
	p, err := s.Path(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes the site directory. Returns ErrNotFound if it does not exist.
func (s *Store) Delete(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := os.RemoveAll(p); err != nil {
		return err
	}
	_ = os.Remove(s.expiryPath(name))
	return nil
}

func (s *Store) expiryPath(name string) string {
	return filepath.Join(s.root, expiryDir, name)
}

// SetExpiry records when a site should be removed. A nil time means the site
// is retained indefinitely. Metadata is kept outside the publicly served tree.
func (s *Store) SetExpiry(name string, expiresAt *time.Time) error {
	if _, err := s.Path(name); err != nil {
		return err
	}
	metadata := s.expiryPath(name)
	if expiresAt == nil {
		if err := os.Remove(metadata); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(metadata), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(metadata), ".expiry-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.WriteString(tmp, expiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, metadata)
}

func (s *Store) expiry(name string) (*time.Time, error) {
	b, err := os.ReadFile(s.expiryPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("parse expiry for %s: %w", name, err)
	}
	return &t, nil
}

// DeleteExpired removes sites whose recorded deadline is at or before now.
func (s *Store) DeleteExpired(now time.Time) ([]string, error) {
	sites, err := s.List()
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, site := range sites {
		if site.ExpiresAt == nil || site.ExpiresAt.After(now) {
			continue
		}
		if err := s.Delete(site.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return deleted, err
		}
		deleted = append(deleted, site.Name)
	}
	return deleted, nil
}

// Names returns the names of every site under root, without stat'ing their
// contents. It is the cheap counterpart to List for callers that only need to
// know which sites exist (for example resolving a per-project group prefix on
// what would otherwise be a 404).
func (s *Store) Names() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if ValidateName(name) != nil {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// List returns metadata for every site under root.
func (s *Store) List() ([]wire.Site, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]wire.Site, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if ValidateName(name) != nil {
			continue
		}
		info, err := s.Stat(name)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stat returns metadata for a single site.
func (s *Store) Stat(name string) (wire.Site, error) {
	p, err := s.Path(name)
	if err != nil {
		return wire.Site{}, err
	}
	di, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return wire.Site{}, ErrNotFound
	}
	if err != nil {
		return wire.Site{}, err
	}

	var size int64
	var count int
	updated := di.ModTime()

	err = filepath.WalkDir(p, func(_ string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return ferr
		}
		size += fi.Size()
		count++
		if fi.ModTime().After(updated) {
			updated = fi.ModTime()
		}
		return nil
	})
	if err != nil {
		return wire.Site{}, err
	}
	expiresAt, err := s.expiry(name)
	if err != nil {
		return wire.Site{}, err
	}

	return wire.Site{
		Name:      name,
		UpdatedAt: updated,
		SizeBytes: size,
		FileCount: count,
		ExpiresAt: expiresAt,
	}, nil
}

// ReplaceFromTar extracts a tar stream into a staging directory and then
// atomically swaps it in as the site's directory. If extraction fails the
// existing site is left untouched.
func (s *Store) ReplaceFromTar(name string, r io.Reader) (wire.Site, error) {
	return s.replaceFromTar(name, r, nil)
}

// ReplaceFromTarWithExpiry atomically replaces a site and records its expiry.
// The duration is measured after the archive has been extracted, so slow
// uploads receive their full requested lifetime. A nil duration retains the
// site indefinitely.
func (s *Store) ReplaceFromTarWithExpiry(name string, r io.Reader, expiry *time.Duration) (wire.Site, error) {
	return s.replaceFromTar(name, r, func() error {
		var expiresAt *time.Time
		if expiry != nil {
			t := time.Now().Add(*expiry)
			expiresAt = &t
		}
		return s.SetExpiry(name, expiresAt)
	})
}

func (s *Store) replaceFromTar(name string, r io.Reader, afterInstall func() error) (wire.Site, error) {
	p, err := s.Path(name)
	if err != nil {
		return wire.Site{}, err
	}

	stage, err := os.MkdirTemp(s.root, "."+name+".stage-*")
	if err != nil {
		return wire.Site{}, fmt.Errorf("stage dir: %w", err)
	}

	if err := extractTar(stage, r, s.maxSiteBytes); err != nil {
		_ = os.RemoveAll(stage)
		return wire.Site{}, err
	}

	// Move existing site aside, swap in stage, then delete the old copy.
	backup := ""
	if _, err := os.Stat(p); err == nil {
		backup = p + ".old-" + fmt.Sprintf("%d", time.Now().UnixNano())
		if err := os.Rename(p, backup); err != nil {
			_ = os.RemoveAll(stage)
			return wire.Site{}, fmt.Errorf("rename existing: %w", err)
		}
	}

	if err := os.Rename(stage, p); err != nil {
		// roll back
		if backup != "" {
			_ = os.Rename(backup, p)
		}
		_ = os.RemoveAll(stage)
		return wire.Site{}, fmt.Errorf("install site: %w", err)
	}
	if afterInstall != nil {
		if err := afterInstall(); err != nil {
			_ = os.RemoveAll(p)
			if backup != "" {
				_ = os.Rename(backup, p)
			}
			return wire.Site{}, fmt.Errorf("set expiry: %w", err)
		}
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}

	return s.Stat(name)
}

func extractTar(dst string, r io.Reader, limit int64) error {
	tr := tar.NewReader(r)
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		clean := filepath.Clean(hdr.Name)
		// Reject the path if it escapes the destination root. filepath.Clean
		// already resolves any embedded "/../"; we only need to make sure the
		// cleaned path doesn't start at "..", "../...", or "/...". Names that
		// merely *begin* with two dots (e.g. "..foo") are valid filenames.
		if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return fmt.Errorf("%w: %s", ErrUnsafePath, hdr.Name)
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is legacy but still used by some tar writers
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Copy through a limiter so an oversized (e.g. sparse) archive is
			// cut off mid-file instead of after it has already hit the disk.
			src := io.Reader(tr)
			if limit > 0 {
				src = io.LimitReader(tr, limit-written+1)
			}
			n, err := io.Copy(f, src)
			written += n
			if err != nil {
				_ = f.Close()
				return err
			}
			if limit > 0 && written > limit {
				_ = f.Close()
				return fmt.Errorf("%w: extracted more than %d bytes", ErrSiteTooLarge, limit)
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links — sites are plain static files; links open up escape risks.
			continue
		default:
			// Skip anything else (devices, fifos, etc).
			continue
		}
	}
}
