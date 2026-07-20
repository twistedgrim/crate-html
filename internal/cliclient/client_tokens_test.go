package cliclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Twistedgrim/crate-html/internal/wire"
)

func TestCreateTokenPostsJSONAndDecodes(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != wire.PathAPITokens {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization header: got %q", got)
		}
		var req wire.CreateTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Name != "pi-agent" || req.Expires != "720h" {
			t.Errorf("request: %+v", req)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(wire.CreateTokenResponse{
			Token: "crate_aabbccdd_secret",
			Info:  wire.TokenInfo{ID: "aabbccdd", Name: "pi-agent"},
		})
	})

	res, err := client.CreateToken(context.Background(), "pi-agent", "720h")
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != "crate_aabbccdd_secret" || res.Info.ID != "aabbccdd" {
		t.Errorf("decoded: %+v", res)
	}
}

func TestCreateTokenDecodesError(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Error: "token name already in use"})
	})
	_, err := client.CreateToken(context.Background(), "dup", "")
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("err: %v", err)
	}
}

func TestListTokensDecodes(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != wire.PathAPITokens {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wire.ListTokensResponse{
			Tokens: []wire.TokenInfo{{ID: "x", Name: "a"}, {ID: "y", Name: "b"}},
		})
	})
	tokens, err := client.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0].ID != "x" {
		t.Errorf("decoded: %+v", tokens)
	}
}

func TestRevokeTokenHitsDeleteAndAccepts204(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != wire.PathAPITokens+"/pi-agent" {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := client.RevokeToken(context.Background(), "pi-agent"); err != nil {
		t.Fatal(err)
	}
}
