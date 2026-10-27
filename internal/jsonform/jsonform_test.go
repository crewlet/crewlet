package jsonform_test

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonform"
)

type face struct {
	Kit   string `json:"kit,omitempty"`
	Files []file `json:"files,omitempty"`
}

type file struct {
	Path   string `json:"path"`
	Weight int    `json:"weight"`
}

type base struct {
	V int `json:"v"`
}

type own struct{ s string }

func (o *own) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"bad"` {
		return errors.New("own refuses bad")
	}
	o.s = string(raw)
	return nil
}

type text string

func (t *text) UnmarshalText(b []byte) error { *t = text(b); return nil }

type index struct {
	base
	Name    string          `json:"name"`
	Ratio   float64         `json:"ratio,omitempty"`
	On      bool            `json:"on,omitempty"`
	Face    face            `json:"face"`
	Lines   []string        `json:"lines,omitempty"`
	Boards  map[string]file `json:"boards,omitempty"`
	Own     own             `json:"own,omitzero"`
	Text    text            `json:"text,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
	Skipped string          `json:"-"`
	hidden  string
}

// hidden is read so the unexported field is part of the type the form is
// derived from, and left out of it.
var _ = index{}.hidden

var subject = jsonform.Subject{File: "index.json", Format: "the test format"}

