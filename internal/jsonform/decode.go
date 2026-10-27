package jsonform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Fault is one structural fault of a JSON value: the field it is at (`""` for
// the whole value), the 1-based line it was written on where one is known, and
// a sentence that names the field and the shape the format reads there.
type Fault struct {
	Field   string
	Line    int
	Message string
}

// Decoded is what [Decode] found: every fault, the line each field was
// written on, and which fields were dropped from the typed decode.
type Decoded struct {
	// Faults are every structural fault, in file order.
	Faults []Fault

	// tolerated marks the faults, by index, that a [Stored] read admits (a
	// key written twice, a key in another case of one the object carries):
	// a refusal of a stored version names only the faults its read refused.
	tolerated map[int]bool

	// Fatal reports the bytes could not be read as one value of the form
	// at all (not JSON, more after the value, or a value its own reader
	// refused), so nothing was decoded and no field rule should run.
	Fatal bool

	lines   map[string]int
	dropped map[string]bool
}

// Line is the line field was written on, or 0.
func (d *Decoded) Line(field string) int { return d.lines[field] }

// Dropped reports field, or an object or list it is inside, was left out of
// the typed decode for a structural fault: a rule about it would be about a
// value the author never wrote, so its fault already says what to change.
func (d *Decoded) Dropped(field string) bool {
	for p := field; ; {
		if d.dropped[p] {
			return true
		}
		parent, ok := parentOf(p)
		if !ok {
			return d.dropped[""]
		}
		p = parent
	}
}

// Touched reports a structural fault at field or anywhere inside it.
func (d *Decoded) Touched(field string) bool {
	for p := range d.dropped {
		if p == field || strings.HasPrefix(p, field+".") || strings.HasPrefix(p, field+"[") {
			return true
		}
	}
	return false
}

// parentOf is the field a path is inside: `a.b` is in `a`, `a[2]` in `a`,
// `a["x"]` in `a`.
func parentOf(p string) (string, bool) {
	i := strings.LastIndexAny(p, ".[")
	if i < 0 {
		return "", p != ""
	}
	if p[i] == '[' && strings.HasSuffix(p, "\"]") {
		// A map key may itself hold a dot or a bracket: cut at its
		// opening `["`.
		if j := strings.LastIndex(p, "[\""); j >= 0 {
			i = j
		}
	}
	return p[:i], true
}

// Decode reads raw as one value of form into `into` (a pointer to the Go type
// the form was derived from), naming every structural fault in one pass: a key
// written twice, a key the form does not carry (with the keys it does), a
// value of the wrong type (with the shape the form reads there), and anything
// after the value. A faulted value is left out of the typed decode, so `into`
// holds every well-formed field and the format's own rules can run over them;
// [Decoded.Dropped] tells those rules which fields to leave to their fault.
//
// The subject names the file and its format in the faults.
//
// THIS IS THE PUBLISH READ: what a version is held to when it is written. A
// version already stored is read with [Stored].
func Decode(raw []byte, form *Form, subject Subject, into any) *Decoded {
	d := &Decoded{lines: map[string]int{}, dropped: map[string]bool{}}
	w := &walker{d: d, raw: raw, subject: subject, dec: json.NewDecoder(bytes.NewReader(raw))}
	w.dec.UseNumber()
	clean, keep, err := w.value(form, "")
	if err != nil {
		d.fatal(w.line(), "%s is not JSON: %s", subject.File, jsonReason(err))
		return d
	}
	if _, trailing := w.dec.Token(); !errors.Is(trailing, io.EOF) {
		d.fatal(w.line(), "%s holds more after its value: one value is the whole file", subject.File)
		return d
	}
	if !keep {
		d.Fatal = true
		return d
	}
	body, err := encode(clean)
	if err != nil {
		d.fatal(0, "%s cannot be read: %v", subject.File, err)
		return d
	}
	// UNKNOWN FIELDS REFUSED HERE TOO, as a backstop: the walk admitted only
	// the form's keys, so a key the typed decode does not know is a form
	// that disagrees with its struct, and it is a fault rather than a value
	// silently dropped.
	typed := json.NewDecoder(bytes.NewReader(body))
	typed.DisallowUnknownFields()
	if decodeErr := typed.Decode(into); decodeErr != nil {
		// THE WALK HELD EVERY VALUE TO ITS FORM, and every value with its
		// own reader to that reader, so this is a value the form could not
		// describe (a `,string` field, a key the struct does not carry):
		// its own sentence, at its field.
		var mismatch *json.UnmarshalTypeError
		if errors.As(decodeErr, &mismatch) && mismatch.Field != "" {
			d.fatal(d.lines[mismatch.Field], "%s is a JSON %s, and the format reads %s there",
				mismatch.Field, mismatch.Value, mismatch.Type)
			return d
		}
		d.fatal(0, "%s cannot be read: %s", subject.File, jsonReason(decodeErr))
	}
	return d
}

// encode is a well-formed value as the typed decode and a value's own reader
// read it: UNESCAPED, so a reader is handed the text the author wrote
// (compacted), never `&` re-spelt as `\u0026`.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Reader is [Decode] or a [Stored] read, for a format read both ways: the
// publish of a version and the read of one already stored.
type Reader func(raw []byte, form *Form, subject Subject, into any) *Decoded

// Trailing is what a format's read before ADR-0059 refused after its value.
type Trailing uint8

const (
	// UntilEOF refused anything but white space after the value: the
	// decoder's next token was io.EOF (the deck, doc and dashboard indexes).
	UntilEOF Trailing = iota

	// NoMoreValues refused only another value after it: the decoder's
	// More was false, which it also is before a stray `}` or `]`, so a
	// canvas ending `} }` was accepted (the design canvas).
	NoMoreValues
)

// Stored is the read of a version already accepted, for a format that read
// its JSON with encoding/json and DisallowUnknownFields before ADR-0059
// tightened the publish: ACCEPTANCE IS THAT EXACT CALL, the decoder, the
// option, the decode and the format's own trailing rule, so whatever was
// stored then reads now, to the same value, by construction (a key in another
// case, a key written twice, a stray closing bracket after a canvas). Only when
// that call refuses does the walk run, to name every fault at once the way a
// publish names them.
//
// WHY NOT ONE READ. A format tightened at publish must not turn a version it
// stored under the older rule into one its own reader refuses: every read of
// a stored version (a render, get_artifact, the dashboard's data, a compare)
// would fail on bytes nobody can now republish without a person's edit.
func Stored(trailing Trailing) Reader {
	return func(raw []byte, form *Form, subject Subject, into any) *Decoded {
		err := legacy(raw, into, trailing)
		if err == nil {
			return &Decoded{lines: map[string]int{}, dropped: map[string]bool{}}
		}
		fresh := reflect.New(reflect.TypeOf(into).Elem()).Interface()
		walked := Decode(raw, form, subject, fresh)
		// ONLY WHAT THIS READ REFUSES: a key in another case and a key
		// written twice are faults to a publish and fine here, so a refusal
		// of a stored version never asks its reader to fix what it accepts.
		d := &Decoded{lines: walked.lines, dropped: walked.dropped, Fatal: true}
		for i, f := range walked.Faults {
			if !walked.tolerated[i] {
				d.Faults = append(d.Faults, f)
			}
		}
		if len(d.Faults) == 0 {
			d.Faults = append(d.Faults, Fault{Message: fmt.Sprintf("%s cannot be read: %s",
				subject.File, jsonReason(err))})
		}
		return d
	}
}

// legacy is the read a format made before ADR-0059.
func legacy(raw []byte, into any, trailing Trailing) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	switch trailing {
	case NoMoreValues:
		if dec.More() {
			return errors.New("more than one JSON value")
		}
	default:
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return errors.New("more than one JSON value")
		}
	}
	return nil
}

