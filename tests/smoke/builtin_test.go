//go:build smoke

package smoke

import (
	"strings"
	"testing"
)

func TestBuiltinCratesplainerServesIndex(t *testing.T) {
	code, body := httpGet(t, "/cratesplainer/")
	if code != 200 {
		t.Fatalf("GET /cratesplainer/: got %d, want 200", code)
	}
	if !strings.Contains(body, "cratesplainer") {
		t.Errorf("body missing 'cratesplainer': %s", body[:min(200, len(body))])
	}
}

func TestBuiltinCratesplainerServesAssets(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/cratesplainer/style.css",
		"/cratesplainer/commands.html",
		"/cratesplainer/gotchas.html",
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if code := httpStatus(t, path); code != 200 {
				t.Errorf("got %d, want 200", code)
			}
		})
	}
}

func TestBuiltinCratesplainerListedOnIndex(t *testing.T) {
	_, body := httpGet(t, "/")
	if !strings.Contains(body, ">cratesplainer<") {
		t.Errorf("/ index missing cratesplainer entry")
	}
	if !strings.Contains(body, "built-in") {
		t.Errorf("/ index missing built-in tag")
	}
}

// TestDiskShadowsBuiltin confirms a disk site of the same name as a builtin
// takes precedence, and removing the disk copy resurfaces the builtin.
func TestDiskShadowsBuiltin(t *testing.T) {
	dir := writeFiles(t, map[string]string{"index.html": "DISK-OVERRIDE"})
	runCrateOK(t, "push", dir, "cratesplainer")

	_, body := httpGet(t, "/cratesplainer/")
	if !strings.Contains(body, "DISK-OVERRIDE") {
		t.Errorf("disk site should shadow builtin; got %s", body[:min(200, len(body))])
	}

	runCrateOK(t, "rm", "cratesplainer")

	_, body = httpGet(t, "/cratesplainer/")
	if strings.Contains(body, "DISK-OVERRIDE") {
		t.Errorf("builtin should re-emerge after rm; got disk content")
	}
	if !strings.Contains(body, "cratesplainer") {
		t.Errorf("builtin body missing expected content")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
