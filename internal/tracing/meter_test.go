package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE PROVIDER IS INSTALLED WITHOUT AN ENDPOINT, exactly as the TracerProvider
// is and for the same reason: with it unconditional, no measurement site
// branches on whether metrics are "on", and the operator record reads the same
// recorder a collector would.
//
// Mutation: install the provider only when an endpoint is set, and every
// instrument becomes a no-op on a deployment with no collector — which is
// every development deployment, so the code path nobody tests is the one
// everybody runs.
func TestTheProviderIsInstalledWithoutAnEndpoint(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	flush, err := Configure(t.Context(), Options{
		NodeID:   "node-a",
		Env:      func(string) string { return "" },
		Recorder: rec,
	})
	if err != nil {
		t.Fatalf("Configure with no endpoint: %v", err)
	}
	defer func() { _ = flush(t.Context()) }()

	// A METER THAT WORKS, not the API's no-op. The observable check is
	// that building an instrument through the global provider succeeds:
	// the no-op provider's Meter answers too, so the assertion below is on
	// the recorder having been WIRED, which the no-op cannot do.
	if otel.GetMeterProvider() == nil {
		t.Fatal("no MeterProvider is installed")
	}
	rec.Observe(metrics.StatelogBarrierDuration, 2*time.Millisecond,
		metrics.Attrs{"domain": "tracker"})
	got := rec.Read()
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("the recorder holds %d series after one observation, want 1", len(got))
	}
}

// collectingProvider registers the catalogue against a provider whose reader a
// test collects from on demand.
func collectingProvider(t *testing.T, rec *metrics.Recorder) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.WithoutCancel(t.Context())) })
	if err := registerInstruments(mp, rec); err != nil {
		t.Fatalf("registerInstruments: %v", err)
	}
	return reader
}

