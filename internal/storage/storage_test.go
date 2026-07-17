package storage_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/storage"
)

func TestValidateName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		valid bool
	}{
		// valid
		{"a", true},
		{"site", true},
		{"site1", true},
		{"my-site", true},
		{"my_site", true},
		{"my.site", true},
		{"a.b-c_d", true},
		{strings.Repeat("a", 63), true},

		// invalid
		{"", false},
		{".hidden", false},  // can't start with dot
		{"-leading", false}, // can't start with hyphen
		{"_leading", false}, // can't start with underscore
		{"/abs", false},     // slash
		{"..", false},       // can't start with dot
		{"UPPER", false},    // uppercase
		{"sub/dir", false},  // slash
		{"with space", false},
		{strings.Repeat("a", 64), false}, // one over the cap
		{"eé", false},                    // accented char
		{".", false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := storage.ValidateName(c.name)
			if c.valid && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !c.valid && err == nil {
				t.Errorf("expected invalid, got nil")
			}
		})
	}
}

func TestReplaceFromTarHappyPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	tarball := buildTar(t, map[string]string{
		"index.html":    "<h1>hi</h1>",
		"about.html":    "<p>about</p>",
		"sub/page.html": "<p>nested</p>",
		"assets/x.css":  "body{}",
	})

	site, err := store.ReplaceFromTar("mysite", bytes.NewReader(tarball))
	if err != nil {
		t.Fatalf("ReplaceFromTar: %v", err)
	}
	if site.Name != "mysite" {
		t.Errorf("name: got %q", site.Name)
	}
	if site.FileCount != 4 {
		t.Errorf("file_count: got %d, want 4", site.FileCount)
	}

	want := map[string]string{
		"index.html":    "<h1>hi</h1>",
		"about.html":    "<p>about</p>",
		"sub/page.html": "<p>nested</p>",
		"assets/x.css":  "body{}",
	}
	for path, content := range want {
		got, err := os.ReadFile(filepath.Join(root, "mysite", path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(got) != content {
			t.Errorf("%s: got %q want %q", path, got, content)
		}
	}
}

func TestReplaceFromTarOverwritesAtomically(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	// First push.
	v1 := buildTar(t, map[string]string{"index.html": "v1", "stays.html": "v1"})
	if _, err := store.ReplaceFromTar("foo", bytes.NewReader(v1)); err != nil {
		t.Fatalf("v1 push: %v", err)
	}

	// Second push with a different file layout.
	v2 := buildTar(t, map[string]string{"index.html": "v2", "new.html": "v2"})
	if _, err := store.ReplaceFromTar("foo", bytes.NewReader(v2)); err != nil {
		t.Fatalf("v2 push: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "foo", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Errorf("index.html: got %q want v2", got)
	}
	if _, err := os.Stat(filepath.Join(root, "foo", "stays.html")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("stays.html should have been replaced away")
	}
	if _, err := os.Stat(filepath.Join(root, "foo", "new.html")); err != nil {
		t.Errorf("new.html missing: %v", err)
	}

	// Make sure no stage/old dirs leaked.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leaked transient dir: %s", e.Name())
		}
	}
}

func TestReplaceFromTarRejectsTraversal(t *testing.T) {
	t.Parallel()
	cases := []string{
		"../escape.html",
		"sub/../../escape.html",
		"/abs/path.html",
	}
	for _, badPath := range cases {
		badPath := badPath
		t.Run(badPath, func(t *testing.T) {
			root := t.TempDir()
			store := storage.New(root)
			tarball := buildTar(t, map[string]string{badPath: "x"})
			_, err := store.ReplaceFromTar("foo", bytes.NewReader(tarball))
			if !errors.Is(err, storage.ErrUnsafePath) {
				t.Errorf("got %v, want ErrUnsafePath", err)
			}
			// Site should not exist after failed extract.
			if _, err := os.Stat(filepath.Join(root, "foo")); !errors.Is(err, fs.ErrNotExist) {
				t.Error("foo should not exist after rejected push")
			}
		})
	}
}

func TestReplaceFromTarStripsSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteHeader(t, tw, &tar.Header{Name: "index.html", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	mustWriteHeader(t, tw, &tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ReplaceFromTar("foo", &buf); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "foo", "link")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("symlink should have been stripped")
	}
	if _, err := os.Stat(filepath.Join(root, "foo", "index.html")); err != nil {
		t.Errorf("index.html missing: %v", err)
	}
}

