package dashspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonform"
)

// EntryPath is where a dashboard's spec lives in its version: the kind's
// entry.
const EntryPath = "dashboard.json"

// FormatVersion is the spec format this build reads and writes (`"v": 1`).
const FormatVersion = 1

// Spec is one version's dashboard.
type Spec struct {
	Version  int       `json:"v"`
	Title    string    `json:"title,omitempty"`
	Params   []Param   `json:"params,omitempty"`
	Datasets []Dataset `json:"datasets"`
	Widgets  []Widget  `json:"widgets"`
}

// Param is one filter the board offers: a closed set of choices, so a value a
// viewer sends is always one the author wrote.
type Param struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Options []Option `json:"options"`
	Default string   `json:"default"`
}

// Option is one choice of a filter.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// SourceKind is where a dataset's rows come from.
type SourceKind string

// The four sources (D62).
const (
	SourceStore  SourceKind = "store"
	SourceEngine SourceKind = "engine"
	SourceFile   SourceKind = "file"
	SourceCalc   SourceKind = "calc"
)

// SourceKinds are every source a dataset may read.
var SourceKinds = []SourceKind{SourceStore, SourceEngine, SourceFile, SourceCalc}

// Valid reports whether k is a source this build reads.
func (k SourceKind) Valid() bool { return slices.Contains(SourceKinds, k) }

// Dataset is one named set of rows a widget draws.
type Dataset struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Source SourceKind `json:"source"`

	// Exactly the one block Source names.
	Store  *StoreSource `json:"store,omitempty"`
	File   *FileSource  `json:"file,omitempty"`
	Engine *EngineRead  `json:"engine,omitempty"`
	Calc   *Calc        `json:"calc,omitempty"`

	// Columns are what a store or a file dataset holds, declared because
	// neither the store's schema nor a file's header is the spec's to
	// promise. An engine dataset's are the catalogue's and a calculated
	// one's are derived, so neither declares any.
	Columns []Column `json:"columns,omitempty"`

	// Key is the column naming a row, for a store or a file dataset; a
	// store dataset's default is the document id ([StoreIDColumn]).
	Key string `json:"key,omitempty"`

	// Refresh is an engine dataset's interval.
	Refresh *Refresh `json:"refresh,omitempty"`
}

// EngineRead is an engine dataset's read: a catalogue name and its arguments.
type EngineRead struct {
	Name   string         `json:"name"`
	Params map[string]Arg `json:"params,omitempty"`
}

// StoreSource is a store dataset's collection.
type StoreSource struct {
	Collection string `json:"collection"`
}

// StoreIDColumn is the column a store dataset's rows carry their document id
// under, and the key a store dataset takes unless it names another.
const StoreIDColumn = "_id"

// FileSource is a file dataset's file of the version.
type FileSource struct {
	Path string `json:"path"`
}

// Refresh is how often an engine dataset is read again when its domain
// announces no change (D63). A schedule an agent keeps a store dataset on is
// a later field here, beside the interval.
type Refresh struct {
	EverySeconds int `json:"every_seconds"`
}

// Arg is one engine parameter's value: a literal, or the board's filter it is
// bound to.
type Arg struct {
	// Literal is the value in its text form, when the argument is one.
	Literal string

	// Param is the filter's key, when the argument is bound to one.
	Param string
}

