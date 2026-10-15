package sandbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/otlpenv"
)

// otelForwarder sends one signal's decoded exports to its destination, in the
// destination's own protocol.
type otelForwarder interface {
	// send forwards one export, answering the upstream's response — nil
	// when it carried none — or why it was not delivered.
	send(ctx context.Context, req proto.Message) (proto.Message, error)
	close() error
}

// newOtelForwarder builds the forwarder for one destination that exports.
func newOtelForwarder(signal otlpenv.Signal, d otlpenv.Destination) (otelForwarder, error) {
	codec, ok := otelCodecs[signal]
	if !ok {
		return nil, fmt.Errorf("no OTLP codec for signal %q", signal)
	}
	tlsConfig, err := d.Config()
	if err != nil {
		return nil, err
	}
	switch d.Protocol {
	case otlpenv.HTTPProtobuf:
		client := httpx.Client(OtelForwardTimeout)
		if tlsConfig != nil {
			// A TRANSPORT OF ITS OWN only where the TLS genuinely differs
			// — a private authority, a client certificate — which is the
			// one case httpx.Customized exists for.
			client = httpx.Customized(OtelForwardTimeout, func(t *http.Transport) {
				t.TLSClientConfig = tlsConfig
			})
		}
		return &otelHTTPForwarder{d: d, codec: codec, client: client}, nil
	case otlpenv.GRPC:
		return newOtelGRPCForwarder(d, codec, tlsConfig)
	}
	// [otlpenv.Resolve] refuses any other protocol.
	return nil, fmt.Errorf("no OTLP forwarder is built for protocol %q", d.Protocol)
}

// otelHTTPForwarder POSTs an export as OTLP/HTTP protobuf.
type otelHTTPForwarder struct {
	d      otlpenv.Destination
	codec  otelCodec
	client *http.Client
}

// send writes the message as the engine's own HTTP exporter would: protobuf,
// gzipped when the destination compresses, with the destination's headers.
func (f *otelHTTPForwarder) send(ctx context.Context, req proto.Message) (proto.Message, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	encoding := ""
	if f.d.Compression == otlpenv.Gzip {
		if body, err = gzipBody(body); err != nil {
			return nil, err
		}
		encoding = "gzip"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, f.d.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range f.d.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", otlpProtobuf)
	if encoding != "" {
		httpReq.Header.Set("Content-Encoding", encoding)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, otelAnswerBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, otelRefused{status: resp.StatusCode}
	}
	// The response is the signal's Export response, which carries a
	// partial success when the backend dropped part of the export. One it
	// cannot read is not a failure of the forward: the export was accepted.
	out := f.codec.response()
	if proto.Unmarshal(answer, out) != nil {
		return nil, nil
	}
	return out, nil
}

func (f *otelHTTPForwarder) close() error { return nil }

// gzipBody is body compressed whole.
func gzipBody(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// otelAnswerBytes bounds what is read of an upstream's answer: an Export
// response is at most a partial success — a count and a sentence — and a
// backend that sends more is not owed this process's memory for it.
const otelAnswerBytes = 64 << 10

// otelRefused is an upstream that answered an export with a refusal.
type otelRefused struct{ status int }

func (e otelRefused) Error() string { return fmt.Sprintf("the upstream answered %d", e.status) }

// otelGRPCForwarder makes the collector's own gRPC Export call.
//
// ITS CONNECTION IS MADE ON THE FIRST FORWARD, not when the receiver is built.
// A gRPC client runs goroutines from the moment it exists, dialled or not, and
// every node builds a receiver at boot whether or not a box ever exports to it
// — so a connection made eagerly would be one a node holds for nothing, and
// one a boot that fails after building the receiver leaks.
type otelGRPCForwarder struct {
	target string
	opts   []grpc.DialOption
	codec  otelCodec
	md     metadata.MD

	mu     sync.Mutex
	conn   *grpc.ClientConn
	closed bool
}

// newOtelGRPCForwarder settles how the collector is reached, and dials nothing.
//
// THE TARGET AND THE TRANSPORT ARE THE ENGINE'S EXPORTER'S: the endpoint's host
// and port, dialled with TLS for an https endpoint — verified against the
// destination's own authorities when it names some, the system's otherwise —
// and in plaintext for an http one; the destination's headers as metadata; and
// gzip when the destination compresses. The URL's path means nothing to gRPC,
// which addresses the service by its method name.
func newOtelGRPCForwarder(d otlpenv.Destination, codec otelCodec, tlsConfig *tls.Config) (*otelGRPCForwarder, error) {
	u, err := url.Parse(d.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("%s does not parse as a URL", d.EndpointVar)
	}
	creds := insecure.NewCredentials()
	if d.Secure() {
		creds = credentials.NewTLS(tlsConfig)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if d.Compression == otlpenv.Gzip {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor(grpcgzip.Name)))
	}
	return &otelGRPCForwarder{target: u.Host, opts: opts, codec: codec, md: metadata.New(d.Headers)}, nil
}

func (f *otelGRPCForwarder) send(ctx context.Context, req proto.Message) (proto.Message, error) {
	conn, err := f.connection()
	if err != nil {
		return nil, err
	}
	return f.codec.export(metadata.NewOutgoingContext(ctx, f.md), conn, req)
}

// connection is the client, made on first use.
func (f *otelGRPCForwarder) connection() (*grpc.ClientConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errOtelReceiverClosed
	}
	if f.conn == nil {
		conn, err := grpc.NewClient(f.target, f.opts...)
		if err != nil {
			return nil, err
		}
		f.conn = conn
	}
	return f.conn, nil
}

func (f *otelGRPCForwarder) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.conn == nil {
		return nil
	}
	err := f.conn.Close()
	f.conn = nil
	return err
}

// errOtelReceiverClosed is a forward after [OtelReceiver.Close].
var errOtelReceiverClosed = errors.New("the OTLP receiver is closed")
