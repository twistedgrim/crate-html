package server_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/wire"
)

func doJSON(t *testing.T, method, url, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set(wire.HeaderAuth, "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func createToken(t *testing.T, url, name, expires string) wire.CreateTokenResponse {
	t.Helper()
	resp, body := doJSON(t, "POST", url+wire.PathAPITokens, testToken,
		wire.CreateTokenRequest{Name: name, Expires: expires})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d: %s", resp.StatusCode, body)
	}
	var out wire.CreateTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMintedTokenAuthorizesSitesAPI(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	created := createToken(t, ts.URL, "agent-a", "")

	if !strings.HasPrefix(created.Token, "crate_") {
		t.Fatalf("token %q lacks crate_ prefix", created.Token)
	}
	resp, body := doJSON(t, "GET", ts.URL+wire.PathAPISites, created.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("minted token on GET /api/sites: %d: %s", resp.StatusCode, body)
	}
}

func TestRevokedTokenIs401(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	created := createToken(t, ts.URL, "gone", "")

	resp, _ := doJSON(t, "DELETE", ts.URL+wire.PathAPITokens+"/"+created.Info.ID, testToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+wire.PathAPISites, created.Token, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: %d, want 401", resp.StatusCode)
	}
}

func TestExpiredTokenIs401(t *testing.T) {
	ts, _, tokens := newTestServerWithTokens(t, nil)
	ttl := time.Nanosecond
	plain, _, err := tokens.Create("flash", &ttl, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := doJSON(t, "GET", ts.URL+wire.PathAPISites, plain, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired token: %d, want 401", resp.StatusCode)
	}
}

func TestTokenManagementIsRootOnly(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	minted := createToken(t, ts.URL, "unprivileged", "")

	// A valid minted token gets 403 (authenticated, not authorized).
	for _, c := range []struct{ method, path string }{
		{"POST", wire.PathAPITokens},
		{"GET", wire.PathAPITokens},
		{"DELETE", wire.PathAPITokens + "/" + minted.Info.ID},
	} {
		resp, _ := doJSON(t, c.method, ts.URL+c.path, minted.Token,
			wire.CreateTokenRequest{Name: "escalate"})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with minted token: %d, want 403", c.method, c.path, resp.StatusCode)
		}
	}

	// Garbage and missing tokens get 401.
	for _, bearer := range []string{"", "garbage"} {
		resp, _ := doJSON(t, "GET", ts.URL+wire.PathAPITokens, bearer, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bearer %q: %d, want 401", bearer, resp.StatusCode)
		}
	}
}

func TestListTokensNeverLeaksSecrets(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	created := createToken(t, ts.URL, "leaky", "")
	secret := strings.TrimPrefix(created.Token, "crate_"+created.Info.ID+"_")

	resp, body := doJSON(t, "GET", ts.URL+wire.PathAPITokens, testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if strings.Contains(string(body), secret) {
		t.Error("token list response contains a plaintext secret")
	}
	var out wire.ListTokensResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tokens) != 1 || out.Tokens[0].Name != "leaky" {
		t.Errorf("got %+v", out.Tokens)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	createToken(t, ts.URL, "dup", "")

	for _, c := range []struct {
		name string
		req  wire.CreateTokenRequest
		want int
	}{
		{"duplicate name", wire.CreateTokenRequest{Name: "dup"}, http.StatusConflict},
		{"bad name", wire.CreateTokenRequest{Name: "Bad Name"}, http.StatusBadRequest},
		{"bad expiry", wire.CreateTokenRequest{Name: "ok-name", Expires: "tomorrow"}, http.StatusBadRequest},
		{"negative expiry", wire.CreateTokenRequest{Name: "ok-name", Expires: "-1h"}, http.StatusBadRequest},
	} {
		resp, body := doJSON(t, "POST", ts.URL+wire.PathAPITokens, testToken, c.req)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d (%s), want %d", c.name, resp.StatusCode, body, c.want)
		}
	}
}

func TestRevokeUnknownTokenIs404(t *testing.T) {
	ts, _, _ := newTestServerWithTokens(t, nil)
	resp, _ := doJSON(t, "DELETE", ts.URL+wire.PathAPITokens+"/nope", testToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got %d, want 404", resp.StatusCode)
	}
}

func TestOversizedUploadIs413(t *testing.T) {
	ts, store := newTestServer(t, nil)

	// Cap in newTestServerWithTokens is 1 MiB; send a valid tar holding a
	// 2 MiB file so the limit trips mid-extraction rather than at parse time.
	content := bytes.Repeat([]byte("x"), 2<<20)
	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	if err := tw.WriteHeader(&tar.Header{Name: "big.html", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", ts.URL+wire.PathAPISites+"/toobig", &tarbuf)
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d, want 413", resp.StatusCode)
	}
	if exists, _ := store.Exists("toobig"); exists {
		t.Error("oversized upload created a site")
	}
}
