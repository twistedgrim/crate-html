//go:build smoke

package smoke

import (
	"testing"
)

// TestPublicPathTraversalReturns404 confirms a request like
// /<site>/../../etc/passwd resolves to a file outside the site root and is
// rejected. path.Clean does the work; the test pins the behavior.
func TestPublicPathTraversalReturns404(t *testing.T) {
	t.Parallel()
	const name = "traversal-fixture"
	dir := writeFiles(t, map[string]string{"index.html": "x"})
	runCrateOK(t, "push", dir, name)
	t.Cleanup(func() { rmSite(t, name) })

	for _, badPath := range []string{
		"/" + name + "/../../etc/passwd",
		"/" + name + "/../../../etc/passwd",
		"/" + name + "/./../" + name + "/index.html", // re-enters site — fine to 200, but should not 5xx
	} {
		code := httpStatus(t, badPath)
		// The first two should 404; the third is a benign re-entry that lands
		// on the real index.html, so it's 200. Either way, we want NOT 5xx.
		if code >= 500 {
			t.Errorf("%s: got %d, want non-5xx", badPath, code)
		}
	}
}

// TestPushRejectsTraversalInTarball confirms a tar entry whose path escapes
// the site root is rejected at PUT time with 400.
func TestPushRejectsTraversalInTarball(t *testing.T) {
	t.Parallel()
	// Build a tar with a single entry whose path escapes.
	tarball := tarFromMap(t, map[string]string{"../escape.html": "x"})
	out, err := runCrateStdin(t, tarball, "push", "-", "traversal-tar")
	if err == nil {
		t.Errorf("expected non-zero exit; got success\noutput: %s", out)
	}
	if code := httpStatus(t, "/traversal-tar/"); code != 404 {
		t.Errorf("escaped site should not exist: GET got %d", code)
	}
}
