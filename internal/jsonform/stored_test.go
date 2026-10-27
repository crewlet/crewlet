package jsonform_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
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
// ADR-0059: encoding/json with DisallowUnknownFields and the format's own
// trailing rule (the decoder's next token io.EOF for the deck, doc and
// dashboard; its More false for the canvas).
func oldRead(raw []byte, trailing jsonform.Trailing) (storedIndex, bool) {
	var out storedIndex
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return storedIndex{}, false
	}
	if trailing == jsonform.NoMoreValues {
		return out, !dec.More()
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
	`{"order": ["a", "b"], "order": [null]}`,
	`{"order": ["a"], "order": null}`,
	`{"leaves": [{"path": "a"}], "leaves": [{"weight": 4}]}`,
	`{"leaves": [{"path": "a"}, {"path": "b"}], "leaves": [null]}`,
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
	`{"v": 1} }`,
	`{"v": 1}]]]`,
	`{"v": 1} x`,
	`{"v": 1,`,
}

// THE STORED READ ACCEPTS EXACTLY WHAT THE FORMAT'S OLD READ ACCEPTED, TO THE
// SAME VALUE, under each trailing rule: acceptance is that call, so this holds
// by construction, and what the fuzz proves is the other half, that a refusal
// always names at least one fault.
//
// Mutation: accept with the walk instead of the old call, and a repeated list
// of objects, a null list element or a canvas's stray `}` decodes differently
// or is refused.
func FuzzTheStoredReadIsEncodingJSONs(f *testing.F) {
	for _, s := range storedSeeds {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	form := jsonform.Of[storedIndex]()
	f.Fuzz(func(t *testing.T, raw []byte, canvas bool) {
		trailing := jsonform.UntilEOF
		if canvas {
			trailing = jsonform.NoMoreValues
		}
		want, accepted := oldRead(raw, trailing)
		var got storedIndex
		d := jsonform.Stored(trailing)(raw, form, jsonform.Subject{File: "index.json", Format: "the test format"}, &got)
		read := len(d.Faults) == 0
		if read != accepted {
			t.Fatalf("%s: encoding/json accepted %v, the stored read %v (%v)", raw, accepted, read, d.Faults)
		}
		if read == d.Fatal {
			t.Fatalf("%s: Fatal is %v with faults %v", raw, d.Fatal, d.Faults)
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
		if d := jsonform.Stored(jsonform.UntilEOF)([]byte(raw), form, subject, &stored); len(d.Faults) != 0 {
			t.Errorf("%s: the stored read refused it: %v", raw, d.Faults)
		}
	}
}

// A REFUSED STORED VERSION IS TOLD ONLY WHAT ITS READ REFUSES: a key in
// another case and a key written twice are faults to a publish and fine to a
// stored read, so the refusal of a stored version that has both and an
// unknown key names the unknown key alone.
//
// Mutation: answer a stored refusal with the walk's faults unfiltered and the
// case and repeat faults are listed too.
func TestAStoredRefusalNamesOnlyWhatTheStoredReadRefuses(t *testing.T) {
	t.Parallel()
	form := jsonform.Of[storedIndex]()
	var got storedIndex
	d := jsonform.Stored(jsonform.UntilEOF)([]byte(`{"Title": "t", "order": ["a"], "order": ["b"], "nope": 1}`),
		form, jsonform.Subject{File: "index.json", Format: "the test format"}, &got)
	if !d.Fatal || len(d.Faults) != 1 || !strings.Contains(d.Faults[0].Message, "nope is not a key") {
		t.Errorf("faults %v, want the unknown key alone", d.Faults)
	}
	// AND A REFUSAL NO LISTED FAULT EXPLAINS still says why, in the old
	// read's words: a wrong type under a key in another case.
	d = jsonform.Stored(jsonform.UntilEOF)([]byte(`{"Title": 7}`), form,
		jsonform.Subject{File: "index.json", Format: "the test format"}, &got)
	if !d.Fatal || len(d.Faults) != 1 || !strings.Contains(d.Faults[0].Message, "index.json cannot be read") {
		t.Errorf("faults %v, want the old read's reason", d.Faults)
	}
}
