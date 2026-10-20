package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// Text somebody else wrote, as a read tool hands it to a model.
//
// A read that answers a page body, a work item's description or comments, or a
// project file hands a model words a colleague, a person or an outside party
// wrote, and a model reads every word of its context as a possible
// instruction. So each of those reads answers through a [foreignRead]:
//
//   - the long text fields (a body, a comment, a file's content) go out inside
//     a [provenance.Fence] carrying the turn's boundary, the surface and the
//     writers, at the trust those writers earn from the reader;
//   - EVERY OTHER STRING in the answer, titles, option labels, revision notes,
//     history excerpts and custom-field values alike, is neutralised
//     ([provenance.Neutralise]), so no field outside a fence can open or close
//     one. That is done over the encoded answer rather than field by field, so
//     a field added to a record later is covered without anybody remembering
//     it here, and so a value that spells a bracket as a JSON escape is
//     judged as the character it decodes to;
//   - the answer carries a `provenance` envelope beside its fields: the least
//     trust of its fences, every writer once, and the note that trust carries
//     for a reader that never saw the seat prompt's standing sentence.
//
// The writers are resolved against the turn's chart where a record names a
// handle without a kind, so a page revision by a person reads `human:ana` and
// a revision by a seat the chart no longer has still reads as a colleague's.
//
// It fails closed. A fence registered for a field the answer does not hold is
// a tool's own bug, and the answer is refused rather than sent with that text
// unlabelled somewhere else.

// foreignRead is one read answer's provenance as its tool assembles it.
type foreignRead struct {
	self     string
	boundary provenance.Boundary
	chart    *org.Organization

	fences  map[string]string
	trusts  []provenance.Trust
	writers []provenance.Author
}

// newForeignRead starts the answer self reads in this turn: self is the
// handle the call reads as (the seat in a turn, the person or credential on
// the operator surface), which is the one writer whose text is `own`.
func newForeignRead(turn *turnctx.Turn, self string) *foreignRead {
	r := &foreignRead{self: self, boundary: turn.FenceBoundary(), fences: map[string]string{}}
	if turn != nil {
		r.chart = turn.Org
	}
	return r
}

// writer is a recorded writer as a fence names them.
//
// A record that keeps a kind beside the handle is taken at its word, with one
// exception: an operator credential's login is never a seat, so it is not
// printed as one. A record that keeps only the handle (a page revision, a work
// item's body author, a file's writer) is resolved against the turn's chart;
// a handle the chart does not hold is a seat that has left it when it is
// shaped like a seat handle, and a credential's login otherwise. An empty
// handle with no kind is a writer nobody recorded, which nothing vouches for.
func (r *foreignRead) writer(handle string, kind iam.ActorKind) provenance.Author {
	handle = strings.TrimSpace(handle)
	switch kind {
	case iam.ActorOperator, iam.ActorSystem:
		return provenance.Author{Kind: kind}
	case iam.ActorAgent, iam.ActorHuman:
		return provenance.Author{Kind: kind, Seat: handle}
	}
	if handle == "" {
		return provenance.Author{}
	}
	if r.chart != nil {
		if seat := r.chart.SeatByHandle(handle); seat != nil {
			if seat.IsHuman() {
				return provenance.Author{Kind: iam.ActorHuman, Seat: handle}
			}
			return provenance.Author{Kind: iam.ActorAgent, Seat: handle}
		}
	}
	if iam.ValidSeatHandle(handle) {
		return provenance.Author{Seat: handle}
	}
	return provenance.Author{Kind: iam.ActorOperator}
}

// trustOf is the trust text by writers earns from this reader: [provenance.TrustFor],
// or [provenance.TrustForPartial] when the record says other writers it does
// not name had a hand in it.
func (r *foreignRead) trustOf(writers []provenance.Author, partial bool) provenance.Trust {
	if partial {
		return provenance.TrustForPartial(r.self, writers)
	}
	return provenance.TrustFor(r.self, writers)
}

// attribute records that the answer carries text by writers without fencing
// it: content the answer holds in a form a model does not read as prose (a
// file's bytes as base64), which still needs its label on the envelope.
func (r *foreignRead) attribute(writers []provenance.Author, partial bool) {
	r.trusts = append(r.trusts, r.trustOf(writers, partial))
	for _, w := range writers {
		r.name(w)
	}
}

