package jsonform_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonform"
)

type leaf struct {
	Path   string `json:"path"`
	Weight int    `json:"weight,omitempty"`
}

// opaque reads its own JSON by meaning, as dashspec's Arg and Operand do: a
// value with its own reader is handed the author's value, compacted.
type opaque struct{ v any }

func (o *opaque) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"bad"` {
		return errors.New("opaque refuses bad")
	}
	return json.Unmarshal(raw, &o.v)
}

type storedBase struct {
	V int `json:"v"`
}

type storedIndex struct {
	storedBase
	Title  string            `json:"title"`
	Ratio  float64           `json:"ratio,omitempty"`
	On     bool              `json:"on,omitempty"`
	Leaf   leaf              `json:"leaf"`
	Ptr    *leaf             `json:"ptr,omitempty"`
	Count  *int              `json:"count,omitempty"`
	Order  []string          `json:"order"`
	Leaves []leaf            `json:"leaves,omitempty"`
	Boards map[string]leaf   `json:"boards,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Own    opaque            `json:"own,omitzero"`
}

// oldRead is how the deck, doc, canvas and dashboard indexes were read before
// ADR-0059: encoding/json with DisallowUnknownFields, one value, nothing after.
func oldRead(raw []byte) (storedIndex, bool) {
	var out storedIndex
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return storedIndex{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return storedIndex{}, false
	}
	return out, true
}

var storedSeeds = []string{
	`{"v": 1, "title": "t", "order": ["a"]}`,
	`{"V": 1, "Title": "t", "ORDER": ["a"]}`,
	`{"title": "a", "title": "b"}`,
	`{"title": "a", "Title": "b", "TITLE": null}`,
	`{"leaf": {"path": "a"}, "leaf": {"weight": 4}}`,
	`{"leaf": {"path": "a", "weight": 2}, "Leaf": null}`,
	`{"ptr": {"path": "a"}, "ptr": {"weight": 4}}`,
	`{"ptr": {"path": "a"}, "ptr": null}`,
	`{"count": 3, "count": null}`,
	`{"order": ["a", "b"], "order": ["c"]}`,
	`{"order": ["a"], "order": null}`,
	`{"boards": {"x": {"path": "a"}}, "boards": {"y": {"path": "b"}}}`,
	`{"boards": {"x": {"path": "a"}, "x": {"weight": 3}}}`,
	`{"boards": {"x": {"PATH": "a"}}}`,
	`{"boards": {"x": null}}`,
	`{"labels": {"a": "1"}, "labels": null}`,
	`{"leaves": [{"Path": "a", "path": "b"}]}`,
	`{"own": "x", "own": "y"}`,
	`{"own": "&<>", "own": {"a": [1, 2.50]}}`,
	`{"own": "bad"}`,
	`{"tıtle": "dotless"}`,
	`{"ſ": 1}`,
	`{"title": 7}`,
	`{"v": 1.5}`,
	`{"nope": 1}`,
	`{"skipped": 1}`,
	`null`,
	`{} {}`,
	`{"v": 1,`,
}

// THE STORED READ ADMITS EXACTLY WHAT encoding/json ADMITTED, and decodes it to
// the same value: a key in another case, a key written twice (into a struct, a
// pointer, a list, a map and its elements, with and without null), and still
// refuses an unknown key and a wrong type. This is what keeps a version stored
// before ADR-0059 readable on every read path.
//
// Mutation: drop the fold, or merge a repeated struct field by replacing it,
// and a seed below decodes to another value than encoding/json's.
func FuzzTheStoredReadIsEncodingJSONs(f *testing.F) {
	for _, s := range storedSeeds {
		f.Add([]byte(s))
	}
	form := jsonform.Of[storedIndex]()
	f.Fuzz(func(t *testing.T, raw []byte) {
		want, accepted := oldRead(raw)
		var got storedIndex
		d := jsonform.DecodeStored(raw, form, jsonform.Subject{File: "index.json", Format: "the test format"}, &got)
		read := len(d.Faults) == 0
		if read != accepted {
			t.Fatalf("%s: encoding/json accepted %v, the stored read %v (%v)", raw, accepted, read, d.Faults)
		}
		if accepted && !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the stored read decoded\n%+v\nencoding/json decoded\n%+v", raw, got, want)
		}
	})
}

// THE PUBLISH READ IS STRICTER, ON PURPOSE: a key in another case and a key
// written twice are refused when a version is written, never when one is read.
func TestThePublishReadRefusesWhatTheStoredReadAdmits(t *testing.T) {
	t.Parallel()
	form := jsonform.Of[storedIndex]()
	for _, raw := range []string{`{"Title": "t"}`, `{"title": "a", "title": "b"}`} {
		var strict, stored storedIndex
		subject := jsonform.Subject{File: "index.json", Format: "the test format"}
		if d := jsonform.Decode([]byte(raw), form, subject, &strict); len(d.Faults) == 0 {
			t.Errorf("%s: the publish read admitted it", raw)
		}
		if d := jsonform.DecodeStored([]byte(raw), form, subject, &stored); len(d.Faults) != 0 {
			t.Errorf("%s: the stored read refused it: %v", raw, d.Faults)
		}
	}
}
