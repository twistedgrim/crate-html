package server_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
)

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestIndexRendersMetadata(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "docs", map[string]string{"index.html": "hi", "a.html": "a"})
	future := time.Now().Add(3 * time.Hour)
	if err := store.SetExpiry("docs", &future); err != nil {
		t.Fatal(err)
	}

	_, body := getBody(t, ts.URL+"/")
	for _, want := range []string{">docs<", "2 files", "updated", "expires in"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q\n%s", want, body)
		}
	}
}

func TestIndexGroupsDottedSites(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "myproject.docs", map[string]string{"index.html": "d"})
	pushFixture(t, store, "myproject.plan", map[string]string{"index.html": "p"})
	pushFixture(t, store, "loose", map[string]string{"index.html": "l"})

	_, body := getBody(t, ts.URL+"/")
	if !strings.Contains(body, `href="/myproject/"`) {
		t.Errorf("index missing group header link\n%s", body)
	}
	for _, want := range []string{">docs<", ">plan<", ">loose<"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	// "loose" has no dot -> bare row, not promoted to a header.
	if strings.Contains(body, `href="/loose/"><`) {
		t.Error("undotted site should not render a group header")
	}
}

func TestSyntheticGroupIndex(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "proj.docs", map[string]string{"index.html": "d"})
	pushFixture(t, store, "proj.plan", map[string]string{"index.html": "p"})

	status, body := getBody(t, ts.URL+"/proj/")
	if status != 200 {
		t.Fatalf("GET /proj/: %d", status)
	}
	for _, want := range []string{">docs<", ">plan<", "proj"} {
		if !strings.Contains(body, want) {
			t.Errorf("group index missing %q\n%s", want, body)
		}
	}
}

func TestSyntheticGroupRedirectsTrailingSlash(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "proj.docs", map[string]string{"index.html": "d"})

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/proj")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("got %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/proj/" {
		t.Errorf("Location: %q", got)
	}
}

func TestExactSiteWinsOverSyntheticGroup(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "proj", map[string]string{"index.html": "REAL"})
	pushFixture(t, store, "proj.docs", map[string]string{"index.html": "d"})

	status, body := getBody(t, ts.URL+"/proj/")
	if status != 200 {
		t.Fatalf("GET /proj/: %d", status)
	}
	if body != "REAL" {
		t.Errorf("exact site should win: got %q", body)
	}
}

func TestUnknownGroupPrefixIs404(t *testing.T) {
	ts, store := newTestServer(t, nil)
	pushFixture(t, store, "proj.docs", map[string]string{"index.html": "d"})

	if status, _ := getBody(t, ts.URL+"/nope/"); status != 404 {
		t.Errorf("GET /nope/: got %d, want 404", status)
	}
	// A non-prefix path under an existing group prefix still 404s (no child).
	if status, _ := getBody(t, ts.URL+"/proj/missing.html"); status != 404 {
		t.Errorf("GET /proj/missing.html: got %d, want 404", status)
	}
}

// newServerWithConfig builds a Server directly so a test can exercise
// UseIndexTemplateFile before the handler is mounted.
func newServerWithConfig(t *testing.T) (*server.Server, *storage.Store) {
	t.Helper()
	store := storage.New(t.TempDir())
	cfg := config.Config{ListenAddr: "127.0.0.1:0", Token: testToken}
	return server.New(cfg, store, nil, nil, log.New(io.Discard, "", 0)), store
}

func TestCustomIndexTemplate(t *testing.T) {
	srv, store := newServerWithConfig(t)
	tmpl := filepath.Join(t.TempDir(), "index.tmpl")
	if err := os.WriteFile(tmpl, []byte(`CUSTOM {{.Title}} {{range .Groups}}{{range .Rows}}[{{.Name}}]{{end}}{{end}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.UseIndexTemplateFile(tmpl); err != nil {
		t.Fatalf("UseIndexTemplateFile: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Seed after wiring; the template reads live data.
	pushFixture(t, store, "alpha", map[string]string{"index.html": "x"})
	_, body := getBody(t, ts.URL+"/")
	if !strings.HasPrefix(body, "CUSTOM crate") || !strings.Contains(body, "[alpha]") {
		t.Errorf("custom template not used: %q", body)
	}
}

func TestCustomIndexTemplateExecFailureIs500(t *testing.T) {
	srv, _ := newServerWithConfig(t)
	tmpl := filepath.Join(t.TempDir(), "index.tmpl")
	// Parses fine, fails at execution (indexView has no Missing field).
	if err := os.WriteFile(tmpl, []byte(`{{.Missing.Field}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.UseIndexTemplateFile(tmpl); err != nil {
		t.Fatalf("UseIndexTemplateFile: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if status, _ := getBody(t, ts.URL+"/"); status != 500 {
		t.Errorf("exec-failing template: got %d, want 500", status)
	}
}

func TestCustomIndexTemplateErrors(t *testing.T) {
	srv, _ := newServerWithConfig(t)
	if err := srv.UseIndexTemplateFile(filepath.Join(t.TempDir(), "missing.tmpl")); err == nil {
		t.Error("expected error for missing template file")
	}
	bad := filepath.Join(t.TempDir(), "bad.tmpl")
	if err := os.WriteFile(bad, []byte(`{{ .Unclosed `), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.UseIndexTemplateFile(bad); err == nil {
		t.Error("expected parse error for malformed template")
	}
}
