package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// What decoding an export COSTS, decided before anything is decoded.
//
// # Why bytes were not a bound
//
// [MaxOtelPayload] bounds what an export is on the wire, and what it inflates
// to; it does not bound what decoding it holds. A repeated message costs two
// bytes on the wire when it is empty (`0x12 0x00`) and a whole Go struct once
// decoded — a span is a 280-byte struct, so four mebibytes of empty spans
// decoded to about 600 MiB, and four KiB of gzip carried those four
// mebibytes. Any code in a box can read its run's endpoint out of its own
// environment, so a few dozen such requests in parallel took the engine
// process — and its broker member and its store with it — past its memory. Forwarding the body untouched, as the
// receiver once did, held four mebibytes per request; decoding it is what made
// the cost a multiple nobody had bounded.
//
// # How it is bounded
//
// Twice, and both before anything proportional to the decoded export exists:
//
//   - PER EXPORT, by MEASURING it. A streaming pass reads the export — through
//     its gzip, never inflating it into memory — against the trace service's
//     own message descriptors, and adds up what decoding would allocate: each
//     message's Go struct, each slot a repeated field grows, each oneof's
//     wrapper, each string's and byte string's copy, each unknown field's
//     retained bytes ([otelCost]). Past [MaxOtelDecoded] the export is refused
//     as too large, having allocated nothing that grows with it. The JSON
//     path is the same pass over the same descriptors, as protojson reads them.
//   - ACROSS EXPORTS, by a BUDGET. What one export may hold is bounded, and
//     how many run at once is not — the route is reached by every box — so each
//     export reserves its measured weight from one receiver-wide budget
//     ([otelBudgetBytes]) before it inflates or decodes, and holds it until its
//     forward is done. One that cannot get its weight within [otelBudgetWait]
//     is answered as busy ([ErrOtelBusy], a 503 an exporter retries after its
//     Retry-After) without having been inflated or decoded.
//
// Weighting by what an export measures rather than counting exports is what
// lets an ordinary batch — a few hundred kilobytes decoding to a few
// mebibytes — share the budget with dozens of others, while the largest export
// the receiver accepts takes half of it.

// MaxOtelDecoded bounds what decoding one export may hold, as [otelCost]
// measures it.
//
// SIXTEEN TIMES [MaxOtelPayload], from what OTLP decodes to, measured against
// the heap a decode keeps (TestTheMeasureIsWhatDecodingHolds): spans with
// twenty string attributes measure about four times their wire size; the
// densest shape an exporter writes — twenty short integer attributes, an event
// and a link on every span — about thirteen; and an export of empty messages,
// which nothing but a hand-built body is, a hundred and eighty. So sixteen
// times the wire bound admits every export an exporter writes at the largest
// size the route accepts (TestTheDecodedBoundAdmitsWhatAnExporterSends), and
// refuses what only a hand-built body can be.
const MaxOtelDecoded = 16 * MaxOtelPayload

// otelExportWeightMax is the most one export reserves from the budget: its
// decoded footprint, plus the plain bytes it is decoded from and the two
// encodings the forward makes of it (see [otelCost.weight]), with room for a
// JSON export's read reservation.
const otelExportWeightMax = MaxOtelDecoded + 4*MaxOtelPayload

// otelBudgetBytes bounds what every export a receiver is decoding at once may
// hold together.
//
// TWO OF THE LARGEST EXPORTS THE RECEIVER ACCEPTS — 160 MiB — and so, at the
// few mebibytes an ordinary batch weighs, dozens of them at once. That is the
// most memory the engine spends on boxes' spans whatever every box does
// together, and it is spent only while exports are in flight: an export holds
// its weight until its forward ends, which [OtelForwardTimeout] bounds, so a
// slow collector turns a burst into a short wait and a 503 rather than a heap
// that grows with it. A JUDGMENT between the heap a node can spare for
// telemetry and how many of the largest exports may wait on a slow collector
// at once; neither is measured on a production fleet.
const otelBudgetBytes = 2 * otelExportWeightMax

// otelBudgetWait bounds how long an export waits for the budget before it is
// answered busy. One second, which [otelAnswerMargin] sets aside for it beside
// reading, decoding and answering: an exporter abandons an export the receiver
// has not answered within [OtelAnswerWithin], so a wait the answer window has
// no room for only turns a 503 the exporter would retry into a timeout it
// counts as a failure.
const otelBudgetWait = time.Second

