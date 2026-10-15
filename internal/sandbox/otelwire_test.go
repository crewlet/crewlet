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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/protobuf/encoding/protowire"
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

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// protobufExport is msg as a box's OTLP/HTTP exporter sends it.
func protobufExport(t *testing.T, msg *coltracepb.ExportTraceServiceRequest) sandbox.OtelExport {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.OtelExport{Body: body, ContentType: "application/x-protobuf"}
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
	e := protobufExport(t, sent)
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
	// VALID PROTOBUF THROUGHOUT — a field this build does not know,
	// repeated, which a decoder keeps — so it is the export's size and
	// nothing else that refuses it.
	field := protowire.AppendBytes(protowire.AppendTag(nil, 15, protowire.BytesType), make([]byte, 64<<10))
	for range inflated / len(field) {
		if _, err := zw.Write(field); err != nil {
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
		Body:        bomb.Bytes(),
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
	good := protobufExport(t, spansIn(launchedFrom.TraceID))

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
	reply := accept(t, receiver, sandbox.OtelExport{Body: body, ContentType: "application/json"})

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
	accept(t, receiver, protobufExport(t, sent))
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
	accept(t, untrusting, protobufExport(t, spansIn(launchedFrom.TraceID)))
	if got := upstream.saw(); len(got) != 0 {
		t.Fatalf("a collector nobody trusted was sent %v", got)
	}

	trusting := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: server.URL, otlpenv.CertificateVar: ca,
	})
	accept(t, trusting, protobufExport(t, spansIn(launchedFrom.TraceID)))
	if got := upstream.saw(); len(got) != 1 {
		t.Fatalf("the collector the environment trusts received %v", got)
	}
}

// grpcCollector is an in-process OTLP/gRPC trace collector, recording every
// Export call it is made and the metadata it came with.
type grpcCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	mu       sync.Mutex
	traces   []*coltracepb.ExportTraceServiceRequest
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

// startGRPCCollector serves the trace collector service on a loopback port,
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
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return c, scheme + "://" + lis.Addr().String()
}

func (c *grpcCollector) counts() (traces int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.traces)
}

// A GRPC UPSTREAM IS MADE THE COLLECTOR'S OWN EXPORT CALL, with the
// destination's headers as metadata. The receiver used to POST every
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
	reply := accept(t, receiver, protobufExport(t, sent))
	if traces := collector.counts(); traces != 1 {
		t.Fatalf("the collector received %d traces exports, want one", traces)
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
	accept(t, without, protobufExport(t, spansIn(launchedFrom.TraceID)))
	if traces := collector.counts(); traces != 0 {
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
	accept(t, with, protobufExport(t, spansIn(launchedFrom.TraceID)))
	if traces := collector.counts(); traces != 1 {
		t.Fatalf("the collector received %d traces exports over mutual TLS, want one", traces)
	}
}

// AN HTTPS ENDPOINT IN ANY CASE IS DIALLED OVER TLS, as the engine's own
// exporter dials it. The forward once read the scheme as the prefix `https://`
// of the endpoint as written, so for `HTTPS://` it dialled a TLS collector in
// plaintext — every box's export failed its handshake, visible only as one
// forward warning per batch — while the engine's exporter, which parses the
// URL, exported over TLS.
//
// Mutation: store the endpoint as written in otlpenv and match a prefix in
// Destination.Secure (the code this replaced) and nothing arrives.
func TestAnUpperCaseHTTPSUpstreamIsDialledOverTLS(t *testing.T) {
	t.Parallel()
	server := newPKI(t, "collector")
	collector, endpoint := startGRPCCollector(t, "HTTPS", grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{server.cert},
		MinVersion:   tls.VersionTLS12,
	})))
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar:    endpoint,
		otlpenv.ProtocolVar:    "grpc",
		otlpenv.CertificateVar: server.certFile,
	})
	accept(t, receiver, protobufExport(t, spansIn(launchedFrom.TraceID)))
	if traces := collector.counts(); traces != 1 {
		t.Fatalf("a TLS collector at %s received %d traces exports, want one", endpoint, traces)
	}
}

