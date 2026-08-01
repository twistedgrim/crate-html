// Package telemetry provides the broker's deliberately small metrics surface.
// It keeps OpenTelemetry setup and metric names out of HTTP handlers so the
// public web role can remain entirely unaware of broker observability.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const (
	// EnvMetricsExporter follows the standard OpenTelemetry exporter setting.
	// An absent setting is deliberately treated as none for compatibility.
	EnvMetricsExporter = "OTEL_METRICS_EXPORTER"
	// EnvMetricExportInterval and EnvMetricExportTimeout are standard OTel
	// periodic metric reader settings, expressed as integer milliseconds.
	EnvMetricExportInterval = "OTEL_METRIC_EXPORT_INTERVAL"
	EnvMetricExportTimeout  = "OTEL_METRIC_EXPORT_TIMEOUT"

	metricsExporterNone       = "none"
	metricsExporterPrometheus = "prometheus"
	metricsExporterOTLP       = "otlp"
)

// BrokerMetrics is the explicit dependency passed to broker handlers. All
// arguments are normalized enum-like values; callers must never pass user
// input, site names, tokens, paths, request IDs, or error messages.
type BrokerMetrics interface {
	HTTP(method, route string, status int, duration time.Duration, requestBytes int64)
	Mutation(operation, outcome string)
	AuthRejection(reason string)
	Storage(operation, outcome string, duration time.Duration)
	Expiry(duration time.Duration, deleted int, err error)
	SetSiteCount(count int)
}

// Provider owns the SDK lifetime and, in Prometheus mode, the dedicated
// operational HTTP handler. Handler is nil for disabled and OTLP modes.
type Provider struct {
	metrics  BrokerMetrics
	handler  http.Handler
	shutdown func(context.Context) error
	mode     string
}

// Metrics returns the broker metrics implementation. It is always safe to
// call, including when metrics are disabled.
func (p *Provider) Metrics() BrokerMetrics { return p.metrics }

// Handler returns a Prometheus handler only when the Prometheus exporter is
// selected. It is intended for a separate listener, never the public crate
// listener.
func (p *Provider) Handler() http.Handler { return p.handler }

// Mode returns none, prometheus, or otlp.
func (p *Provider) Mode() string { return p.mode }

// Shutdown flushes the SDK readers and releases exporter resources.
func (p *Provider) Shutdown(ctx context.Context) error { return p.shutdown(ctx) }

// Start configures OpenTelemetry metrics from standard environment variables.
// OTLP exporter constructors consume OTEL_EXPORTER_OTLP_* and
// OTEL_EXPORTER_OTLP_METRICS_* settings directly. The protocol setting is used
// only to choose gRPC (the default) or HTTP/protobuf transport.
func Start(ctx context.Context) (*Provider, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvMetricsExporter)))
	if mode == "" || mode == metricsExporterNone {
		return disabledProvider(), nil
	}

	var (
		reader  sdkmetric.Reader
		handler http.Handler
	)
	switch mode {
	case metricsExporterPrometheus:
		registry := prometheus.NewRegistry()
		exporter, err := otelprometheus.New(otelprometheus.WithRegisterer(registry))
		if err != nil {
			return nil, fmt.Errorf("create Prometheus metrics exporter: %w", err)
		}
		reader = exporter
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		handler = mux
	case metricsExporterOTLP:
		readerConfig, err := periodicReaderConfigFromEnv()
		if err != nil {
			return nil, err
		}
		exporter, err := newOTLPExporter(ctx)
		if err != nil {
			return nil, err
		}
		reader = sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(readerConfig.interval),
			sdkmetric.WithTimeout(readerConfig.timeout),
		)
	default:
		return nil, fmt.Errorf("unsupported %s %q (want none, prometheus, or otlp)", EnvMetricsExporter, mode)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(durationHistogramView()),
	)
	return &Provider{
		metrics:  newMetrics(provider.Meter("github.com/Twistedgrim/crate-html/broker")),
		handler:  handler,
		shutdown: provider.Shutdown,
		mode:     mode,
	}, nil
}

