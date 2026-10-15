package tracing

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// unanswering is a collector that reads an export and never answers it, until
// the exporter gives up: how long an export lasts is then exactly the bound it
// was held to. It records how long the exporter held each request open, which
// is the bound on one ATTEMPT where an export retries.
type unanswering struct {
	url  string
	mu   sync.Mutex
	held []time.Duration
}

func newUnanswering(t *testing.T) *unanswering {
	t.Helper()
	c := &unanswering{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		c.mu.Lock()
		c.held = append(c.held, time.Since(start))
		c.mu.Unlock()
	}))
	t.Cleanup(server.Close)
	c.url = server.URL
	return c
}

// longest is the longest any request was held open.
func (c *unanswering) longest() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out time.Duration
	for _, d := range c.held {
		out = max(out, d)
	}
	return out
}

// ONE TIMEOUT BOUNDS EVERY LAYER OF AN EXPORT, and it is the operator's.
//
// An export passes three bounds — the exporter's own on each attempt, the
// batch span processor's around the export, and for metrics the periodic
// reader's — and each used to read a number of its own: the batch processor's
// was fixed at five seconds while the SDK read OTEL_EXPORTER_OTLP_TIMEOUT for
// the exporter by itself, so a timeout above five seconds did nothing and
// nothing said so. Against a collector that never answers, the final flush
// lasts exactly as long as the bound it is held to, so each case measures it: a
// long trace timeout (past the five seconds the batch processor once imposed,
// inside the flush), and a short one per signal (well inside every default).
//
// And the exporter's own bound on an attempt is the RESOLVED one, never the
// SDK's reading of the process environment: with the process saying one thing
// and the resolution another, the collector sees the attempt held as long as
// the resolution says.
//
// Not parallel: Configure installs the process's providers, and one case sets
// the process environment.
//
// Mutations: bound the batch processor by the default again (the long case
// ends at five seconds); bound the metric shutdown by the flush alone (the
// metric case waits it out); hand the trace exporter no timeout of its own (the
// process's case is cut at the process's value).
func TestOneTimeoutBoundsEveryLayerOfAnExport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		process string
		within  [2]time.Duration
		attempt time.Duration
		metrics bool
	}{
		{name: "a trace timeout past the old five-second cap",
			env:    map[string]string{otlpenv.Traces.TimeoutVar(): "6500"},
			within: [2]time.Duration{6400 * time.Millisecond, flushGrace - 500*time.Millisecond}},
		{name: "a short trace timeout",
			env:    map[string]string{otlpenv.TimeoutVar: "300"},
			within: [2]time.Duration{250 * time.Millisecond, 2 * time.Second}},
		{name: "a short metric timeout", metrics: true,
			env:    map[string]string{otlpenv.Metrics.TimeoutVar(): "300"},
			within: [2]time.Duration{250 * time.Millisecond, 2 * time.Second}},
		{name: "the process environment saying otherwise",
			env:     map[string]string{otlpenv.Traces.TimeoutVar(): "1500"},
			process: "100",
			within:  [2]time.Duration{1400 * time.Millisecond, 3 * time.Second},
			attempt: 1400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.process != "" {
				// What the SDK would read for itself, were it asked.
				t.Setenv(otlpenv.TimeoutVar, tc.process)
			}
			collector := newUnanswering(t)
			env := map[string]string{otlpenv.EndpointVar: collector.url}
			for k, v := range tc.env {
				env[k] = v
			}
			declined := otlpenv.Metrics
			if tc.metrics {
				declined = otlpenv.Traces
			}
			env[declined.ExporterVar()] = "none"
			rec, err := metrics.New()
			if err != nil {
				t.Fatal(err)
			}
			shutdown := install(t, Options{Env: envOf(env), Recorder: rec})
			if tc.metrics {
				for _, inst := range metrics.Catalogue() {
					if inst.Kind == metrics.KindCounter {
						attrs := metrics.Attrs{}
						for _, a := range inst.Attributes {
							attrs[a] = "probe"
						}
						rec.Add(inst.Name, 1, attrs)
						break
					}
				}
			} else {
				_, span := Start(context.Background(), "test", "an.operation")
				span.End()
			}

			start := time.Now()
			_ = shutdown(context.Background()) // an export that was cut off is the point
			if took := time.Since(start); took < tc.within[0] || took > tc.within[1] {
				t.Errorf("the final export lasted %v against a collector that never "+
					"answers, want %v to %v — the timeout the environment set",
					took.Round(time.Millisecond), tc.within[0], tc.within[1])
			}
			if held := collector.longest(); held < tc.attempt {
				t.Errorf("the collector held an attempt open for %v at most, want at least "+
					"%v — an attempt was bounded by something other than the resolution",
					held.Round(time.Millisecond), tc.attempt)
			}
		})
	}
}

