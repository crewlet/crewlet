package tracing_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE ENGINE'S EXPORTERS AND THE SANDBOX RECEIVER RESOLVE ONE DESTINATION —
// ADR-0010's gate.
//
// The receiver forwards a coding box's telemetry to the backend the engine
// exports to, so the box's spans nest under the turn that started them; the
// link resolves only if both halves send a signal to the same place with the
// same credential. They were meant to agree because they read the same
// variables, and they disagreed three ways while a test asserting the two
// VARIABLE NAMES passed: the receiver ignored the signal endpoint and the
// protocol, and neither decoded a header value.
//
// So for each environment the engine's traces destination must equal the
// receiver's upstream — endpoint, headers, protocol, compression, TLS, whether
// the signal was declined, and what was dropped — and a value one half refuses
// the other must refuse too. Traces are the one signal both halves send: the
// receiver forwards a box's traces and nothing else. Each half's own wire
// behaviour is held to its resolution beside it: TestTheExporterSendsWhatThe-
// ResolverResolved here, and internal/sandbox's TestTracesAreForwardedToThe-
// TracesDestination.
//
// Mutation: have the receiver resolve the traces endpoint from the base
// variable alone, and the signal-endpoint cases go red; have it skip header
// decoding, and the percent-encoded case does.
func TestTheExporterAndTheReceiverResolveOneDestination(t *testing.T) {
	t.Parallel()
	const base = "http://collector.example.com:4318"
	ca := certificateFile(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"nothing configured", nil},
		{"the base alone", map[string]string{otlpenv.EndpointVar: base}},
		{"a trailing slash on the base", map[string]string{otlpenv.EndpointVar: base + "/otlp/"}},
		{"signal endpoints beside the base", map[string]string{
			otlpenv.EndpointVar:           base,
			otlpenv.Traces.EndpointVar():  "https://traces.example.com/custom",
			otlpenv.Metrics.EndpointVar(): "https://metrics.example.com",
		}},
		{"a signal endpoint and no base", map[string]string{
			otlpenv.Traces.EndpointVar(): "https://traces.example.com/v1/traces",
		}},
		{"a percent-encoded header", map[string]string{
			otlpenv.EndpointVar: base,
			otlpenv.HeadersVar:  "authorization=Bearer%20secret, x-tenant = acme",
		}},
		{"per-signal headers", map[string]string{
			otlpenv.EndpointVar:          base,
			otlpenv.HeadersVar:           "x-tenant=acme",
			otlpenv.Traces.HeadersVar():  "authorization=Bearer%20traces",
			otlpenv.Metrics.HeadersVar(): "authorization=Bearer%20metrics",
		}},
		{"a dropped header entry", map[string]string{
			otlpenv.EndpointVar: base,
			otlpenv.HeadersVar:  "authorization Bearer secret,ok=1",
		}},
		{"grpc", map[string]string{
			otlpenv.EndpointVar: "https://collector.example.com:4317",
			otlpenv.ProtocolVar: "grpc",
		}},
		{"a per-signal protocol", map[string]string{
			otlpenv.EndpointVar:           base,
			otlpenv.Metrics.ProtocolVar(): "GRPC",
		}},
		{"signals declined", map[string]string{
			otlpenv.EndpointVar:           base,
			otlpenv.Traces.ExporterVar():  "none",
			otlpenv.Metrics.ExporterVar(): "none",
		}},
		{"gzip, and a signal's own choice", map[string]string{
			otlpenv.EndpointVar:              base,
			otlpenv.CompressionVar:           "gzip",
			otlpenv.Metrics.CompressionVar(): "none",
		}},
		{"a private authority for an https collector", map[string]string{
			otlpenv.EndpointVar:          "https://collector.example.com:4318",
			otlpenv.CertificateVar:       ca,
			otlpenv.Traces.InsecureVar(): "false",
		}},
		{"an https scheme in upper case", map[string]string{
			otlpenv.EndpointVar:          "HTTPS://collector.example.com:4318",
			otlpenv.CertificateVar:       ca,
			otlpenv.Traces.InsecureVar(): "false",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := lookup(tc.env)
			receiver, err := sandbox.BuildOtelReceiver(env, []string{"k1:test-material"})
			if err != nil {
				t.Fatalf("the receiver refused what the exporter accepts: %v", err)
			}
			engine, err := tracing.DestinationFor(env, otlpenv.Traces)
			if err != nil {
				t.Fatalf("the engine refused what the receiver accepts: %v", err)
			}
			if forwards := receiver.Upstream(); !reflect.DeepEqual(engine, forwards) {
				t.Errorf("the engine exports traces to\n\t%+v\nand the receiver forwards them to\n\t%+v",
					engine, forwards)
			}
		})
	}

	// A VALUE ONE HALF REFUSES, THE OTHER REFUSES: a receiver that accepted
	// what the engine refused would answer every box 200 and forward nowhere.
	for name, value := range map[string]string{
		otlpenv.EndpointVar:          "localhost:4318",
		otlpenv.Traces.EndpointVar(): "https://user:pass@collector.example.com",
		otlpenv.ProtocolVar:          "http/json",
		otlpenv.Traces.ProtocolVar(): "otlp",
		otlpenv.Traces.ExporterVar(): "console",
		otlpenv.CompressionVar:       "zstd",
		otlpenv.CertificateVar:       "/nonexistent/ca.pem",
		otlpenv.InsecureVar:          "maybe",
	} {
		env := lookup(map[string]string{name: value})
		_, recvErr := sandbox.BuildOtelReceiver(env, []string{"k1:test-material"})
		_, tracesErr := tracing.DestinationFor(env, otlpenv.Traces)
		if recvErr == nil || tracesErr == nil {
			t.Errorf("%s=%q: receiver %v, engine traces %v — one half accepted what "+
				"the other refused", name, value, recvErr, tracesErr)
		}
	}
}

// lookup is an environment over a literal map, with the receiver switched on so
// BuildOtelReceiver builds one.
func lookup(kv map[string]string) func(string) string {
	return func(name string) string {
		if name == sandbox.OtelReceiverURLVar {
			return "https://engine.example.com"
		}
		return kv[name]
	}
}

// certificateFile writes a certificate a collector could serve as a PEM file,
// for a case that names one.
func certificateFile(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