// OtelBusyRetryAfter is the Retry-After an export answered busy is sent: the
// budget turns over as the forwards holding it end, which against a healthy
// collector is well inside a second, and an OTLP exporter honours it before
// its own backoff.
const OtelBusyRetryAfter = time.Second

// ErrOtelBusy is an export that could not reserve its weight from the
// receiver's budget in time: the receiver is decoding as much as it will at
// once. Answered 503, which an exporter retries.
var ErrOtelBusy = errors.New("sandbox: the OTLP receiver is decoding as much as it holds at once; retry")

// otelBudget is the receiver-wide reservation every export draws on.
type otelBudget struct {
	sem  *semaphore.Weighted
	size int64
	wait time.Duration
}

func newOtelBudget(size int64) *otelBudget {
	return &otelBudget{sem: semaphore.NewWeighted(size), size: size, wait: otelBudgetWait}
}

// hold is one export's reservation, which [otelHold.to] grows or shrinks and
// [otelHold.release] gives back. Every growth draws on one allowance of
// waiting, so an export that reserves twice — a JSON one reserves its read
// before it measures — waits at most [otelBudgetWait] in all. What it spends
// measuring is not waiting and is not charged: under load a measure can take
// a while, and charging it would answer busy a receiver with room to spare.
func (b *otelBudget) hold() *otelHold {
	return &otelHold{b: b, wait: b.wait}
}

type otelHold struct {
	b    *otelBudget
	held int64
	// wait is what is left of the export's allowance of waiting.
	wait time.Duration
}

// to makes the reservation n bytes: waiting for the difference when n is more
// than is held, giving it back when it is less.
func (h *otelHold) to(ctx context.Context, n int64) error {
	switch {
	case n > h.b.size:
		// No wait could ever be granted it: [MaxOtelDecoded] refuses an
		// export this heavy before it is weighed, so reaching here is a
		// budget smaller than one export, which is a construction error.
		return fmt.Errorf("sandbox: an export weighing %d bytes exceeds the receiver's %d-byte budget", n, h.b.size)
	case n > h.held:
		if !h.b.sem.TryAcquire(n - h.held) {
			if h.wait <= 0 {
				return ErrOtelBusy
			}
			started := time.Now()
			wait, cancel := context.WithTimeout(ctx, h.wait)
			err := h.b.sem.Acquire(wait, n-h.held)
			cancel()
			h.wait -= time.Since(started)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return ErrOtelBusy
			}
		}
	case n < h.held:
		h.b.sem.Release(h.held - n)
	}
	h.held = n
	return nil
}

func (h *otelHold) release() {
	if h.held > 0 {
		h.b.sem.Release(h.held)
		h.held = 0
	}
}

// otelCost is what decoding one export holds, measured before it is decoded.
type otelCost struct {
	// plain is the export's size once inflated.
	plain int64
	// decoded is what the decoded message holds: every struct, slot,
	// wrapper and copy decoding it allocates.
	decoded int64
}

// weight is what the export reserves from the budget while it is decoded and
// forwarded: the decoded message; the plain bytes it is decoded from (an
// inflated body, or the JSON rewritten for protojson); and the forward's two
// encodings of it — the marshalled message and its compressed copy, or the
// gRPC client's own — each no larger than the plain export, since the decoded
// message is at most what the export said.
func (c otelCost) weight() int64 { return c.decoded + 3*c.plain }

// alloc is what an allocation of n bytes holds once the allocator has rounded
// it up to a size class — never less, so a measure built of it is a bound.
//
// Up to 256 bytes every multiple of sixteen is a class (8, 24 and 48 sit
// between them, never above), so rounding to sixteen is at least the class;
// from there to 32 KiB no class is more than a quarter above the smallest
// request it serves; and past that an object takes whole 8 KiB pages.
func alloc(n int64) int64 {
	switch {
	case n <= 0:
		return 0
	case n <= 8:
		return 8
	case n <= 256:
		return (n + 15) &^ 15
	case n <= 32<<10:
		return n + n/4
	}
	return (n + 8<<10 - 1) &^ (8<<10 - 1)
}

