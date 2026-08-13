package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	selfupdate "github.com/creativeprojects/go-selfupdate"
)

func TestUpdateSelectsClientAssetAndReplacesExecutable(t *testing.T) {
	const (
		archiveName = "crate_0.1.6_linux_amd64.tar.gz"
		oldBinary   = "old crate binary"
		newBinary   = "new crate binary"
	)
	archive := tarGzip(t, "crate", []byte(newBinary))
	sum := sha256.Sum256(archive)
	source := &fakeSource{
		releases: []selfupdate.SourceRelease{fakeRelease{
			tag: "v0.1.6",
			assets: []selfupdate.SourceAsset{
				fakeAsset{id: 1, name: "crated_0.1.6_linux_amd64.tar.gz"},
				fakeAsset{id: 2, name: archiveName, size: len(archive)},
				fakeAsset{id: 3, name: "checksums.txt"},
			},
		}},
		files: map[int64][]byte{
			1: []byte("daemon archive must not be selected"),
			2: archive,
			3: []byte(fmt.Sprintf("%x  %s\n", sum, archiveName)),
		},
	}
	target := filepath.Join(t.TempDir(), "crate")
	if err := os.WriteFile(target, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}

	client, err := New(Options{
		Source: source,
		OS:     "linux",
		Arch:   "amd64",
		ExecutablePath: func() (string, error) {
			return target, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Update(t.Context(), "v0.1.5")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.From != "0.1.5" || result.To != "0.1.6" {
		t.Fatalf("result = %#v", result)
	}
	if got := string(mustReadFile(t, target)); got != newBinary {
		t.Fatalf("updated executable = %q, want %q", got, newBinary)
	}
	if source.downloads[1] != 0 {
		t.Fatal("downloaded crated asset instead of crate asset")
	}
	if source.downloads[2] != 1 || source.downloads[3] != 1 {
		t.Fatalf("downloads = %#v, want archive and checksum once", source.downloads)
	}
	for _, temporary := range []string{".crate.new", ".crate.old"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(target), temporary)); !os.IsNotExist(err) {
			t.Fatalf("temporary file %s remains after update: %v", temporary, err)
		}
	}
}

func TestUpdateRejectsBadChecksumWithoutReplacingExecutable(t *testing.T) {
	const archiveName = "crate_0.1.6_linux_amd64.tar.gz"
	archive := tarGzip(t, "crate", []byte("new"))
	source := &fakeSource{
		releases: []selfupdate.SourceRelease{fakeRelease{
			tag: "v0.1.6",
			assets: []selfupdate.SourceAsset{
				fakeAsset{id: 1, name: archiveName, size: len(archive)},
				fakeAsset{id: 2, name: "checksums.txt"},
			},
		}},
		files: map[int64][]byte{
			1: archive,
			2: []byte(strings.Repeat("0", 64) + "  " + archiveName + "\n"),
		},
	}
	target := filepath.Join(t.TempDir(), "crate")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{
		Source: source,
		OS:     "linux",
		Arch:   "amd64",
		ExecutablePath: func() (string, error) {
			return target, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Update(t.Context(), "v0.1.5"); err == nil || !strings.Contains(err.Error(), "sha256 validation failed") {
		t.Fatalf("Update() error = %v, want checksum failure", err)
	}
	if got := string(mustReadFile(t, target)); got != "old" {
		t.Fatalf("executable changed after failed validation: %q", got)
	}
}

func TestUpdateDoesNotDowngradeOrDownload(t *testing.T) {
	source := &fakeSource{releases: []selfupdate.SourceRelease{fakeRelease{
		tag: "v0.1.5",
		assets: []selfupdate.SourceAsset{
			fakeAsset{id: 1, name: "crate_0.1.5_linux_amd64.tar.gz"},
			fakeAsset{id: 2, name: "checksums.txt"},
		},
	}}}
	client, err := New(Options{Source: source, OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Update(t.Context(), "v0.1.6")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated {
		t.Fatalf("result = %#v, want no downgrade", result)
	}
	if len(source.downloads) != 0 {
		t.Fatalf("downloads = %#v, want none", source.downloads)
	}
}

func TestUpdateRejectsDevelopmentBuildBeforeNetwork(t *testing.T) {
	source := &fakeSource{}
	client, err := New(Options{Source: source, OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Update(t.Context(), "0.1.0-dev"); err == nil || !strings.Contains(err.Error(), "development or prerelease") {
		t.Fatalf("Update() error = %v, want development-build error", err)
	}
	if source.listCalls != 0 {
		t.Fatalf("ListReleases called %d times", source.listCalls)
	}
}

func TestAssetPattern(t *testing.T) {
	platforms := []struct {
		goos   string
		goarch string
	}{
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
	}

	for _, platform := range platforms {
		name := platform.goos + "/" + platform.goarch
		t.Run(name, func(t *testing.T) {
			pattern := regexpForTest(t, assetPattern(platform.goos, platform.goarch))
			for _, version := range []string{"0.1.5", "12.34.56"} {
				archive := fmt.Sprintf("crate_%s_%s_%s.tar.gz", version, platform.goos, platform.goarch)
				if !pattern.MatchString(archive) {
					t.Errorf("pattern does not match %q", archive)
				}
			}

			for _, archive := range []string{
				fmt.Sprintf("crated_0.1.5_%s_%s.tar.gz", platform.goos, platform.goarch),
				fmt.Sprintf("crate_0.1.5_other_%s.tar.gz", platform.goarch),
				fmt.Sprintf("crate_0.1.5_%s_other.tar.gz", platform.goos),
				fmt.Sprintf("crate_0.1.5_%s_%s.zip", platform.goos, platform.goarch),
			} {
				if pattern.MatchString(archive) {
					t.Errorf("pattern unexpectedly matches %q", archive)
				}
			}
		})
	}
}

type fakeSource struct {
	releases  []selfupdate.SourceRelease
	files     map[int64][]byte
	downloads map[int64]int
	listCalls int
}

func (s *fakeSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	s.listCalls++
	return s.releases, nil
}

func (s *fakeSource) DownloadReleaseAsset(_ context.Context, _ *selfupdate.Release, assetID int64) (io.ReadCloser, error) {
	if s.downloads == nil {
		s.downloads = make(map[int64]int)
	}
	s.downloads[assetID]++
	data, ok := s.files[assetID]
	if !ok {
		return nil, selfupdate.ErrAssetNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type fakeRelease struct {
	tag    string
	assets []selfupdate.SourceAsset
}

func (r fakeRelease) GetID() int64                        { return 1 }
func (r fakeRelease) GetTagName() string                  { return r.tag }
func (r fakeRelease) GetDraft() bool                      { return false }
func (r fakeRelease) GetPrerelease() bool                 { return false }
func (r fakeRelease) GetPublishedAt() time.Time           { return time.Time{} }
func (r fakeRelease) GetReleaseNotes() string             { return "release notes" }
func (r fakeRelease) GetName() string                     { return r.tag }
func (r fakeRelease) GetURL() string                      { return "https://example.test/releases/" + r.tag }
func (r fakeRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

type fakeAsset struct {
	id   int64
	name string
	size int
}

func (a fakeAsset) GetID() int64                  { return a.id }
func (a fakeAsset) GetName() string               { return a.name }
func (a fakeAsset) GetSize() int                  { return a.size }
func (a fakeAsset) GetBrowserDownloadURL() string { return "https://example.test/" + a.name }

func tarGzip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func regexpForTest(t *testing.T, expression string) *regexp.Regexp {
	t.Helper()
	pattern, err := regexp.Compile(expression)
	if err != nil {
		t.Fatal(err)
	}
	return pattern
}
