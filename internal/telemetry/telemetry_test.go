package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartDefaultsToDisabled(t *testing.T) {
	t.Setenv(EnvMetricsExporter, "")
	provider, err := Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.Mode() != metricsExporterNone {
		t.Fatalf("mode = %q, want %q", provider.Mode(), metricsExporterNone)
	}
	if provider.Handler() != nil {
		t.Fatal("disabled metrics unexpectedly returned an HTTP handler")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsUnsupportedExporter(t *testing.T) {
	t.Setenv(EnvMetricsExporter, "console")
	if _, err := Start(context.Background()); err == nil {
		t.Fatal("unsupported exporter unexpectedly started")
	}
}

func TestStartPrometheusReturnsOnlyAnOperationalHandler(t *testing.T) {
	t.Setenv(EnvMetricsExporter, metricsExporterPrometheus)
	provider, err := Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if provider.Mode() != metricsExporterPrometheus {
		t.Fatalf("mode = %q, want %q", provider.Mode(), metricsExporterPrometheus)
	}
	if provider.Handler() == nil {
		t.Fatal("Prometheus exporter did not provide an operational handler")
	}
	provider.Metrics().Mutation("push", "success")
	response := httptest.NewRecorder()
	provider.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("metrics handler status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "crate_broker_mutations") {
		t.Fatalf("Prometheus output did not include broker metric: %s", response.Body.String())
	}
	notFound := httptest.NewRecorder()
	provider.Handler().ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("non-metrics path status = %d, want %d", notFound.Code, http.StatusNotFound)
	}
}

func TestPeriodicReaderConfigFromEnv(t *testing.T) {
	t.Setenv(EnvMetricExportInterval, "1250")
	t.Setenv(EnvMetricExportTimeout, "250")
	config, err := periodicReaderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.interval != 1250*time.Millisecond || config.timeout != 250*time.Millisecond {
		t.Fatalf("config = %+v, want interval=1250ms timeout=250ms", config)
	}
}

func TestPeriodicReaderConfigDefaults(t *testing.T) {
	t.Setenv(EnvMetricExportInterval, "")
	t.Setenv(EnvMetricExportTimeout, "")
	config, err := periodicReaderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.interval != time.Minute || config.timeout != 30*time.Second {
		t.Fatalf("config = %+v, want interval=1m timeout=30s", config)
	}
}

func TestPeriodicReaderConfigRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero interval", key: EnvMetricExportInterval, value: "0"},
		{name: "negative timeout", key: EnvMetricExportTimeout, value: "-1"},
		{name: "non-numeric interval", key: EnvMetricExportInterval, value: "soon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMetricExportInterval, "")
			t.Setenv(EnvMetricExportTimeout, "")
			t.Setenv(tc.key, tc.value)
			if _, err := periodicReaderConfigFromEnv(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v, want clear %s validation error", err, tc.key)
			}
		})
	}
}

func TestStartRejectsInvalidOTLPPeriodicReaderSetting(t *testing.T) {
	t.Setenv(EnvMetricsExporter, metricsExporterOTLP)
	t.Setenv(EnvMetricExportInterval, "0")
	if _, err := Start(context.Background()); err == nil || !strings.Contains(err.Error(), EnvMetricExportInterval) {
		t.Fatalf("Start error = %v, want clear %s validation error", err, EnvMetricExportInterval)
	}
}