// otelStructSizes is the Go size of every message a traces export can carry,
// keyed by its descriptor's name — the struct a decode allocates for each one.
// Read from the generated types themselves, so a newer OTLP that adds a field
// is weighed at its new size without anyone remembering to.
var otelStructSizes = func() map[protoreflect.FullName]int64 {
	sizes := map[protoreflect.FullName]int64{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if _, seen := sizes[md.FullName()]; seen {
			return
		}
		sizes[md.FullName()] = otelUnknownStructSize
		if mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName()); err == nil {
			sizes[md.FullName()] = int64(reflect.TypeOf(mt.Zero().Interface()).Elem().Size())
		}
		fields := md.Fields()
		for i := range fields.Len() {
			if child := fields.Get(i).Message(); child != nil {
				walk(child)
			}
		}
	}
	walk(otelTracesRequest)
	return sizes
}()

// otelTracesRequest is the message a traces export is.
var otelTracesRequest = (&coltracepb.ExportTraceServiceRequest{}).ProtoReflect().Descriptor()

// otelUnknownStructSize weighs a message with no generated Go type, which no
// message of a traces export is (TestEveryTracesMessageHasAGoSize).
const otelUnknownStructSize = 512

func structSize(md protoreflect.MessageDescriptor) int64 {
	if n, ok := otelStructSizes[md.FullName()]; ok {
		return n
	}
	return otelUnknownStructSize
}

// slot is what one value of fd takes where it is held by value: an element of
// a repeated field's slice, or a oneof member's wrapper struct.
func slot(fd protoreflect.FieldDescriptor) int64 {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return 8
	case protoreflect.StringKind:
		return 16
	case protoreflect.BytesKind:
		return 24
	case protoreflect.BoolKind:
		return 1
	case protoreflect.Int64Kind, protoreflect.Uint64Kind, protoreflect.Sint64Kind,
		protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return 8
	}
	return 4
}

// value adds what decoding one value of fd holds, payload being the length of
// its string or bytes.
//
// A repeated field's slice is counted at twice its elements, the capacity an
// append-grown slice can end at; a oneof member allocates its wrapper; a
// message its struct; a string or a byte string its copy — the decoders copy
// both rather than alias the input, and an empty one allocates nothing.
func (c *otelCost) value(fd protoreflect.FieldDescriptor, payload int64) {
	if fd.IsList() {
		c.decoded += 2 * slot(fd)
	}
	if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
		// The wrapper struct holds the one value, as a slice would.
		c.decoded += alloc(slot(fd))
	}
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		c.decoded += alloc(structSize(fd.Message()))
	case protoreflect.StringKind, protoreflect.BytesKind:
		c.decoded += alloc(payload)
	}
}

// unknown adds a field the descriptor does not name, which the protobuf
// decoder keeps whole among the message's unknown bytes — appended, so at up
// to twice its length.
func (c *otelCost) unknown(n int64) { c.decoded += 2 * n }

// check refuses a cost already past the bound, so a measure stops at the
// first field that crosses it rather than reading the rest.
func (c *otelCost) check() error {
	if c.decoded > MaxOtelDecoded {
		return fmt.Errorf("%w: it decodes to more than %d bytes", ErrOtelTooLarge, MaxOtelDecoded)
	}
	return nil
}

// errOtelPlainTooLarge is an export past [MaxOtelPayload] once inflated.
var errOtelPlainTooLarge = fmt.Errorf("%w: past %d bytes", ErrOtelTooLarge, MaxOtelPayload)

// measureProtobuf weighs a protobuf traces export read from r, which yields
// the plain bytes and is held to [MaxOtelPayload] plus one by its caller, who
// also judges whether more than the bound was read (see [plainReader]).
//
// A STREAMING WALK of the wire format, driven by the request's descriptor: a
// length-delimited field the descriptor calls a message is walked into, a
// string or bytes field is skipped by its length, and every value is weighed
// by [otelCost.value]. Nothing it reads is kept, so measuring the largest
// export costs a read buffer — never the export, inflated or decoded. It
// refuses what the decoder would refuse — a truncated field, a length past its
// message's end, a wire type that does not exist, nesting past the decoder's
// own recursion limit — as malformed.
func measureProtobuf(r io.Reader) (otelCost, error) {
	w := &protoWalk{r: bufio.NewReader(r)}
	if err := w.message(otelTracesRequest, -1, 0); err != nil {
		return otelCost{}, err
	}
	w.cost.plain = w.pos
	return w.cost, nil
}

