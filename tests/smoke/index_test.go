//go:build smoke

package smoke

import (
	"strings"
	"testing"
)

// TestGroupedIndexViaCLI exercises the namespaced-index flow through the
// released surfaces: real crate CLI pushes, then public HTTP requests to the
// root and synthetic group index.
func TestGroupedIndexViaCLI(t *testing.T) {
	const prefix = "smoke-index"
	children := []struct {
		name string
		body string
	}{
		{prefix + ".docs", "<h1>docs</h1>"},
		{prefix + ".plan", "<h1>plan</h1>"},
	}
	for _, child := range children {
		dir := writeFiles(t, map[string]string{"index.html": child.body})
		runCrateOK(t, "push", dir, child.name)
		t.Cleanup(func() { rmSite(t, child.name) })
	}

	if code, body := httpGet(t, "/"); code != 200 {
		t.Fatalf("GET /: got %d", code)
	} else {
		for _, want := range []string{
			`href="/smoke-index/"`,
			`href="/smoke-index.docs/"`,
			`href="/smoke-index.plan/"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("root index missing %q", want)
			}
		}
	}

	resp, _ := httpReq(t, "GET", "/"+prefix, nil, "")
	if resp.StatusCode != 302 {
		t.Errorf("GET /%s: got %d, want 302", prefix, resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/"+prefix+"/" {
		t.Errorf("group redirect: got %q, want %q", got, "/"+prefix+"/")
	}

	code, body := httpGet(t, "/"+prefix+"/")
	if code != 200 {
		t.Fatalf("GET /%s/: got %d", prefix, code)
	}
	for _, want := range []string{">docs<", ">plan<"} {
		if !strings.Contains(body, want) {
			t.Errorf("group index missing %q", want)
		}
	}
}