// durationHistogramView keeps the exported buckets in seconds while retaining
// useful resolution for sub-second broker operations. The SDK defaults are
// 0, 5, 10, ... which are appropriate for millisecond-valued histograms, but
// make nearly every broker request fall into the first five-second bucket.
func durationHistogramView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{
			Name: "crate.broker.*.duration",
			Kind: sdkmetric.InstrumentKindHistogram,
			Unit: "s",
		},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10},
				NoMinMax:   true,
			},
		},
	)
}

type periodicReaderConfig struct {
	interval time.Duration
	timeout  time.Duration
}

func periodicReaderConfigFromEnv() (periodicReaderConfig, error) {
	interval, err := metricExportDuration(EnvMetricExportInterval, time.Minute)
	if err != nil {
		return periodicReaderConfig{}, err
	}
	timeout, err := metricExportDuration(EnvMetricExportTimeout, 30*time.Second)
	if err != nil {
		return periodicReaderConfig{}, err
	}
	return periodicReaderConfig{interval: interval, timeout: timeout}, nil
}

func metricExportDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || milliseconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer of milliseconds", name)
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration <= 0 {
		return 0, fmt.Errorf("%s is too large", name)
	}
	return duration, nil
}

func newOTLPExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	protocol := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")))
	if protocol == "" {
		protocol = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	switch protocol {
	case "", "grpc":
		exporter, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP gRPC metrics exporter: %w", err)
		}
		return exporter, nil
	case "http/protobuf":
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP HTTP metrics exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("unsupported OTLP metrics protocol %q (want grpc or http/protobuf)", protocol)
	}
}

func disabledProvider() *Provider {
	return &Provider{
		metrics:  DisabledMetrics(),
		shutdown: func(context.Context) error { return nil },
		mode:     metricsExporterNone,
	}
}

// DisabledMetrics returns the no-op implementation for explicit dependency
// injection where metrics are intentionally unavailable.
func DisabledMetrics() BrokerMetrics { return noopMetrics{} }

type metrics struct {
	httpRequests   otelmetric.Int64Counter
	httpDuration   otelmetric.Float64Histogram
	uploadBytes    otelmetric.Int64Counter
	mutations      otelmetric.Int64Counter
	authRejections otelmetric.Int64Counter
	storageOps     otelmetric.Int64Counter
	storageLatency otelmetric.Float64Histogram
	expiryDuration otelmetric.Float64Histogram
	expiryErrors   otelmetric.Int64Counter
	expiryDeleted  otelmetric.Int64Counter
	siteCount      otelmetric.Int64ObservableGauge
	currentSites   atomic.Int64
}

