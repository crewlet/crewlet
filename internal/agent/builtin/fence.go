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

// Text somebody else wrote, as a read tool hands it to a model, which reads
// every word of its context as a possible instruction (ADR-0040). A read of a
// page body, a work item's text or a project file answers through a
// [foreignRead]: its long text fields go inside a [provenance.Fence] naming
// the turn's boundary, the surface and the writers; EVERY OTHER STRING is
// neutralised ([provenance.Neutralise]) so none can open or close a fence,
// over the encoded answer rather than field by field, so a field added later
// is covered and a JSON-escaped bracket is judged as the character it decodes
// to; and a `provenance` envelope carries the least trust, every writer once,
// and that trust's note for a reader that never saw the seat prompt's standing
// sentence. It fails closed: a fence registered for a field the answer lacks
// is the tool's bug, and the answer is refused rather than sent unlabelled.

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

// writer is a recorded writer as a fence names them. A recorded kind is taken
// at its word, except that an operator credential's login is never printed as
// a seat. A bare handle is resolved against the turn's chart; one the chart
// lacks is a departed seat when shaped like a seat handle, else a credential's
// login. An empty handle with no kind is a writer nobody recorded, which on
// these surfaces is a colleague ([foreignRead.label]), never an outside party.
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

// label is what text by writers, read from src, is fenced under for this
// reader: partial when the record says writers it does not name had a hand in
// it. Every read here is of a surface the engine hosts, so it takes the one
// rule every door to such a surface reads by ([provenance.LabelForHosted]): an
// unnamed writer is a colleague, never an outside party.
func (r *foreignRead) label(src provenance.Source, writers []provenance.Author,
	partial bool) provenance.Label {

	return provenance.LabelForHosted(r.self, src, writers, !partial)
}

// attribute records that the answer carries text by writers without fencing
// it: content the answer holds in a form a model does not read as prose (a
// file's bytes as base64), which still needs its label on the envelope.
func (r *foreignRead) attribute(writers []provenance.Author, partial bool) {
	label := r.label("", writers, partial)
	r.trusts = append(r.trusts, label.Trust)
	for _, w := range label.Authors {
		r.name(w)
	}
}

// adopt records text another door labelled — a knowledge hit, which the
// backend that found it labels, a vendor's as a vendor's — on the envelope as
// that label says, rather than deciding again here as though every source were
// a surface the engine hosts.
func (r *foreignRead) adopt(label provenance.Label) {
	r.trusts = append(r.trusts, label.Trust)
	for _, w := range label.Authors {
		r.name(w)
	}
}

// name adds w to the envelope's writers once. ONE SEAT IS ONE WRITER, kinded
// or bare: outside a turn no chart resolves a bare handle, and `agent-cto`
// beside `agent:agent-cto` would read as two people. The kinded entry wins,
// because it says more.
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
	r.fences[path.id()] = r.label(src, writers, partial).Fence(r.boundary, text)
}

// fenceRepository registers the string at path as text read out of a
// repository, or printed by a check of one of its commits, from src, by
// [provenance.Repository]: naming nobody, at the least trust. Never
// [foreignRead.fence], whose hosted rule reads an unnamed writer as a
// colleague. Empty text is not fenced.
func (r *foreignRead) fenceRepository(path fieldPath, src provenance.Source, text string) {
	r.fenceLabelled(path, provenance.Repository(src), text)
}

// fenceServer registers the string at path in the answer as what a command
// printed on one of the company's servers, by [provenance.ServerOutput]:
// naming nobody, at the least trust. Empty text is not fenced.
func (r *foreignRead) fenceServer(path fieldPath, text string) {
	r.fenceLabelled(path, provenance.ServerOutput(), text)
}

// fenceLabelled registers the string at path under label. Empty text is not
// fenced.
func (r *foreignRead) fenceLabelled(path fieldPath, label provenance.Label, text string) {
	if text == "" {
		return
	}
	r.trusts = append(r.trusts, label.Trust)
	r.fences[path.id()] = label.Fence(r.boundary, text)
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

// answer renders v, which must encode as a JSON object, as the tool's answer:
// every string neutralised, every registered field fenced, and the envelope
// beside the top-level fields. It also sets [tools.Result.PersonReceipt] to v
// as stored, unfenced and unescaped ([encodeAnswer]): the labels are for a
// model, and a person's surface could not strip them back off, since the
// escapes do not undo. Done here rather than at each tool, for reads and
// writes alike, so no tool has to remember it.
func (r *foreignRead) answer(ctx context.Context, tool string, v any, narrowWith string) (tools.Result, error) {
	plain, err := encodeAnswer(v)
	var rendered json.RawMessage
	if err == nil {
		rendered, err = r.render(v)
	}
	if err != nil {
		return labelFault(ctx, tool, err), nil
	}
	res, err := jsonAnswer(rendered, narrowWith)
	if err == nil && !res.Failed {
		res.PersonReceipt = plain
	}
	return res, err
}

// page is [foreignRead.answer] for a code tool's answer, sent COMPACT
// ([compactAnswer]): what it wraps is a page something below this package cut
// by its own encoded bytes, and rendered indented it would arrive past the
// bound it was cut to fit.
func (r *foreignRead) page(ctx context.Context, tool string, v any, narrowWith string) (tools.Result, error) {
	rendered, err := r.render(v)
	if err != nil {
		return labelFault(ctx, tool, err), nil
	}
	return compactAnswer(rendered, narrowWith)
}

// labelFault is the answer of a read whose labelling failed: the tool's own
// fault, never the text unlabelled ([foreignRead]'s fail-closed rule).
func labelFault(ctx context.Context, tool string, err error) tools.Result {
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not label what it "+
		"read with who wrote it, so it is not answering at all: %s.", tool, faultSaid))
}

// render is [foreignRead.answer]'s encoding, kept apart so its refusals are
// errors a caller can wrap. Every string goes through [plainjson], as
// [encodeAnswer] writes one: the final encoder compacts a raw value rather than
// decoding it, so `json.Marshal`'s `\u003c` would reach the model as the
// escape and weigh six bytes a bracket against [ToolAnswerBytes].
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
	if err := r.labelsDeclared(v, w.written); err != nil {
		return nil, err
	}
	return w.out.Bytes(), nil
}

// labelsDeclared refuses an answer v that leaves a field its records declare
// somebody's words unlabelled: one to fence must be among the fenced paths
// written, and one that is a name needs an envelope saying whose it is. A
// tool that answers a record without labelling what the record declares is
// refused here rather than heard from in a transcript ([declaredIn]).
func (r *foreignRead) labelsDeclared(v any, written map[string]bool) error {
	declared, err := declaredIn(v)
	if err != nil {
		return err
	}
	_, labelled := r.envelope()
	for _, f := range declared {
		switch f.kind {
		case provenance.FieldFence:
			if !written[f.path.id()] {
				return fmt.Errorf("builtin: the answer carries %q, which its record "+
					"declares somebody's words, outside a fence", readablePath(f.path.id()))
			}
		case provenance.FieldName:
			if !labelled {
				return fmt.Errorf("builtin: the answer carries %q, which its record "+
					"declares somebody's words, and says nobody wrote it", readablePath(f.path.id()))
			}
		default:
			return fmt.Errorf("builtin: %q is declared %s:%q, which is no kind of field "+
				"this package knows", readablePath(f.path.id()), provenance.FieldTag, f.kind)
		}
	}
	return nil
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
