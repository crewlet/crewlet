package otlpenv_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/otlpenv"
)

// envOf is a lookup over a literal map, so no case mutates the process.
func envOf(kv map[string]string) func(string) string {
	return func(name string) string { return kv[name] }
}

func resolve(t *testing.T, s otlpenv.Signal, kv map[string]string) otlpenv.Destination {
	t.Helper()
	d, err := otlpenv.Resolve(envOf(kv), s)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", s, err)
	}
	return d
}

// THE VARIABLE NAMES ARE THE SPECIFICATION'S, spelled out once here rather
// than derived, because a derivation that drifted would drift every reader at
// once and agree with itself while it did.
func TestTheVariableNamesAreTheSpecifications(t *testing.T) {
	t.Parallel()
	for s, want := range map[otlpenv.Signal][4]string{
		otlpenv.Traces: {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_TRACES_EXPORTER"},
		otlpenv.Metrics: {"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_METRICS_EXPORTER"},
	} {
		got := [4]string{s.EndpointVar(), s.HeadersVar(), s.ProtocolVar(), s.ExporterVar()}
		if got != want {
			t.Errorf("%s reads %v, want %v", s, got, want)
		}
	}
	if otlpenv.EndpointVar != "OTEL_EXPORTER_OTLP_ENDPOINT" ||
		otlpenv.HeadersVar != "OTEL_EXPORTER_OTLP_HEADERS" ||
		otlpenv.ProtocolVar != "OTEL_EXPORTER_OTLP_PROTOCOL" {
		t.Error("a base variable name is not the specification's")
	}
}

// THE BASE IS A BASE AND THE SIGNAL VARIABLE IS A WHOLE URL. Conflating them is
// what put `/v1/traces/v1/traces` in front of a collector.
func TestTheEndpointVariablesKeepTheirSpecMeanings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		env     map[string]string
		signal  otlpenv.Signal
		want    string
		wantVar string
	}{
		{"the base gets the signal path",
			map[string]string{otlpenv.EndpointVar: "http://localhost:4318"},
			otlpenv.Traces, "http://localhost:4318/v1/traces", otlpenv.EndpointVar},
		{"every signal gets its own path",
			map[string]string{otlpenv.EndpointVar: "http://localhost:4318"},
			otlpenv.Metrics, "http://localhost:4318/v1/metrics", otlpenv.EndpointVar},
		{"a trailing slash on the base does not double up",
			map[string]string{otlpenv.EndpointVar: "http://localhost:4318/otlp/"},
			otlpenv.Metrics, "http://localhost:4318/otlp/v1/metrics", otlpenv.EndpointVar},
		{"the signal variable is used verbatim",
			map[string]string{
				otlpenv.EndpointVar:          "http://localhost:4318",
				otlpenv.Traces.EndpointVar(): "https://collector.example.com/custom",
			},
			otlpenv.Traces, "https://collector.example.com/custom", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"},
		{"another signal's variable is not this one's",
			map[string]string{
				otlpenv.EndpointVar:          "http://localhost:4318",
				otlpenv.Traces.EndpointVar(): "https://collector.example.com/custom",
			},
			otlpenv.Metrics, "http://localhost:4318/v1/metrics", otlpenv.EndpointVar},
		{"surrounding space is not part of the URL",
			map[string]string{otlpenv.EndpointVar: "  http://localhost:4318 \n"},
			otlpenv.Traces, "http://localhost:4318/v1/traces", otlpenv.EndpointVar},
		{"neither set exports nothing", nil, otlpenv.Traces, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := resolve(t, tc.signal, tc.env)
			if d.Endpoint != tc.want || d.EndpointVar != tc.wantVar {
				t.Errorf("endpoint = %q from %q, want %q from %q",
					d.Endpoint, d.EndpointVar, tc.want, tc.wantVar)
			}
			if d.Exports() != (tc.want != "") {
				t.Errorf("Exports() = %v with endpoint %q", d.Exports(), d.Endpoint)
			}
		})
	}
}