// A VARIABLE AN EXPLICIT SETTING SUPERSEDES IS REFUSED BY NAME, rather than
// read by the SDK and then overridden in silence: the batch processor's and the
// metric reader's own export timeouts, superseded by the one OTLP timeout, and
// the metric exporter's default histogram aggregation, superseded by the view
// every histogram is registered under.
//
// Mutation: drop any one from refuseSuperseded and its case boots.
func TestASupersededVariableIsRefusedByName(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		BSPExportTimeoutVar:     "60000",
		MetricExportTimeoutVar:  "60000",
		HistogramAggregationVar: "base2_exponential_bucket_histogram",
	} {
		_, err := Configure(context.Background(), Options{Env: envOf(map[string]string{name: value})})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: Configure = %v, want a refusal naming it", name, value, err)
		}
	}
}

// A TIMEOUT THAT WILL NOT BOUND AN EXPORT AS WRITTEN IS SAID SO AT BOOT: one
// set aside as not whole positive milliseconds, named once though both
// signals read it, and one past the final flush, which a shutdown cuts short,
// named per signal that exports.
//
// Not parallel: it captures the process-wide logger.
//
// Mutations: stop reporting what was set aside, or report it per signal (the
// count); compare against the default rather than the resolved value (the
// flush case).
func TestATimeoutThatWillNotBoundAnExportIsReported(t *testing.T) {
	logs := captureLogs(t)
	reportTimeouts(t.Context(), mustResolve(t, otlpenv.Traces, map[string]string{
		otlpenv.EndpointVar: "http://localhost:4318", otlpenv.TimeoutVar: "5s",
	}), mustResolve(t, otlpenv.Metrics, map[string]string{
		otlpenv.EndpointVar: "http://localhost:4318", otlpenv.TimeoutVar: "5s",
	}))
	if n := strings.Count(logs.String(), "otel_export_timeout_invalid"); n != 1 {
		t.Errorf("a base timeout both signals set aside was reported %d times, want once: %s",
			n, logs.String())
	}

	logs = captureLogs(t)
	reportTimeouts(t.Context(), mustResolve(t, otlpenv.Traces, map[string]string{
		otlpenv.EndpointVar: "http://localhost:4318", otlpenv.Traces.TimeoutVar(): "30000",
	}), mustResolve(t, otlpenv.Metrics, map[string]string{
		otlpenv.EndpointVar: "http://localhost:4318",
	}))
	out := logs.String()
	if strings.Count(out, "otel_export_timeout_past_flush") != 1 || !strings.Contains(out, "signal=traces") {
		t.Errorf("a trace timeout past the flush, and a metric one inside it, reported: %s", out)
	}
}

func mustResolve(t *testing.T, s otlpenv.Signal, env map[string]string) otlpenv.Destination {
	t.Helper()
	d, err := otlpenv.Resolve(envOf(env), s)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", s, err)
	}
	return d
}

// lockedBuffer is a log destination two goroutines may write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs points the process's logger at a buffer for the rest of the test
// and puts back the destination logging's own init installs.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, out)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return out
}
