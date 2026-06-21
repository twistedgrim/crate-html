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

// ErrInvalidName is returned when a site name fails validation.
var ErrInvalidName = errors.New("invalid site name")

// ErrNotFound is returned when a site does not exist.
var ErrNotFound = errors.New("site not found")

// ErrUnsafePath is returned when a tar entry would escape the site directory.
var ErrUnsafePath = errors.New("unsafe path in archive")

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Store owns the sites root directory.
type Store struct {
	root string
}

// New returns a Store rooted at sitesDir. The directory must already exist.
func New(sitesDir string) *Store {
	return &Store{root: sitesDir}
}

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
	return os.RemoveAll(p)
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

	return wire.Site{
		Name:      name,
		UpdatedAt: updated,
		SizeBytes: size,
		FileCount: count,
	}, nil
}

// ReplaceFromTar extracts a tar stream into a staging directory and then
// atomically swaps it in as the site's directory. If extraction fails the
// existing site is left untouched.
func (s *Store) ReplaceFromTar(name string, r io.Reader) (wire.Site, error) {
	p, err := s.Path(name)
	if err != nil {
		return wire.Site{}, err
	}

	stage, err := os.MkdirTemp(s.root, "."+name+".stage-*")
	if err != nil {
		return wire.Site{}, fmt.Errorf("stage dir: %w", err)
	}

	if err := extractTar(stage, r); err != nil {
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
	if backup != "" {
		_ = os.RemoveAll(backup)
	}

	return s.Stat(name)
}

func extractTar(dst string, r io.Reader) error {
	tr := tar.NewReader(r)
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
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
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
