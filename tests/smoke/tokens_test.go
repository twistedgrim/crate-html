//go:build smoke

package smoke

import (
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

	// A separate agent uses the minted token exactly as documented: as a
	// CRATE_TOKEN override for the real CLI, not by issuing raw HTTP itself.
	dir := writeFiles(t, map[string]string{"index.html": "minted"})
	pushOut, err := runCrateWithEnv(t, []string{"CRATE_TOKEN=" + minted}, "push", dir, "minted-push")
	if err != nil {
		t.Fatalf("push with minted token: %v\n%s", err, pushOut)
	}
	if !strings.Contains(pushOut, baseURL+"/minted-push/") {
		t.Errorf("minted-token push missing URL: %s", pushOut)
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