func TestReplaceFromTarRejectsBadName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)
	_, err := store.ReplaceFromTar("Bad/Name", bytes.NewReader([]byte{}))
	if !errors.Is(err, storage.ErrInvalidName) {
		t.Errorf("got %v, want ErrInvalidName", err)
	}
}

// TestReplaceFromTarAcceptsLeadingDots ensures the traversal guard doesn't
// over-reject filenames that *start* with two dots (e.g. "..foo"). filepath.Clean
// leaves those alone; only "..", "../...", and "/..." should be rejected.
func TestReplaceFromTarAcceptsLeadingDots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	tarball := buildTar(t, map[string]string{
		"index.html":      "<h1>ok</h1>",
		"..weirdname.txt": "fine",
	})
	if _, err := store.ReplaceFromTar("dots", bytes.NewReader(tarball)); err != nil {
		t.Fatalf("ReplaceFromTar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dots", "..weirdname.txt")); err != nil {
		t.Errorf("..weirdname.txt should have been extracted: %v", err)
	}
}

func TestListStatDeleteExists(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	// Empty list.
	sites, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 0 {
		t.Errorf("empty list: got %d sites", len(sites))
	}

	// Push two.
	for _, name := range []string{"alpha", "beta"} {
		data := buildTar(t, map[string]string{"index.html": name})
		if _, err := store.ReplaceFromTar(name, bytes.NewReader(data)); err != nil {
			t.Fatalf("push %s: %v", name, err)
		}
	}

	sites, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || sites[0].Name != "alpha" || sites[1].Name != "beta" {
		t.Errorf("list: got %+v", sites)
	}

	// Exists / Stat / Path.
	ok, err := store.Exists("alpha")
	if err != nil || !ok {
		t.Errorf("Exists(alpha): %v / %v", ok, err)
	}
	ok, err = store.Exists("missing")
	if err != nil || ok {
		t.Errorf("Exists(missing): %v / %v", ok, err)
	}
	if _, err := store.Stat("missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(missing): want ErrNotFound, got %v", err)
	}
	p, err := store.Path("alpha")
	if err != nil || p != filepath.Join(root, "alpha") {
		t.Errorf("Path(alpha): got %q / %v", p, err)
	}

	// Delete.
	if err := store.Delete("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("alpha"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Delete twice: want ErrNotFound, got %v", err)
	}
	sites, _ = store.List()
	if len(sites) != 1 || sites[0].Name != "beta" {
		t.Errorf("after delete: got %+v", sites)
	}
}

func TestExpiryMetadataAndCleanup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)
	data := buildTar(t, map[string]string{"index.html": "hello"})
	if _, err := store.ReplaceFromTar("temporary", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(-time.Minute).UTC()
	if err := store.SetExpiry("temporary", &expiresAt); err != nil {
		t.Fatal(err)
	}

	site, err := store.Stat("temporary")
	if err != nil {
		t.Fatal(err)
	}
	if site.ExpiresAt == nil || !site.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expiry: got %v, want %v", site.ExpiresAt, expiresAt)
	}
	if _, err := os.Stat(filepath.Join(root, "temporary", ".crate-expiry")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("expiry metadata must not be stored in the served site")
	}

	deleted, err := store.DeleteExpired(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "temporary" {
		t.Fatalf("deleted: got %v", deleted)
	}
	if ok, _ := store.Exists("temporary"); ok {
		t.Error("expired site still exists")
	}
}

func TestNeverExpiryIsNotCleanedUp(t *testing.T) {
	t.Parallel()
	store := storage.New(t.TempDir())
	data := buildTar(t, map[string]string{"index.html": "hello"})
	if _, err := store.ReplaceFromTar("permanent", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpiry("permanent", nil); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteExpired(time.Now().Add(100 * 365 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("permanent site was deleted: %v", deleted)
	}
}

func TestWriteDirAsTarRoundTrip(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	// Build a small tree on disk.
	files := map[string]string{
		"index.html":    "hello",
		"sub/inner.txt": "world",
	}
	for path, content := range files {
		full := filepath.Join(src, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := storage.WriteDirAsTar(src, &buf); err != nil {
		t.Fatal(err)
	}

	// Extract through ReplaceFromTar — verifies symmetry with the write side.
	root := t.TempDir()
	store := storage.New(root)
	if _, err := store.ReplaceFromTar("roundtrip", &buf); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		got, err := os.ReadFile(filepath.Join(root, "roundtrip", path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(got) != content {
			t.Errorf("%s: got %q want %q", path, got, content)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		mustWriteHeader(t, tw, hdr)
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustWriteHeader(t *testing.T, tw *tar.Writer, hdr *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
}
