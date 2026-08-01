package server_test

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Twistedgrim/crate-html/internal/config"
	"github.com/Twistedgrim/crate-html/internal/server"
	"github.com/Twistedgrim/crate-html/internal/storage"
	"github.com/Twistedgrim/crate-html/internal/telemetry"
	"github.com/Twistedgrim/crate-html/internal/token"
	"github.com/Twistedgrim/crate-html/internal/wire"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestBrokerMetricsUseNormalizedSafeAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	store := storage.New(t.TempDir())
	store.SetMaxSiteBytes(1 << 20)
	tokens, err := token.Load(filepath.Join(t.TempDir(), "tokens.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewWithMetrics(config.Config{
		Token:          testToken,
		MaxUploadBytes: 1 << 20,
	}, store, tokens, nil, discardLogger(), telemetry.MeterProviderForTest(provider))
	ts := httptest.NewServer(srv.BrokerHandler())
	t.Cleanup(ts.Close)

	// This rejected request must produce the bounded invalid_token reason, not
	// the bearer value or its raw request path.
	const secret = "bearer-value-that-must-never-be-a-metric-label"
	req, _ := http.NewRequest(http.MethodGet, ts.URL+wire.PathAPISites, nil)
	req.Header.Set(wire.HeaderAuth, "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rejected request status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("ok"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/sensitive-site-name", &archive)
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	if len(collected.ScopeMetrics) == 0 {
		t.Fatal("broker metrics were not emitted")
	}
	all := metricAttributes(collected)
	joined := strings.Join(all, "\n")
	for _, forbidden := range []string{secret, "sensitive-site-name", "request_id", "token_id", "token_name", "path"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("metric attributes leaked %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "http.route=/api/sites/{name}") {
		t.Fatalf("metrics did not use normalized route: %s", joined)
	}
	if !strings.Contains(joined, "crate.reason=invalid_token") {
		t.Fatalf("metrics did not record bounded auth rejection: %s", joined)
	}
	if !strings.Contains(joined, "crate.operation=push") || !strings.Contains(joined, "crate.outcome=success") {
		t.Fatalf("metrics did not record push outcome: %s", joined)
	}
}

func TestBrokerMetricsAreDisabledUnlessInjected(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	store := storage.New(t.TempDir())
	tokens, err := token.Load(filepath.Join(t.TempDir(), "tokens.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(config.Config{Token: testToken}, store, tokens, nil, discardLogger())
	req := httptest.NewRequest(http.MethodGet, wire.PathAPIStatus, nil)
	resp := httptest.NewRecorder()
	srv.BrokerHandler().ServeHTTP(resp, req)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	if len(collected.ScopeMetrics) != 0 {
		t.Fatalf("disabled broker unexpectedly emitted metrics: %+v", collected.ScopeMetrics)
	}
}

func TestWebRoleDoesNotExposeMetricsRoute(t *testing.T) {
	srv := server.NewReadOnly(config.Config{}, storage.New(t.TempDir()), nil, discardLogger())
	resp := httptest.NewRecorder()
	srv.PublicHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("web /metrics status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestCachedSiteGaugeDoesNotListDuringCollection(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	backend := &countingBackend{Store: storage.New(t.TempDir())}
	tokens, err := token.Load(filepath.Join(t.TempDir(), "tokens.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewWithMetrics(config.Config{
		Token:          testToken,
		MaxUploadBytes: 1 << 20,
	}, backend, tokens, nil, discardLogger(), telemetry.MeterProviderForTest(provider))

	// A collection before any mutation reports the initial cached value without
	// asking storage for a list.
	if got := collectSiteGauge(t, reader); got != 0 {
		t.Fatalf("initial site gauge = %d, want 0", got)
	}
	if got := backend.listCalls.Load(); got != 0 {
		t.Fatalf("collection called List %d times, want 0", got)
	}

	ts := httptest.NewServer(srv.BrokerHandler())
	t.Cleanup(ts.Close)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("ok"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+wire.PathAPISites+"/gauge-check", &archive)
	req.Header.Set(wire.HeaderAuth, "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := backend.listCalls.Load(); got != 1 {
		t.Fatalf("mutation List calls = %d, want 1 cache refresh", got)
	}
	if got := collectSiteGauge(t, reader); got != 1 {
		t.Fatalf("site gauge after push = %d, want 1", got)
	}
	if got := backend.listCalls.Load(); got != 1 {
		t.Fatalf("gauge collection called List %d times after push, want 1", got)
	}
}

type countingBackend struct {
	*storage.Store
	listCalls atomic.Int32
}

func (b *countingBackend) List() ([]wire.Site, error) {
	b.listCalls.Add(1)
	return b.Store.List()
}

func collectSiteGauge(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name != "crate.broker.sites" {
				continue
			}
			data, ok := item.Data.(metricdata.Gauge[int64])
			if !ok || len(data.DataPoints) != 1 {
				t.Fatalf("site gauge data = %#v, want one int64 point", item.Data)
			}
			return data.DataPoints[0].Value
		}
	}
	t.Fatal("site gauge was not collected")
	return 0
}

func metricAttributes(collected metricdata.ResourceMetrics) []string {
	var out []string
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			switch data := item.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					out = append(out, item.Name+" "+metricAttributeString(point.Attributes))
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					out = append(out, item.Name+" "+metricAttributeString(point.Attributes))
				}
			}
		}
	}
	return out
}

func metricAttributeString(set attribute.Set) string {
	parts := make([]string, 0, set.Len())
	for _, pair := range set.ToSlice() {
		parts = append(parts, string(pair.Key)+"="+pair.Value.Emit())
	}
	return strings.Join(parts, ",")
}
