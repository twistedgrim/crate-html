package cliclient_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/cliclient"
	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

const testToken = "test-token-xyz"

// newFakeServer wires an httptest server to a fresh Client pointed at it.
// Tests get a handler hook to override per-call behavior.
func newFakeServer(t *testing.T, handler http.HandlerFunc) *cliclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return cliclient.New(config.Config{BaseURL: srv.URL, Token: testToken})
}

func TestStatusAttachesBearerAndDecodesJSON(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization header: got %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != wire.PathAPIStatus {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.StatusResponse{Version: "1.2.3", SiteCount: 7})
	})

	st, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "1.2.3" || st.SiteCount != 7 {
		t.Errorf("decoded: %+v", st)
	}
}

func TestListDecodesSites(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.ListSitesResponse{
			Sites: []wire.Site{
				{Name: "a", UpdatedAt: time.Unix(0, 0).UTC(), SizeBytes: 1, FileCount: 1},
				{Name: "b", UpdatedAt: time.Unix(0, 0).UTC(), SizeBytes: 2, FileCount: 2},
			},
		})
	})

	sites, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || sites[0].Name != "a" || sites[1].Name != "b" {
		t.Errorf("decoded: %+v", sites)
	}
}

func TestDeleteHappyPath(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method: %s", r.Method)
		}
		if r.URL.Path != wire.PathAPISites+"/foo" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.Delete(context.Background(), "foo"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestPushReaderSendsTarBody(t *testing.T) {
	var gotBody bytes.Buffer
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-tar" {
			t.Errorf("Content-Type: got %q", got)
		}
		if r.Method != http.MethodPut || r.URL.Path != wire.PathAPISites+"/x" {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		if _, err := io.Copy(&gotBody, r.Body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(wire.PutSiteResponse{
			Site: wire.Site{Name: "x", FileCount: 1, SizeBytes: 5},
			URL:  "http://example/x/",
		})
	})

	payload := []byte("tar-stream-bytes")
	res, err := client.PushReader(context.Background(), "x", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody.Bytes(), payload) {
		t.Errorf("server received: %q, want %q", gotBody.String(), payload)
	}
	if res.URL != "http://example/x/" {
		t.Errorf("res.URL: %q", res.URL)
	}
}

func TestPushTarsDirAndDelegatesToPushReader(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotBody bytes.Buffer
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(&gotBody, r.Body)
		_ = json.NewEncoder(w).Encode(wire.PutSiteResponse{
			Site: wire.Site{Name: "y", FileCount: 1, SizeBytes: 2},
			URL:  "http://example/y/",
		})
	})

	if _, err := client.Push(context.Background(), "y", dir); err != nil {
		t.Fatal(err)
	}

	// Server received a tar stream; pop the first header to prove it.
	tr := tar.NewReader(&gotBody)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("server body wasn't a tar stream: %v", err)
	}
	if hdr.Name != "index.html" {
		t.Errorf("first tar entry: %q", hdr.Name)
	}
}

func TestDecodeErrFormatsJSONBody(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Error: "bad request"})
	})

	_, err := client.Status(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad request") {
		t.Errorf("err missing server message: %v", err)
	}
	if !strings.Contains(err.Error(), "crated:") {
		t.Errorf("err missing 'crated:' prefix: %v", err)
	}
}

func TestDecodeErrFallsBackToStatusText(t *testing.T) {
	client := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	_, err := client.Status(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err missing status: %v", err)
	}
}
