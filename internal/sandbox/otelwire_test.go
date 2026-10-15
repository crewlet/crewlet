package sandbox_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these tests protect.
//
// The receiver used to pass a box's body through untouched with its
// Content-Type, which was right for exactly one shape of request. A box that
// gzipped its export — an image can say so, and Claude Code honours it — had
// its gzip relabelled as plain protobuf and refused by the backend; and a
// deployment whose engine exports over gRPC had every box's export POSTed as
// OTLP/HTTP at its gRPC port. Both failed inside the backend while the box was
// told 200. So the receiver reads the export into its message and writes that
// message upstream as the engine's own exporter would.

// spansIn is a traces export of one span, with one link, in traceHex.
func spansIn(traceHex string) *coltracepb.ExportTraceServiceRequest {
	trace := mustHex(traceHex)
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId:           trace,
			SpanId:            mustHex("1e9de682dcad293d"),
			ParentSpanId:      mustHex("00f067aa0ba902b7"),
			Name:              "claude_code.interaction",
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: 1_760_000_000_000_000_000,
			EndTimeUnixNano:   1_760_000_001_000_000_000,
			Links: []*tracepb.Span_Link{{
				TraceId: trace, SpanId: mustHex("2ab8283139d528ed"),
			}},
		}}}},
	}}}
}

// emptyRequest is a well-formed export of signal carrying one record of it.
func emptyRequest(signal otlpenv.Signal) proto.Message {
	switch signal {
	case otlpenv.Metrics:
		return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "probe"}}}},
		}}}
	case otlpenv.Logs:
		return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{SeverityText: "INFO"}}}},
		}}}
	}
	return spansIn(launchedFrom.TraceID)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// protobufExport is msg as a box's OTLP/HTTP exporter sends it.
func protobufExport(t *testing.T, signal otlpenv.Signal, msg proto.Message) sandbox.OtelExport {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.OtelExport{Signal: signal, Body: body, ContentType: "application/x-protobuf"}
}

// accept hands the receiver an export under launchedFrom's token and requires
// it be accepted.
func accept(t *testing.T, r *sandbox.OtelReceiver, e sandbox.OtelExport) sandbox.OtelReply {
	t.Helper()
	reply, err := r.Accept(t.Context(), launchedFrom.TraceID, e)
	if err != nil {
		t.Fatalf("an export was refused: %v", err)
	}
	return reply
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// receiverTo is a receiver forwarding as env resolves, closed with the test.
func receiverTo(t *testing.T, env map[string]string) *sandbox.OtelReceiver {
	t.Helper()
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL:  "https://engine.internal",
		Tokens:   sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
		Upstream: upstreamOf(t, env),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	return receiver
}

// tracesAt is every traces export an OTLP/HTTP upstream received, decoded.
func (u *otelUpstream) tracesAt(t *testing.T) []*coltracepb.ExportTraceServiceRequest {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*coltracepb.ExportTraceServiceRequest
	for i, body := range u.bodies {
		if u.seen[i].Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("the upstream got gzip it cannot read: %v", err)
			}
			if body, err = io.ReadAll(zr); err != nil {
				t.Fatal(err)
			}
		}
		msg := &coltracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, msg); err != nil {
			t.Fatalf("the upstream got a body that is not an export: %v", err)
		}
		out = append(out, msg)
	}
	return out
}

// A COMPRESSED EXPORT IS READ, NOT RELABELLED. The receiver dropped the box's
// Content-Encoding and sent its gzip upstream as plain protobuf, which the
// backend refused.
//
// Mutation: read the body without inflating it and the export is refused as
// malformed.
func TestACompressedExportIsReadNotRelabelled(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	receiver := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})

	sent := spansIn(launchedFrom.TraceID)
	e := protobufExport(t, otlpenv.Traces, sent)
	e.Body, e.ContentEncoding = gzipped(t, e.Body), "gzip"
	accept(t, receiver, e)

	got := upstream.tracesAt(t)
	if len(got) != 1 || !proto.Equal(got[0], sent) {
		t.Fatalf("the upstream received %v, want the export the box compressed", got)
	}
	if enc := upstream.seen[0].Get("Content-Encoding"); enc != "" {
		t.Errorf("an upstream that asked for no compression got %q", enc)
	}
}

