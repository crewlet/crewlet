package tracing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The metric half of the standard OTel environment.
const (
	// MetricsEndpointVar overrides the base for metrics alone, exactly as
	// its traces sibling does.
	MetricsEndpointVar = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"

	// MetricsExporterVar is the spec's own switch: `otlp` or `none`. It is
	// honoured because an operator who wants traces and not metrics has a
	// standard way to say so, and inventing a second one would be the
	// two-keys-for-one-value state Tier A refuses everywhere else.
	MetricsExporterVar = "OTEL_METRICS_EXPORTER"

	// MetricsIntervalVar is the export period in milliseconds.
	MetricsIntervalVar = "OTEL_METRIC_EXPORT_INTERVAL"
)

// defaultMetricInterval is the spec's own default, kept.
//
// Sixty seconds is as fast as a collector's scrape usefully wants, and one
// export per node per minute at this catalogue's width is tens of kilobytes —
// small enough that nothing here is worth tuning against.
const defaultMetricInterval = 60 * time.Second

// metricsEndpoint resolves the metrics endpoint from the same pair the traces
// exporter reads, for the reason stated at the top of this package: a company
// has one telemetry backend, and two settings would let an operator split
// signals across two of them where a correlation resolves on neither.
func metricsEndpoint(opts Options) string {
	if signal := opts.env(MetricsEndpointVar); signal != "" {
		return signal
	}
	base := strings.TrimRight(opts.env(EndpointVar), "/")
	if base == "" {
		return ""
	}
	return base + "/v1/metrics"
}

// metricsEnabled reports whether the exporter is switched on.
//
// The endpoint decides, and `OTEL_METRICS_EXPORTER=none` overrides it — which
// is the operator saying "traces yes, metrics no" in the spec's own words.
func metricsEnabled(opts Options) bool {
	switch strings.ToLower(strings.TrimSpace(opts.env(MetricsExporterVar))) {
	case "none":
		return false
	}
	return metricsEndpoint(opts) != ""
}

// metricInterval is the export period, with a bad value WARNED and defaulted
// on the same never-fail-on-a-typo rule the sampler ratio follows.
func metricInterval(opts Options) time.Duration {
	raw := strings.TrimSpace(opts.env(MetricsIntervalVar))
	if raw == "" {
		return defaultMetricInterval
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Warn("otel_metric_interval_invalid", "value", raw,
			"detail", "expected whole milliseconds; taking the 60s default")
		return defaultMetricInterval
	}
	return time.Duration(ms) * time.Millisecond
}

// configureMeter installs the MeterProvider and registers every instrument in
// the catalogue against the recorder.
//
// # Installed unconditionally, exactly as the TracerProvider is
//
// The provider is always built and always installed; only the EXPORTER is
// conditional. That is the same decision this package already makes for
// traces, and for the same reason one layer down: with the provider
// unconditional, no call site branches on whether metrics are "on", and the
// operator record reads the same recorder a collector would. A provider
// installed only when an endpoint is set would make every measurement site a
// place where two behaviours are possible.
func configureMeter(
	ctx context.Context, opts Options, rec *metrics.Recorder,
) (Shutdown, error) {
	res, err := resourceFor(opts)
	if err != nil {
		return nil, err
	}

	providerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if metricsEnabled(opts) {
		endpoint := metricsEndpoint(opts)
		exp, err := newMetricExporter(ctx, opts, endpoint)
		if err != nil {
			return nil, err
		}
		providerOpts = append(providerOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp,
				sdkmetric.WithInterval(metricInterval(opts)))))
	}

	// THE HISTOGRAM BOUNDARIES ARE THE RECORDER'S, so a bucket on a
	// collector's panel is the bucket the recorder counted into. The SDK's
	// own default boundaries stop at 10 s, which is inside the range this
	// engine measures — a 16-second apply would land in an overflow bucket
	// and read as "at least 10 s" for ever.
	providerOpts = append(providerOpts, sdkmetric.WithView(sdkmetric.NewView(
		sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
			Boundaries: metrics.Bins(),
		}},
	)))

	mp := sdkmetric.NewMeterProvider(providerOpts...)
	otel.SetMeterProvider(mp)

	if err := registerInstruments(mp, rec); err != nil {
		return nil, err
	}

	log.InfoContext(ctx, "metrics_configured",
		"exporting", metricsEnabled(opts),
		"endpoint", metricsEndpoint(opts),
		"interval", metricInterval(opts),
		"instruments", len(metrics.Catalogue()))

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
		defer cancel()
		if err := mp.Shutdown(ctx); err != nil {
			return fmt.Errorf("tracing: flush metrics: %w", err)
		}
		return nil
	}, nil
}

