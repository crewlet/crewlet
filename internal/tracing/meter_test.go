package tracing

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// installProbe is an instrument no catalogue declares, made through the GLOBAL
// provider so its arrival says which provider the global is.
const installProbe = "crewlet.test.install_probe"

// THE PROVIDER IS BUILT AND INSTALLED WITHOUT AN ENDPOINT, carrying the whole
// catalogue — exactly as the TracerProvider is, and for the same reason: with
// it unconditional, no measurement site branches on whether metrics are "on",
// and the operator record reads the same recorder a collector would.
//
// COLLECTED FROM THE PROVIDER [Configure] BUILDS, through a reader attached to
// it, rather than read back from the recorder: the recorder keeps a series for
// every write whether or not any provider was told about the instrument, so a
// count of its series passes with nothing registered at all.
//
// NOT PARALLEL, because it installs the process's global MeterProvider, which
// is what the probe is about.
//
// Mutation: drop the registerInstruments call from [configureMeter] and no
// catalogue entry is collected; drop its views and every histogram arrives on
// the SDK's default bounds; drop otel.SetMeterProvider from [Configure] and the
// probe made through the global provider never arrives.
func TestTheProviderIsInstalledWithoutAnEndpoint(t *testing.T) {
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader := sdkmetric.NewManualReader()
	flush, err := Configure(t.Context(), Options{
		NodeID:        "node-a",
		Env:           envOf(nil),
		Recorder:      rec,
		metricReaders: []sdkmetric.Reader{reader},
	})
	if err != nil {
		t.Fatalf("Configure with no endpoint: %v", err)
	}
	t.Cleanup(func() { _ = flush(context.WithoutCancel(t.Context())) })

	probe, err := otel.GetMeterProvider().Meter("test").Int64Counter(installProbe)
	if err != nil {
		t.Fatalf("build the probe through the global provider: %v", err)
	}
	probe.Add(t.Context(), 1)
	recordEveryInstrument(rec)

	got := collected(t, reader)
	if _, installed := got[installProbe]; !installed {
		t.Error("an instrument made through the global MeterProvider never " +
			"reached the provider Configure built, so the global is some other " +
			"provider — and nothing recorded through it reaches a collector")
	}
	for _, inst := range metrics.Catalogue() {
		assertExported(t, got, inst)
	}
}

// A SUB-SECOND CONTRIBUTION REACHES THE COLLECTOR, not just the recorder's own
// reading.
//
// The fractional counter sums the applier seconds bulk edits project, and each
// is a fraction of a second whenever the bulk is smaller than one second's
// drain. Observed as an int64, the total reaches the collector without its
// fraction, and the collector's number and the recorder's disagree.
//
// Mutation: register every counter as an Int64ObservableCounter observed with
// int64(s.Total), and the sum arrives as an integer; truncate each increment to
// a whole number in the recorder, and it arrives as zero.
func TestAFractionalCounterIsExportedWithItsFraction(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader := builtProvider(t, rec)

	// 999 EIGHTHS OF A SECOND, a total a float64 holds exactly, so no
	// whole-number export can equal it.
	for range 999 {
		rec.AddValue(metrics.TrackerBulkApplySeconds, 0.125, nil)
	}
	rec.Add(metrics.StatelogReadServed, 3,
		metrics.Attrs{"domain": "tracker", "level": "linearizable"})

	got := collected(t, reader)
	occupancy, ok := got[metrics.TrackerBulkApplySeconds].Data.(metricdata.Sum[float64])
	if !ok {
		t.Fatalf("%s was exported as %T, want a floating-point sum",
			metrics.TrackerBulkApplySeconds, got[metrics.TrackerBulkApplySeconds].Data)
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
	served, ok := got[metrics.StatelogReadServed].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s was exported as %T, want an integer sum",
			metrics.StatelogReadServed, got[metrics.StatelogReadServed].Data)
	}
	if len(served.DataPoints) != 1 || served.DataPoints[0].Value != 3 {
		t.Errorf("exported reads served = %+v, want one point of 3", served.DataPoints)
	}
}