func (d *Decoded) fault(field string, line int, format string, args ...any) {
	d.Faults = append(d.Faults, Fault{Field: field, Line: line, Message: fmt.Sprintf(format, args...)})
	d.dropped[field] = true
}

// tolerate marks the fault just reported as one a [Stored] read admits.
func (d *Decoded) tolerate() {
	if d.tolerated == nil {
		d.tolerated = map[int]bool{}
	}
	d.tolerated[len(d.Faults)-1] = true
}

func (d *Decoded) fatal(line int, format string, args ...any) {
	d.fault("", line, format, args...)
	d.Fatal = true
}

// jsonReason is a decode error in an author's words.
func jsonReason(err error) string {
	return strings.TrimPrefix(err.Error(), "json: ")
}

// walker walks a value's tokens once, recording where each field was written
// and building the well-formed copy the typed decode reads.
type walker struct {
	d       *Decoded
	raw     []byte
	subject Subject
	dec     *json.Decoder
}

// Subject is what a fault calls the value it reads: the file (`brand.json`),
// and the format it is held to (`the brand format`).
type Subject struct {
	File   string
	Format string
}

func (w *walker) line() int {
	offset := w.dec.InputOffset()
	if offset > int64(len(w.raw)) {
		offset = int64(len(w.raw))
	}
	return 1 + bytes.Count(w.raw[:offset], []byte("\n"))
}

// value reads one value whose form is f (nil: anything, its keys unchecked
// but never repeated), answering its well-formed copy and whether to keep it.
func (w *walker) value(f *Form, path string) (any, bool, error) {
	v, keep, err := w.read(f, path)
	if err != nil || !keep || f == nil || f.reader == nil {
		return v, keep, err
	}
	// A VALUE WITH ITS OWN READER IS HELD TO IT HERE, at its path, so its
	// refusal is one fault among the others rather than the one fatal
	// error the typed decode would stop at (a dashboard's engine argument,
	// a calc operand).
	body, encErr := encode(v)
	if encErr == nil {
		encErr = reflect.New(f.reader).Interface().(json.Unmarshaler).UnmarshalJSON(body)
	}
	if encErr != nil {
		w.d.fault(path, w.d.lines[path], "%s: %s", path, jsonReason(encErr))
		return nil, false, nil
	}
	return v, true, nil
}