// registerInstruments wires every catalogue entry to the recorder.
//
// # Why every instrument is OBSERVABLE
//
// The recorder is the source of truth and the SDK is a reader of it. An
// observable instrument is one the SDK asks for a value at export time, which
// is exactly that relationship — and it means a measurement costs an atomic
// add on the hot path rather than a call into the SDK.
//
// It also means there is ONE callback for the whole catalogue rather than one
// per instrument: the recorder's read is a single pass, so asking it once and
// distributing is both cheaper and impossible to leave half-registered.
func registerInstruments(mp otelmetric.MeterProvider, rec *metrics.Recorder) error {
	meter := mp.Meter("github.com/crewlet/crewlet")

	counters := map[string]otelmetric.Int64ObservableCounter{}
	gauges := map[string]otelmetric.Float64ObservableGauge{}
	// Histograms are the exception: a distribution cannot be observed
	// after the fact, so the SDK needs the observations as they happen.
	// They are registered here and recorded through [Bridge].
	var observables []otelmetric.Observable

	for _, inst := range metrics.Catalogue() {
		switch inst.Kind {
		case metrics.KindCounter:
			c, err := meter.Int64ObservableCounter(inst.Name,
				otelmetric.WithUnit(inst.Unit),
				otelmetric.WithDescription(inst.Shows))
			if err != nil {
				return fmt.Errorf("tracing: register %s: %w", inst.Name, err)
			}
			counters[inst.Name] = c
			observables = append(observables, c)
		case metrics.KindGauge:
			g, err := meter.Float64ObservableGauge(inst.Name,
				otelmetric.WithUnit(inst.Unit),
				otelmetric.WithDescription(inst.Shows))
			if err != nil {
				return fmt.Errorf("tracing: register %s: %w", inst.Name, err)
			}
			gauges[inst.Name] = g
			observables = append(observables, g)
		case metrics.KindHistogram:
			// Registered by the bridge below, which needs the synchronous
			// instrument rather than an observable one.
			if err := rec.BindHistogram(inst.Name, histogramFor(meter, inst)); err != nil {
				return fmt.Errorf("tracing: register %s: %w", inst.Name, err)
			}
		}
	}

	if len(observables) == 0 {
		return nil
	}
	_, err := meter.RegisterCallback(func(_ context.Context, o otelmetric.Observer) error {
		for _, s := range rec.Read() {
			attrs := otelmetric.WithAttributeSet(attribute.NewSet(attrSet(s.Attrs)...))
			switch s.Kind {
			case metrics.KindCounter:
				if c, ok := counters[s.Name]; ok {
					o.ObserveInt64(c, int64(s.Total), attrs)
				}
			case metrics.KindGauge:
				if g, ok := gauges[s.Name]; ok {
					o.ObserveFloat64(g, s.Value, attrs)
				}
			}
		}
		return nil
	}, observables...)
	if err != nil {
		return fmt.Errorf("tracing: register the metrics callback: %w", err)
	}
	return nil
}

// histogramFor builds the synchronous instrument a histogram records through.
func histogramFor(meter otelmetric.Meter, inst metrics.Instrument) func(float64, map[string]string) {
	h, err := meter.Float64Histogram(inst.Name,
		otelmetric.WithUnit(inst.Unit),
		otelmetric.WithDescription(inst.Shows))
	if err != nil {
		// A histogram that could not be built records nowhere rather than
		// failing the boot: this is telemetry, and the recorder's own copy
		// still answers the operator record.
		log.Warn("otel_histogram_unavailable", "instrument", inst.Name, "error", err)
		return nil
	}
	return func(v float64, attrs map[string]string) {
		h.Record(context.Background(), v,
			otelmetric.WithAttributeSet(attribute.NewSet(attrSet(attrs)...)))
	}
}

// attrSet converts the recorder's attributes to OTel's.
func attrSet(in map[string]string) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for k, v := range in {
		out = append(out, attribute.String(k, v))
	}
	return out
}

// newMetricExporter builds the OTLP metric exporter for the configured
// protocol.
func newMetricExporter(
	ctx context.Context, opts Options, endpoint string,
) (sdkmetric.Exporter, error) {
	headers := sandbox.ParseOtelHeaders(opts.env(HeadersVar))
	switch p := protocol(opts); p {
	case ProtocolGRPC:
		exp, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpointURL(endpoint),
			otlpmetricgrpc.WithHeaders(headers))
		if err != nil {
			return nil, fmt.Errorf("otlp grpc metric exporter for %q: %w", endpoint, err)
		}
		return exp, nil
	case ProtocolHTTP:
		exp, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(endpoint),
			otlpmetrichttp.WithHeaders(headers))
		if err != nil {
			return nil, fmt.Errorf("otlp http metric exporter for %q: %w", endpoint, err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("%s=%q is not one of %s",
			ProtocolVar, p, strings.Join(Protocols, ", "))
	}
}
