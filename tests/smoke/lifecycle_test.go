//go:build smoke

package smoke

import (
	"strings"
	"testing"
)

func TestStatus(t *testing.T) {
	if code := httpStatus(t, "/api/status"); code != 200 {
		t.Errorf("GET /api/status: got %d, want 200", code)
	}
	out := runCrateOK(t, "status")
	if !strings.Contains(out, baseURL) {
		t.Errorf("crate status missing base URL %q in output: %s", baseURL, out)
	}
}

func TestPushFromDir(t *testing.T) {
	t.Parallel()
	const name = "lifecycle-dir"
	dir := writeFiles(t, map[string]string{
		"index.html":    "<h1>hello from dir</h1>",
		"sub/page.html": "<p>nested</p>",
	})
	t.Cleanup(func() { rmSite(t, name) })

	out := runCrateOK(t, "push", dir, name)
	if !strings.Contains(out, baseURL+"/"+name+"/") {
		t.Errorf("push output missing URL: %s", out)
	}

	code, body := httpGet(t, "/"+name+"/")
	if code != 200 {
		t.Errorf("GET /%s/: got %d, want 200", name, code)
	}
	if !strings.Contains(body, "hello from dir") {
		t.Errorf("body missing expected content: %s", body)
	}

	if code := httpStatus(t, "/"+name+"/sub/page.html"); code != 200 {
		t.Errorf("nested page: got %d, want 200", code)
	}
}

func TestPushReplacesAtomically(t *testing.T) {
	t.Parallel()
	const name = "lifecycle-replace"
	t.Cleanup(func() { rmSite(t, name) })

	d1 := writeFiles(t, map[string]string{"index.html": "v1", "old.html": "stays"})
	runCrateOK(t, "push", d1, name)

	d2 := writeFiles(t, map[string]string{"index.html": "v2", "new.html": "added"})
	runCrateOK(t, "push", d2, name)

	if _, body := httpGet(t, "/"+name+"/"); body != "v2" {
		t.Errorf("expected v2, got %q", body)
	}
	if code := httpStatus(t, "/"+name+"/new.html"); code != 200 {
		t.Errorf("new.html should be reachable: got %d", code)
	}
	if code := httpStatus(t, "/"+name+"/old.html"); code != 404 {
		t.Errorf("old.html should be gone: got %d", code)
	}
}

func TestLsAndRm(t *testing.T) {
	const name = "lifecycle-ls-rm"
	dir := writeFiles(t, map[string]string{"index.html": "x"})
	runCrateOK(t, "push", dir, name)

	out := runCrateOK(t, "ls")
	if !strings.Contains(out, name) {
		t.Errorf("ls missing %q: %s", name, out)
	}

	runCrateOK(t, "rm", name)

	out = runCrateOK(t, "ls")
	if strings.Contains(out, name) {
		t.Errorf("ls still shows %q after rm: %s", name, out)
	}

	if code := httpStatus(t, "/"+name+"/"); code != 404 {
		t.Errorf("GET after rm: got %d, want 404", code)
	}
}

func TestIndexPage(t *testing.T) {
	code, body := httpGet(t, "/")
	if code != 200 {
		t.Fatalf("GET /: got %d", code)
	}
	if !strings.Contains(body, "<title>crate</title>") {
		t.Errorf("/ missing expected title")
	}
}

func TestRedirectMissingTrailingSlash(t *testing.T) {
	t.Parallel()
	const name = "lifecycle-redirect"
	dir := writeFiles(t, map[string]string{"index.html": "x"})
	runCrateOK(t, "push", dir, name)
	t.Cleanup(func() { rmSite(t, name) })

	// Don't auto-follow; we want to assert the 302.
	resp, _ := httpReq(t, "GET", "/"+name, nil, "")
	if resp.StatusCode != 302 {
		t.Errorf("got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/"+name+"/" {
		t.Errorf("redirect to %q, want /%s/", loc, name)
	}
}