// A HEADER VALUE IS PERCENT-DECODED, as the specification and the SDK decode
// it. The documented `authorization=Bearer%20…` reached the backend with its
// %20 intact, and the backend refused every export.
//
// Mutation: drop the url.PathUnescape and this goes red on the first case.
func TestHeaderValuesArePercentDecoded(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]map[string]string{
		"authorization=Bearer%20secret": {"authorization": "Bearer secret"},
		"api-key=abc123, x-tenant = acme ,,": {
			"api-key": "abc123", "x-tenant": "acme"},
		// Decoded BEFORE it is trimmed, the SDK's order: an encoded space
		// at an end is still an end.
		"x=%20padded%20": {"x": "padded"},
		// A `+` is a plus, not a space — the path decoder, not a form's.
		"x=a+b": {"x": "a+b"},
		// The first `=` splits; the rest belong to the value.
		"authorization=Basic%20dXNlcjpwYXNz==": {"authorization": "Basic dXNlcjpwYXNz=="},
		"x=1,x=2":                              {"x": "2"},
	} {
		d := resolve(t, otlpenv.Traces, map[string]string{otlpenv.HeadersVar: raw})
		if !maps.Equal(d.Headers, want) {
			t.Errorf("%q resolved to %v, want %v", raw, d.Headers, want)
		}
		if len(d.Dropped) != 0 {
			t.Errorf("%q dropped %v", raw, d.Dropped)
		}
	}
}

// A SIGNAL'S OWN HEADERS REPLACE THE BASE SET WHOLESALE, never merge into it —
// the SDK applies the two as successive options and the second assigns the
// map. Merging here would send the base credential to a signal the operator
// gave a different one.
//
// Mutation: merge the signal's entries over the base's, and the tenant leaks
// into the metrics request below.
func TestASignalsOwnHeadersReplaceTheBaseSet(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		otlpenv.HeadersVar:           "x-tenant=acme,authorization=Bearer%20base",
		otlpenv.Metrics.HeadersVar(): "authorization=Bearer%20metrics",
	}
	if got, want := resolve(t, otlpenv.Metrics, env).Headers,
		map[string]string{"authorization": "Bearer metrics"}; !maps.Equal(got, want) {
		t.Errorf("metrics headers = %v, want %v", got, want)
	}
	if got, want := resolve(t, otlpenv.Traces, env).Headers,
		map[string]string{"x-tenant": "acme", "authorization": "Bearer base"}; !maps.Equal(got, want) {
		t.Errorf("traces headers = %v, want the base set %v", got, want)
	}

	// SET IS SET, even when nothing in it survives: the SDK assigns the
	// empty map too, so the base credential is not quietly reinstated.
	env[otlpenv.Metrics.HeadersVar()] = "garbage"
	if got := resolve(t, otlpenv.Metrics, env).Headers; len(got) != 0 {
		t.Errorf("an unusable signal variable fell back to the base: %v", got)
	}
}

// AN ENTRY THAT CANNOT BE USED IS DROPPED — as the SDK drops it — AND NAMED, by
// variable and position, never by value: the entry that fails to parse is most
// often a credential typed in the wrong shape, and this is reported in a log.
func TestAnUnusableHeaderIsDroppedAndNamedWithoutItsValue(t *testing.T) {
	t.Parallel()
	d := resolve(t, otlpenv.Traces, map[string]string{
		otlpenv.Traces.HeadersVar(): "good=1,Bearer sk-secret-one,bad name=sk-secret-two,api-key=%zzsk-secret-three,",
	})
	if !maps.Equal(d.Headers, map[string]string{"good": "1"}) {
		t.Errorf("headers = %v, want only the good one", d.Headers)
	}
	if len(d.Dropped) != 3 {
		t.Fatalf("dropped %d entries, want 3: %+v", len(d.Dropped), d.Dropped)
	}
	for i, want := range []otlpenv.Dropped{
		{Var: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", Entry: 2},
		{Var: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", Entry: 3},
		{Var: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", Entry: 4, Header: "api-key"},
	} {
		got := d.Dropped[i]
		if got.Var != want.Var || got.Entry != want.Entry || got.Header != want.Header {
			t.Errorf("dropped[%d] = %+v, want %+v", i, got, want)
		}
		if got.Reason == "" {
			t.Errorf("dropped[%d] says nothing about what to write instead", i)
		}
		if strings.Contains(got.Reason+got.Header, "sk-secret") {
			t.Errorf("dropped[%d] carries the entry's value: %+v", i, got)
		}
	}
}

// THE PROTOCOL IS THE SIGNAL'S OWN, ELSE THE BASE, ELSE THE SPECIFICATION'S
// DEFAULT — and case is not significant.
func TestTheProtocolResolvesPerSignal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want otlpenv.Protocol
	}{
		{"default", nil, otlpenv.HTTPProtobuf},
		{"base", map[string]string{otlpenv.ProtocolVar: "grpc"}, otlpenv.GRPC},
		{"case-insensitive", map[string]string{otlpenv.ProtocolVar: "GRPC"}, otlpenv.GRPC},
		{"the signal's own wins", map[string]string{
			otlpenv.ProtocolVar:          "grpc",
			otlpenv.Traces.ProtocolVar(): "http/protobuf",
		}, otlpenv.HTTPProtobuf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(t, otlpenv.Traces, tc.env).Protocol; got != tc.want {
				t.Errorf("protocol = %q, want %q", got, tc.want)
			}
		})
	}
}

