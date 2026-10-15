package tracing

import (
	"context"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// collector is a stand-in OTLP/HTTP backend that records what reaches it.
type collector struct {
	mu   sync.Mutex
	seen []received
}

// received is one export as the backend saw it.
type received struct {
	path   string
	header http.Header
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	t.Helper()
	c := &collector{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.seen = append(c.seen, received{path: r.URL.Path, header: r.Header.Clone()})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return c, server
}

// at is every export that reached path.
func (c *collector) at(path string) []received {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []received
	for _, r := range c.seen {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// transportHeaders are the headers the exporter sends of its own accord, so
// what is left is what the configuration asked for.
var transportHeaders = []string{
	"Accept-Encoding", "Content-Encoding", "Content-Length", "Content-Type", "User-Agent",
}

// configured is the headers of one request that configuration put there,
// canonicalised.
func configured(h http.Header) map[string]string {
	out := map[string]string{}
	for k := range h {
		if !slices.Contains(transportHeaders, k) {
			out[k] = h.Get(k)
		}
	}
	return out
}

func canonical(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

// THE EXPORTERS SEND EXACTLY WHAT THE RESOLVER RESOLVED: each signal to its
// own endpoint, with its own headers, decoded. TestTheExporterAndTheReceiver-
// ResolveOneDestination holds the engine's resolution equal to the sandbox
// receiver's; this holds the engine's resolution equal to what it SENDS,
// which is the half of "agree" a comparison of two resolutions cannot see.
//
// The credential is written the way the documentation writes it,
// `Bearer%20…`. Both exporters once sent the `%20` verbatim: the engine passed
// its own undecoded parse to the SDK, overriding the SDK's decoded one.
//
// Mutation: hand newExporter the raw header string's split instead of the
// destination's decoded headers, and the authorization check goes red.
func TestTheExporterSendsWhatTheResolverResolved(t *testing.T) {
	c, server := newCollector(t)
	env := map[string]string{
		otlpenv.EndpointVar:          server.URL + "/base/",
		otlpenv.HeadersVar:           "authorization=Bearer%20traces-secret",
		otlpenv.Metrics.HeadersVar(): "authorization=Bearer%20metrics-secret,x-tenant=acme",
	}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	shutdown := install(t, Options{Env: envOf(env), Recorder: rec})
	_, span := Start(context.Background(), "test", "an.operation")
	span.End()
	// Something for the metric export to carry, so the flush has a reason
	// to send one.
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
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, want := range []struct {
		path    string
		headers map[string]string
	}{
		{"/base/v1/traces", map[string]string{"Authorization": "Bearer traces-secret"}},
		{"/base/v1/metrics", map[string]string{
			"Authorization": "Bearer metrics-secret", "X-Tenant": "acme"}},
	} {
		got := c.at(want.path)
		if len(got) == 0 {
			t.Errorf("nothing reached %s; the collector saw %v", want.path, c.seen)
			continue
		}
		if h := configured(got[0].header); !maps.Equal(h, want.headers) {
			t.Errorf("%s carried %v, want %v", want.path, h, want.headers)
		}
	}
}

// AN HTTPS ENDPOINT IN ANY CASE IS EXPORTED OVER TLS, verified against the
// authority the environment names — the exporter's half of the scheme rule
// whose receiver half internal/sandbox's TestAnUpperCaseHTTPSUpstreamIsDialled-
// OverTLS holds. The two once read `HTTPS://` differently: this exporter used
// TLS and the receiver's gRPC forward did not.
//
// Mutation: hand the exporter an endpoint whose scheme it reads as plaintext
// (or drop the certificate from its options) and nothing reaches the
// collector.
func TestAnUpperCaseHTTPSEndpointIsExportedOverTLS(t *testing.T) {
	c := &collector{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.seen = append(c.seen, received{path: r.URL.Path, header: r.Header.Clone()})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	shutdown := install(t, Options{Env: envOf(map[string]string{
		otlpenv.EndpointVar:           "HTTPS://" + u.Host,
		otlpenv.CertificateVar:        ca,
		otlpenv.Metrics.ExporterVar(): "none",
	})})
	_, span := Start(context.Background(), "test", "an.operation")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := c.at("/v1/traces"); len(got) == 0 {
		t.Errorf("nothing reached the TLS collector through an HTTPS:// endpoint; it saw %v", c.seen)
	}
}

// A SIGNAL THE OPERATOR DECLINED IS NOT SENT, whatever the endpoint says —
// OTEL_TRACES_EXPORTER=none is honoured as OTEL_METRICS_EXPORTER=none always
// was, so "metrics yes, traces no" can be said in the specification's words.
//
// Mutation: build the batcher whenever the endpoint is set, ignoring
// Destination.Declined, and a span reaches /v1/traces below.
func TestADeclinedSignalIsNotExported(t *testing.T) {
	c, server := newCollector(t)
	shutdown := install(t, Options{Env: envOf(map[string]string{
		otlpenv.EndpointVar:           server.URL,
		otlpenv.Traces.ExporterVar():  "none",
		otlpenv.Metrics.ExporterVar(): "NONE",
	})})
	ctx, span := Start(context.Background(), "test", "an.operation")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(c.seen) != 0 {
		t.Errorf("a declined signal was exported: %v", c.seen)
	}
	// IDS STILL FLOW. Declining the export is not declining the trace —
	// the event store and the dashboard's tree read these.
	if len(TraceOf(ctx).TraceID) != 32 {
		t.Error("declining the export took the trace ids with it")
	}

	// AND A VALUE THAT NAMES NO EXPORTER IS REFUSED, not read as otlp.
	for _, v := range []string{otlpenv.Traces.ExporterVar(), otlpenv.Metrics.ExporterVar()} {
		_, err := Configure(context.Background(), Options{Env: envOf(map[string]string{
			otlpenv.EndpointVar: server.URL, v: "prometheus",
		})})
		if err == nil {
			t.Errorf("%s=prometheus was accepted", v)
		}
	}
}

// THE RESOLVER READS THE ENVIRONMENT AS THE SDK DOES.
//
// otlpenv mirrors the OTel Go SDK's own reading of the OTLP variables, and
// this holds the mirror to the original: for each environment, the SDK's
// exporter is built from the environment ALONE — no options, the SDK's own
// parse — and what it sends is compared with what otlpenv resolved from the
// same environment. The engine passes its resolution to the SDK explicitly,
// so a resolver that drifted from the SDK would not show up in any test that
// goes through the engine; it would show up as a box that exports somewhere
// the engine does not.
//
// NOT PARALLEL: the SDK reads the process environment, so the cases set it.
func TestTheResolverReadsTheEnvironmentAsTheSDKDoes(t *testing.T) {
	c, server := newCollector(t)
	vars := []string{
		otlpenv.EndpointVar, otlpenv.HeadersVar, otlpenv.ProtocolVar,
		otlpenv.Traces.EndpointVar(), otlpenv.Traces.HeadersVar(), otlpenv.Traces.ProtocolVar(),
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"a base gets the signal path", map[string]string{
			otlpenv.EndpointVar: server.URL}},
		{"a trailing slash on the base", map[string]string{
			otlpenv.EndpointVar: server.URL + "/otlp/"}},
		{"a signal endpoint is verbatim", map[string]string{
			otlpenv.EndpointVar:          server.URL + "/ignored",
			otlpenv.Traces.EndpointVar(): server.URL + "/custom/spans"}},
		{"a signal endpoint with no path is the root", map[string]string{
			otlpenv.Traces.EndpointVar(): server.URL}},
		{"values are percent-decoded, then trimmed", map[string]string{
			otlpenv.EndpointVar: server.URL,
			otlpenv.HeadersVar:  "authorization=Bearer%20secret, x-tenant = acme ,x-pad=%20p%20"}},
		{"unusable entries are dropped", map[string]string{
			otlpenv.EndpointVar: server.URL,
			otlpenv.HeadersVar:  "good=1,,no-equals,bad name=v,x-bad=%zz"}},
		{"a signal's headers replace the base set", map[string]string{
			otlpenv.EndpointVar:         server.URL,
			otlpenv.HeadersVar:          "x-tenant=acme,authorization=base",
			otlpenv.Traces.HeadersVar(): "authorization=Bearer%20traces"}},
		{"a signal's unusable headers still replace the base set", map[string]string{
			otlpenv.EndpointVar:         server.URL,
			otlpenv.HeadersVar:          "x-tenant=acme",
			otlpenv.Traces.HeadersVar(): "garbage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range vars {
				t.Setenv(v, tc.env[v])
			}
			want, err := otlpenv.Resolve(os.Getenv, otlpenv.Traces)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			wantURL, err := url.Parse(want.Endpoint)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := wantURL.Path
			if wantPath == "" {
				wantPath = "/"
			}

			before := len(c.at(wantPath))
			exp, err := otlptracehttp.New(context.Background())
			if err != nil {
				t.Fatalf("the SDK's exporter from the environment: %v", err)
			}
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			_, span := tp.Tracer("conformance").Start(context.Background(), "probe")
			span.End()
			if err := tp.Shutdown(context.Background()); err != nil {
				t.Fatalf("flush: %v", err)
			}

			got := c.at(wantPath)
			if len(got) != before+1 {
				t.Fatalf("the SDK did not send to %s, where otlpenv resolved %q; "+
					"the collector saw %v", wantPath, want.Endpoint, c.seen)
			}
			if h, w := configured(got[len(got)-1].header), canonical(want.Headers); !maps.Equal(h, w) {
				t.Errorf("the SDK sent headers %v; otlpenv resolved %v", h, w)
			}
		})
	}
}

// newTLSCollector is [newCollector] behind TLS, with the PEM file of the
// certificate it serves — the authority an exporter must be told to trust.
func newTLSCollector(t *testing.T) (*collector, *httptest.Server, string) {
	t.Helper()
	c := &collector{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.seen = append(c.seen, received{path: r.URL.Path, header: r.Header.Clone()})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "collector.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	return c, server, ca
}

// THE ENGINE'S EXPORTERS TAKE THE CERTIFICATE AND THE COMPRESSION FROM THE
// RESOLVER, AND THE RESOLVER READS THEM AS THE SDK DOES.
//
// Two halves. The SDK's own exporter, built from the environment alone, trusts
// a collector the CERTIFICATE variable names and gzips what COMPRESSION says —
// which is the reading otlpenv mirrors. And the engine, given the same
// environment ONLY through Options.Env, reaches that collector the same way:
// so what the engine sends is the resolver's reading passed explicitly, never
// the SDK's own second reading — which is what the sandbox receiver forwards
// with, and why the two cannot drift.
//
// Mutation: drop the TLS option from exporterSettings and the engine's export
// never completes its handshake; drop the compression option and it arrives
// uncompressed.
//
// NOT PARALLEL: the SDK half sets the process environment.
func TestTheExporterHonoursTheCertificateAndCompressionAsTheSDKDoes(t *testing.T) {
	c, server, ca := newTLSCollector(t)
	env := map[string]string{
		otlpenv.EndpointVar:    server.URL,
		otlpenv.CertificateVar: ca,
		otlpenv.CompressionVar: "gzip",
	}
	want, err := otlpenv.Resolve(envOf(env), otlpenv.Traces)
	if err != nil {
		t.Fatal(err)
	}
	if want.TLS.Certificate != ca || want.Compression != otlpenv.Gzip {
		t.Fatalf("otlpenv resolved %+v", want)
	}

	t.Run("the SDK from the environment alone", func(t *testing.T) {
		for k, v := range env {
			t.Setenv(k, v)
		}
		before := len(c.at("/v1/traces"))
		exp, err := otlptracehttp.New(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
		_, span := tp.Tracer("conformance").Start(context.Background(), "probe")
		span.End()
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
		got := c.at("/v1/traces")
		if len(got) != before+1 || got[len(got)-1].header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("the SDK did not export gzip to the collector it was told to trust: %v", got)
		}
	})

	t.Run("the engine from the resolver", func(t *testing.T) {
		before := len(c.at("/v1/traces"))
		shutdown := install(t, Options{Env: envOf(env)})
		_, span := Start(context.Background(), "test", "an.operation")
		span.End()
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		got := c.at("/v1/traces")
		if len(got) != before+1 || got[len(got)-1].header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("the engine did not export gzip to the collector the resolver "+
				"trusts: %v", got)
		}
	})
}
