package storage_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Twistedgrim/crate-html/internal/storage"
)

func tarWithFile(t *testing.T, name string, size int) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := strings.Repeat("x", size)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestMaxSiteBytesRejectsOversizedExtraction(t *testing.T) {
	store := storage.New(t.TempDir())
	store.SetMaxSiteBytes(100)

	_, err := store.ReplaceFromTar("big", tarWithFile(t, "index.html", 200))
	if !errors.Is(err, storage.ErrSiteTooLarge) {
		t.Fatalf("got %v, want ErrSiteTooLarge", err)
	}
	if exists, _ := store.Exists("big"); exists {
		t.Error("oversized extraction left a site behind")
	}
}

func TestMaxSiteBytesKeepsExistingSiteOnOversizedReplace(t *testing.T) {
	store := storage.New(t.TempDir())
	store.SetMaxSiteBytes(100)

	if _, err := store.ReplaceFromTar("keep", tarWithFile(t, "index.html", 50)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceFromTar("keep", tarWithFile(t, "index.html", 500)); !errors.Is(err, storage.ErrSiteTooLarge) {
		t.Fatalf("got %v, want ErrSiteTooLarge", err)
	}
	site, err := store.Stat("keep")
	if err != nil {
		t.Fatal(err)
	}
	if site.SizeBytes != 50 {
		t.Errorf("existing site size after failed replace: %d, want 50", site.SizeBytes)
	}
}

func TestMaxSiteBytesZeroMeansUnlimited(t *testing.T) {
	store := storage.New(t.TempDir())
	if _, err := store.ReplaceFromTar("free", tarWithFile(t, "index.html", 4096)); err != nil {
		t.Fatal(err)
	}
}

func TestMaxSiteBytesAppliesAcrossMultipleFiles(t *testing.T) {
	store := storage.New(t.TempDir())
	store.SetMaxSiteBytes(100)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"a.html", "b.html", "c.html"} {
		content := strings.Repeat("y", 40)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 40, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceFromTar("multi", &buf); !errors.Is(err, storage.ErrSiteTooLarge) {
		t.Fatalf("got %v, want ErrSiteTooLarge (3×40 bytes > 100-byte cap)", err)
	}
}
