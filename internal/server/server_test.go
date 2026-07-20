package server_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Twistedgrim/crate-html/internal/builtin"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

const testToken = "server-test-token"

// newTestServer wires a Server with isolated storage and optional builtins
// behind an httptest.NewServer. Returns the httptest URL so tests can dial it.
func newTestServer(t *testing.T, builtins []builtin.Site) (*httptest.Server, *storage.Store) {
	ts, store, _ := newTestServerWithTokens(t, builtins)
	return ts, store
}

// newTestServerWithTokens additionally exposes the token store for tests
// that exercise /api/tokens and minted-token auth.
func newTestServerWithTokens(t *testing.T, builtins []builtin.Site) (*httptest.Server, *storage.Store, *token.Store) {
	t.Helper()
	root := t.TempDir()
	store := storage.New(root)
	store.SetMaxSiteBytes(1 << 20)
	tokens, err := token.Load(filepath.Join(t.TempDir(), "tokens.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	cfg := config.Config{
		BaseURL:        "", // populated below once the httptest URL is known
		ListenAddr:     "127.0.0.1:0",
		Port:           0,
		Token:          testToken,
		MaxUploadBytes: 1 << 20, // small cap so upload-limit tests stay cheap
	}
	srv := server.New(cfg, store, tokens, builtins, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store, tokens
}

// pushFixture pushes a tiny site to disk via the storage layer so we can
// test the serve side without going through the HTTP push path.
func pushFixture(t *testing.T, store *storage.Store, name string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, content := range files {
		hdr := &tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceFromTar(name, &buf); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

// --- public endpoints ------------------------------------------------------

func TestStatusIsPublic(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + wire.PathAPIStatus)
	if resp.StatusCode != 200 {
		t.Errorf("GET %s: %d, want 200", wire.PathAPIStatus, resp.StatusCode)
	}
}

func TestIndexShowsBothDiskAndBuiltinSites(t *testing.T) {
	builtins := []builtin.Site{
		{Name: "embedded-foo", FS: oneFileFS("index.html", "embedded")},
	}
	ts, store := newTestServer(t, builtins)
	pushFixture(t, store, "disk-bar", map[string]string{"index.html": "ok"})

	resp, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s := string(body)
	if !strings.Contains(s, "disk-bar") {
		t.Error("/ missing disk site")
	}
	if !strings.Contains(s, "embedded-foo") {
		t.Error("/ missing builtin site")
	}
	if !strings.Contains(s, "built-in") {
		t.Error("/ missing built-in tag")
	}
}

func TestDiskShadowsBuiltinOnIndex(t *testing.T) {
	builtins := []builtin.Site{
		{Name: "shadowed", FS: oneFileFS("index.html", "EMBEDDED")},
	}
	ts, store := newTestServer(t, builtins)
	pushFixture(t, store, "shadowed", map[string]string{"index.html": "DISK"})

	resp, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if count := strings.Count(string(body), ">shadowed<"); count != 1 {
		t.Errorf("expected exactly one 'shadowed' entry, got %d", count)
	}
	if strings.Contains(string(body), "EMBEDDED") {
		t.Error("built-in copy should not appear on index when shadowed")
	}
}

func TestServeDiskSite(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "site", map[string]string{"index.html": "ix", "about.html": "ab"})

	for path, want := range map[string]string{
		"/site/":           "ix",
		"/site/about.html": "ab",
	} {
		resp, _ := http.Get(ts.URL + path)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != want {
			t.Errorf("%s: got %q, want %q", path, body, want)
		}
	}
}

func TestServeBuiltinFallsThroughWhenNoDiskSite(t *testing.T) {
	builtins := []builtin.Site{
		{Name: "fromembed", FS: oneFileFS("index.html", "EMBED_BODY")},
	}
	ts, _ := newTestServer(t, builtins)

	resp, _ := http.Get(ts.URL + "/fromembed/")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "EMBED_BODY" {
		t.Errorf("body: %q", body)
	}
}

func TestServeMissingSiteIs404(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + "/does-not-exist/")
	if resp.StatusCode != 404 {
		t.Errorf("got %d, want 404", resp.StatusCode)
	}
}

func TestRedirectMissingTrailingSlash(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "ts", map[string]string{"index.html": "x"})

	c := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(ts.URL + "/ts")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("got %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/ts/" {
		t.Errorf("Location: %q", got)
	}
}

func TestInvalidSiteNameInURLIs404(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + "/UPPER/")
	if resp.StatusCode != 404 {
		t.Errorf("got %d, want 404", resp.StatusCode)
	}
}

// --- auth ------------------------------------------------------------------