// collected is one collection from the reader, keyed by metric name.
func collected(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

// EVERY CATALOGUE ENTRY REACHES THE PROVIDER, as the number type it declares.
//
// COLLECTED FROM THE PROVIDER rather than read back from the recorder: the
// recorder keeps a series for every write whether or not the provider was told
// about the instrument, so a count of the recorder's series passes with nothing
// registered at all.
//
// Mutation: skip the gauges in [registerInstruments] and every gauge is missing
// from the collection; register a fractional counter as an integer one and it
// arrives as the wrong type.
func TestEveryInstrumentReachesTheProviderAsItsDeclaredType(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader := collectingProvider(t, rec)

	for _, inst := range metrics.Catalogue() {
		attrs := metrics.Attrs{}
		for _, a := range inst.Attributes {
			attrs[a] = "probe"
		}
		switch {
		case inst.Kind == metrics.KindCounter && inst.Fractional:
			rec.AddValue(inst.Name, 0.5, attrs)
		case inst.Kind == metrics.KindCounter:
			rec.Add(inst.Name, 1, attrs)
		case inst.Kind == metrics.KindGauge:
			rec.Set(inst.Name, 1, attrs)
		case inst.Kind == metrics.KindHistogram:
			rec.ObserveValue(inst.Name, 1, attrs)
		}
	}

	got := collected(t, reader)
	for _, inst := range metrics.Catalogue() {
		data, reached := got[inst.Name]
		if !reached {
			t.Errorf("%s (%s) never reached the provider: a collector would "+
				"show no such metric", inst.Name, inst.Kind)
			continue
		}
		var typed bool
		switch {
		case inst.Kind == metrics.KindCounter && inst.Fractional:
			_, typed = data.(metricdata.Sum[float64])
		case inst.Kind == metrics.KindCounter:
			// A COUNTER THAT COUNTS EVENTS STAYS AN INTEGER on the wire.
			_, typed = data.(metricdata.Sum[int64])
		case inst.Kind == metrics.KindGauge:
			_, typed = data.(metricdata.Gauge[float64])
		case inst.Kind == metrics.KindHistogram:
			_, typed = data.(metricdata.Histogram[float64])
		}
		if !typed {
			t.Errorf("%s (%s, fractional: %v) reached the provider as %T",
				inst.Name, inst.Kind, inst.Fractional, data)
		}
	}
}

// A SUB-SECOND CONTRIBUTION REACHES THE COLLECTOR, not just the operator record.
//
// The fractional counter sums the applier seconds bulk edits project, and each
// is a fraction of a second whenever the bulk is smaller than one second's
// drain. Observed as an int64, the total reaches the collector without its
// fraction, and the collector's number and the operator record's disagree.
//
// Mutation: register every counter as an Int64ObservableCounter observed with
// int64(s.Total), and the sum arrives as an integer; put the recorder's
// uint64(v) back and it arrives as zero.
func TestAFractionalCounterIsExportedWithItsFraction(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader := collectingProvider(t, rec)

	// 999 EIGHTHS OF A SECOND, a total a float64 holds exactly, so no
	// whole-number export can equal it.
	for range 999 {
		rec.AddValue(metrics.TrackerBulkApplySeconds, 0.125, nil)
	}
	rec.Add(metrics.StatelogReadServed, 3,
		metrics.Attrs{"domain": "tracker", "level": "linearizable"})

	got := collected(t, reader)
	occupancy, ok := got[metrics.TrackerBulkApplySeconds].(metricdata.Sum[float64])
	if !ok {
		t.Fatalf("%s was exported as %T, want a floating-point sum",
			metrics.TrackerBulkApplySeconds, got[metrics.TrackerBulkApplySeconds])
	}
	if len(occupancy.DataPoints) != 1 || occupancy.DataPoints[0].Value != 124.875 {
		t.Errorf("exported occupancy = %+v, want one point of 124.875 seconds",
			occupancy.DataPoints)
	}
	if !occupancy.IsMonotonic {
		t.Error("the occupancy was exported as a sum that can fall, which is " +
			"not a counter")
	}

	// AND A COUNTER THAT COUNTS EVENTS KEEPS ITS EXACT WHOLE NUMBER.
	served, ok := got[metrics.StatelogReadServed].(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s was exported as %T, want an integer sum",
			metrics.StatelogReadServed, got[metrics.StatelogReadServed])
	}
	if len(served.DataPoints) != 1 || served.DataPoints[0].Value != 3 {
		t.Errorf("exported reads served = %+v, want one point of 3", served.DataPoints)
	}
}

// THE EXPORTER IS SWITCHED OFF BY THE SPEC'S OWN VARIABLE, so an operator who
// wants traces and not metrics says so in the standard way rather than in one
// this engine invented.
func TestMetricsExporterCanBeDeclined(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		EndpointVar:        "http://collector.example.com:4318",
		MetricsExporterVar: "none",
	}
	opts := Options{Env: func(k string) string { return env[k] }}
	if metricsEnabled(opts) {
		t.Error("OTEL_METRICS_EXPORTER=none did not switch the exporter off")
	}
	delete(env, MetricsExporterVar)
	if !metricsEnabled(opts) {
		t.Error("an endpoint alone did not switch the exporter on")
	}
	if got, want := metricsEndpoint(opts),
		"http://collector.example.com:4318/v1/metrics"; got != want {
		t.Errorf("endpoint = %q, want %q — the same base the traces half reads, "+
			"because a company has one telemetry backend", got, want)
	}
}

// A BAD INTERVAL WARNS AND DEFAULTS, on the never-fail-on-a-typo rule the
// sampler ratio already follows.
func TestABadMetricIntervalDefaults(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]time.Duration{
		"":         defaultMetricInterval,
		"15000":    15 * time.Second,
		"nonsense": defaultMetricInterval,
		"-1":       defaultMetricInterval,
		"0":        defaultMetricInterval,
	} {
		opts := Options{Env: func(k string) string {
			if k == MetricsIntervalVar {
				return raw
			}
			return ""
		}}
		if got := metricInterval(opts); got != want {
			t.Errorf("interval for %q = %v, want %v", raw, got, want)
		}
	}
}
