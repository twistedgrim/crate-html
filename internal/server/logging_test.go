package server_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/Twistedgrim/crate-html/internal/wire"
)

func brokerWithLogBuffer(t *testing.T) (*httptest.Server, *token.Store, *bytes.Buffer) {
	t.Helper()
	store := storage.New(t.TempDir())
	store.SetMaxSiteBytes(1 << 20)
	tokens, err := token.Load(filepath.Join(t.TempDir(), "tokens.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	srv := server.New(config.Config{
		BaseURL:        "http://broker.invalid",
		PublicURL:      "https://crate.example",
		Token:          testToken,
		MaxUploadBytes: 1 << 20,
	}, store, tokens, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, tokens, &logs
}

func decodeLogLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func findLogEntry(t *testing.T, entries []map[string]any, message string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["msg"] == message {
			return entry
		}
	}
	t.Fatalf("missing %q in log entries: %+v", message, entries)
	return nil
}

func TestBrokerLoggingCorrelatesSiteMutation(t *testing.T) {
	ts, _, logs := brokerWithLogBuffer(t)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("ok"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/logged", &archive)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	requestID := resp.Header.Get(wire.HeaderRequestID)
	if len(requestID) != 16 {
		t.Fatalf("request id = %q, want 16 hex characters", requestID)
	}

	entries := decodeLogLines(t, logs)
	mutation := findLogEntry(t, entries, "site stored")
	request := findLogEntry(t, entries, "broker request")
	if mutation["request_id"] != requestID || request["request_id"] != requestID {
		t.Fatalf("request ids do not correlate: header=%q mutation=%v request=%v", requestID, mutation["request_id"], request["request_id"])
	}
	if mutation["site"] != "logged" || mutation["auth_kind"] != "root" {
		t.Fatalf("mutation fields = %+v", mutation)
	}
	if request["route"] != "/api/sites/{name}" || request["status"] != float64(http.StatusOK) {
		t.Fatalf("request fields = %+v", request)
	}
	if request["request_bytes"].(float64) <= 0 || request["response_bytes"].(float64) <= 0 {
		t.Fatalf("byte counts = %+v", request)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Fatal("root bearer token leaked into logs")
	}
}

func TestBrokerLoggingNamesMintedTokenWithoutSecret(t *testing.T) {
	ts, tokens, logs := brokerWithLogBuffer(t)
	plain, rec, err := tokens.Create("pi-agent", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+wire.PathAPISites, nil)
	req.Header.Set(wire.HeaderAuth, "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	entry := findLogEntry(t, decodeLogLines(t, logs), "broker request")
	if entry["auth_kind"] != "token" || entry["token_id"] != rec.ID || entry["token_name"] != rec.Name {
		t.Fatalf("token identity fields = %+v", entry)
	}
	if strings.Contains(logs.String(), plain) {
		t.Fatal("minted bearer token leaked into logs")
	}
}

func TestBrokerLoggingWarnsOnRejectedAuth(t *testing.T) {
	ts, _, logs := brokerWithLogBuffer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+wire.PathAPISites, nil)
	req.Header.Set(wire.HeaderAuth, "Bearer do-not-log-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	entry := findLogEntry(t, decodeLogLines(t, logs), "broker request")
	if entry["level"] != "WARN" || entry["rejection"] != "invalid_token" {
		t.Fatalf("rejection fields = %+v", entry)
	}
	if strings.Contains(logs.String(), "do-not-log-me") {
		t.Fatal("rejected bearer token leaked into logs")
	}
}

func TestPublicAndHealthRequestsAreNotLogged(t *testing.T) {
	ts, _, logs := brokerWithLogBuffer(t)
	for _, path := range []string{"/", "/healthz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if logs.Len() != 0 {
		t.Fatalf("non-broker requests produced logs: %s", logs.String())
	}
}