// AN EXPORT THAT INFLATES PAST THE BOUND IS REFUSED, AND IS NEVER INFLATED
// WHOLE: four mebibytes of gzip can hold four gigabytes of zeros, and a
// compressed export must not make the engine hold more than an uncompressed
// one may send.
//
// Measured by what the refusal ALLOCATES, because a refusal alone cannot tell
// a bounded read from one that inflated everything first and then compared.
// NOT PARALLEL, so no other test's allocations are counted: a top-level test
// that does not call t.Parallel runs while every parallel one is paused.
//
// Mutation: read the gzip stream without the bound and the refusal allocates
// the whole bomb.
func TestAnExportThatInflatesPastTheBoundIsRefused(t *testing.T) {
	receiver := receiverTo(t, nil)
	const inflated = 128 << 20
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	zeros := make([]byte, 1<<20)
	for range inflated / len(zeros) {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if bomb.Len() > sandbox.MaxOtelPayload {
		t.Fatalf("the bomb is %d bytes compressed, past what the route reads", bomb.Len())
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := receiver.Accept(t.Context(), launchedFrom.TraceID, sandbox.OtelExport{
		Signal: otlpenv.Traces, Body: bomb.Bytes(),
		ContentType: "application/x-protobuf", ContentEncoding: "gzip",
	})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, sandbox.ErrOtelTooLarge) {
		t.Fatalf("an export inflating past the bound: %v, want ErrOtelTooLarge", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8*sandbox.MaxOtelPayload {
		t.Errorf("refusing a %d MiB bomb allocated %d MiB: it was inflated past the bound",
			inflated>>20, allocated>>20)
	}
}

// WHAT THE RECEIVER CANNOT READ IS REFUSED AS THE BOX'S, and goes nowhere: the
// specification's answer to a malformed export is a final 400, which an
// exporter does not retry, rather than a 200 for a batch the backend refuses.
func TestWhatTheReceiverCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	receiver := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})
	good := protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID))

	for _, tc := range []struct {
		name string
		edit func(*sandbox.OtelExport)
		want error
	}{
		{"a media type OTLP does not define", func(e *sandbox.OtelExport) {
			e.ContentType = "text/plain"
		}, sandbox.ErrOtelUnsupported},
		{"no media type at all", func(e *sandbox.OtelExport) {
			e.ContentType = ""
		}, sandbox.ErrOtelUnsupported},
		{"an encoding the receiver does not read", func(e *sandbox.OtelExport) {
			e.ContentEncoding = "br"
		}, sandbox.ErrOtelUnsupported},
		{"a body that is not the message", func(e *sandbox.OtelExport) {
			e.Body = []byte("spans")
		}, sandbox.ErrOtelMalformed},
		{"gzip that is not gzip", func(e *sandbox.OtelExport) {
			e.ContentEncoding = "gzip"
		}, sandbox.ErrOtelMalformed},
		{"JSON that is not JSON", func(e *sandbox.OtelExport) {
			e.ContentType, e.Body = "application/json", []byte("{")
		}, sandbox.ErrOtelMalformed},
		{"a JSON id that is not hex", func(e *sandbox.OtelExport) {
			e.ContentType = "application/json"
			e.Body = []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"not-hex"}]}]}]}`)
		}, sandbox.ErrOtelMalformed},
	} {
		e := good
		tc.edit(&e)
		if _, err := receiver.Accept(t.Context(), launchedFrom.TraceID, e); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := upstream.saw(); len(got) != 0 {
		t.Errorf("an export the receiver could not read was forwarded: %v", got)
	}
}

// AN OTLP/JSON EXPORT KEEPS ITS IDS. OTLP/JSON writes trace and span ids as
// hex where the protobuf JSON mapping reads base64 — and 32 hex characters
// are valid base64, so a plain decode accepts them and yields a different id:
// every span would land under a trace nobody started.
//
// Mutation: decode with protojson alone and the ids differ.
func TestAJSONExportKeepsItsIDs(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	receiver := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})

	// The shape Claude Code's http/json exporter sent, measured: enums as
	// integers, 64-bit times as strings, ids as hex — here in upper case,
	// which the specification also allows.
	body := []byte(`{"resourceSpans":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"claude-code"}}]},` +
		`"scopeSpans":[{"spans":[{"traceId":"4BF92F3577B34DA6A3CE929D0E0E4736",` +
		`"spanId":"1e9de682dcad293d","parentSpanId":"00f067aa0ba902b7",` +
		`"name":"claude_code.interaction","kind":1,` +
		`"startTimeUnixNano":"1760000000000000000","endTimeUnixNano":"1760000001000000000",` +
		`"links":[{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"2ab8283139d528ed"}],` +
		`"aFieldFromANewerOTLP":true}]}]}]}`)
	reply := accept(t, receiver, sandbox.OtelExport{
		Signal: otlpenv.Traces, Body: body, ContentType: "application/json",
	})

	got := upstream.tracesAt(t)
	if len(got) != 1 {
		t.Fatalf("the upstream received %d exports", len(got))
	}
	span := got[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	want := spansIn(launchedFrom.TraceID).GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	if !bytes.Equal(span.TraceId, want.TraceId) || !bytes.Equal(span.SpanId, want.SpanId) ||
		!bytes.Equal(span.ParentSpanId, want.ParentSpanId) ||
		!bytes.Equal(span.GetLinks()[0].GetSpanId(), want.GetLinks()[0].GetSpanId()) {
		t.Errorf("the ids arrived as trace %x span %x parent %x, want %x %x %x",
			span.TraceId, span.SpanId, span.ParentSpanId, want.TraceId, want.SpanId, want.ParentSpanId)
	}
	// AND THE REPLY IS IN THE EXPORT'S OWN MEDIA TYPE, as the specification
	// asks: a JSON exporter is answered in JSON.
	if reply.ContentType != "application/json" || string(reply.Body) != "{}" {
		t.Errorf("a JSON export was answered %q %q", reply.ContentType, reply.Body)
	}
}

// AN UPSTREAM THAT COMPRESSES IS SENT GZIP, as the engine's own exporter sends
// it: the forward speaks the destination's compression, not the box's.
//
// Mutation: ignore the destination's compression and the upstream gets plain
// protobuf.
func TestAGzipUpstreamIsSentGzip(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: server.URL, otlpenv.CompressionVar: "gzip",
	})

	sent := spansIn(launchedFrom.TraceID)
	accept(t, receiver, protobufExport(t, otlpenv.Traces, sent))
	if enc := upstream.seen[0].Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if got := upstream.tracesAt(t); len(got) != 1 || !proto.Equal(got[0], sent) {
		t.Errorf("the gzip did not hold the export")
	}
}

// pemOf writes PEM blocks to a file in dir.
func pemOf(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pki is a self-signed certificate for 127.0.0.1, written as PEM files.
type pki struct {
	cert     tls.Certificate
	pool     *x509.CertPool
	certFile string
	keyFile  string
}

func newPKI(t *testing.T, name string) pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := pki{
		certFile: pemOf(t, dir, name+".pem", "CERTIFICATE", der),
		keyFile:  pemOf(t, dir, name+"-key.pem", "PRIVATE KEY", keyDER),
		pool:     x509.NewCertPool(),
	}
	if p.cert, err = tls.LoadX509KeyPair(p.certFile, p.keyFile); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	p.pool.AddCert(leaf)
	return p
}

// AN HTTPS UPSTREAM IS VERIFIED AGAINST THE AUTHORITY THE ENVIRONMENT NAMES,
// as the engine's exporter verifies it. The receiver used to apply none of the
// certificate variables, so a collector behind a private authority received
// the engine's spans and refused every box's handshake.
//
// Mutation: build the forward's client without the destination's TLS and
// nothing arrives.
func TestAnHTTPSUpstreamIsVerifiedAgainstTheNamedAuthority(t *testing.T) {
	t.Parallel()
	upstream := newOtelUpstream()
	server := httptest.NewTLSServer(upstream)
	t.Cleanup(server.Close)
	ca := pemOf(t, t.TempDir(), "collector.pem", "CERTIFICATE", server.Certificate().Raw)

	// Without the authority, the handshake fails, and that is the
	// backend's problem rather than the box's.
	untrusting := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})
	accept(t, untrusting, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if got := upstream.saw(); len(got) != 0 {
		t.Fatalf("a collector nobody trusted was sent %v", got)
	}

	trusting := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: server.URL, otlpenv.CertificateVar: ca,
	})
	accept(t, trusting, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if got := upstream.saw(); len(got) != 1 {
		t.Fatalf("the collector the environment trusts received %v", got)
	}
}

// grpcCollector is an in-process OTLP/gRPC collector, recording every Export
// call it is made and the metadata it came with.
type grpcCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	colmetricspb.UnimplementedMetricsServiceServer
	collogspb.UnimplementedLogsServiceServer

	mu       sync.Mutex
	traces   []*coltracepb.ExportTraceServiceRequest
	metrics  []*colmetricspb.ExportMetricsServiceRequest
	logs     []*collogspb.ExportLogsServiceRequest
	metadata []metadata.MD
}

func (c *grpcCollector) note(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	c.metadata = append(c.metadata, md)
}

func (c *grpcCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.note(ctx)
	c.traces = append(c.traces, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// metricsService and logsService adapt the collector to the two other
// services, whose method is also named Export.
type metricsService struct{ *grpcCollector }

func (s metricsService) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note(ctx)
	s.metrics = append(s.metrics, req)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

type logsService struct{ *grpcCollector }

func (s logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note(ctx)
	s.logs = append(s.logs, req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// startGRPCCollector serves the three collector services on a loopback port,
// with opts (TLS, say), and answers the endpoint to reach it at.
func startGRPCCollector(t *testing.T, scheme string, opts ...grpc.ServerOption) (*grpcCollector, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &grpcCollector{}
	srv := grpc.NewServer(opts...)
	coltracepb.RegisterTraceServiceServer(srv, c)
	colmetricspb.RegisterMetricsServiceServer(srv, metricsService{c})
	collogspb.RegisterLogsServiceServer(srv, logsService{c})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return c, scheme + "://" + lis.Addr().String()
}

func (c *grpcCollector) counts() (traces, metrics, logs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.traces), len(c.metrics), len(c.logs)
}

// A GRPC UPSTREAM IS MADE THE COLLECTOR'S OWN EXPORT CALL, for every signal,
// with the destination's headers as metadata. The receiver used to POST every
// box's export as OTLP/HTTP whatever protocol the engine exported in, so a
// deployment on gRPC received the engine's spans and none of a box's.
//
// Mutation: forward over HTTP whatever the protocol and nothing arrives;
// drop the metadata and the credential does not.
func TestAGRPCUpstreamGetsTheCollectorsExportCall(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint,
		otlpenv.ProtocolVar: "grpc",
		otlpenv.HeadersVar:  "authorization=Bearer%20ingest-secret",
	})

	sent := spansIn(launchedFrom.TraceID)
	reply := accept(t, receiver, protobufExport(t, otlpenv.Traces, sent))
	for _, signal := range []otlpenv.Signal{otlpenv.Metrics, otlpenv.Logs} {
		accept(t, receiver, protobufExport(t, signal, emptyRequest(signal)))
	}

	if traces, metrics, logs := collector.counts(); traces != 1 || metrics != 1 || logs != 1 {
		t.Fatalf("the collector received %d traces, %d metrics, %d logs exports; want one each",
			traces, metrics, logs)
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if !proto.Equal(collector.traces[0], sent) {
		t.Error("the collector received a different traces export")
	}
	if got := collector.metadata[0].Get("authorization"); len(got) != 1 || got[0] != "Bearer ingest-secret" {
		t.Errorf("the credential reached the collector as %q", got)
	}
	// The box is answered in OTLP/HTTP whatever the upstream speaks.
	if reply.ContentType != "application/x-protobuf" {
		t.Errorf("the box was answered as %q", reply.ContentType)
	}
}

// A GRPC UPSTREAM OVER TLS IS VERIFIED AGAINST THE NAMED AUTHORITY AND SHOWN
// THE CLIENT CERTIFICATE THE ENVIRONMENT NAMES — the engine's exporter's TLS,
// on the forward.
//
// Mutation: dial without the destination's TLS material and the handshake
// fails, so nothing arrives.
func TestAGRPCUpstreamOverTLSPresentsTheClientCertificate(t *testing.T) {
	t.Parallel()
	server, client := newPKI(t, "collector"), newPKI(t, "engine")
	collector, endpoint := startGRPCCollector(t, "https", grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{server.cert},
		ClientCAs:    client.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})))

	// No client certificate: the collector refuses the handshake.
	without := receiverTo(t, map[string]string{
		otlpenv.EndpointVar:    endpoint,
		otlpenv.ProtocolVar:    "grpc",
		otlpenv.CertificateVar: server.certFile,
	})
	accept(t, without, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if traces, _, _ := collector.counts(); traces != 0 {
		t.Fatal("a collector requiring a client certificate accepted a forward without one")
	}

	with := receiverTo(t, map[string]string{
		otlpenv.EndpointVar:          endpoint,
		otlpenv.ProtocolVar:          "grpc",
		otlpenv.CertificateVar:       server.certFile,
		otlpenv.ClientCertificateVar: client.certFile,
		otlpenv.ClientKeyVar:         client.keyFile,
		otlpenv.CompressionVar:       "gzip",
	})
	accept(t, with, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if traces, _, _ := collector.counts(); traces != 1 {
		t.Fatalf("the collector received %d traces exports over mutual TLS, want one", traces)
	}
}

// A CLOSED RECEIVER STILL ANSWERS AND FORWARDS NOTHING, and closing twice is
// harmless: the engine closes it at its teardown, after its listeners.
func TestAClosedReceiverForwardsNothing(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc",
	})
	accept(t, receiver, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("a second close: %v", err)
	}
	accept(t, receiver, protobufExport(t, otlpenv.Traces, spansIn(launchedFrom.TraceID)))
	if traces, _, _ := collector.counts(); traces != 1 {
		t.Errorf("the collector received %d exports, want only the one before the close", traces)
	}
}