func newMetrics(meter otelmetric.Meter) *metrics {
	m := &metrics{
		httpRequests:   must(meter.Int64Counter("crate.broker.http.requests", otelmetric.WithUnit("{request}"), otelmetric.WithDescription("Broker HTTP requests by normalized route, method, and status."))),
		httpDuration:   must(meter.Float64Histogram("crate.broker.http.request.duration", otelmetric.WithUnit("s"), otelmetric.WithDescription("Broker HTTP request duration."))),
		uploadBytes:    must(meter.Int64Counter("crate.broker.upload.bytes", otelmetric.WithUnit("By"), otelmetric.WithDescription("Broker upload request bytes read."))),
		mutations:      must(meter.Int64Counter("crate.broker.mutations", otelmetric.WithUnit("{mutation}"), otelmetric.WithDescription("Broker push and delete outcomes."))),
		authRejections: must(meter.Int64Counter("crate.broker.auth.rejections", otelmetric.WithUnit("{rejection}"), otelmetric.WithDescription("Broker authentication and authorization rejections."))),
		storageOps:     must(meter.Int64Counter("crate.broker.storage.operations", otelmetric.WithUnit("{operation}"), otelmetric.WithDescription("Broker storage operation outcomes."))),
		storageLatency: must(meter.Float64Histogram("crate.broker.storage.operation.duration", otelmetric.WithUnit("s"), otelmetric.WithDescription("Broker storage operation duration."))),
		expiryDuration: must(meter.Float64Histogram("crate.broker.expiry.cleanup.duration", otelmetric.WithUnit("s"), otelmetric.WithDescription("Broker expiry cleanup duration."))),
		expiryErrors:   must(meter.Int64Counter("crate.broker.expiry.cleanup.errors", otelmetric.WithUnit("{error}"), otelmetric.WithDescription("Broker expiry cleanup errors."))),
		expiryDeleted:  must(meter.Int64Counter("crate.broker.expiry.cleanup.deleted", otelmetric.WithUnit("{site}"), otelmetric.WithDescription("Sites deleted by broker expiry cleanup."))),
		siteCount:      must(meter.Int64ObservableGauge("crate.broker.sites", otelmetric.WithUnit("{site}"), otelmetric.WithDescription("Cached number of stored sites."))),
	}
	_, _ = meter.RegisterCallback(func(_ context.Context, observer otelmetric.Observer) error {
		observer.ObserveInt64(m.siteCount, m.currentSites.Load())
		return nil
	}, m.siteCount)
	return m
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("create metric instrument: %v", err))
	}
	return value
}

func (m *metrics) HTTP(method, route string, status int, duration time.Duration, requestBytes int64) {
	attrs := otelmetric.WithAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route), attribute.Int("http.response.status_code", status))
	m.httpRequests.Add(context.Background(), 1, attrs)
	m.httpDuration.Record(context.Background(), duration.Seconds(), attrs)
	if requestBytes > 0 && method == http.MethodPut {
		m.uploadBytes.Add(context.Background(), requestBytes, otelmetric.WithAttributes(attribute.String("http.route", route)))
	}
}

func (m *metrics) Mutation(operation, outcome string) {
	m.mutations.Add(context.Background(), 1, otelmetric.WithAttributes(attribute.String("crate.operation", operation), attribute.String("crate.outcome", outcome)))
}

func (m *metrics) AuthRejection(reason string) {
	m.authRejections.Add(context.Background(), 1, otelmetric.WithAttributes(attribute.String("crate.reason", reason)))
}

func (m *metrics) Storage(operation, outcome string, duration time.Duration) {
	attrs := otelmetric.WithAttributes(attribute.String("crate.operation", operation), attribute.String("crate.outcome", outcome))
	m.storageOps.Add(context.Background(), 1, attrs)
	m.storageLatency.Record(context.Background(), duration.Seconds(), attrs)
}

func (m *metrics) Expiry(duration time.Duration, deleted int, err error) {
	m.expiryDuration.Record(context.Background(), duration.Seconds())
	if err != nil {
		m.expiryErrors.Add(context.Background(), 1)
		return
	}
	if deleted > 0 {
		m.expiryDeleted.Add(context.Background(), int64(deleted))
	}
}

func (m *metrics) SetSiteCount(count int) { m.currentSites.Store(int64(count)) }

type noopMetrics struct{}

func (noopMetrics) HTTP(string, string, int, time.Duration, int64) {}
func (noopMetrics) Mutation(string, string)                        {}
func (noopMetrics) AuthRejection(string)                           {}
func (noopMetrics) Storage(string, string, time.Duration)          {}
func (noopMetrics) Expiry(time.Duration, int, error)               {}
func (noopMetrics) SetSiteCount(int)                               {}

// MeterProviderForTest supplies a metrics implementation wired to a caller's
// SDK provider without mutating OTel global state.
func MeterProviderForTest(provider *sdkmetric.MeterProvider) BrokerMetrics {
	return newMetrics(provider.Meter("github.com/Twistedgrim/crate-html/broker"))
}