// name adds w to the envelope's writers once. ONE SEAT IS ONE WRITER, whether
// a record named its kind or only its handle: outside a turn there is no chart
// to resolve a bare handle against, and an envelope listing `agent-cto` and
// `agent:agent-cto` side by side would read as two people. The kinded entry
// wins, because it says more.
func (r *foreignRead) name(w provenance.Author) {
	for i, held := range r.writers {
		switch {
		case held == w:
			return
		case w.Seat != "" && held.Seat == w.Seat && w.Kind == "":
			return
		case w.Seat != "" && held.Seat == w.Seat && held.Kind == "":
			r.writers[i] = w
			return
		}
	}
	r.writers = append(r.writers, w)
}

// fence registers the string at path in the answer as text by writers, read
// from src. Empty text is not fenced and earns no label: there is nothing in
// it to rely on or not.
func (r *foreignRead) fence(path fieldPath, src provenance.Source,
	writers []provenance.Author, partial bool, text string) {

	if text == "" {
		return
	}
	r.attribute(writers, partial)
	r.fences[path.id()] = provenance.Fence(r.boundary, src, r.trustOf(writers, partial), writers, text)
}

// fenceAt registers the string at path in the answer as text by writers, read
// from src, at a trust its caller decided rather than one the writers earn:
// [provenance.TrustCompany] for what an administrator adopted for everybody,
// which no set of authors is ever inferred to be. Empty text is not fenced.
func (r *foreignRead) fenceAt(path fieldPath, src provenance.Source, trust provenance.Trust,
	writers []provenance.Author, text string) {

	if text == "" {
		return
	}
	r.trusts = append(r.trusts, trust)
	for _, w := range writers {
		r.name(w)
	}
	r.fences[path.id()] = provenance.Fence(r.boundary, src, trust, writers, text)
}

// envelope labels the whole answer, or is false when it fenced nothing.
func (r *foreignRead) envelope() (provenance.Envelope, bool) {
	if len(r.trusts) == 0 {
		return provenance.Envelope{}, false
	}
	return provenance.NewEnvelope(provenance.Least(r.trusts...), r.writers), true
}

// answer renders v as the tool's answer: every string neutralised, every
// registered field fenced, and the envelope beside the top-level fields. v
// must encode as a JSON object.
func (r *foreignRead) answer(ctx context.Context, tool string, v any, narrowWith string) (tools.Result, error) {
	rendered, err := r.render(v)
	if err != nil {
		return faulted(ctx, tool, err, fmt.Sprintf("%s could not label what it "+
			"read with who wrote it, so it is not answering at all: %s.", tool, faultSaid)), nil
	}
	return jsonAnswer(rendered, narrowWith)
}

