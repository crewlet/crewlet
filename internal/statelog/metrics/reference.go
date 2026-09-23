package metrics

import (
	"fmt"
	"strconv"
	"strings"
)

// Reference renders the catalogue as the published markdown page.
//
// GENERATED, and a test regenerates and diffs — the same idiom schema/ uses,
// and for the same reason: a reference maintained by hand is one that stops
// matching the code, and a metric whose meaning lives only in the source is
// one an operator cannot act on.
func Reference() string {
	var b strings.Builder
	b.WriteString(referenceHeader)

	for _, kind := range []Kind{KindHistogram, KindGauge, KindCounter} {
		b.WriteString("\n## " + sectionTitle(kind) + "\n\n")
		b.WriteString(sectionBlurb(kind))
		// A HISTOGRAM'S BUCKETS ARE A COLUMN OF ITS OWN, because they are
		// per instrument: a quantile read off a panel is only as fine as
		// the boundaries it was counted against.
		if kind == KindHistogram {
			b.WriteString("\n| Metric | Unit | Buckets | Attributes | What it makes visible |\n")
			b.WriteString("|---|---|---|---|---|\n")
		} else {
			b.WriteString("\n| Metric | Unit | Attributes | What it makes visible |\n")
			b.WriteString("|---|---|---|---|\n")
		}
		for _, inst := range Catalogue() {
			if inst.Kind != kind {
				continue
			}
			attrs := "—"
			if len(inst.Attributes) > 0 {
				attrs = "`" + strings.Join(inst.Attributes, "`, `") + "`"
			}
			if kind == KindHistogram {
				fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n",
					inst.Name, inst.Unit, bucketsOf(inst), attrs, inst.Shows)
				continue
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
				inst.Name, inst.Unit, attrs, inst.Shows)
		}
	}
	b.WriteString(referenceFooter)
	return b.String()
}

// bucketsOf names a histogram's boundaries for the reference: the default set
// by name, and an instrument's own by its span and how many there are.
func bucketsOf(inst Instrument) string {
	if inst.Bounds == nil {
		return "default"
	}
	bounds := inst.Buckets()
	return fmt.Sprintf("%d, from %s to %s", len(bounds),
		strconv.FormatFloat(bounds[0], 'f', -1, 64),
		strconv.FormatFloat(bounds[len(bounds)-1], 'f', -1, 64))
}

func sectionTitle(k Kind) string {
	switch k {
	case KindHistogram:
		return "Histograms"
	case KindGauge:
		return "Gauges"
	default:
		return "Counters"
	}
}

func sectionBlurb(k Kind) string {
	switch k {
	case KindHistogram:
		return "A distribution, exported with each instrument's own bucket " +
			"boundaries, so a bucket on your panel is the bucket the engine " +
			"counted into. The default set is twenty-one powers of two from " +
			"1/16 to 65 536 in the instrument's own unit — 62.5 µs to about " +
			"65.5 s for a duration in milliseconds — so a percentile read " +
			"from them is within a factor of two anywhere in that range. An " +
			"instrument whose range that does not cover declares its own, " +
			"listed in its row. The SDK's default boundaries stop at " +
			"10 000, which for a duration in milliseconds is 10 s: anything " +
			"slower would land in the overflow bucket and read only as " +
			"\"more than 10 s\".\n"
	case KindGauge:
		return "A value that goes both ways, sampled at each export.\n"
	default:
		return "Only ever rises. Rates and totals are your collector's " +
			"arithmetic, never this engine's.\n"
	}
}

const referenceHeader = `# Metrics

**Generated from ` + "`metrics.Catalogue()`" + `. Do not edit — change the catalogue.**

The engine exports OpenTelemetry metrics through the same ` + "`OTEL_*`" + ` environment
its traces use, because a company has one telemetry backend and two settings
would let you split signals across two of them where a correlation resolves on
neither:

| Variable | What it does |
|---|---|
| ` + "`OTEL_EXPORTER_OTLP_ENDPOINT`" + ` | The collector. Metrics are posted to ` + "`<endpoint>/v1/metrics`" + `. |
| ` + "`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`" + ` | Overrides the base for metrics alone. |
| ` + "`OTEL_EXPORTER_OTLP_PROTOCOL`" + ` | ` + "`http/protobuf`" + ` (default) or ` + "`grpc`" + `. |
| ` + "`OTEL_EXPORTER_OTLP_HEADERS`" + ` | Sent with every export — an API key, a tenant. |
| ` + "`OTEL_METRICS_EXPORTER`" + ` | ` + "`none`" + ` switches the export off and leaves traces on. |
| ` + "`OTEL_METRIC_EXPORT_INTERVAL`" + ` | Export period in milliseconds. Default 60000. |

**The provider is installed whether or not you are collecting.** Every
instrument exists either way, so no code path in the engine branches on
whether metrics are "on", and the numbers on ` + "`crewlet retention status`" + ` are
read from the same recorder a collector would be reading. With no endpoint set
the measurements are taken and not shipped.

**Every attribute below is a closed set.** No metric carries a task key, a
subject id, a page title or a node id as an attribute — the node is the
resource, set once — so the number of time series is bounded by this page
rather than by how much work your company has done.
`

const referenceFooter = `
## What is deliberately not here

**Rates, bar the applier's own.** Every counter is a monotonic total and your
collector divides: a rate computed in this process would be a rate over a
window nobody chose, disagreeing with the one on your dashboard. The two
` + "`crewlet.statelog.drain.*`" + ` gauges are the exception, because the engine needs
the rate itself: it divides a record backlog by the records-a-second drain to
state that backlog as a time — against a stale read's staleness bound, in a
refused read's retry hint and in a bulk edit's projection — and the commits a
second beside it is measured over the same apply runs. Both are smoothed across
those runs rather than taken over a window.

**A ` + "`/metrics`" + ` route.** OTLP reaches Prometheus through the collector you
already run for traces. A second wire format would be a second thing to
authenticate on an API whose guard and whose exemptions are both load-bearing.

**Per-object series.** See the attribute rule above.
`