// THE FORM IS THE TYPE'S, by encoding/json's own rules: keys from tags, an
// embedded struct inlined, `-` and unexported fields left out, a type with its
// own reader opaque, a text reader a string.
func TestAFormIsDerivedFromItsType(t *testing.T) {
	t.Parallel()
	f := jsonform.Of[index]()
	if got, want := f.Keys(), []string{"v", "name", "ratio", "on", "face", "lines", "boards", "own",
		"text", "raw"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	for key, want := range map[string]jsonform.Type{"v": jsonform.Integer, "name": jsonform.String,
		"ratio": jsonform.Number, "on": jsonform.Boolean, "face": jsonform.Object,
		"lines": jsonform.Array, "boards": jsonform.Map, "own": jsonform.Any,
		"text": jsonform.String, "raw": jsonform.Any} {
		if got := f.Field(key).Type; got != want {
			t.Errorf("%s is %v, want %v", key, got, want)
		}
	}
	if got := f.Field("face").Describe(); got != "an object {kit, files}" {
		t.Errorf("face describes as %q", got)
	}
	if got := f.Field("face").Field("files").Describe(); got != "a list of objects {path, weight}" {
		t.Errorf("files describe as %q", got)
	}
	var paths []string
	for _, p := range f.Paths() {
		paths = append(paths, p.Path)
	}
	for _, want := range []string{"face.files[].weight", "boards{}.path", "lines"} {
		if !strings.Contains(strings.Join(paths, " "), want) {
			t.Errorf("paths %v carry no %s", paths, want)
		}
	}
}

// EVERY STRUCTURAL FAULT IN ONE PASS, each naming the field, its line and the
// shape the format reads there, and every well-formed field still decoded.
//
// Mutation: return at the first fault and only one comes back.
func TestEveryFaultComesBackInOnePass(t *testing.T) {
	t.Parallel()
	raw := `{
  "v": 1,
  "name": 7,
  "extra": {"deep": [1, 2]},
  "face": {"kit": "geist", "files": [{"path": "a", "weight": 1.5}, {"path": "b", "weight": 400}], "zzz": true},
  "lines": ["one", 2, "three"],
  "boards": {"hero.html": {"path": "x", "weight": "heavy"}},
  "on": "yes",
  "name": "again"
}`
	var got index
	d := jsonform.Decode([]byte(raw), jsonform.Of[index](), subject, &got)
	if d.Fatal {
		t.Fatalf("a fault in a field was fatal: %v", d.Faults)
	}
	want := []jsonform.Fault{
		{Field: "name", Line: 3, Message: "name is a number, and the format reads a string there"},
		{Field: "extra", Line: 4, Message: "extra is not a key the test format carries: the top level of index.json takes v, name, ratio, on, face, lines, boards, own, text, raw"},
		{Field: "face.files[0].weight", Line: 5, Message: "face.files[0].weight is the number 1.5, and the format reads a whole number there"},
		{Field: "face.zzz", Line: 5, Message: "face.zzz is not a key the test format carries: face takes kit, files"},
		{Field: "lines[1]", Line: 6, Message: "lines[1] is a number, and the format reads a string there"},
		{Field: `boards["hero.html"].weight`, Line: 7, Message: `boards["hero.html"].weight is a string, and the format reads a whole number there`},
		{Field: "on", Line: 8, Message: "on is a string, and the format reads true or false there"},
		{Field: "name", Line: 9, Message: "name is written twice: write each key once"},
	}
	if !reflect.DeepEqual(d.Faults, want) {
		t.Errorf("faults\n%+v\nwant\n%+v", d.Faults, want)
	}
	// THE REST IS DECODED, and a list keeps its places.
	if got.V != 1 || got.Face.Kit != "geist" || len(got.Face.Files) != 2 || got.Face.Files[1].Weight != 400 {
		t.Errorf("the well-formed fields were not decoded: %+v", got)
	}
	if !reflect.DeepEqual(got.Lines, []string{"one", "", "three"}) {
		t.Errorf("lines %q, want their places kept", got.Lines)
	}
	for field, dropped := range map[string]bool{"name": true, "lines[1]": true, "lines[2]": false,
		"face.files[0].weight": true, "face.files[0]": false, `boards["hero.html"].weight`: true,
		"face.kit": false, "v": false} {
		if d.Dropped(field) != dropped {
			t.Errorf("Dropped(%s) is %v, want %v", field, !dropped, dropped)
		}
	}
	if !d.Touched("face") || !d.Touched("boards") || d.Touched("v") {
		t.Error("Touched does not report the faults inside a field")
	}
	if d.Line("face.kit") != 5 {
		t.Errorf("face.kit is on line %d, want 5", d.Line("face.kit"))
	}
}

// A FIELD INSIDE A DROPPED VALUE IS DROPPED: an object where a string goes
// takes every key under it with it.
func TestAValueOfTheWrongShapeDropsWhatIsInsideIt(t *testing.T) {
	t.Parallel()
	var got index
	d := jsonform.Decode([]byte(`{"v": 1, "face": "geist", "lines": {"a": 1}}`), jsonform.Of[index](), subject, &got)
	if len(d.Faults) != 2 || d.Fatal {
		t.Fatalf("faults %v", d.Faults)
	}
	if !strings.Contains(d.Faults[0].Message, "face is a string, and the format reads an object {kit, files} there") {
		t.Errorf("fault %q", d.Faults[0].Message)
	}
	if !strings.Contains(d.Faults[1].Message, "lines is an object, and the format reads a list of strings there") {
		t.Errorf("fault %q", d.Faults[1].Message)
	}
	if !d.Dropped("face.kit") || !d.Dropped("face.files[0].path") {
		t.Error("a key inside a dropped value is not dropped")
	}
}

// WHAT CANNOT BE READ AT ALL IS FATAL: bytes that are not JSON, more after
// the value, and a root of the wrong shape.
func TestWhatCannotBeReadIsFatal(t *testing.T) {
	t.Parallel()
	for raw, says := range map[string]string{
		`{"v": 1,`:                 "index.json is not JSON",
		`{"v": 1} {}`:              "index.json holds more after its value",
		`[1]`:                      "index.json is a list, and the format reads an object {v, name",
		`{"v": 1, "name": "a"} x`:  "index.json holds more after its value",
		`"text"`:                   "index.json is a string",
		`{"v": 1, "own": "fine"} `: "",
	} {
		var got index
		d := jsonform.Decode([]byte(raw), jsonform.Of[index](), subject, &got)
		switch {
		case says == "" && (d.Fatal || len(d.Faults) > 0):
			t.Errorf("%s: faults %v, want none", raw, d.Faults)
		case says != "" && (!d.Fatal || len(d.Faults) == 0 || !strings.Contains(d.Faults[0].Message, says)):
			t.Errorf("%s: fatal %v, faults %v, want a fatal fault saying %q", raw, d.Fatal, d.Faults, says)
		}
	}
}

// NULL IS ABSENT, as encoding/json reads it, and a value of an opaque form is
// handed to its own reader.
func TestNullIsAbsent(t *testing.T) {
	t.Parallel()
	var got index
	d := jsonform.Decode([]byte(`{"v": 1, "name": null, "face": null, "lines": [null], "own": "ok", "raw": {"any": [1]}}`),
		jsonform.Of[index](), subject, &got)
	if len(d.Faults) > 0 {
		t.Fatalf("faults %v", d.Faults)
	}
	if got.Own.s != `"ok"` || string(got.Raw) != `{"any":[1]}` {
		t.Errorf("own %q, raw %s", got.Own.s, got.Raw)
	}
}

// THE OUTLINE AND ITS RULES MOVE TOGETHER: a key with no rule, and a rule for
// no key, are both reported.
func TestUnruledNamesBothDirections(t *testing.T) {
	t.Parallel()
	f := jsonform.Of[face]()
	missing, unknown := f.Unruled(map[string]string{"kit": "a kit face", "files": "its files",
		"files[].path": "a path", "gone": "a key removed"})
	if !reflect.DeepEqual(missing, []string{"files[].weight"}) || !reflect.DeepEqual(unknown, []string{"gone"}) {
		t.Errorf("missing %v, unknown %v", missing, unknown)
	}
	out := f.Outline(map[string]string{"kit": "a kit face"})
	for _, want := range []string{"- `kit` (string): a kit face", "- `files` (list of objects)", "  - `files[].weight` (whole number)"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline\n%s\ncarries no %q", out, want)
		}
	}
}

