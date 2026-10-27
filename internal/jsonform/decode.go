package jsonform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	body, err := json.Marshal(clean)
	if err != nil {
		d.fatal(0, "%s cannot be read: %v", subject.File, err)
		return d
	}
	if decodeErr := json.Unmarshal(body, into); decodeErr != nil {
		// A value with its own reader (an Any form) refused what it was
		// handed: its own sentence, at the field it names.
		var typed *json.UnmarshalTypeError
		if errors.As(decodeErr, &typed) && typed.Field != "" {
			d.fatal(d.lines[typed.Field], "%s is a JSON %s, and the format reads %s there",
				typed.Field, typed.Value, typed.Type)
			return d
		}
		d.fatal(0, "%s cannot be read: %s", subject.File, jsonReason(decodeErr))
	}
	return d
}

func (d *Decoded) fault(field string, line int, format string, args ...any) {
	d.Faults = append(d.Faults, Fault{Field: field, Line: line, Message: fmt.Sprintf(format, args...)})
	d.dropped[field] = true
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
			ok = false
		case closed && f.Field(key) == nil:
			w.d.fault(name, at, "%s is not a key %s carries: %s takes %s", name, w.subject.Format,
				w.where(path), strings.Join(f.Keys(), ", "))
			ok = false
		case closed:
			next = f.Field(key)
		case open:
			next = f.Elem
		}
		seen[key] = true
		w.d.lines[name] = at
		if !ok {
			if skipErr := w.discard(); skipErr != nil {
				return nil, false, skipErr
			}
			continue
		}
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