// A PROTOCOL THIS BUILD DOES NOT SPEAK IS REFUSED, NAMING THE VARIABLE IT CAME
// FROM AND THE ONES THAT WORK. The SDK reads an unknown value as
// http/protobuf, so a typo aimed at a gRPC port exported nothing in silence.
func TestAnUnknownProtocolIsRefusedByTheVariableThatSaidIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env     map[string]string
		wantVar string
	}{
		{map[string]string{otlpenv.ProtocolVar: "http"}, otlpenv.ProtocolVar},
		{map[string]string{otlpenv.ProtocolVar: "http/json"}, otlpenv.ProtocolVar},
		{map[string]string{otlpenv.Traces.ProtocolVar(): "otlp"}, "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"},
	} {
		_, err := otlpenv.Resolve(envOf(tc.env), otlpenv.Traces)
		if err == nil {
			t.Errorf("%v was accepted", tc.env)
			continue
		}
		for _, want := range []string{tc.wantVar, "http/protobuf", "grpc"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %v does not name %q: %v", tc.env, want, err)
			}
		}
	}
}

// OTEL_<SIGNAL>_EXPORTER=none SWITCHES A SIGNAL OFF WHATEVER THE ENDPOINT SAYS,
// and anything but otlp or none is refused by name rather than read as otlp.
func TestTheExporterVariableSwitchesASignalOff(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		otlpenv.EndpointVar:          "http://collector.example.com:4318",
		otlpenv.Traces.ExporterVar(): "None",
	}
	if d := resolve(t, otlpenv.Traces, env); !d.Declined || d.Exports() {
		t.Errorf("OTEL_TRACES_EXPORTER=None did not switch traces off: %+v", d)
	}
	if d := resolve(t, otlpenv.Metrics, env); d.Declined || !d.Exports() {
		t.Errorf("declining traces switched metrics off too: %+v", d)
	}
	env[otlpenv.Traces.ExporterVar()] = "otlp"
	if d := resolve(t, otlpenv.Traces, env); !d.Exports() {
		t.Errorf("OTEL_TRACES_EXPORTER=otlp did not export: %+v", d)
	}

	for _, bad := range []string{"console", "zipkin", "otlp,console", "prometheus"} {
		env[otlpenv.Traces.ExporterVar()] = bad
		_, err := otlpenv.Resolve(envOf(env), otlpenv.Traces)
		if err == nil || !strings.Contains(err.Error(), "OTEL_TRACES_EXPORTER") {
			t.Errorf("OTEL_TRACES_EXPORTER=%q was not refused by name: %v", bad, err)
		}
	}
}

// AN ENDPOINT THE EXPORTERS CANNOT HONOUR AS WRITTEN IS REFUSED, NAMING THE
// VARIABLE AND NEVER QUOTING ITS VALUE.
//
// The SDK reads only the host and the path, so userinfo, a query or a fragment
// is dropped without a word — while the receiver's plain HTTP client would have
// sent userinfo as basic auth. And `localhost:4318` parses as a URL whose
// SCHEME is `localhost`, which exported to an empty host for as long as nobody
// looked.
func TestAnEndpointTheExporterCannotHonourIsRefused(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"localhost:4318",
		"collector.example.com",
		"ftp://collector.example.com",
		"http://",
		"http:collector.example.com",
		"https://user:sk-secret@collector.example.com",
		"https://collector.example.com/v1?api-key=sk-secret",
		"https://collector.example.com/?",
		"https://collector.example.com/#sk-secret",
		"http://[::1",
	} {
		for _, name := range []string{otlpenv.EndpointVar, otlpenv.Metrics.EndpointVar()} {
			_, err := otlpenv.Resolve(envOf(map[string]string{name: raw}), otlpenv.Metrics)
			if err == nil {
				t.Errorf("%s=%q was accepted", name, raw)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal of %q does not name %s: %v", raw, name, err)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("the refusal of an endpoint quotes its credential: %v", err)
			}
		}
	}
}

