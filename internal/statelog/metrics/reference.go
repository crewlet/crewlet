package metrics

import (
	"fmt"
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
		b.WriteString("\n| Metric | Unit | Attributes | What it makes visible |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, inst := range Catalogue() {
			if inst.Kind != kind {
				continue
			}
			attrs := "—"
			if len(inst.Attributes) > 0 {
				attrs = "`" + strings.Join(inst.Attributes, "`, `") + "`"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
				inst.Name, inst.Unit, attrs, inst.Shows)
		}
	}
	b.WriteString(referenceFooter)
	return b.String()
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
		return "A distribution, exported with the engine's own bucket " +
			"boundaries: twenty-one powers of two from 64 µs to 64 s, which " +
			"resolves a percentile to within a factor of two at every scale " +
			"here — from a 40 µs index probe to a 16-second bulk apply. The " +
			"SDK's default boundaries stop at 10 s, so a slow apply would " +
			"land in an overflow bucket and read as \"at least 10 s\" for ever.\n"
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

**Rates.** Every counter is a monotonic total and your collector divides. A
rate computed in this process would be a rate over a window nobody chose,
disagreeing with the one on your dashboard.

**A ` + "`/metrics`" + ` route.** OTLP reaches Prometheus through the collector you
already run for traces. A second wire format would be a second thing to
authenticate on an API whose guard and whose exemptions are both load-bearing.

**Per-object series.** See the attribute rule above.
`