// A KEY IS MATCHED EXACTLY: encoding/json reads `"Name"` into `name`, a reader
// that matches case another way reads something else from the same bytes, so
// a key in another case is refused as the unknown key it is.
//
// Mutation: match keys with strings.EqualFold and `Name` is decoded.
func TestAKeyIsMatchedExactly(t *testing.T) {
	t.Parallel()
	var got index
	d := jsonform.Decode([]byte(`{"v": 1, "Name": "x"}`), jsonform.Of[index](), subject, &got)
	if len(d.Faults) != 1 || !strings.Contains(d.Faults[0].Message, "Name is not a key") || got.Name != "" {
		t.Errorf("a key in another case: faults %v, name %q", d.Faults, got.Name)
	}
}

// A VALUE ITS OWN READER REFUSES IS ONE FAULT AMONG THE OTHERS, at its path and
// line, never a fatal error with no place: a dashboard's engine argument and a
// calc operand read themselves, and their refusals come back beside the rest.
//
// Mutation: leave the own reader to the typed decode and the fault is fatal,
// at line 0, and the other fault is lost.
func TestAnOwnReadersRefusalIsAFaultAtItsPath(t *testing.T) {
	t.Parallel()
	var got index
	d := jsonform.Decode([]byte("{\"v\": 1,\n \"own\": \"bad\",\n \"name\": 7}"), jsonform.Of[index](), subject, &got)
	if d.Fatal {
		t.Fatalf("an own reader's refusal was fatal: %v", d.Faults)
	}
	want := []jsonform.Fault{
		{Field: "own", Line: 2, Message: "own: own refuses bad"},
		{Field: "name", Line: 3, Message: "name is a number, and the format reads a string there"},
	}
	if !reflect.DeepEqual(d.Faults, want) {
		t.Errorf("faults\n%+v\nwant\n%+v", d.Faults, want)
	}
	if got.V != 1 {
		t.Error("the well-formed fields were not decoded")
	}
}

