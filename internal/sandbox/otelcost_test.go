package sandbox_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these tests protect.
//
// The receiver decodes what a box exports, and decoding is an amplifier the
// byte bound never saw: an empty span is two bytes on the wire and a 280-byte
// struct decoded, so four mebibytes of them — four KiB of gzip — decoded to
// about 600 MiB, and any code in a box can read the endpoint that sends them.
// So an export is MEASURED before it is decoded and refused past
// MaxOtelDecoded, and what every export in flight holds is reserved from one
// budget. These cases hold the measure to what decoding really holds, the
// bound to what real exports need, and the budget to refusing before it
// inflates.

// emptySpans is a traces export of n spans with nothing in them: two bytes
// each on the wire, a whole struct each decoded.
func emptySpans(n int) []byte {
	scope := bytes.Repeat([]byte{0x12, 0x00}, n) // ScopeSpans.spans, empty
	resource := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), scope)
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), resource)
}

// emptyJSONSpans is the same export as OTLP/JSON: three bytes a span.
func emptyJSONSpans(n int) []byte {
	return []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[` +
		strings.Repeat("{},", n-1) + `{}]}]}]}`)
}

// attributedSpans is the export an OTel exporter writes: spans with ids,
// times, a status and attrs string attributes each — the shape the bound was
// derived from.
func attributedSpans(spans, attrs int) *coltracepb.ExportTraceServiceRequest {
	trace := mustHex(launchedFrom.TraceID)
	ss := &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code"}}
	for i := range spans {
		span := &tracepb.Span{
			TraceId:           trace,
			SpanId:            []byte(fmt.Sprintf("%08d", i)),
			ParentSpanId:      mustHex(launchedFrom.SpanID),
			Name:              "claude_code.tool",
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: 1_760_000_000_000_000_000 + uint64(i),
			EndTimeUnixNano:   1_760_000_001_000_000_000 + uint64(i),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		}
		for a := range attrs {
			span.Attributes = append(span.Attributes, &commonpb.KeyValue{
				Key: fmt.Sprintf("claude_code.attribute_%02d", a),
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{
					StringValue: fmt.Sprintf("a value of the kind a tool records %d", i),
				}},
			})
		}
		ss.Spans = append(ss.Spans, span)
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-code"}},
		}}},
		ScopeSpans: []*tracepb.ScopeSpans{ss},
	}}}
}

// intAttributedSpans is the densest export an exporter writes: short keys
// and integer values, about seven times larger decoded than sent.
func intAttributedSpans(spans, attrs int) *coltracepb.ExportTraceServiceRequest {
	req := attributedSpans(spans, 0)
	for _, span := range req.ResourceSpans[0].ScopeSpans[0].Spans {
		for a := range attrs {
			span.Attributes = append(span.Attributes, &commonpb.KeyValue{
				Key:   fmt.Sprintf("k%d", a),
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(a)}},
			})
		}
		span.Events = append(span.Events, &tracepb.Span_Event{Name: "e", TimeUnixNano: 1})
		span.Links = append(span.Links, &tracepb.Span_Link{TraceId: span.TraceId, SpanId: span.SpanId})
	}
	return req
}

func marshal(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// otlpJSONOf is msg as OTLP/JSON writes it: the protobuf JSON mapping, with
// trace and span ids in hex rather than base64.
func otlpJSONOf(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	raw, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var toHex func(v any)
	toHex = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				if s, ok := child.(string); ok && (k == "traceId" || k == "spanId" || k == "parentSpanId") {
					id, err := base64.StdEncoding.DecodeString(s)
					if err != nil {
						t.Fatal(err)
					}
					v[k] = hex.EncodeToString(id)
					continue
				}
				toHex(child)
			}
		case []any:
			for _, child := range v {
				toHex(child)
			}
		}
	}
	toHex(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// allocatedBy is what f allocated, all of it, garbage included.
//
// NOT PARALLEL by contract: a top-level test that does not call t.Parallel
// runs while every parallel one is paused, so nothing else's allocations are
// counted.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// AN EXPORT OF EMPTY MESSAGES IS REFUSED BEFORE IT IS DECODED. Four KiB of gzip
// inflating to four mebibytes of empty spans decoded to about 600 MiB; the
// export is now measured as it streams — never inflated into memory, never
// decoded — and refused as too large having allocated almost nothing. The JSON
// form, `[{},{},…]`, is the same bomb through a different decoder, and a
// receiver forwarding nowhere decodes too, so it is the configuration here.
//
// Mutation: decode the export without measuring it first and each case
// allocates hundreds of mebibytes.
func TestAnExportOfEmptyMessagesIsRefusedBeforeItIsDecoded(t *testing.T) {
	receiver, err := sandbox.NewOtelReceiver(sandbox.OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  sandbox.NewOtelTokens(sandbox.OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	protobufBomb := emptySpans((sandbox.MaxOtelPayload - 16) / 2)
	jsonBomb := emptyJSONSpans((sandbox.MaxOtelPayload - 64) / 3)
	for _, tc := range []struct {
		name string
		e    sandbox.OtelExport
	}{
		{"protobuf, gzipped", sandbox.OtelExport{
			Body: gzipped(t, protobufBomb), ContentType: "application/x-protobuf", ContentEncoding: "gzip"}},
		{"protobuf, as sent", sandbox.OtelExport{
			Body: protobufBomb, ContentType: "application/x-protobuf"}},
		{"JSON, gzipped", sandbox.OtelExport{
			Body: gzipped(t, jsonBomb), ContentType: "application/json", ContentEncoding: "gzip"}},
	} {
		if len(tc.e.Body) > sandbox.MaxOtelPayload {
			t.Fatalf("%s: the bomb is %d bytes, past what the route reads", tc.name, len(tc.e.Body))
		}
		var err error
		allocated := allocatedBy(func() {
			_, err = receiver.Accept(t.Context(), launchedFrom.TraceID, tc.e)
		})
		if !errors.Is(err, sandbox.ErrOtelTooLarge) {
			t.Errorf("%s: %v, want ErrOtelTooLarge", tc.name, err)
		}
		if allocated > sandbox.MaxOtelPayload {
			t.Errorf("%s: refusing it allocated %d MiB: it was inflated or decoded first",
				tc.name, allocated>>20)
		}
	}
}

// THE BOUND ADMITS WHAT AN EXPORTER SENDS, at the largest size the route
// takes: attributed spans, and the densest shape an exporter writes — short
// integer attributes with an event and a link each — each just under four
// mebibytes, are accepted and reach the collector whole.
//
// Mutation: hold an export's decoded size to its wire size (MaxOtelDecoded at
// MaxOtelPayload) and these are refused as too large.
func TestTheDecodedBoundAdmitsWhatAnExporterSends(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc",
	})
	for _, tc := range []struct {
		name string
		make func(n int) *coltracepb.ExportTraceServiceRequest
		per  int // wire bytes a span takes, near enough
	}{
		{"twenty string attributes a span", func(n int) *coltracepb.ExportTraceServiceRequest {
			return attributedSpans(n, 20)
		}, 1500},
		{"short integer attributes, an event and a link", func(n int) *coltracepb.ExportTraceServiceRequest {
			return intAttributedSpans(n, 20)
		}, 350},
	} {
		n := sandbox.MaxOtelPayload / tc.per
		sent := tc.make(n)
		for proto.Size(sent) > sandbox.MaxOtelPayload {
			n = n * 9 / 10
			sent = tc.make(n)
		}
		if size := proto.Size(sent); size < sandbox.MaxOtelPayload*3/4 {
			t.Fatalf("%s: the export is %d bytes, not near the route's bound", tc.name, size)
		}
		before := collector.counts()
		e := sandbox.OtelExport{Body: marshal(t, sent), ContentType: "application/x-protobuf"}
		if _, err := receiver.Accept(t.Context(), launchedFrom.TraceID, e); err != nil {
			t.Fatalf("%s: an exporter's %d-span export was refused: %v", tc.name, n, err)
		}
		if got := collector.counts(); got != before+1 {
			t.Fatalf("%s: the collector received %d exports, want one more than %d", tc.name, got, before)
		}
		collector.mu.Lock()
		forwarded := collector.traces[len(collector.traces)-1]
		collector.mu.Unlock()
		if !proto.Equal(forwarded, sent) {
			t.Errorf("%s: the collector received a different export", tc.name)
		}
	}
}

// THE MEASURE IS WHAT DECODING HOLDS — never less, or the bound bounds
// nothing, and not wildly more, or it refuses what fits. For each shape the
// receiver's measure of an export is held against the heap a decode of it
// actually keeps; and an export measures the same however it was encoded, so
// the JSON form of each measures at least its protobuf form.
//
// Mutation: weigh a message without its struct, or a repeated field without
// its slots, and the measure falls under the heap.
func TestTheMeasureIsWhatDecodingHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  *coltracepb.ExportTraceServiceRequest
		raw  []byte
	}{
		{"string attributes", attributedSpans(500, 20), nil},
		{"integer attributes, events and links", intAttributedSpans(500, 20), nil},
		{"empty spans", nil, emptySpans(50_000)},
	} {
		body := tc.raw
		if tc.msg != nil {
			body = marshal(t, tc.msg)
		}
		_, measured, _, err := sandbox.MeasureOtel(sandbox.OtelExport{
			Body: body, ContentType: "application/x-protobuf"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		// The heap a decode keeps: live after a collection, less what was
		// live before one. TWO collections first, because a pool's
		// objects survive the first in its victim cache and are freed by
		// the second — which would otherwise land inside the measurement
		// and read as the decode holding less than it does.
		var held uint64
		var before, after runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&before)
		decoded := &coltracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, decoded); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(decoded)
		if after.HeapAlloc > before.HeapAlloc {
			held = after.HeapAlloc - before.HeapAlloc
		}

		t.Logf("%s: %d bytes sent, measured %d, decoding held %d", tc.name, len(body), measured, held)
		if uint64(measured) < held {
			t.Errorf("%s: measured %d bytes, and decoding held %d: the measure is not a bound",
				tc.name, measured, held)
		}
		if uint64(measured) > held*8/5+64<<10 {
			t.Errorf("%s: measured %d bytes for a decode holding %d: it refuses what fits",
				tc.name, measured, held)
		}

		if tc.msg != nil {
			_, inJSON, _, err := sandbox.MeasureOtel(sandbox.OtelExport{
				Body: otlpJSONOf(t, tc.msg), ContentType: "application/json"})
			if err != nil {
				t.Fatalf("%s as JSON: %v", tc.name, err)
			}
			if inJSON < measured {
				t.Errorf("%s: measured %d as JSON and %d as protobuf; one message, one cost",
					tc.name, inJSON, measured)
			}
		}
	}
}

// A JSON EXPORT DECODES TO THE MESSAGE IT IS, through the streaming rewrite
// that replaced a decode into a generic tree — ids from hex, every other field
// as the protobuf JSON mapping reads it, a member this build does not know
// left out — and in either spelling of a member's name.
//
// Mutation: pass an id through as it was written and the decoded ids differ.
func TestAJSONExportDecodesToTheMessageItIs(t *testing.T) {
	t.Parallel()
	collector, endpoint := startGRPCCollector(t, "http")
	receiver := receiverTo(t, map[string]string{
		otlpenv.EndpointVar: endpoint, otlpenv.ProtocolVar: "grpc",
	})
	sent := intAttributedSpans(20, 3)
	sent.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes = append(
		sent.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes,
		&commonpb.KeyValue{Key: "nested", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
			KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{
				Key: "list", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
					ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
						{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("raw")}},
						{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}},
					}},
				}},
			}}},
		}}})
	body := otlpJSONOf(t, sent)
	// A proto-name spelling and a member from a newer OTLP.
	body = bytes.Replace(body, []byte(`"resourceSpans"`), []byte(`"resource_spans"`), 1)
	body = bytes.Replace(body, []byte(`{"resource_spans"`), []byte(`{"aFieldFromANewerOTLP":{"x":[1,2]},"resource_spans"`), 1)

	accept(t, receiver, sandbox.OtelExport{
		Body: gzipped(t, body), ContentType: "application/json", ContentEncoding: "gzip"})
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if len(collector.traces) != 1 || !proto.Equal(collector.traces[0], sent) {
		t.Errorf("the JSON export decoded to a different message")
	}
}

// blockingUpstream is a collector that holds every export until released, so
// an export's weight stays reserved for as long as a case needs.
type blockingUpstream struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *blockingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.arrived <- struct{}{}
	<-u.release
	w.WriteHeader(http.StatusOK)
}

// A RECEIVER AT ITS BUDGET ANSWERS BUSY WITHOUT DECODING. What one export holds
// is bounded, and how many arrive at once is not, so each reserves its
// measured weight before it inflates or decodes and holds it until its forward
// ends. One that cannot get its weight in time is answered busy — a 503 an
// exporter retries — having been measured as it streamed and nothing more;
// once the budget is free it goes through.
//
// Mutation: decode without reserving and the second export goes through while
// the first still holds the budget.
func TestAReceiverAtItsBudgetAnswersBusyWithoutDecoding(t *testing.T) {
	upstream := &blockingUpstream{arrived: make(chan struct{}, 4), release: make(chan struct{})}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	t.Cleanup(func() { upstream.once.Do(func() { close(upstream.release) }) })
	receiver := receiverTo(t, map[string]string{otlpenv.EndpointVar: server.URL})

	first := protobufExport(t, spansIn(launchedFrom.TraceID))
	second := sandbox.OtelExport{
		Body:            gzipped(t, marshal(t, attributedSpans(1000, 20))),
		ContentType:     "application/x-protobuf",
		ContentEncoding: "gzip",
	}
	_, _, firstWeight, err := sandbox.MeasureOtel(first)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, secondWeight, err := sandbox.MeasureOtel(second)
	if err != nil {
		t.Fatal(err)
	}
	sandbox.SetOtelBudget(receiver, firstWeight+secondWeight-1)

	held := make(chan error, 1)
	go func() {
		_, err := receiver.Accept(t.Context(), launchedFrom.TraceID, first)
		held <- err
	}()
	select {
	case <-upstream.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the first export never reached the upstream")
	}

	var busy error
	allocated := allocatedBy(func() {
		_, busy = receiver.Accept(t.Context(), launchedFrom.TraceID, second)
	})
	if !errors.Is(busy, sandbox.ErrOtelBusy) {
		t.Fatalf("an export past the budget: %v, want ErrOtelBusy", busy)
	}
	if allocated >= uint64(plain)/2 {
		t.Errorf("answering busy allocated %d KiB for a %d KiB export: it was inflated or decoded",
			allocated>>10, plain>>10)
	}

	upstream.once.Do(func() { close(upstream.release) })
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Accept(t.Context(), launchedFrom.TraceID, second); err != nil {
		t.Errorf("once the budget was free, the export was refused: %v", err)
	}
}

// EVERY MESSAGE A TRACES EXPORT CAN CARRY HAS A GO SIZE, read from its
// generated type — so none is weighed at a guess — and none is a well-known
// type or a map entry, whose JSON is not an object of fields: the JSON walk
// reads every message as one.
func TestEveryTracesMessageHasAGoSize(t *testing.T) {
	t.Parallel()
	sizes := sandbox.OtelTraceMessageSizes()
	if len(sizes) < 10 {
		t.Fatalf("the traces export reaches %d messages; the walk of its descriptors is broken", len(sizes))
	}
	for name, size := range sizes {
		if size == sandbox.OtelUnknownStructSize {
			t.Errorf("%s has no generated Go type to weigh", name)
		}
		if strings.HasPrefix(name, "google.protobuf.") {
			t.Errorf("%s is a well-known type, whose JSON form the walk does not read", name)
		}
	}
	if sizes["opentelemetry.proto.trace.v1.Span"] < 200 {
		t.Errorf("a span weighs %d bytes, which is not its struct", sizes["opentelemetry.proto.trace.v1.Span"])
	}
}
