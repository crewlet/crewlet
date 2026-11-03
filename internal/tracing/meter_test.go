package tracing

import (
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	otelmetric "go.opentelemetry.io/otel/metric"
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

// EVERY CATALOGUE ENTRY REGISTERS, and a histogram's sink is bound rather than
// silently absent.
func TestEveryInstrumentRegistersAgainstTheProvider(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mp, err := configureMeter(t.Context(), Options{
		Env: func(string) string { return "" },
	}, rec)
	if err != nil {
		t.Fatalf("configureMeter: %v", err)
	}
	defer func() { _ = shutdownMeter(t.Context(), mp) }()

	// Recording into every instrument must reach the recorder, whatever
	// its kind — a kind the registration forgot would silently drop.
	for _, inst := range metrics.Catalogue() {
		attrs := metrics.Attrs{}
		for _, a := range inst.Attributes {
			attrs[a] = "probe"
		}
		switch inst.Kind {
		case metrics.KindCounter:
			rec.Add(inst.Name, 1, attrs)
		case metrics.KindGauge:
			rec.Set(inst.Name, 1, attrs)
		case metrics.KindHistogram:
			rec.ObserveValue(inst.Name, 1, attrs)
		}
	}
	if got, want := len(rec.Read()), len(metrics.Catalogue()); got != want {
		t.Errorf("%d series after recording into every instrument, want %d: an "+
			"instrument that did not register drops silently", got, want)
	}
}

// A HISTOGRAM IS EXPORTED IN ITS UNIT'S BUCKETS — the boundaries the recorder
// counted it into and an alarm read it against, so a collector's panel and the
// operator record bucket one observation alike. Every histogram in the
// catalogue is recorded once through a provider built with the engine's views,
// collected, and its exported boundaries compared with metrics.Bounds.
//
// Mutation: one view handing every histogram one set, and the byte and count
// histograms export in the millisecond set; no views, and the SDK's defaults.
func TestAHistogramIsExportedInItsUnitsBuckets(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(histogramViews()...))
	defer func() { _ = mp.Shutdown(t.Context()) }()
	meter := mp.Meter("crewlet")
	units := map[string]string{}
	for _, inst := range metrics.Catalogue() {
		if inst.Kind != metrics.KindHistogram {
			continue
		}
		h, err := meter.Float64Histogram(inst.Name, otelmetric.WithUnit(inst.Unit))
		if err != nil {
			t.Fatalf("histogram %s: %v", inst.Name, err)
		}
		h.Record(t.Context(), 1)
		units[inst.Name] = inst.Unit
	}
	var got metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &got); err != nil {
		t.Fatalf("collect: %v", err)
	}
	seen := map[string]bool{}
	for _, scope := range got.ScopeMetrics {
		for _, m := range scope.Metrics {
			unit, ok := units[m.Name]
			if !ok {
				continue
			}
			data, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(data.DataPoints) != 1 {
				t.Errorf("%s exported %T, want one float histogram point", m.Name, m.Data)
				continue
			}
			want, _ := metrics.Bounds(unit)
			if !slices.Equal(data.DataPoints[0].Bounds, want) {
				t.Errorf("%s (%s) exported buckets %v, want its unit's boundaries %v", m.Name, unit,
					data.DataPoints[0].Bounds, want)
			}
			seen[m.Name] = true
		}
	}
	if len(seen) != len(units) || len(units) == 0 {
		t.Errorf("collected %d of the catalogue's %d histograms", len(seen), len(units))
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