// render is [foreignRead.answer]'s encoding, kept apart so its refusals are
// errors a caller can wrap.
//
// EVERY STRING IT WRITES IS WRITTEN AS [encodeAnswer] WRITES ONE, through
// [plainjson]: a fenced answer is the same tool answer as any other, and the
// encoder it is finally handed to compacts a raw value rather than decoding
// it, so a body written here with `json.Marshal`'s `\u003c` reached the model
// as the escape — a fenced page of markup read as escape soup, and weighed
// against [ToolAnswerBytes] at six bytes a bracket, exactly what the plain
// encoding exists to stop.
func (r *foreignRead) render(v any) (json.RawMessage, error) {
	data, err := plainjson.Marshal(v, "")
	if err != nil {
		return nil, fmt.Errorf("builtin: encode a read answer: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	w := &fenceWriter{read: r, dec: dec, written: map[string]bool{}}
	if err := w.value(nil, true); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("builtin: a read answer encoded as more than one value")
	}
	for path := range r.fences {
		if !w.written[path] {
			return nil, fmt.Errorf("builtin: a fence was registered at %q, which "+
				"the answer does not hold as a string", readablePath(path))
		}
	}
	return w.out.Bytes(), nil
}

// pathSep joins the steps of a path; a byte no JSON key a record writes holds.
const pathSep = "\x1f"

// fieldPath is where one field sits in an answer, outermost step first: an
// object key or an array position. The zero value is the answer itself.
type fieldPath []string

// key is the field under k in the object at p.
func (p fieldPath) key(k string) fieldPath { return append(slices.Clip(p), "k"+k) }

// index is the i-th element of the array at p.
func (p fieldPath) index(i int) fieldPath {
	return append(slices.Clip(p), "i"+strconv.Itoa(i))
}

func (p fieldPath) id() string { return strings.Join(p, pathSep) }

// readablePath is a path id as an error names it.
func readablePath(id string) string {
	parts := strings.Split(id, pathSep)
	for i, part := range parts {
		if part != "" {
			parts[i] = part[1:]
		}
	}
	return strings.Join(parts, ".")
}

// fenceWriter re-encodes an answer token by token, in the order its encoder
// wrote it, so a reader sees the record's own field order.
type fenceWriter struct {
	read    *foreignRead
	dec     *json.Decoder
	out     bytes.Buffer
	written map[string]bool
}

func (w *fenceWriter) value(path fieldPath, top bool) error {
	tok, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("builtin: re-read a read answer: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(path, top)
		case '[':
			if top {
				return errors.New("builtin: a read answer is an object, not a list")
			}
			return w.array(path)
		}
		return fmt.Errorf("builtin: unexpected %q in a read answer", t)
	case string:
		key := path.id()
		if fenced, ok := w.read.fences[key]; ok {
			w.written[key] = true
			return w.str(fenced)
		}
		return w.str(provenance.Neutralise(w.read.boundary, t))
	case json.Number:
		w.out.WriteString(t.String())
	case bool:
		w.out.WriteString(strconv.FormatBool(t))
	case nil:
		w.out.WriteString("null")
	default:
		return fmt.Errorf("builtin: unexpected %T in a read answer", tok)
	}
	if top {
		return errors.New("builtin: a read answer is an object, not a scalar")
	}
	return nil
}

func (w *fenceWriter) object(path fieldPath, top bool) error {
	w.out.WriteByte('{')
	first := true
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return fmt.Errorf("builtin: re-read a read answer: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("builtin: an object key in a read answer is %T", tok)
		}
		if top && key == provenanceField {
			return fmt.Errorf("builtin: a read answer already has a %q field", provenanceField)
		}
		if !first {
			w.out.WriteByte(',')
		}
		first = false
		// A KEY IS TEXT TOO: a custom field's values are keyed by ids the
		// engine mints, but a map a record keeps under writer-chosen keys
		// would otherwise be the one string nothing defused.
		if err := w.str(provenance.Neutralise(w.read.boundary, key)); err != nil {
			return err
		}
		w.out.WriteByte(':')
		if err := w.value(path.key(key), false); err != nil {
			return err
		}
	}
	if top {
		if env, ok := w.read.envelope(); ok {
			label, err := plainjson.Marshal(env, "")
			if err != nil {
				return fmt.Errorf("builtin: encode a read answer's provenance: %w", err)
			}
			if !first {
				w.out.WriteByte(',')
			}
			w.out.WriteString(`"` + provenanceField + `":`)
			w.out.Write(label)
		}
	}
	if _, err := w.dec.Token(); err != nil {
		return fmt.Errorf("builtin: re-read a read answer: %w", err)
	}
	w.out.WriteByte('}')
	return nil
}

func (w *fenceWriter) array(path fieldPath) error {
	w.out.WriteByte('[')
	for i := 0; w.dec.More(); i++ {
		if i > 0 {
			w.out.WriteByte(',')
		}
		if err := w.value(path.index(i), false); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil {
		return fmt.Errorf("builtin: re-read a read answer: %w", err)
	}
	w.out.WriteByte(']')
	return nil
}

func (w *fenceWriter) str(s string) error {
	data, err := plainjson.Marshal(s, "")
	if err != nil {
		return fmt.Errorf("builtin: encode a string in a read answer: %w", err)
	}
	w.out.Write(data)
	return nil
}

// provenanceField is the key the envelope is answered under.
const provenanceField = "provenance"

// unfence is a body or a content argument as a write stores it: without the
// fence of this turn's boundary a model sent back round it
// ([provenance.Unfence]). Outside a turn, or in one that carries no boundary,
// nothing this surface wrote can be recognised, so the text is the caller's.
func unfence(turn *turnctx.Turn, text string) string {
	if turn == nil || !turn.Boundary.Valid() {
		return text
	}
	return provenance.Unfence(turn.Boundary, text)
}
