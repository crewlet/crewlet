package tracing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// MetricsIntervalVar is the export period in milliseconds — the one variable
// of the metric half that is not a destination, which [otlpenv] resolves for
// metrics exactly as it does for traces: the same base endpoint and headers,
// OTEL_EXPORTER_OTLP_METRICS_* beside them, and OTEL_METRICS_EXPORTER=none as
// the operator saying "traces yes, metrics no" in the specification's own
// words. A company has one telemetry backend, and two settings would let an
// operator split signals across two of them where a correlation resolves on
// neither.
const MetricsIntervalVar = "OTEL_METRIC_EXPORT_INTERVAL"

// defaultMetricInterval is the spec's own default, kept.
//
// Sixty seconds is as fast as a collector's scrape usefully wants, and one
// export per node per minute at this catalogue's width is tens of kilobytes —
// small enough that nothing here is worth tuning against.
const defaultMetricInterval = 60 * time.Second

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

// configureMeter builds the MeterProvider and registers every instrument in
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
//
// # Returned rather than installed
//
// The install itself belongs to [Configure], which puts both globals in place
// together once the last thing that can fail has succeeded — see the commit
// there. A half that installed itself would be a global written by a call that
// can still return an error to a caller with no handle to undo it.
func configureMeter(
	ctx context.Context, opts Options, res *resource.Resource,
	at otlpenv.Destination, rec *metrics.Recorder,
) (*sdkmetric.MeterProvider, error) {
	// Read once: a bad value warns, and a second read would warn twice.
	interval := metricInterval(opts)
	providerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if at.Exports() {
		exp, err := newMetricExporter(ctx, at)
		if err != nil {
			return nil, err
		}
		providerOpts = append(providerOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
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

	//nolint:contextcheck // The chain reaches histogramFor's recording
	// closure, which takes no context by design — see the paragraph there.
	if err := registerInstruments(mp, rec); err != nil {
		// The periodic reader's goroutine starts inside NewPeriodicReader,
		// so a provider whose registration failed is ALREADY exporting on
		// a timer, and this is the only frame that still holds it — the
		// caller is about to get an error and nothing else. Same rule as
		// [abandon] on the traces side, and the registration error is
		// returned alone for the same reason: it names the instrument an
		// operator's catalogue change broke, which a flush failure behind
		// it would only bury.
		if ferr := shutdownMeter(ctx, mp); ferr != nil {
			log.WarnContext(ctx, "meter_provider_abandoned_unflushed", "error", ferr)
		}
		return nil, err
	}

	log.InfoContext(ctx, "metrics_configured",
		"exporting", at.Exports(), "declined", at.Declined,
		"endpoint", at.Endpoint, "protocol", at.Protocol,
		"interval", interval,
		"instruments", len(metrics.Catalogue()))

	return mp, nil
}

// shutdownMeter flushes and stops the meter half, bounded — the mirror of
// [shutdownTracer], including its detached context, and for the reason written
// down there: the context a drain is given is normally the cancelled one that
// woke it, and a flush inheriting that exports nothing at all.
func shutdownMeter(ctx context.Context, mp *sdkmetric.MeterProvider) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
	defer cancel()
	if err := mp.Shutdown(ctx); err != nil {
		return fmt.Errorf("flush metrics: %w", err)
	}
	return nil
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
	// A RECORDING TAKES NO CONTEXT, and this is the frame where that
	// becomes visible. The recorder's bridge hands an observation straight
	// through from wherever the measurement was taken, which is often a
	// deferred timing on a path whose request context is already done —
	// and a histogram observation is arithmetic that has ALREADY happened,
	// so threading a cancellable context here would let a finished request
	// drop its own measurement on the way out. The directive sits at the
	// registration call, which is where contextcheck reports the chain.
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

// newMetricExporter builds the OTLP metric exporter for a resolved
// destination, passing everything [otlpenv] resolved explicitly for the reason
// [newExporter] gives — through the same translation, [exporterOptions].
func newMetricExporter(ctx context.Context, d otlpenv.Destination) (sdkmetric.Exporter, error) {
	opts, err := exporterOptions(d)
	if err != nil {
		return nil, err
	}
	switch d.Protocol {
	case otlpenv.GRPC:
		exp, err := otlpmetricgrpc.New(ctx, opts.metricGRPC()...)
		if err != nil {
			return nil, fmt.Errorf("otlp grpc metric exporter for %s=%s: %w", d.EndpointVar, d.Endpoint, err)
		}
		return exp, nil
	case otlpenv.HTTPProtobuf:
		exp, err := otlpmetrichttp.New(ctx, opts.metricHTTP()...)
		if err != nil {
			return nil, fmt.Errorf("otlp http metric exporter for %s=%s: %w", d.EndpointVar, d.Endpoint, err)
		}
		return exp, nil
	}
	// [otlpenv.Resolve] refuses any other protocol, so reaching here is a
	// protocol added to the closed set with no exporter built for it.
	return nil, fmt.Errorf("tracing: no metric exporter is built for protocol %q", d.Protocol)
}
