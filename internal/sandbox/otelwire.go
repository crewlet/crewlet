package sandbox

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The wire half of the receiver: what a box's export IS, read off its body.
//
// # Traces, and nothing else
//
// A box exports its TRACES here, and the receiver serves no other signal: the
// route is `/otlp/{token}/v1/traces`, and a metrics or a logs path under the
// same token is the mux's own 404, which an exporter takes as final. No runner
// the engine ships exports anything else — Claude Code's metrics and logs are
// switched off ([Runner.Telemetry]) and OpenCode is minted no endpoint — so a
// metrics or logs export under a box token could only come from something in
// the box other than the agent, and the receiver used to forward it with the
// backend's credential: log records naming no trace, with any resource and
// body, and metric exemplars naming any trace. Serving what the runners
// declare, and only that, is what makes "spans in its own trace" the whole of
// what a token is worth.
//
// # Why the receiver decodes what it forwards
//
// It used to pass the body through untouched, with its Content-Type, and that
// was only right for one shape of request. A box that compressed its export —
// OTEL_EXPORTER_OTLP_COMPRESSION=gzip, which an image can set and Claude Code
// honours — sent gzip bytes the forward relabelled as plain protobuf, because
// Content-Encoding was not carried, and the backend refused every batch. And a
// deployment whose engine exports over gRPC had every box's export POSTed as
// OTLP/HTTP to its gRPC port. Carrying the header would mend the first and
// nothing could mend the second: a gRPC Export call takes a MESSAGE, not a
// body. So the receiver reads the export into the OTLP message it is, and
// writes that message to the upstream in the upstream's own protocol and
// compression — the one [otlpenv] resolved, which is the engine's own — exactly
// as the engine's exporter would. Decoding is also what lets a malformed export
// be refused as malformed, which the specification asks of a receiver.

// MaxOtelPayload bounds one export: what the route reads off the wire, and
// what a compressed export may inflate to.
//
// An OTLP batch is small — spans and metric points, not artefacts — and the
// exporter inside the box splits its own batches. The route needs no other
// credential to reach, so an unbounded read is a box choosing this process's
// memory; and the bound is on the DECODED size too, because four mebibytes of
// gzip can inflate a thousandfold, and a compressed export must not be able to
// make the engine hold more than an uncompressed one may send.
const MaxOtelPayload = 4 << 20

// The ways the receiver refuses an export it was handed. Each is what the
// box sent, never what the backend did — a forward's failure is not the box's
// to hear about (see [OtelReceiver.Accept]).
var (
	// ErrOtelUnsupported is an export in a media type or an encoding the
	// receiver does not read: OTLP/HTTP is `application/x-protobuf` or
	// `application/json`, uncompressed or gzip.
	ErrOtelUnsupported = errors.New("sandbox: an OTLP export in a media type or encoding the receiver does not read")

	// ErrOtelMalformed is an export whose body is not the OTLP message its
	// signal and media type say it is.
	ErrOtelMalformed = errors.New("sandbox: an OTLP export that does not decode")

	// ErrOtelTooLarge is an export past [MaxOtelPayload] once decompressed.
	ErrOtelTooLarge = errors.New("sandbox: an OTLP export past the receiver's bound once decompressed")
)

// OtelExport is one traces export a box made, as the route received it.
type OtelExport struct {
	Body            []byte
	ContentType     string
	ContentEncoding string
}

// OtelReply is what the route answers an accepted export with: the trace
// service's Export response, in the media type the export arrived in, as the
// OTLP/HTTP specification asks.
type OtelReply struct {
	ContentType string
	Body        []byte
}

// The two media types OTLP/HTTP defines.
const (
	otlpProtobuf = "application/x-protobuf"
	otlpJSON     = "application/json"
)

// scopeToTrace removes from req every span in a trace other than trace, and
// reports how many it removed and how many it kept. A nil trace — a token
// whose subject is not a trace id — keeps none.
func scopeToTrace(req *coltracepb.ExportTraceServiceRequest, trace []byte) (removed, kept int64) {
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			ss.Spans = slices.DeleteFunc(ss.Spans, func(span *tracepb.Span) bool {
				if trace != nil && bytes.Equal(span.GetTraceId(), trace) {
					kept++
					return false
				}
				removed++
				return true
			})
		}
	}
	return removed, kept
}

// outOfScope is the response telling a box that removed spans were refused: a
// partial success, which an exporter neither retries nor fails its batch for.
func outOfScope(removed int64, why string) *coltracepb.ExportTraceServiceResponse {
	return &coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{
		RejectedSpans: removed, ErrorMessage: why,
	}}
}