type protoWalk struct {
	r    *bufio.Reader
	pos  int64
	cost otelCost
}

func (w *protoWalk) ReadByte() (byte, error) {
	b, err := w.r.ReadByte()
	if err == nil {
		w.pos++
	}
	return b, err
}

// varint reads one varint; at a field boundary with nothing left it answers
// io.EOF itself, which is how the top-level message ends.
func (w *protoWalk) varint() (uint64, error) {
	start := w.pos
	v, err := binary.ReadUvarint(w)
	if errors.Is(err, io.EOF) && w.pos != start {
		err = io.ErrUnexpectedEOF
	}
	return v, err
}

func (w *protoWalk) skip(n int64) error {
	skipped, err := w.r.Discard(int(n))
	w.pos += int64(skipped)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

// message walks one message of md, ending at end — or at the end of the
// stream for the top level (end < 0). A nil md is a group nothing names,
// walked only to find its end.
func (w *protoWalk) message(md protoreflect.MessageDescriptor, end int64, depth int) error {
	if depth > protowire.DefaultRecursionLimit {
		return errors.New("nested past the decoder's recursion limit")
	}
	for end < 0 || w.pos < end {
		start := w.pos
		tag, err := w.varint()
		if errors.Is(err, io.EOF) && end < 0 {
			return nil
		}
		if err != nil {
			return err
		}
		num, typ := protowire.DecodeTag(tag)
		if num < protowire.MinValidNumber {
			return fmt.Errorf("field number %d", num)
		}
		if typ == protowire.EndGroupType {
			// Only a group ends at a tag. Its walk answers this to the
			// field that began it, which checks the numbers match;
			// anywhere else it ends the walk as malformed.
			return errGroupEnd{num}
		}
		var fd protoreflect.FieldDescriptor
		if md != nil {
			fd = md.Fields().ByNumber(num)
		}
		if err := w.field(fd, num, typ, start, depth); err != nil {
			return err
		}
		if err := w.cost.check(); err != nil {
			return err
		}
	}
	if w.pos != end {
		return errors.New("a field runs past the end of its message")
	}
	return nil
}

// errGroupEnd is the end-group tag that ends a group walk.
type errGroupEnd struct{ num protowire.Number }

func (e errGroupEnd) Error() string { return fmt.Sprintf("an end-group tag for field %d", e.num) }

// field walks the value of one field whose tag began at start.
func (w *protoWalk) field(fd protoreflect.FieldDescriptor, num protowire.Number, typ protowire.Type, start int64, depth int) error {
	switch typ {
	case protowire.VarintType:
		if _, err := w.varint(); err != nil {
			return unexpected(err)
		}
		return w.scalar(fd, typ, start)
	case protowire.Fixed32Type:
		if err := w.skip(4); err != nil {
			return err
		}
		return w.scalar(fd, typ, start)
	case protowire.Fixed64Type:
		if err := w.skip(8); err != nil {
			return err
		}
		return w.scalar(fd, typ, start)
	case protowire.BytesType:
		n, err := w.varint()
		if err != nil {
			return unexpected(err)
		}
		if n > MaxOtelPayload {
			return errOtelPlainTooLarge
		}
		return w.delimited(fd, int64(n), start, depth)
	case protowire.StartGroupType:
		var md protoreflect.MessageDescriptor
		known := fd != nil && fd.Kind() == protoreflect.GroupKind
		if known {
			md = fd.Message()
			w.cost.value(fd, 0)
		}
		err := w.message(md, -1, depth+1)
		var ended errGroupEnd
		switch {
		case errors.As(err, &ended) && ended.num == num:
		case errors.As(err, &ended):
			return fmt.Errorf("group %d ended by the tag of %d", num, ended.num)
		case err == nil:
			return io.ErrUnexpectedEOF
		default:
			return err
		}
		if !known {
			w.cost.unknown(w.pos - start)
		}
		return nil
	}
	return fmt.Errorf("wire type %d", typ)
}

// scalar weighs a varint or a fixed-width value: a known field's own, or an
// unknown field's bytes — which is also what the decoder makes of a value
// whose wire type its field does not take.
func (w *protoWalk) scalar(fd protoreflect.FieldDescriptor, typ protowire.Type, start int64) error {
	if fd == nil || wireTypeOf(fd) != typ {
		w.cost.unknown(w.pos - start)
		return nil
	}
	w.cost.value(fd, 0)
	return nil
}

// delimited walks a length-delimited value of n bytes.
func (w *protoWalk) delimited(fd protoreflect.FieldDescriptor, n, start int64, depth int) error {
	end := w.pos + n
	switch {
	case fd == nil:
	case fd.Kind() == protoreflect.MessageKind:
		w.cost.value(fd, 0)
		return w.message(fd.Message(), end, depth+1)
	case fd.Kind() == protoreflect.StringKind || fd.Kind() == protoreflect.BytesKind:
		w.cost.value(fd, n)
		return w.skip(n)
	case fd.IsList() && wireTypeOf(fd) != protowire.BytesType:
		// PACKED: n bytes of fixed-width or varint elements.
		count, err := w.packed(fd, n)
		if err != nil {
			return err
		}
		w.cost.decoded += 2 * count * slot(fd)
		return nil
	}
	if err := w.skip(n); err != nil {
		return err
	}
	w.cost.unknown(w.pos - start)
	return nil
}

// packed counts the elements of a packed repeated field of n bytes.
func (w *protoWalk) packed(fd protoreflect.FieldDescriptor, n int64) (int64, error) {
	switch wireTypeOf(fd) {
	case protowire.Fixed32Type:
		return n / 4, w.skip(n)
	case protowire.Fixed64Type:
		return n / 8, w.skip(n)
	}
	var count int64
	for range n {
		b, err := w.ReadByte()
		if err != nil {
			return 0, unexpected(err)
		}
		if b < 0x80 {
			count++
		}
	}
	return count, nil
}

// wireTypeOf is the wire type a field's values are written in, unpacked.
func wireTypeOf(fd protoreflect.FieldDescriptor) protowire.Type {
	switch fd.Kind() {
	case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind,
		protoreflect.Sint32Kind, protoreflect.Uint32Kind, protoreflect.Int64Kind,
		protoreflect.Sint64Kind, protoreflect.Uint64Kind:
		return protowire.VarintType
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.GroupKind:
		return protowire.StartGroupType
	}
	return protowire.BytesType
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// measureJSON weighs an OTLP/JSON traces export read from r, and — when out
// is not nil — writes it to out as protojson reads it: each trace and span id
// rewritten from OTLP/JSON's hex to the base64 the protobuf JSON mapping takes
// (see [unmarshalOTLPJSON]), and each member the message does not have left
// out, as protojson's DiscardUnknown would skip it.
//
// THE SAME WALK AS [measureProtobuf], over the same descriptors and weighed by
// the same [otelCost.value], so an export costs the same whichever way it was
// encoded. Streaming: jsontext holds a token at a time, never the document,
// so measuring holds at most the largest single string in the export.
// Members are matched as protojson matches them — the JSON name, else the
// proto name — and a value that is not what its field takes is passed on for
// protojson to refuse, after weighing it at its raw length.
func measureJSON(r io.Reader, out *jsontext.Encoder) (otelCost, error) {
	j := &jsonWalk{
		d:   jsontext.NewDecoder(r, jsontext.AllowDuplicateNames(true)),
		out: out,
	}
	if err := j.message(otelTracesRequest, 0); err != nil {
		return otelCost{}, err
	}
	// One document, as protojson reads one: anything after it is not an
	// export.
	if _, err := j.d.ReadToken(); !errors.Is(err, io.EOF) {
		return otelCost{}, errors.New("data after the export")
	}
	j.cost.plain = j.d.InputOffset()
	return j.cost, nil
}

type jsonWalk struct {
	d    *jsontext.Decoder
	out  *jsontext.Encoder
	cost otelCost
}

func (j *jsonWalk) token(tok jsontext.Token) error {
	if j.out == nil {
		return nil
	}
	return j.out.WriteToken(tok)
}

func (j *jsonWalk) raw(v jsontext.Value) error {
	if j.out == nil {
		return nil
	}
	return j.out.WriteValue(v)
}

// open reads the delimiter that begins an object or an array — want — and
// writes it through; what names the value for a refusal.
func (j *jsonWalk) open(want jsontext.Kind, what string) error {
	tok, err := j.d.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != want {
		return fmt.Errorf("%s is a JSON %s where %s was expected", what, tok.Kind(), want)
	}
	return j.token(tok)
}

// close reads the delimiter that ends an object or an array, and writes it
// through.
func (j *jsonWalk) close() error {
	tok, err := j.d.ReadToken()
	if err != nil {
		return err
	}
	return j.token(tok)
}

// message walks one JSON object as a message of md.
func (j *jsonWalk) message(md protoreflect.MessageDescriptor, depth int) error {
	if depth > protowire.DefaultRecursionLimit {
		return errors.New("nested past the decoder's recursion limit")
	}
	if err := j.open('{', string(md.Name())); err != nil {
		return err
	}
	fields := md.Fields()
	for j.d.PeekKind() != '}' {
		name, err := j.d.ReadToken()
		if err != nil {
			return err
		}
		key := name.String()
		fd := fields.ByJSONName(key)
		if fd == nil {
			fd = fields.ByTextName(key)
		}
		if fd == nil {
			if err = j.d.SkipValue(); err != nil {
				return err
			}
			continue
		}
		if err = j.token(name); err != nil {
			return err
		}
		if err = j.member(fd, depth); err != nil {
			return err
		}
		if err = j.cost.check(); err != nil {
			return err
		}
	}
	return j.close()
}

// member walks the value of one field: a list's array, a map's object, or a
// single value.
func (j *jsonWalk) member(fd protoreflect.FieldDescriptor, depth int) error {
	if j.d.PeekKind() == 'n' {
		// null is an unset field, as protojson reads it.
		v, err := j.d.ReadValue()
		if err != nil {
			return err
		}
		return j.raw(v)
	}
	switch {
	case fd.IsList():
		return j.array(string(fd.Name()), func() error { return j.element(fd, depth) })
	case fd.IsMap():
		// No message of a traces export has a map; one is weighed as an
		// object of entries, each a struct beside its value.
		if err := j.open('{', string(fd.Name())); err != nil {
			return err
		}
		for j.d.PeekKind() != '}' {
			key, err := j.d.ReadToken()
			if err != nil {
				return err
			}
			j.cost.decoded += alloc(otelUnknownStructSize) + alloc(int64(len(key.String())))
			if err = j.token(key); err != nil {
				return err
			}
			if err = j.element(fd.MapValue(), depth); err != nil {
				return err
			}
		}
		return j.close()
	}
	return j.element(fd, depth)
}

// array walks a JSON array, each element by each.
func (j *jsonWalk) array(what string, each func() error) error {
	if err := j.open('[', what); err != nil {
		return err
	}
	for j.d.PeekKind() != ']' {
		if err := each(); err != nil {
			return err
		}
		if err := j.cost.check(); err != nil {
			return err
		}
	}
	return j.close()
}

// element walks one value of fd.
func (j *jsonWalk) element(fd protoreflect.FieldDescriptor, depth int) error {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if j.d.PeekKind() == 'n' {
			v, err := j.d.ReadValue()
			if err != nil {
				return err
			}
			return j.raw(v)
		}
		j.cost.value(fd, 0)
		return j.message(fd.Message(), depth+1)
	}
	v, err := j.d.ReadValue()
	if err != nil {
		return err
	}
	switch {
	case fd.Kind() == protoreflect.BytesKind && otlpIDFields[fd.Name()] && v.Kind() == '"':
		id, err := hexID(fd, v)
		if err != nil {
			return err
		}
		j.cost.value(fd, int64(len(id)))
		if j.out == nil {
			return nil
		}
		return j.out.WriteToken(jsontext.String(base64.StdEncoding.EncodeToString(id)))
	case fd.Kind() == protoreflect.BytesKind:
		// base64 holds three bytes in four characters.
		j.cost.value(fd, int64(len(v))*3/4)
	default:
		j.cost.value(fd, int64(len(v)))
	}
	return j.raw(v)
}

// hexID decodes one OTLP/JSON id, which is hex in either case.
func hexID(fd protoreflect.FieldDescriptor, v jsontext.Value) ([]byte, error) {
	text, err := jsontext.AppendUnquote(nil, v)
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(string(text))
	if err != nil {
		return nil, fmt.Errorf("%s %q is not hex", fd.JSONName(), text)
	}
	return id, nil
}