// A VARIABLE THAT IS NOT SET IS NOT AN ERROR, AND A VALID ONE STILL VALIDATES
// WHEN THE SIGNAL IS DECLINED: an endpoint that can never work is wrong whether
// or not this signal uses it today.
func TestAnInvalidEndpointIsRefusedEvenWhenDeclined(t *testing.T) {
	t.Parallel()
	_, err := otlpenv.Resolve(envOf(map[string]string{
		otlpenv.EndpointVar:          "localhost:4318",
		otlpenv.Traces.ExporterVar(): "none",
	}), otlpenv.Traces)
	if err == nil {
		t.Error("a malformed endpoint was accepted because the signal was declined")
	}
}

// EVERY SIGNAL RESOLVES, AND AN UNKNOWN SIGNAL IS NO SIGNAL: a signal is
// concatenated into a variable name and an upstream path, and an unchecked one
// would choose part of the address the engine's own credential is sent to.
// Logs is among the unknown: nothing in the engine sends them, so nothing
// reads their variables.
func TestEverySignalResolvesAndNoOtherDoes(t *testing.T) {
	t.Parallel()
	for _, s := range otlpenv.Signals {
		d := resolve(t, s, map[string]string{otlpenv.EndpointVar: "http://localhost:4318"})
		if d.Signal != s || d.Endpoint != "http://localhost:4318"+s.Path() {
			t.Errorf("%s resolved to %+v", s, d)
		}
	}
	for _, bad := range []otlpenv.Signal{"", "TRACES", "logs", "profiles", "traces/../../admin", ".."} {
		if bad.Valid() {
			t.Errorf("%q is accepted as a signal", bad)
		}
		if _, err := otlpenv.Resolve(envOf(nil), bad); err == nil {
			t.Errorf("%q resolved", bad)
		}
	}
}

// ONE DROPPED ENTRY, ONE REPORT. Two signals reading one base variable each
// drop the same entry, and a reader serving both should say so once.
func TestADroppedEntryIsReportedOnce(t *testing.T) {
	t.Parallel()
	env := map[string]string{otlpenv.HeadersVar: "oops"}
	if got := otlpenv.DroppedOf(resolve(t, otlpenv.Traces, env), resolve(t, otlpenv.Metrics, env)); len(got) != 1 {
		t.Errorf("DroppedOf = %+v, want one entry", got)
	}
}

// A DROPPED ENTRY IS REPORTED UNDER ONE NAME, the documented one, with the
// reader and the signals it was dropped for — and never its value. The sandbox
// receiver reported its drops as `sandbox_otel_header_dropped`, which no page
// named, so a search for the documented event found half of them.
//
// Mutation: report under any other name, or leave the signals off, and this
// goes red.
func TestADroppedEntryIsReportedUnderOneName(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		otlpenv.EndpointVar:          "http://localhost:4318",
		otlpenv.HeadersVar:           "authorization Bearer sk-base",
		otlpenv.Metrics.HeadersVar(): "x-key sk-metrics",
	}
	traces, metrics := resolve(t, otlpenv.Traces, env), resolve(t, otlpenv.Metrics, env)
	var out bytes.Buffer
	otlpenv.ReportDropped(t.Context(), slog.New(slog.NewJSONHandler(&out, nil)),
		otlpenv.ReaderExporter, traces, metrics)

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		lines = append(lines, rec)
	}
	if len(lines) != 2 {
		t.Fatalf("reported %d drops, want 2: %s", len(lines), out.String())
	}
	want := map[string]string{
		otlpenv.HeadersVar:           "traces",
		otlpenv.Metrics.HeadersVar(): "metrics",
	}
	for _, rec := range lines {
		if rec["msg"] != otlpenv.DroppedEvent || otlpenv.DroppedEvent != "otel_header_dropped" {
			t.Errorf("reported as %v, want %s", rec["msg"], "otel_header_dropped")
		}
		if rec["reader"] != otlpenv.ReaderExporter {
			t.Errorf("reported by %v", rec["reader"])
		}
		variable, _ := rec["var"].(string)
		if signals, ok := want[variable]; !ok || rec["signals"] != signals {
			t.Errorf("%s was reported for %v, want %q", variable, rec["signals"], signals)
		}
	}
	if strings.Contains(out.String(), "sk-") {
		t.Errorf("a report quotes a dropped entry's value: %s", out.String())
	}
}