// decodedExport is an export read into its message, with the media type its
// reply is written in.
type decodedExport struct {
	request   *coltracepb.ExportTraceServiceRequest
	mediaType string
}

// decodeExport reads an export into the OTLP message it is.
func decodeExport(e OtelExport) (decodedExport, error) {
	mediaType, _, err := mime.ParseMediaType(e.ContentType)
	if err != nil {
		return decodedExport{}, fmt.Errorf("%w: Content-Type %q", ErrOtelUnsupported, e.ContentType)
	}
	body, err := inflate(e.Body, e.ContentEncoding)
	if err != nil {
		return decodedExport{}, err
	}
	req := &coltracepb.ExportTraceServiceRequest{}
	switch mediaType {
	case otlpProtobuf:
		err = proto.Unmarshal(body, req)
	case otlpJSON:
		err = unmarshalOTLPJSON(body, req)
	default:
		return decodedExport{}, fmt.Errorf("%w: Content-Type %q", ErrOtelUnsupported, mediaType)
	}
	if err != nil {
		return decodedExport{}, fmt.Errorf("%w: traces as %s: %w", ErrOtelMalformed, mediaType, err)
	}
	return decodedExport{request: req, mediaType: mediaType}, nil
}

// inflate undoes a Content-Encoding, holding the result to [MaxOtelPayload].
//
// gzip is the one compression the OTLP/HTTP specification asks a receiver to
// read, and the one an OTLP exporter applies; anything else is refused rather
// than guessed at.
func inflate(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
	default:
		return nil, fmt.Errorf("%w: Content-Encoding %q", ErrOtelUnsupported, encoding)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: its gzip stream: %w", ErrOtelMalformed, err)
	}
	// Read to its END, which is where the stream's checksum is checked — a
	// body cut short or corrupted fails here rather than decoding as a
	// shorter export. A read stopped at the bound is refused for its size.
	out, err := io.ReadAll(io.LimitReader(zr, MaxOtelPayload+1))
	if len(out) > MaxOtelPayload {
		return nil, ErrOtelTooLarge
	}
	if err == nil {
		err = zr.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: its gzip stream: %w", ErrOtelMalformed, err)
	}
	return out, nil
}

// encodeReply writes a response in the media type its export came in.
func encodeReply(resp proto.Message, mediaType string) (OtelReply, error) {
	var (
		body []byte
		err  error
	)
	if mediaType == otlpJSON {
		body, err = protojson.Marshal(resp)
	} else {
		body, err = proto.Marshal(resp)
	}
	if err != nil {
		return OtelReply{}, err
	}
	return OtelReply{ContentType: mediaType, Body: body}, nil
}

// otlpIDFields are the members OTLP/JSON writes as HEX where the protobuf JSON
// mapping writes base64: a span's and a link's trace and span ids, and a
// span's parent. Every one is named one of these, in either spelling a
// receiver must accept, and nothing else in a traces export is.
var otlpIDFields = map[string]bool{
	"traceId": true, "spanId": true, "parentSpanId": true,
	"trace_id": true, "span_id": true, "parent_span_id": true,
}

// unmarshalOTLPJSON reads an OTLP/JSON body.
//
// NOT protojson ALONE, and the difference is silent: OTLP/JSON writes trace and
// span ids as hex, where the protobuf JSON mapping reads a bytes field as
// base64 — and a 32-character hex string is valid base64, so protojson accepts
// it and decodes a different, 24-byte id. Every span would arrive under a
// trace nobody started. So the ids are rewritten to base64 first, and the rest
// is the mapping's: enums as integers or names, lowerCamelCase or the proto
// names, 64-bit integers as strings or numbers. A field this build's message
// does not have is skipped rather than refused, as the protobuf decoder skips
// one, so a box speaking a newer OTLP is not refused for its additions.
func unmarshalOTLPJSON(body []byte, into proto.Message) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	if err := hexIDsToBase64(doc); err != nil {
		return err
	}
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(rewritten, into)
}

// hexIDsToBase64 rewrites every id member under v in place.
func hexIDsToBase64(v any) error {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok && otlpIDFields[k] {
				raw, err := hex.DecodeString(s)
				if err != nil {
					return fmt.Errorf("%s %q is not hex", k, s)
				}
				v[k] = raw // encoding/json writes a []byte as base64
				continue
			}
			if err := hexIDsToBase64(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := hexIDsToBase64(child); err != nil {
				return err
			}
		}
	}
	return nil
}
