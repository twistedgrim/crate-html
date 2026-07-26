package s3store

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Twistedgrim/crate-html/internal/storage"
)

// memFS is a read-only fs.FS holding one expanded site in memory. Object
// storage has no directories to serve from, so an uploaded archive is unpacked
// into this and handed to the same http.ServeFileFS path that serves embedded
// built-in sites.
type memFS struct {
	files map[string][]byte   // clean unrooted path -> contents
	dirs  map[string][]string // clean unrooted dir -> child base names
	mod   time.Time
}

// stats describes an unpacked archive, matching the metadata the index shows.
type stats struct {
	fileCount int
	sizeBytes int64
}

// unpackTar reads a tar stream into a memFS. It applies the same entry-name
// safety rule and size cap as the local backend: limit is the maximum total
// extracted size, and 0 disables the cap.
func unpackTar(r io.Reader, limit int64) (*memFS, stats, error) {
	m := &memFS{
		files: make(map[string][]byte),
		dirs:  map[string][]string{".": {}},
		mod:   time.Now().UTC(),
	}
	tr := tar.NewReader(r)
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, stats{}, fmt.Errorf("tar read: %w", err)
		}
		clean, err := storage.SafeTarPath(hdr.Name)
		if err != nil {
			return nil, stats{}, err
		}
		clean = path.Clean(filepathToSlash(clean))
		if clean == "." {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			m.addDir(clean)
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // legacy flag still emitted by some tar writers
			// Read through a limiter so an oversized (e.g. sparse) archive is
			// cut off mid-file rather than after it is already buffered.
			src := io.Reader(tr)
			if limit > 0 {
				src = io.LimitReader(tr, limit-written+1)
			}
			var buf bytes.Buffer
			n, err := io.Copy(&buf, src)
			written += n
			if err != nil {
				return nil, stats{}, err
			}
			if limit > 0 && written > limit {
				return nil, stats{}, fmt.Errorf("%w: extracted more than %d bytes", storage.ErrSiteTooLarge, limit)
			}
			m.addFile(clean, buf.Bytes())
		default:
			// Skip links, devices, fifos — sites are plain static files, and
			// links in particular reintroduce escape risks.
			continue
		}
	}
	m.sortDirs()
	return m, stats{fileCount: len(m.files), sizeBytes: written}, nil
}

// filepathToSlash normalizes separators so archives written on Windows still
// address the same fs.FS paths.
func filepathToSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func (m *memFS) addFile(name string, data []byte) {
	m.files[name] = data
	m.addDir(path.Dir(name))
	dir := path.Dir(name)
	m.dirs[dir] = append(m.dirs[dir], path.Base(name))
}

// addDir records dir and every ancestor, so an archive that omits explicit
// directory entries still yields a walkable tree.
func (m *memFS) addDir(dir string) {
	for {
		if dir == "" {
			dir = "."
		}
		if _, ok := m.dirs[dir]; !ok {
			m.dirs[dir] = []string{}
			if dir != "." {
				parent := path.Dir(dir)
				m.dirs[parent] = append(m.dirs[parent], path.Base(dir))
			}
		}
		if dir == "." {
			return
		}
		dir = path.Dir(dir)
	}
}

func (m *memFS) sortDirs() {
	for k := range m.dirs {
		sort.Strings(m.dirs[k])
	}
}

// Open implements fs.FS.
func (m *memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := m.files[name]; ok {
		return &memFile{
			Reader: bytes.NewReader(data),
			info:   memInfo{name: path.Base(name), size: int64(len(data)), mod: m.mod},
		}, nil
	}
	if children, ok := m.dirs[name]; ok {
		base := name
		if base != "." {
			base = path.Base(name)
		}
		return &memDir{
			info:    memInfo{name: base, mod: m.mod, dir: true},
			entries: m.entriesFor(name, children),
		}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (m *memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	children, ok := m.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return m.entriesFor(name, children), nil
}

func (m *memFS) entriesFor(dir string, children []string) []fs.DirEntry {
	out := make([]fs.DirEntry, 0, len(children))
	for _, c := range children {
		full := c
		if dir != "." {
			full = dir + "/" + c
		}
		info := memInfo{name: c, mod: m.mod}
		if data, ok := m.files[full]; ok {
			info.size = int64(len(data))
		} else {
			info.dir = true
		}
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	return out
}

// memFile is an open regular file. It embeds *bytes.Reader so it satisfies
// io.ReadSeeker, which is what lets http.ServeFileFS answer Range requests.
type memFile struct {
	*bytes.Reader
	info memInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

type memDir struct {
	info    memInfo
	entries []fs.DirEntry
	off     int
}

func (d *memDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memDir) Close() error               { return nil }
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}

func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		rest := d.entries[d.off:]
		d.off = len(d.entries)
		return rest, nil
	}
	if d.off >= len(d.entries) {
		return nil, io.EOF
	}
	end := min(d.off+n, len(d.entries))
	out := d.entries[d.off:end]
	d.off = end
	return out, nil
}

type memInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) ModTime() time.Time { return i.mod }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