// A GRPC UPSTREAM THAT COMPRESSES IS SENT GZIP, as the engine's own gRPC
// exporter sends it — and one that does not is sent plain. The mutual-TLS case
// above runs with gzip on and would pass whether or not the call was
// compressed, so this reads the compression the collector's transport saw.
//
// Mutation: drop the gzip call option from the gRPC dial and the first case
// arrives uncompressed; apply it unconditionally and the second does not.
func TestAGRPCUpstreamIsCompressedAsTheDestinationSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ compression, want string }{
		{"gzip", "gzip"},
		{"", ""},
	} {
		seen := &compressionSeen{}
		collector, endpoint := startGRPCCollector(t, "http", grpc.StatsHandler(seen))
		env := map[string]string{otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc"}
		if tc.compression != "" {
			env[otlpenv.CompressionVar] = tc.compression
		}
		accept(t, receiverTo(t, env), protobufExport(t, spansIn(launchedFrom.TraceID)))
		if traces := collector.counts(); traces != 1 {
			t.Fatalf("compression %q: the collector received %d traces exports, want one", tc.compression, traces)
		}
		if got := seen.compressions(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("compression %q: the collector's transport saw %q, want [%q]", tc.compression, got, tc.want)
		}
	}
}

// compressionSeen is a gRPC stats handler recording the compression each
// inbound call declared — the one place a server can see it, since the
// payload reaches the service already decompressed.
type compressionSeen struct {
	mu   sync.Mutex
	seen []string
}

func (c *compressionSeen) compressions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

func (c *compressionSeen) HandleRPC(_ context.Context, s stats.RPCStats) {
	if h, ok := s.(*stats.InHeader); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.seen = append(c.seen, h.Compression)
	}
}

func (*compressionSeen) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (*compressionSeen) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (*compressionSeen) HandleConn(context.Context, stats.ConnStats) {}

// A CLOSED RECEIVER STILL ANSWERS AND FORWARDS NOTHING, over every protocol,
// and closing twice is harmless: the engine closes it at its teardown, after
// its listeners. The HTTP forwarder's close did nothing, so a closed receiver
// on the default protocol went on posting exports with the backend's
// credential while this case — run over gRPC alone — said it stopped; and a
// transport built for a destination's own TLS kept its connections open.
//
// Mutation: make the HTTP forwarder's close a no-op again and the http cases
// receive the second export; skip releasing an owned transport and the https
// case keeps its connection.
func TestAClosedReceiverForwardsNothing(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"grpc", "http/protobuf", "https with its own authority"} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			var received func() int
			var open func() int
			env := map[string]string{}
			switch protocol {
			case "grpc":
				collector, endpoint := startGRPCCollector(t, "http")
				received = collector.counts
				env[otlpenv.EndpointVar], env[otlpenv.ProtocolVar] = endpoint, "grpc"
			default:
				upstream := newOtelUpstream()
				server := httptest.NewUnstartedServer(upstream)
				conns := &connCounter{}
				server.Config.ConnState = conns.track
				if protocol == "http/protobuf" {
					server.Start()
				} else {
					server.StartTLS()
					env[otlpenv.CertificateVar] = pemOf(t, t.TempDir(), "collector.pem",
						"CERTIFICATE", server.Certificate().Raw)
					open = conns.open
				}
				t.Cleanup(server.Close)
				received = func() int { return len(upstream.saw()) }
				env[otlpenv.EndpointVar] = server.URL
			}
			receiver := receiverTo(t, env)

			accept(t, receiver, protobufExport(t, spansIn(launchedFrom.TraceID)))
			if err := receiver.Close(); err != nil {
				t.Fatal(err)
			}
			if err := receiver.Close(); err != nil {
				t.Fatalf("a second close: %v", err)
			}
			accept(t, receiver, protobufExport(t, spansIn(launchedFrom.TraceID)))
			if got := received(); got != 1 {
				t.Errorf("the collector received %d exports, want only the one before the close", got)
			}
			if open != nil {
				deadline := time.Now().Add(5 * time.Second)
				for open() != 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if n := open(); n != 0 {
					t.Errorf("%d connection(s) to the collector outlived the close", n)
				}
			}
		})
	}
}

// connCounter counts a server's open connections.
type connCounter struct {
	mu sync.Mutex
	n  int
}

func (c *connCounter) track(_ net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch state {
	case http.StateNew:
		c.n++
	case http.StateClosed, http.StateHijacked:
		c.n--
	}
}

