//go:build smoke

package smoke

import (
	"bytes"
	"testing"
)

func TestStatusIsUnauthenticated(t *testing.T) {
	// No Authorization header at all.
	resp, _ := httpReq(t, "GET", "/api/status", nil, "")
	if resp.StatusCode != 200 {
		t.Errorf("GET /api/status without token: got %d, want 200", resp.StatusCode)
	}
}

func TestPutWithoutTokenIs401(t *testing.T) {
	resp, _ := httpReq(t, "PUT", "/api/sites/no-token", bytes.NewReader(nil), "")
	if resp.StatusCode != 401 {
		t.Errorf("PUT without token: got %d, want 401", resp.StatusCode)
	}
}

func TestPutWithWrongTokenIs401(t *testing.T) {
	resp, _ := httpReq(t, "PUT", "/api/sites/wrong-token", bytes.NewReader(nil), "wrong-bearer")
	if resp.StatusCode != 401 {
		t.Errorf("PUT with wrong token: got %d, want 401", resp.StatusCode)
	}
}

func TestDeleteWithoutTokenIs401(t *testing.T) {
	resp, _ := httpReq(t, "DELETE", "/api/sites/anything", nil, "")
	if resp.StatusCode != 401 {
		t.Errorf("DELETE without token: got %d, want 401", resp.StatusCode)
	}
}

func TestListSitesWithoutTokenIs401(t *testing.T) {
	resp, _ := httpReq(t, "GET", "/api/sites", nil, "")
	if resp.StatusCode != 401 {
		t.Errorf("GET /api/sites without token: got %d, want 401", resp.StatusCode)
	}
}

func TestPutWithCorrectTokenPasses(t *testing.T) {
	tarball := tarFromMap(t, map[string]string{"index.html": "auth-ok"})
	resp, _ := httpReq(t, "PUT", "/api/sites/auth-correct", bytes.NewReader(tarball), token)
	if resp.StatusCode != 200 {
		t.Errorf("PUT with correct token: got %d, want 200", resp.StatusCode)
	}
	t.Cleanup(func() { rmSite(t, "auth-correct") })
}