// EACH HISTOGRAM REACHES THE COLLECTOR ON ITS OWN BUCKETS, not on one set
// shared by all of them.
//
// A whole backup outruns the default set, which ends near a minute, so the
// catalogue gives the backup histogram buckets of its own — and a collector
// that counted it on the default set would read every slow copy as "more than
// a minute", its p95 infinite, while the recorder's own reading held the real
// number. The barrier histogram beside it is the control: it declares none and
// must arrive on the default set.
//
// Mutation: register one view over every histogram with the default bounds and
// the backup duration arrives on them; drop the views and both arrive on the
// SDK's own.
func TestEachHistogramIsExportedOnItsOwnBuckets(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader := builtProvider(t, rec)
	rec.Observe(metrics.BackupDuration, 2*time.Hour, nil)
	rec.Observe(metrics.StatelogBarrierDuration, 3*time.Millisecond,
		metrics.Attrs{"domain": "tracker"})

	got := collected(t, reader)
	for name, want := range map[string][]float64{
		metrics.BackupDuration:          instrument(t, metrics.BackupDuration).Buckets(),
		metrics.StatelogBarrierDuration: metrics.DefaultBounds(),
	} {
		histogram, ok := got[name].Data.(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) != 1 {
			t.Fatalf("%s arrived as %+v, want one histogram point", name, got[name].Data)
		}
		if bounds := histogram.DataPoints[0].Bounds; !slices.Equal(bounds, want) {
			t.Errorf("%s arrived on bounds %v, want %v", name, bounds, want)
		}
	}
	// THE CONTROL on the case itself: were the backup's buckets the
	// default set, the comparison above could not tell one shared view from
	// a view per instrument.
	if slices.Equal(instrument(t, metrics.BackupDuration).Buckets(), metrics.DefaultBounds()) {
		t.Fatal("the backup histogram declares the default buckets, so this case " +
			"cannot tell a shared view from its own")
	}
}

// instrument is one catalogue entry by name.
func instrument(t *testing.T, name string) metrics.Instrument {
	t.Helper()
	for _, inst := range metrics.Catalogue() {
		if inst.Name == name {
			return inst
		}
	}
	t.Fatalf("%s is not in the catalogue", name)
	return metrics.Instrument{}
}

// builtProvider is the MeterProvider [configureMeter] builds with no endpoint,
// with a reader attached that a test collects from on demand. It installs
// nothing, so a test using it may run in parallel.
func builtProvider(t *testing.T, rec *metrics.Recorder) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp, err := configureMeter(t.Context(), Options{
		Env:           envOf(nil),
		metricReaders: []sdkmetric.Reader{reader},
	}, rec)
	if err != nil {
		t.Fatalf("configureMeter: %v", err)
	}
	t.Cleanup(func() { _ = shutdownMeter(context.WithoutCancel(t.Context()), mp) })
	return reader
}