// UnmarshalJSON reads a string, a number or a boolean as a literal, and
// `{"param": "<key>"}` as a binding.
func (a *Arg) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		var bound struct {
			Param string `json:"param"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&bound); err != nil || bound.Param == "" {
			return errors.New(`a bound argument is {"param": "<filter key>"}`)
		}
		*a = Arg{Param: bound.Param}
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		*a = Arg{Literal: x}
	case json.Number:
		*a = Arg{Literal: x.String()}
	case bool:
		*a = Arg{Literal: strconv.FormatBool(x)}
	default:
		return errors.New("an argument is a string, a number, a boolean or {\"param\": \"<filter key>\"}")
	}
	return nil
}

// MarshalJSON writes the form [Arg.UnmarshalJSON] reads.
func (a Arg) MarshalJSON() ([]byte, error) {
	if a.Param != "" {
		return json.Marshal(map[string]string{"param": a.Param})
	}
	return json.Marshal(a.Literal)
}

// ---- parsing ------------------------------------------------------------ //

// Finding is one thing wrong with a version's dashboard, at a place a person
// can find: the file, a line where one is known, and the field as a path
// (`datasets[2].engine.params.scope`).
type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.Path)
	if f.Line > 0 {
		b.WriteString(":" + strconv.Itoa(f.Line))
	}
	if f.Field != "" {
		b.WriteString(": " + f.Field)
	}
	b.WriteString(": " + f.Message)
	return b.String()
}

// ErrInvalid is every refusal this package answers, wrapped by [*Invalid].
var ErrInvalid = errors.New("dashspec: the dashboard is not one this build draws")

// Invalid is a spec refused, with every finding in the order found, so an
// author fixes them all in one pass.
type Invalid struct {
	Findings []Finding
}

func (e *Invalid) Error() string {
	lines := make([]string, len(e.Findings))
	for i, f := range e.Findings {
		lines[i] = f.String()
	}
	return "the dashboard breaks its format: " + strings.Join(lines, "; ")
}

// Unwrap makes every refusal an [ErrInvalid].
func (e *Invalid) Unwrap() error { return ErrInvalid }

// Parse reads a spec, refusing a field the format does not have rather than
// dropping it: a misspelt `widgets` is an error naming it, never an empty
// board. It does not validate; [Read] does, with the version's files.
//
// EVERY STRUCTURAL FAULT AT ONCE ([jsonform.Decode]): an unknown key names the
// keys its object takes, a wrong type the shape read there.
func Parse(raw []byte) (*Spec, error) { return parse(raw, jsonform.Decode) }

func parse(raw []byte, decode jsonform.Reader) (*Spec, error) {
	s := &Spec{}
	d := decode(raw, Form, jsonform.Subject{File: EntryPath, Format: "the dashboard format"}, s)
	if len(d.Faults) == 0 {
		return s, nil
	}
	out := &Invalid{Findings: make([]Finding, len(d.Faults))}
	for i, f := range d.Faults {
		out.Findings[i] = Finding{Path: EntryPath, Line: f.Line, Message: f.Message}
	}
	return nil, out
}

// Form is every key `dashboard.json` may carry and the type of each, derived
// from [Spec] ([jsonform.Of]): what [Parse] admits and [Reference] lists.
var Form = jsonform.Of[Spec]()

// Dataset is s's dataset named id.
func (s *Spec) Dataset(id string) (Dataset, bool) {
	i := slices.IndexFunc(s.Datasets, func(d Dataset) bool { return d.ID == id })
	if i < 0 {
		return Dataset{}, false
	}
	return s.Datasets[i], true
}

// Param is s's filter named key.
func (s *Spec) Param(key string) (Param, bool) {
	i := slices.IndexFunc(s.Params, func(p Param) bool { return p.Key == key })
	if i < 0 {
		return Param{}, false
	}
	return s.Params[i], true
}

// FilePaths are the version files s's file datasets read, in order, each
// once: what a validator hands [Validate].
func (s *Spec) FilePaths() []string {
	var out []string
	for _, d := range s.Datasets {
		if d.Source == SourceFile && d.File != nil && !slices.Contains(out, d.File.Path) {
			out = append(out, d.File.Path)
		}
	}
	return out
}

// EachWidget walks every widget, a group's children after the group.
func (s *Spec) EachWidget(yield func(w Widget, field string) bool) {
	for i, w := range s.Widgets {
		if !yield(w, fmt.Sprintf("widgets[%d]", i)) {
			return
		}
		for j, c := range w.Children {
			if !yield(c, fmt.Sprintf("widgets[%d].children[%d]", i, j)) {
				return
			}
		}
	}
}

// Widget is s's widget named id, a group's children included.
func (s *Spec) Widget(id string) (Widget, bool) {
	var found Widget
	ok := false
	s.EachWidget(func(w Widget, _ string) bool {
		if w.ID == id {
			found, ok = w, true
			return false
		}
		return true
	})
	return found, ok
}

// ResolveParams is the board's filters for one read: each value a viewer sent
// held to the filter's options, every filter they did not send at its
// default. A key the board has no filter for is refused, as is a value its
// filter does not offer: the set is closed, so nothing a viewer types reaches
// a domain read that the author did not write.
func (s *Spec) ResolveParams(sent map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(s.Params))
	for key := range sent {
		if _, ok := s.Param(key); !ok {
			return nil, fmt.Errorf("%w: the board has no filter %q", ErrInvalid, key)
		}
	}
	for _, p := range s.Params {
		v, ok := sent[p.Key]
		if !ok {
			out[p.Key] = p.Default
			continue
		}
		if !slices.ContainsFunc(p.Options, func(o Option) bool { return o.Value == v }) {
			return nil, fmt.Errorf("%w: the filter %q offers no choice %q", ErrInvalid, p.Key, v)
		}
		out[p.Key] = v
	}
	return out, nil
}

// EngineArgs are an engine dataset's arguments for one read: each literal as
// written, each bound one at its filter's resolved value, every parameter
// the source has and the spec left out at the source's default. Validation
// held every literal and every option of a bound filter to the parameter's
// type, so what this answers is always a value the source takes.
func EngineArgs(d Dataset, params map[string]string) (map[string]string, error) {
	if d.Source != SourceEngine || d.Engine == nil {
		return nil, fmt.Errorf("%w: %q is not an engine dataset", ErrInvalid, d.ID)
	}
	src, ok := Source(d.Engine.Name)
	if !ok {
		return nil, fmt.Errorf("%w: no engine source %q", ErrInvalid, d.Engine.Name)
	}
	out := map[string]string{}
	for _, p := range src.Params {
		a, set := d.Engine.Params[p.Key]
		switch {
		case !set:
			if p.Default != "" {
				out[p.Key] = p.Default
			}
			continue
		case a.Param != "":
			v, ok := params[a.Param]
			if !ok {
				return nil, fmt.Errorf("%w: %q is bound to the filter %q, which this read did not "+
					"resolve", ErrInvalid, p.Key, a.Param)
			}
			if v == "" {
				if p.Default != "" {
					out[p.Key] = p.Default
				}
				continue
			}
			if why := p.check(v); why != "" {
				return nil, fmt.Errorf("%w: %s.%s %s, and the filter %q gave %q", ErrInvalid,
					src.Name, p.Key, why, a.Param, v)
			}
			out[p.Key] = v
		default:
			out[p.Key] = a.Literal
		}
	}
	return out, nil
}

// SourceArgs are the arguments of a read that names a source directly rather
// than through a board's dataset (a builder's preview, before any board
// exists): each value sent held to its parameter's type, every parameter left
// out at the source's default. A key the source does not take is refused, so
// a preview can ask a domain nothing a board's dataset could not.
func SourceArgs(src EngineSource, sent map[string]string) (map[string]string, error) {
	for key := range sent {
		if _, ok := src.Param(key); !ok {
			return nil, fmt.Errorf("%w: %s takes no parameter %q", ErrInvalid, src.Name, key)
		}
	}
	out := map[string]string{}
	for _, p := range src.Params {
		v, ok := sent[p.Key]
		if !ok || v == "" {
			if p.Default != "" {
				out[p.Key] = p.Default
			}
			continue
		}
		if why := p.check(v); why != "" {
			return nil, fmt.Errorf("%w: %s.%s %s", ErrInvalid, src.Name, p.Key, why)
		}
		out[p.Key] = v
	}
	return out, nil
}
