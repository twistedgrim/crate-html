//go:build smoke

package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPushStdin pushes a tar stream via `crate push - <name>`. This is the
// canonical agent-on-Docker-host flow.
func TestPushStdin(t *testing.T) {
	t.Parallel()
	const name = "push-stdin"
	t.Cleanup(func() { rmSite(t, name) })

	tarball := tarFromMap(t, map[string]string{
		"index.html": "<h1>stdin push</h1>",
	})
	out, err := runCrateStdin(t, tarball, "push", "-", name)
	if err != nil {
		t.Fatalf("push -: %v\n%s", err, out)
	}
	if !strings.Contains(out, baseURL+"/"+name+"/") {
		t.Errorf("output missing URL: %s", out)
	}

	code, body := httpGet(t, "/"+name+"/")
	if code != 200 {
		t.Fatalf("GET /%s/: %d", name, code)
	}
	if !strings.Contains(body, "stdin push") {
		t.Errorf("body wrong: %s", body)
	}
}

// TestPushTarFile pushes from a pre-built tar archive on disk.
func TestPushTarFile(t *testing.T) {
	t.Parallel()
	const name = "push-tarfile"
	t.Cleanup(func() { rmSite(t, name) })

	tarball := tarFromMap(t, map[string]string{
		"index.html": "<h1>tar file</h1>",
	})
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "site.tar")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runCrateOK(t, "push", tarPath, name)
	if !strings.Contains(out, baseURL+"/"+name+"/") {
		t.Errorf("output missing URL: %s", out)
	}

	_, body := httpGet(t, "/"+name+"/")
	if !strings.Contains(body, "tar file") {
		t.Errorf("body wrong: %s", body)
	}
}

// TestPushOpenFlag covers `crate push --open`. We can't verify the browser
// actually opened in CI, but exit-0 means openBrowser returned without
// error (which on macOS means `open` was invoked successfully).
func TestPushOpenFlag(t *testing.T) {
	t.Parallel()
	const name = "push-open"
	t.Cleanup(func() { rmSite(t, name) })

	dir := writeFiles(t, map[string]string{"index.html": "x"})

	// Use a fake BROWSER to ensure --open uses our shim, not actually pop a
	// browser. The openBrowser helper exec.Starts the OS-specific opener;
	// we can't easily intercept that, so this test merely asserts the push
	// itself didn't fail when --open was set. The unit test for openBrowser
	// will cover the shim selection.
	out, err := runCrate(t, "push", "--open", dir, name)
	if err != nil {
		t.Fatalf("push --open: %v\n%s", err, out)
	}
}

// TestPushNameValidation confirms the CLI surfaces server-side name validation.
func TestPushNameValidation(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"index.html": "x"})
	out, err := runCrate(t, "push", dir, "Bad/Name")
	if err == nil {
		t.Errorf("expected non-zero exit for invalid name; got success\n%s", out)
	}
}

// TestPushNonexistentSourceErrors covers the user error case of pointing at
// a path that doesn't exist.
func TestPushNonexistentSourceErrors(t *testing.T) {
	t.Parallel()
	out, err := runCrate(t, "push", "/nonexistent/path", "doesnt-matter")
	if err == nil {
		t.Errorf("expected non-zero exit; got success\n%s", out)
	}
}
