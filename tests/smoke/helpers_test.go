//go:build smoke

package smoke

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCrate exec's the suite's `crate` binary with the suite's XDG env and
// returns combined output. Use runCrateExpect for assertions on exit code.
func runCrate(t *testing.T, args ...string) (string, error) {
	return runCrateWithEnv(t, nil, args...)
}

// runCrateWithEnv runs crate with the suite's XDG environment plus overrides.
// Extra variables are appended last so they take precedence over any inherited
// environment, matching how an agent supplies CRATE_TOKEN or CRATE_BASE_URL.
func runCrateWithEnv(t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(crateBin, args...)
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(xdgHome, "config"),
		"XDG_DATA_HOME="+filepath.Join(xdgHome, "data"),
		"XDG_STATE_HOME="+filepath.Join(xdgHome, "state"),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runCrateOK runs `crate <args>` and fails the test if it exits non-zero.
func runCrateOK(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runCrate(t, args...)
	if err != nil {
		t.Fatalf("crate %s: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runCrateStdin runs `crate <args>` with the given stdin bytes.
func runCrateStdin(t *testing.T, stdin []byte, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(crateBin, args...)
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(xdgHome, "config"),
		"XDG_DATA_HOME="+filepath.Join(xdgHome, "data"),
		"XDG_STATE_HOME="+filepath.Join(xdgHome, "state"),
	)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// httpStatus issues a GET and returns just the status code.
func httpStatus(t *testing.T, path string) int {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// httpGet returns status + body.
func httpGet(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// noRedirectClient is an http.Client that does not follow redirects, so
// tests can assert on 3xx responses directly.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// httpReq sends a custom request (any method) with an optional Authorization
// header. Does not follow redirects. Use for testing auth-protected endpoints
// and asserting redirect behavior.
func httpReq(t *testing.T, method, path string, body io.Reader, bearer string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// writeFiles materializes a map of relative-path → contents into a fresh
// tempdir and returns the dir. Cleanup is registered via t.Cleanup.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// tarFromMap returns a tar stream of the given files (no directory entries).
// Useful for piping through `crate push -`.
func tarFromMap(t *testing.T, files map[string]string) []byte {
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
	return buf.Bytes()
}

// rmSite issues a `crate rm` for cleanup; failure is logged but not fatal so
// teardown of partially-failed tests still completes.
func rmSite(t *testing.T, name string) {
	t.Helper()
	if out, err := runCrate(t, "rm", name); err != nil {
		t.Logf("cleanup rm %s failed: %v\n%s", name, err, out)
	}
}