// collected is one collection from the reader, keyed by metric name.
func collected(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// probeValue is what [recordEveryInstrument] writes to every attribute an
// instrument declares.
const probeValue = "probe"

// recordEveryInstrument writes once into every instrument in the catalogue,
// through the recorder method its kind takes: half a unit into a fractional
// counter, one into every other counter, gauge and histogram.
func recordEveryInstrument(rec *metrics.Recorder) {
	for _, inst := range metrics.Catalogue() {
		attrs := metrics.Attrs{}
		for _, a := range inst.Attributes {
			attrs[a] = probeValue
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
}

// assertExported checks that one catalogue entry reached the collection as the
// number type and the unit it declares, carrying the value and the attributes
// [recordEveryInstrument] wrote — and, for a histogram, on the recorder's own
// bucket boundaries.
func assertExported(t *testing.T, got map[string]metricdata.Metrics, inst metrics.Instrument) {
	t.Helper()
	m, reached := got[inst.Name]
	if !reached {
		t.Errorf("%s (%s) never reached the provider: a collector would show "+
			"no such metric", inst.Name, inst.Kind)
		return
	}
	if m.Unit != inst.Unit {
		t.Errorf("%s arrived in %q, and the catalogue declares %q — the unit "+
			"is what a collector converts by", inst.Name, m.Unit, inst.Unit)
	}
	var points []attribute.Set
	switch {
	case inst.Kind == metrics.KindCounter && inst.Fractional:
		points = sumPoints[float64](t, inst, m.Data, 0.5)
	case inst.Kind == metrics.KindCounter:
		// A COUNTER THAT COUNTS EVENTS STAYS AN INTEGER on the wire.
		points = sumPoints[int64](t, inst, m.Data, 1)
	case inst.Kind == metrics.KindGauge:
		points = gaugePoints(t, inst, m.Data)
	case inst.Kind == metrics.KindHistogram:
		points = histogramPoints(t, inst, m.Data)
	default:
		t.Errorf("%s is a %q, which this test does not know how to check",
			inst.Name, inst.Kind)
	}
	for _, set := range points {
		if set.Len() != len(inst.Attributes) {
			t.Errorf("%s arrived with attributes %v, want exactly %v",
				inst.Name, set.ToSlice(), inst.Attributes)
			continue
		}
		for _, a := range inst.Attributes {
			if v, ok := set.Value(attribute.Key(a)); !ok || v.AsString() != probeValue {
				t.Errorf("%s arrived with %s = %v, want %q",
					inst.Name, a, v.String(), probeValue)
			}
		}
	}
}

// wrongType reports an instrument that reached the provider as a different
// aggregation from the one its catalogue entry declares.
func wrongType(t *testing.T, inst metrics.Instrument, data metricdata.Aggregation) {
	t.Helper()
	t.Errorf("%s (%s, fractional: %v) reached the provider as %T",
		inst.Name, inst.Kind, inst.Fractional, data)
}

// sumPoints checks a counter arrived as a monotonic sum of N holding one point
// of want, and returns its points' attributes.
func sumPoints[N int64 | float64](t *testing.T, inst metrics.Instrument,
	data metricdata.Aggregation, want N) []attribute.Set {

	t.Helper()
	sum, ok := data.(metricdata.Sum[N])
	if !ok {
		wrongType(t, inst, data)
		return nil
	}
	if !sum.IsMonotonic || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != want {
		t.Errorf("%s arrived as %+v (monotonic: %v), want one monotonic point of %v",
			inst.Name, sum.DataPoints, sum.IsMonotonic, want)
	}
	out := make([]attribute.Set, 0, len(sum.DataPoints))
	for _, p := range sum.DataPoints {
		out = append(out, p.Attributes)
	}
	return out
}

// gaugePoints checks a gauge arrived as a floating-point gauge holding one point
// of 1, and returns its points' attributes.
func gaugePoints(t *testing.T, inst metrics.Instrument,
	data metricdata.Aggregation) []attribute.Set {

	t.Helper()
	gauge, ok := data.(metricdata.Gauge[float64])
	if !ok {
		wrongType(t, inst, data)
		return nil
	}
	if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 1 {
		t.Errorf("%s arrived as %+v, want one point of 1", inst.Name, gauge.DataPoints)
	}
	out := make([]attribute.Set, 0, len(gauge.DataPoints))
	for _, p := range gauge.DataPoints {
		out = append(out, p.Attributes)
	}
	return out
}

// histogramPoints checks a histogram arrived holding one observation on the
// recorder's own bucket boundaries, and returns its points' attributes.
func histogramPoints(t *testing.T, inst metrics.Instrument,
	data metricdata.Aggregation) []attribute.Set {

	t.Helper()
	histogram, ok := data.(metricdata.Histogram[float64])
	if !ok {
		wrongType(t, inst, data)
		return nil
	}
	if len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 {
		t.Errorf("%s arrived as %+v, want one point holding one observation",
			inst.Name, histogram.DataPoints)
	}
	out := make([]attribute.Set, 0, len(histogram.DataPoints))
	for _, p := range histogram.DataPoints {
		// THE INSTRUMENT'S OWN BOUNDARIES, so a bucket on a collector's
		// panel is the bucket the recorder counted into.
		if !slices.Equal(p.Bounds, inst.Buckets()) {
			t.Errorf("%s arrived on bounds %v, want its own %v",
				inst.Name, p.Bounds, inst.Buckets())
		}
		out = append(out, p.Attributes)
	}
	return out
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
