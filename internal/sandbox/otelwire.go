package sandbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
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
	"google.golang.org/protobuf/reflect/protoreflect"
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
// be refused as malformed, which the specification asks of a receiver. What
// decoding COSTS is bounded before it happens — see otelcost.go.

// MaxOtelPayload bounds one export's BYTES: what the route reads off the wire,
// and what a compressed export may inflate to.
//
// An OTLP batch is small — spans, not artefacts — and the exporter inside the
// box splits its own batches. The route needs no other credential to reach, so
// an unbounded read is a box choosing this process's memory; and the bound is
// on the inflated size too, because four mebibytes of gzip can inflate a
// thousandfold. It is NOT a bound on what decoding those bytes holds, which a
// message of empty messages makes a hundred and forty times larger: that is
// [MaxOtelDecoded]'s, measured before anything is decoded.
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

	// ErrOtelTooLarge is an export past [MaxOtelPayload] once decompressed,
	// or one that would decode to more than [MaxOtelDecoded].
	ErrOtelTooLarge = errors.New("sandbox: an OTLP export past the receiver's bound once decompressed or decoded")
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

// decodeExport reads an export into the OTLP message it is, holding what that
// costs in hold — measured first, and reserved before anything that grows with
// the export is allocated (see otelcost.go).
func decodeExport(ctx context.Context, e OtelExport, hold *otelHold) (decodedExport, error) {
	mediaType, gzipped, err := exportForm(e)
	if err != nil {
		return decodedExport{}, err
	}
	if mediaType == otlpJSON {
		// jsontext holds a token at a time, and one string can be most of
		// the export: what measuring may hold is reserved before it.
		reserve := int64(len(e.Body))
		if gzipped {
			reserve = MaxOtelPayload
		}
		if err = hold.to(ctx, reserve); err != nil {
			return decodedExport{}, err
		}
	}
	cost, err := measureExport(e.Body, gzipped, mediaType)
	if err != nil {
		return decodedExport{}, err
	}
	if err = hold.to(ctx, cost.weight()); err != nil {
		return decodedExport{}, err
	}
	plain, err := plainOf(e.Body, gzipped, mediaType, cost)
	if err != nil {
		return decodedExport{}, err
	}
	req := &coltracepb.ExportTraceServiceRequest{}
	if mediaType == otlpJSON {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(plain, req)
	} else {
		err = proto.Unmarshal(plain, req)
	}
	if err != nil {
		return decodedExport{}, fmt.Errorf("%w: traces as %s: %w", ErrOtelMalformed, mediaType, err)
	}
	return decodedExport{request: req, mediaType: mediaType}, nil
}

// plainOf is the bytes an export's message is decoded from, once its cost is
// reserved: a protobuf body as it came or inflated, or an OTLP/JSON body
// rewritten for protojson.
//
// The JSON is REWRITTEN, not handed to protojson as it is, and the difference
// is silent: OTLP/JSON writes trace and span ids as hex, where the protobuf
// JSON mapping reads a bytes field as base64 — and a 32-character hex string is
// valid base64, so protojson accepts it and decodes a different, 24-byte id.
// Every span would arrive under a trace nobody started. So the export is
// streamed through the same walk that measured it ([measureJSON]), each id
// written as base64 and each member the message does not have left out, and
// the rest is the mapping's: enums as integers or names, lowerCamelCase or the
// proto names, 64-bit integers as strings or numbers. A field this build's
// message does not have is skipped rather than refused, as the protobuf
// decoder skips one, so a box speaking a newer OTLP is not refused for its
// additions.
func plainOf(body []byte, gzipped bool, mediaType string, cost otelCost) ([]byte, error) {
	if mediaType == otlpProtobuf && !gzipped {
		return body, nil
	}
	buf := bytes.NewBuffer(make([]byte, 0, cost.plain))
	_, err := readPlain(body, gzipped, mediaType, func(r io.Reader) (otelCost, error) {
		if mediaType == otlpJSON {
			return measureJSON(r, jsontext.NewEncoder(buf, jsontext.AllowDuplicateNames(true)))
		}
		_, err := buf.ReadFrom(r)
		return otelCost{}, err
	})
	return buf.Bytes(), err
}

// exportForm is the media type an export is in, and whether it is gzipped —
// each refused as [ErrOtelUnsupported] unless the receiver reads it.
func exportForm(e OtelExport) (mediaType string, gzipped bool, err error) {
	mediaType, _, err = mime.ParseMediaType(e.ContentType)
	if err != nil {
		return "", false, fmt.Errorf("%w: Content-Type %q", ErrOtelUnsupported, e.ContentType)
	}
	if mediaType != otlpProtobuf && mediaType != otlpJSON {
		return "", false, fmt.Errorf("%w: Content-Type %q", ErrOtelUnsupported, mediaType)
	}
	gzipped, err = contentEncoding(e.ContentEncoding)
	return mediaType, gzipped, err
}

// measureExport weighs an export by streaming its plain bytes through the
// walk for its media type.
func measureExport(body []byte, gzipped bool, mediaType string) (otelCost, error) {
	return readPlain(body, gzipped, mediaType, func(r io.Reader) (otelCost, error) {
		if mediaType == otlpJSON {
			return measureJSON(r, nil)
		}
		return measureProtobuf(r)
	})
}

// contentEncoding reports whether an export is gzipped.
//
// gzip is the one compression the OTLP/HTTP specification asks a receiver to
// read, and the one an OTLP exporter applies; anything else is refused rather
// than guessed at.
func contentEncoding(encoding string) (gzipped bool, err error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return false, nil
	case "gzip":
		return true, nil
	}
	return false, fmt.Errorf("%w: Content-Encoding %q", ErrOtelUnsupported, encoding)
}

// readPlain hands read the export's plain bytes — the body, or the body
// inflated as it is read, never whole — held to [MaxOtelPayload], and
// classifies what went wrong: more than the bound is too large, whatever read
// made of it; a gzip stream that does not decode, or a body read refuses, is
// malformed.
//
// A gzip stream is read to its END, which is where its checksum is checked —
// a body cut short or corrupted fails here rather than measuring or decoding
// as a shorter export.
func readPlain(body []byte, gzipped bool, mediaType string, read func(io.Reader) (otelCost, error)) (otelCost, error) {
	var src io.Reader = bytes.NewReader(body)
	if gzipped {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return otelCost{}, fmt.Errorf("%w: its gzip stream: %w", ErrOtelMalformed, err)
		}
		src = zr
	}
	counted := &countingReader{r: io.LimitReader(src, MaxOtelPayload+1)}
	cost, err := read(counted)
	switch {
	case counted.n > MaxOtelPayload:
		return otelCost{}, errOtelPlainTooLarge
	case errors.Is(err, ErrOtelTooLarge):
		return otelCost{}, err
	case err != nil:
		return otelCost{}, fmt.Errorf("%w: traces as %s: %w", ErrOtelMalformed, mediaType, err)
	}
	return cost, nil
}

// countingReader counts what has been read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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

// otlpIDFields are the fields OTLP/JSON writes as HEX where the protobuf JSON
// mapping writes base64: a span's and a link's trace and span ids, and a
// span's parent — named by their proto names, which [measureJSON] matches
// whichever spelling a member used.
var otlpIDFields = map[protoreflect.Name]bool{
	"trace_id": true, "span_id": true, "parent_span_id": true,
}