func TestApiSitesRequiresBearer(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, c := range []struct {
		method string
		path   string
		want   int
	}{
		{"GET", wire.PathAPISites, 401},
		{"PUT", wire.PathAPISites + "/foo", 401},
		{"DELETE", wire.PathAPISites + "/foo", 401},
	} {
		c := c
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
			resp, _ := http.DefaultClient.Do(req)
			if resp.StatusCode != c.want {
				t.Errorf("got %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

func TestApiSitesWrongTokenIs401(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	req, _ := http.NewRequest("GET", ts.URL+wire.PathAPISites, nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("got %d, want 401", resp.StatusCode)
	}
}

func TestApiSitesCorrectTokenAcceptsList(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "alpha", map[string]string{"index.html": "x"})

	req, _ := http.NewRequest("GET", ts.URL+wire.PathAPISites, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var out wire.ListSitesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sites) != 1 || out.Sites[0].Name != "alpha" {
		t.Errorf("got %+v", out.Sites)
	}
}

// --- push ------------------------------------------------------------------

func TestPutSiteHappyPath(t *testing.T) {
	ts, store := newTestServer(t, nil)

	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("PUT", ts.URL+wire.PathAPISites+"/pushed", &tarbuf)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	exists, _ := store.Exists("pushed")
	if !exists {
		t.Error("site not on disk after PUT")
	}
}

func TestPutSiteExpiryPolicies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantExpiry bool
		minTTL     time.Duration
		maxTTL     time.Duration
	}{
		{name: "default", wantExpiry: true, minTTL: 23*time.Hour + 59*time.Minute, maxTTL: 24*time.Hour + time.Minute},
		{name: "custom", header: "90m", wantExpiry: true, minTTL: 89 * time.Minute, maxTTL: 91 * time.Minute},
		{name: "never", header: "never", wantExpiry: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store := newTestServer(t, nil)
			var tarbuf bytes.Buffer
			tw := tar.NewWriter(&tarbuf)
			_ = tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2})
			_, _ = tw.Write([]byte("ok"))
			_ = tw.Close()
			req, _ := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/expiry", &tarbuf)
			req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
			if tc.header != "" {
				req.Header.Set(wire.HeaderExpires, tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			site, err := store.Stat("expiry")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantExpiry {
				if site.ExpiresAt != nil {
					t.Fatalf("expires_at: got %v, want nil", site.ExpiresAt)
				}
				return
			}
			if site.ExpiresAt == nil {
				t.Fatal("expires_at is nil")
			}
			ttl := time.Until(*site.ExpiresAt)
			if ttl < tc.minTTL || ttl > tc.maxTTL {
				t.Fatalf("ttl %v outside [%v, %v]", ttl, tc.minTTL, tc.maxTTL)
			}
		})
	}
}

func TestPutSiteDoesNotReplaceOnExpiryFailure(t *testing.T) {
	ts, store := newTestServer(t, nil)
	var oldTar bytes.Buffer
	oldWriter := tar.NewWriter(&oldTar)
	_ = oldWriter.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 3})
	_, _ = oldWriter.Write([]byte("old"))
	_ = oldWriter.Close()
	oldReq, _ := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/atomic", &oldTar)
	oldReq.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	oldResp, err := http.DefaultClient.Do(oldReq)
	if err != nil {
		t.Fatal(err)
	}
	defer oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusOK {
		t.Fatalf("initial put status: got %d, want %d", oldResp.StatusCode, http.StatusOK)
	}
	expiryRoot := filepath.Join(store.Root(), ".expiries")
	if err := os.RemoveAll(expiryRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expiryRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	_ = tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("new"))
	_ = tw.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/atomic", &tarbuf)
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}

	b, err := os.ReadFile(filepath.Join(store.Root(), "atomic", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "old" {
		t.Fatalf("site content: got %q, want %q", got, "old")
	}
}

func TestPutSiteRejectsInvalidExpiryBeforeUpload(t *testing.T) {
	ts, store := newTestServer(t, nil)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/invalid-expiry", bytes.NewReader(nil))
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	req.Header.Set(wire.HeaderExpires, "tomorrow")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if exists, _ := store.Exists("invalid-expiry"); exists {
		t.Error("invalid expiry created a site")
	}
}

func TestPutSiteInvalidNameIs400(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	req, _ := http.NewRequest("PUT", ts.URL+wire.PathAPISites+"/Bad-Name-CAPS", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("got %d, want 400", resp.StatusCode)
	}
}

// --- helpers ---------------------------------------------------------------

// oneFileFS returns a fs.FS containing a single regular file.
func oneFileFS(name, content string) fs.FS {
	return fstest.MapFS{
		name: &fstest.MapFile{Data: []byte(content), Mode: 0o644},
	}
}

// (kept around to silence "imported and not used" if the test file ever
// drops the os/fs.FS use during refactors)
var _ = os.O_RDONLY
