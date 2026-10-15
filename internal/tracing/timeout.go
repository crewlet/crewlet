package tracing

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/otlpenv"
)

// The variables the OpenTelemetry SDK would read to bound or shape an export a
// SECOND time, beside what [otlpenv] resolves — each superseded by a value
// this package hands the SDK explicitly, so the SDK's own reading of it never
// takes effect.
//
// A superseded BOUND is refused and a superseded SHAPE is reported, and the
// line between them is whether the operator was promised something else. A
// second timeout is a second key for a value the OTLP timeout already sets,
// and an export cut off at one bound while the deployment believes another is
// the disagreement Tier A refuses everywhere. A histogram aggregation promises
// nothing about delivery: every histogram is still exported, at the
// boundaries the engine records it at, and refusing to boot over a shape is
// refusing the vendor snippet ADR-0010 exists to let an operator paste.
const (
	// BSPExportTimeoutVar is the batch span processor's bound on one
	// export, superseded by [exportTimeout].
	BSPExportTimeoutVar = "OTEL_BSP_EXPORT_TIMEOUT"

	// MetricExportTimeoutVar is the periodic metric reader's bound on one
	// collect-and-export, superseded by [exportTimeout].
	MetricExportTimeoutVar = "OTEL_METRIC_EXPORT_TIMEOUT"

	// HistogramAggregationVar is the metric exporter's default histogram
	// aggregation, superseded by the view every histogram is registered
	// under ([configureMeter]): a view's aggregation wins over a reader's
	// default, so the recorder's own boundaries are what every histogram is
	// bucketed by whatever this says.
	HistogramAggregationVar = "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION"
)

// refuseSuperseded refuses a variable this process can never honour because
// an explicit setting supersedes the SDK's reading of it — by name, saying
// what decides instead, which is this package's rule for a value that can
// never work.
//
// NOT IGNORED IN SILENCE, which is what the SDK would do with each: it reads
// the variable and an explicit option then replaces what it read, so a
// deployment that set OTEL_BSP_EXPORT_TIMEOUT=60000 for a slow collector
// believes every export waits a minute while each is cut off at the
// exporter's own bound.
func refuseSuperseded(opts Options) error {
	for _, v := range []struct{ name, instead string }{
		{BSPExportTimeoutVar, "each trace export is bounded by " +
			otlpenv.Traces.TimeoutVar() + ", else " + otlpenv.TimeoutVar +
			", which every layer of an export is held to"},
		{MetricExportTimeoutVar, "each metric export is bounded by " +
			otlpenv.Metrics.TimeoutVar() + ", else " + otlpenv.TimeoutVar +
			", which every layer of an export is held to"},
	} {
		if opts.env(v.name) != "" {
			return fmt.Errorf("%s is not honoured: %s — unset %s", v.name, v.instead, v.name)
		}
	}
	return nil
}

// reportUnhonoured warns about a variable that shapes an export the engine
// shapes itself, so an operator who set it reads at boot that it changed
// nothing, rather than finding out from the backend. See the const block for
// why this one is reported and the timeouts are refused.
func reportUnhonoured(ctx context.Context, opts Options) {
	if v := opts.env(HistogramAggregationVar); v != "" {
		log.WarnContext(ctx, "otel_variable_not_honoured", "var", HistogramAggregationVar,
			"value", v, "detail", "every histogram the engine records is exported at "+
				"its own fixed boundaries, which a collector sums as they are; unset it "+
				"to silence this")
	}
}

// reportTimeouts warns about the timeout each destination resolved that will
// not bound its export as written: a value [otlpenv] set aside, and one longer
// than the final flush, which a shutdown cuts short.
//
// A value set aside in a variable both signals read — the base — is named
// once; a timeout past the flush is named per signal, since each signal's
// last export is cut on its own.
func reportTimeouts(ctx context.Context, ds ...otlpenv.Destination) {
	reported := map[otlpenv.Ignored]bool{}
	for _, d := range ds {
		for _, x := range d.Ignored {
			if reported[x] {
				continue
			}
			reported[x] = true
			log.WarnContext(ctx, "otel_export_timeout_invalid", "var", x.Var, "value", x.Value,
				"reason", x.Reason, "detail", fmt.Sprintf("set aside; the next timeout "+
					"variable decides, else the %s default", defaultExportTimeout))
		}
	}
	for _, d := range ds {
		if d.Exports() && exportTimeout(d) > flushGrace {
			log.WarnContext(ctx, "otel_export_timeout_past_flush", "signal", d.Signal,
				"var", d.TimeoutVar, "timeout", d.Timeout, "flush", flushGrace,
				"detail", "an export may wait this long while the engine runs, but the "+
					"final flush at shutdown is bounded by the flush, so the last "+
					"export can be cut short")
		}
	}
}