func (c *connCounter) open() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A CLOSE ENDS A FORWARD IN FLIGHT rather than waiting out a slow collector:
// teardown is bounded by the engine's stop, not by OtelForwardTimeout per
// export a box happened to send just before it.
//
// Mutation: wait for the sends in flight without ending them and the close
// takes the forward's whole timeout.
func TestACloseEndsAForwardInFlight(t *testing.T) {
	t.Parallel()
	upstream := &blockingUpstream{arrived: make(chan struct{}, 1), release: make(chan struct{})}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	t.Cleanup(func() { upstream.once.Do(func() { close(upstream.release) }) })
	receiver := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})

	done := make(chan error, 1)
	go func() {
		_, err := receiver.Accept(t.Context(), launchedFrom.TraceID, protobufExport(t, spansIn(launchedFrom.TraceID)))
		done <- err
	}()
	<-upstream.arrived
	started := time.Now()
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > sandbox.OtelForwardTimeout/2 {
		t.Errorf("the close took %v: it waited out the forward", took)
	}
	if err := <-done; err != nil {
		t.Errorf("the export in flight was refused rather than answered: %v", err)
	}
}

// AN EXPORT IS HELD TO ITS TOKEN'S TRACE. The token is minted for the trace a
// run was launched in, and the box is handed that trace's TRACEPARENT, so
// every span its agent records descends from it; a span in any other trace —
// or naming none — was written by something else in the box, and is refused.
// It is reported to the box as a partial success, which an exporter neither
// retries nor fails its batch for, and the rest of the export still goes on.
//
// Mutation: forward the decoded export without scoping it and the foreign and
// trace-less spans reach the backend.
func TestAnExportIsHeldToItsTokensTrace(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc",
	})
	const elsewhere = "0af7651916cd43dd8448eb211c80319c"

	mixed := spansIn(launchedFrom.TraceID)
	foreign := spansIn(elsewhere).GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	traceless := &tracepb.Span{SpanId: mustHex("3c9de682dcad293d"), Name: "names no trace"}
	ss := mixed.GetResourceSpans()[0].GetScopeSpans()[0]
	ss.Spans = append(ss.Spans, foreign, traceless)
	reply := accept(t, receiver, protobufExport(t, mixed))

	answer := &coltracepb.ExportTraceServiceResponse{}
	if err := proto.Unmarshal(reply.Body, answer); err != nil {
		t.Fatal(err)
	}
	if got := answer.GetPartialSuccess().GetRejectedSpans(); got != 2 {
		t.Errorf("the box was told %d spans were refused, want 2", got)
	}

	// An export wholly in another trace goes nowhere at all.
	accept(t, receiver, protobufExport(t, spansIn(elsewhere)))

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if len(collector.traces) != 1 {
		t.Fatalf("the collector received %d traces exports, want only the mixed one", len(collector.traces))
	}
	spans := collector.traces[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
	if len(spans) != 1 {
		t.Errorf("the collector received %d spans, want the one in the token's trace", len(spans))
	}
	for _, span := range spans {
		if hex.EncodeToString(span.GetTraceId()) != launchedFrom.TraceID {
			t.Errorf("a span in trace %x reached the collector under a token for %s",
				span.GetTraceId(), launchedFrom.TraceID)
		}
	}
}

// A TOKEN WHOSE SUBJECT IS NOT A TRACE SCOPES EVERY SPAN OUT, rather than
// none: a subject the receiver cannot read as an id is not a licence for
// every trace.
func TestATokenNamingNoTraceForwardsNoSpan(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc",
	})
	if _, err := receiver.Accept(t.Context(), "not-a-trace",
		protobufExport(t, spansIn(launchedFrom.TraceID))); err != nil {
		t.Fatal(err)
	}
	// NOR A SPAN NAMING NO TRACE EITHER: an empty id and a subject that
	// reads as no id are both empty, and compared as bytes they were equal,
	// so such a token forwarded every span that left its trace id out.
	// Mutation: compare the ids without first requiring the token's.
	traceless := spansIn(launchedFrom.TraceID)
	traceless.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId = nil
	if _, err := receiver.Accept(t.Context(), "not-a-trace", protobufExport(t, traceless)); err != nil {
		t.Fatal(err)
	}
	if traces := collector.counts(); traces != 0 {
		t.Errorf("a token naming no trace forwarded %d exports", traces)
	}
}
