package s3store

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/Twistedgrim/crate-html/internal/storage"
)

// tarOf builds an in-memory tar from path -> contents.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackTarBuildsWalkableTree(t *testing.T) {
	raw := tarOf(t, map[string]string{
		"index.html":        "<h1>root</h1>",
		"docs/guide.html":   "<h1>guide</h1>",
		"docs/img/logo.svg": "<svg/>",
	})
	fsys, st, err := unpackTar(bytes.NewReader(raw), 0)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if st.fileCount != 3 {
		t.Errorf("file count = %d, want 3", st.fileCount)
	}
	want := int64(len("<h1>root</h1>") + len("<h1>guide</h1>") + len("<svg/>"))
	if st.sizeBytes != want {
		t.Errorf("size = %d, want %d", st.sizeBytes, want)
	}

	// fstest.TestFS exercises Open/ReadDir/Stat/Glob consistency for us, which
	// is the real check that this hand-rolled fs.FS behaves like a filesystem.
	if err := fstest.TestFS(fsys, "index.html", "docs/guide.html", "docs/img/logo.svg"); err != nil {
		t.Errorf("TestFS: %v", err)
	}

	b, err := fs.ReadFile(fsys, "docs/guide.html")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "<h1>guide</h1>" {
		t.Errorf("contents = %q", b)
	}
}

// TestUnpackTarSyntheticDirs covers an archive with no explicit directory
// entries: the intermediate directories must still be walkable.
func TestUnpackTarSyntheticDirs(t *testing.T) {
	raw := tarOf(t, map[string]string{"a/b/c.html": "deep"})
	fsys, _, err := unpackTar(bytes.NewReader(raw), 0)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for _, dir := range []string{".", "a", "a/b"} {
		if _, err := fs.ReadDir(fsys, dir); err != nil {
			t.Errorf("ReadDir(%q): %v", dir, err)
		}
	}
}

// tarOrdered builds a tar with entries in exactly the order given, unlike
// tarOf, which iterates a map and therefore shuffles them per run.
func tarOrdered(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(name)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUnpackTarEntryOrderIndependent pins the tree against archive ordering.
// A deep entry arriving before a shallower sibling used to leave the shared
// parent directory unlinked from the root: the files were still individually
// addressable, so serving them worked, but the directory was invisible to any
// walk of the tree. Only the shuffled ordering of a map-built fixture exposed
// it, so both orders are asserted explicitly here.
func TestUnpackTarEntryOrderIndependent(t *testing.T) {
	orders := map[string][]string{
		"deep first":    {"docs/img/logo.svg", "docs/guide.html", "index.html"},
		"shallow first": {"index.html", "docs/guide.html", "docs/img/logo.svg"},
		"root last":     {"docs/img/logo.svg", "index.html", "docs/guide.html"},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			fsys, st, err := unpackTar(bytes.NewReader(tarOrdered(t, order...)), 0)
			if err != nil {
				t.Fatalf("unpack: %v", err)
			}
			if st.fileCount != 3 {
				t.Errorf("file count = %d, want 3", st.fileCount)
			}
			if err := fstest.TestFS(fsys, "index.html", "docs/guide.html", "docs/img/logo.svg"); err != nil {
				t.Errorf("TestFS: %v", err)
			}
			// The intermediate directory must be reachable by walking, not
			// merely by addressing a file inside it.
			names := make(map[string]bool)
			if err := fs.WalkDir(fsys, ".", func(p string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				names[p] = true
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
			for _, want := range []string{"docs", "docs/img", "docs/guide.html", "docs/img/logo.svg", "index.html"} {
				if !names[want] {
					t.Errorf("walk did not reach %q", want)
				}
			}
		})
	}
}

func TestUnpackTarRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape.html", "/etc/passwd", "a/../../b.html"} {
		raw := tarOf(t, map[string]string{name: "x"})
		if _, _, err := unpackTar(bytes.NewReader(raw), 0); !errors.Is(err, storage.ErrUnsafePath) {
			t.Errorf("unpack(%q) error = %v, want ErrUnsafePath", name, err)
		}
	}
}

func TestUnpackTarEnforcesSizeCap(t *testing.T) {
	raw := tarOf(t, map[string]string{"big.html": "0123456789"})
	if _, _, err := unpackTar(bytes.NewReader(raw), 5); !errors.Is(err, storage.ErrSiteTooLarge) {
		t.Errorf("error = %v, want ErrSiteTooLarge", err)
	}
	if _, _, err := unpackTar(bytes.NewReader(raw), 10); err != nil {
		t.Errorf("exactly at the cap should pass, got %v", err)
	}
}

func TestUnpackTarSkipsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "index.html", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	fsys, st, err := unpackTar(&buf, 0)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if st.fileCount != 1 {
		t.Errorf("file count = %d, want 1 (symlink skipped)", st.fileCount)
	}
	if _, err := fs.Stat(fsys, "evil"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("symlink should not exist, got %v", err)
	}
}

// TestServeFileFSRangeRequest is the reason memFile embeds *bytes.Reader:
// without io.ReadSeeker, http.ServeFileFS cannot answer Range requests.
func TestServeFileFSRangeRequest(t *testing.T) {
	// Deliberately not index.html: net/http canonicalizes a request path
	// ending in "index.html" with a 301 before it ever reads the file.
	raw := tarOf(t, map[string]string{"data.txt": "0123456789"})
	fsys, _, err := unpackTar(bytes.NewReader(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/data.txt", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	http.ServeFileFS(rec, req, fsys, "data.txt")

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Errorf("body = %q, want %q", got, "2345")
	}
}