type deep struct {
	Name string `json:"name"`
	Deep string
}

type shallow struct {
	Name string
}

type tagged struct {
	Name string `json:"name"`
}

type untaggedTie struct {
	Tie string
}

type otherTie struct {
	Tie string
}

type withDeep struct{ deep }

type promoted struct {
	shallow
	withDeep
	tagged
	untaggedTie
	otherTie
	Raw     []byte `json:"raw"`
	Kept    string `json:"kept,omitempty"`
	Skipped string `json:"-"`
}

// THE KEYS ARE encoding/json's, by its own rules: the shallower of two keys of
// one name, a tagged key winning a tie, a tie neither breaks dropping both,
// `-,` naming the key `-`, and []byte read as base64 text rather than a list of
// numbers. A tag name encoding/json does not take is refused, since its
// versions read one differently.
//
// Mutation: promote every embedded key regardless of depth and `name` is
// carried twice; model []byte as a list and `raw` is a list of numbers.
func TestTheKeysAreEncodingJSONs(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(promoted{Kept: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	f := jsonform.Of[promoted]()
	if got, want := slices.Sorted(slices.Values(f.Keys())), slices.Sorted(maps.Keys(keys)); !slices.Equal(got, want) {
		t.Errorf("the form carries %v, and encoding/json writes %v", got, want)
	}
	if f.Field("raw").Type != jsonform.String {
		t.Error("[]byte is not base64 text")
	}
}

type invalidTag struct {
	Bad string `json:"a§b"`
}

func TestATagNameEncodingJSONReadsTwoWaysIsRefused(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("a form was derived from a tag name encoding/json reads two ways")
		}
	}()
	jsonform.Of[invalidTag]()
}

type base2 struct {
	X int `json:"x"`
}

type viaA struct{ base2 }

type viaB struct{ base2 }

type twoPaths struct {
	viaA
	viaB
	Y int `json:"y"`
}

// A TYPE EMBEDDED THROUGH TWO PATHS AT ONE DEPTH CARRIES NONE OF ITS KEYS, as
// encoding/json counts it: json.Marshal writes {"y":0} for this shape, so a
// form that admitted `x` would let a publish write a key the typed decode then
// drops in silence.
//
// Mutation: visit each embedded type once without counting its paths, and the
// form carries x.
func TestATypeEmbeddedTwiceAtOneDepthCarriesNoKeys(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(twoPaths{})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	f := jsonform.Of[twoPaths]()
	if got, want := f.Keys(), slices.Sorted(maps.Keys(keys)); !slices.Equal(got, want) {
		t.Fatalf("the form carries %v, and encoding/json writes %v", got, want)
	}
	var got twoPaths
	d := jsonform.Decode([]byte(`{"x": 1, "y": 2}`), f, subject, &got)
	if len(d.Faults) != 1 || !strings.Contains(d.Faults[0].Message, "x is not a key") {
		t.Errorf("x was not refused: %v", d.Faults)
	}
}

type wider struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type narrower struct {
	Y int `json:"y"`
}

// A KEY THE STRUCT DOES NOT CARRY IS A FAULT, NEVER A SILENT DROP, even where
// the form and the struct disagree: the typed decode refuses unknown keys as
// a backstop to the walk.
//
// Mutation: decode with json.Unmarshal and x is dropped with no fault.
func TestAFormThatDisagreesWithItsStructIsAFault(t *testing.T) {
	t.Parallel()
	var got narrower
	d := jsonform.Decode([]byte(`{"x": 1, "y": 2}`), jsonform.Of[wider](), subject, &got)
	if !d.Fatal || len(d.Faults) == 0 || !strings.Contains(d.Faults[0].Message, `"x"`) {
		t.Errorf("a key the struct does not carry was dropped: fatal %v, faults %v", d.Fatal, d.Faults)
	}
}