// read reads one value whose form is f, before a value with its own reader is
// held to that reader ([walker.value]).
func (w *walker) read(f *Form, path string) (any, bool, error) {
	if path != "" {
		if _, seen := w.d.lines[path]; !seen {
			w.d.lines[path] = w.line()
		}
	}
	tok, err := w.dec.Token()
	if err != nil {
		return nil, false, err
	}
	switch t := tok.(type) {
	case json.Delim:
		return w.compound(f, path, t)
	case nil:
		// NULL IS ABSENT, as encoding/json reads it into any field.
		return nil, true, nil
	}
	if f == nil || f.Type == Any {
		return tok, true, nil
	}
	if ok := scalarFits(f, tok); !ok {
		w.mismatch(f, path, scalarName(tok))
		return nil, false, nil
	}
	return tok, true, nil
}

// compound reads an object or a list, whose opening delimiter was d.
func (w *walker) compound(f *Form, path string, d json.Delim) (any, bool, error) {
	if d == '{' {
		if f != nil && f.Type != Any && f.Type != Object && f.Type != Map {
			w.mismatch(f, path, "an object")
			return nil, false, w.skip()
		}
		return w.object(f, path)
	}
	if f != nil && f.Type != Any && f.Type != Array {
		w.mismatch(f, path, "a list")
		return nil, false, w.skip()
	}
	var elem *Form
	if f != nil && f.Type == Array {
		elem = f.Elem
	}
	out := []any{}
	for i := 0; w.dec.More(); i++ {
		v, keep, err := w.value(elem, path+"["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, false, err
		}
		if !keep {
			// THE PLACE IS KEPT, so a later element's rule names the
			// index the author wrote; the faulted one decodes as its
			// zero value, which [Decoded.Dropped] keeps every rule off.
			v = nil
		}
		out = append(out, v)
	}
	_, err := w.dec.Token()
	return out, true, err
}

// object reads an object's members after its `{`.
func (w *walker) object(f *Form, path string) (any, bool, error) {
	open := f != nil && f.Type == Map
	closed := f != nil && f.Type == Object
	out := map[string]any{}
	seen := map[string]bool{}
	for w.dec.More() {
		keyTok, err := w.dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, _ := keyTok.(string)
		at := w.line()
		name := member(path, key, open)
		var next *Form
		ok := true
		switch {
		case seen[key]:
			w.d.fault(name, at, "%s is written twice: write each key once", name)
			w.d.tolerate()
			ok = false
		case closed && f.Field(key) == nil:
			w.d.fault(name, at, "%s is not a key %s carries: %s takes %s", name, w.subject.Format,
				w.where(path), strings.Join(f.Keys(), ", "))
			if slices.ContainsFunc(f.Keys(), func(k string) bool { return strings.EqualFold(k, key) }) {
				w.d.tolerate()
			}
			ok = false
		case closed:
			next = f.Field(key)
		case open:
			next = f.Elem
		}
		w.d.lines[name] = at
		if !ok {
			seen[key] = true
			if skipErr := w.discard(); skipErr != nil {
				return nil, false, skipErr
			}
			continue
		}
		seen[key] = true
		v, keep, err := w.value(next, name)
		if err != nil {
			return nil, false, err
		}
		if keep {
			out[key] = v
		}
	}
	_, err := w.dec.Token()
	return out, true, err
}

// member is the path of an object's member: `a.b` for a closed object's key,
// `a["b"]` for a name an author chose.
func member(path, key string, named bool) string {
	switch {
	case named:
		return path + "[" + strconv.Quote(key) + "]"
	case path == "":
		return key
	}
	return path + "." + key
}

// where is the object a key list is about, as a fault names it.
func (w *walker) where(path string) string {
	if path == "" {
		return "the top level of " + w.subject.File
	}
	return path
}

// mismatch reports a value of the wrong type.
func (w *walker) mismatch(f *Form, path, got string) {
	name := path
	if name == "" {
		name = w.subject.File
	}
	w.d.fault(path, w.d.lines[path], "%s is %s, and the format reads %s there", name, got, f.Describe())
}

// skip consumes the rest of a compound value whose opening delimiter was read.
func (w *walker) skip() error {
	for depth := 1; depth > 0; {
		tok, err := w.dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// discard consumes one whole value.
func (w *walker) discard() error {
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		return w.skip()
	}
	return nil
}

// scalarFits reports a scalar token is a value of f.
func scalarFits(f *Form, tok json.Token) bool {
	switch v := tok.(type) {
	case string:
		return f.Type == String
	case bool:
		return f.Type == Boolean
	case json.Number:
		switch f.Type {
		case Number:
			return true
		case Integer:
			_, err := strconv.ParseInt(v.String(), 10, 64)
			return err == nil
		}
	}
	return false
}

// scalarName is what a scalar token is, as a fault says it.
func scalarName(tok json.Token) string {
	switch v := tok.(type) {
	case string:
		return "a string"
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		if _, err := strconv.ParseInt(v.String(), 10, 64); err != nil {
			return "the number " + v.String()
		}
		return "a number"
	}
	return "a value"
}
