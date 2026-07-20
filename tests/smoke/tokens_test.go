//go:build smoke

package smoke

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestTokenLifecycle walks the full named-token flow through the real CLI
// and daemon: mint, use for a push, appear in ls, revoke, and reject after
// revocation.
func TestTokenLifecycle(t *testing.T) {
	// Mint. Stdout is exactly the plaintext token; guidance goes to stderr.
	out := runCrateOK(t, "token", "create", "smoke-lifecycle")
	minted := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "crate_") {
			minted = strings.TrimSpace(line)
			break
		}
	}
	if minted == "" {
		t.Fatalf("no crate_ token in create output:\n%s", out)
	}

	// The minted token can push a site.
	tarBytes := tarFromMap(t, map[string]string{"index.html": "minted"})
	req, err := http.NewRequest(http.MethodPut, baseURL+"/api/sites/minted-push", bytes.NewReader(tarBytes))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+minted)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push with minted token: %d", resp.StatusCode)
	}
	t.Cleanup(func() { rmSite(t, "minted-push") })

	// It shows up in ls with its name.
	lsOut := runCrateOK(t, "token", "ls")
	if !strings.Contains(lsOut, "smoke-lifecycle") {
		t.Errorf("token ls missing smoke-lifecycle:\n%s", lsOut)
	}
	if strings.Contains(lsOut, minted) {
		t.Error("token ls leaked the plaintext token")
	}

	// A minted token cannot manage tokens (403).
	mgmtResp, _ := httpReq(t, http.MethodGet, "/api/tokens", nil, minted)
	if mgmtResp.StatusCode != http.StatusForbidden {
		t.Errorf("token mgmt with minted token: %d, want 403", mgmtResp.StatusCode)
	}

	// Revoke by name; the token stops working.
	runCrateOK(t, "token", "revoke", "smoke-lifecycle")
	after, _ := httpReq(t, http.MethodGet, "/api/sites", nil, minted)
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: %d, want 401", after.StatusCode)
	}
}

// TestBareTokenCommandStillPrintsRoot guards the pre-existing contract that
// `crate token` with no subcommand prints the root token.
func TestBareTokenCommandStillPrintsRoot(t *testing.T) {
	out := runCrateOK(t, "token")
	if got := strings.TrimSpace(out); got != token {
		t.Errorf("bare `crate token`: got %q, want root token", got)
	}
}
