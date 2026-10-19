package plainjson_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// MARKUP IS WRITTEN AS THE CHARACTERS IT IS, and what comes out is still one
// JSON document that decodes back to the value it was handed — the escapes
// are an HTML embedding's concern, and dropping them must not cost the text
// its being JSON. Mutation: delete SetEscapeHTML(false) and every value comes
// back as `\u003c` and its kin.
func TestMarkupIsWrittenAsTheCharactersItIs(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"code": "if a < b && c > d",
		"url":  "https://example.com/?a=1&b=2",
		"body": "<h1>Title</h1>",
	}
	for _, indent := range []string{"", "  "} {
		got, err := plainjson.Marshal(value, indent)
		if err != nil {
			t.Fatalf("Marshal(indent %q): %v", indent, err)
		}
		for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
			if strings.Contains(string(got), escape) {
				t.Errorf("Marshal(indent %q) = %s, want the characters rather than %s",
					indent, got, escape)
			}
		}
		for _, verbatim := range []string{"if a < b && c > d", "?a=1&b=2", "<h1>Title</h1>"} {
			if !strings.Contains(string(got), verbatim) {
				t.Errorf("Marshal(indent %q) = %s, want %q verbatim", indent, got, verbatim)
			}
		}
		var back map[string]any
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("Marshal(indent %q) wrote %s, which is not JSON: %v", indent, got, err)
		}
		for key, want := range value {
			if back[key] != want {
				t.Errorf("Marshal(indent %q): %s decodes back as %q, want %q",
					indent, key, back[key], want)
			}
		}
	}
}

// THE TEXT ENDS WHERE ITS VALUE DOES, under any indent: the encoder's own
// trailing newline is a byte a record would store, an answer would be charged
// for against its ceiling, and a prompt would frame inside its fence.
// Mutation: drop the TrimSuffix and both cases end in a newline.
func TestTheTextEndsWhereItsValueDoes(t *testing.T) {
	t.Parallel()
	for _, indent := range []string{"", "  "} {
		got, err := plainjson.Marshal(map[string]any{"n": 1}, indent)
		if err != nil {
			t.Fatalf("Marshal(indent %q): %v", indent, err)
		}
		if strings.HasSuffix(string(got), "\n") || !strings.HasSuffix(string(got), "}") {
			t.Errorf("Marshal(indent %q) = %q, want it to end at the closing brace", indent, got)
		}
	}
}

// THE INDENT IS THE CALLER'S, and apart from the escapes the bytes are the
// standard library's for it: under an indent exactly `json.MarshalIndent`'s,
// under none exactly `json.Marshal`'s, key order included — which is what
// lets a reader be moved onto this package and change in nothing but the
// characters it exists for. Mutation: hard-code the indent and one of the two
// comparisons fails.
func TestTheIndentIsTheCallers(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"z":     []any{1, "two", map[string]any{"b": true, "a": nil}},
		"a":     "plain",
		"empty": map[string]any{},
	}
	indented, err := plainjson.Marshal(value, "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if string(indented) != string(want) {
		t.Errorf("indented = %q, want MarshalIndent's %q", indented, want)
	}

	flat, err := plainjson.Marshal(value, "")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want, _ := json.Marshal(value); string(flat) != string(want) {
		t.Errorf("one line = %q, want Marshal's %q", flat, want)
	}

	// The same document with markup in it differs from MarshalIndent's in
	// the escapes and nowhere else.
	markup := map[string]any{"body": "a < b && c > d", "n": 1}
	got, err := plainjson.Marshal(markup, "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "{\n  \"body\": \"a < b && c > d\",\n  \"n\": 1\n}"; string(got) != want {
		t.Errorf("indented markup = %q, want %q", got, want)
	}
}

// A VALUE JSON CANNOT SPELL IS AN ERROR, never a document: each caller has a
// fallback of its own for one (a ledger line's Go rendering, a prompt's list
// of call names, a tool's refusal), and an encoder that wrote something
// instead would make every one of them unreachable.
func TestAValueJSONCannotSpellIsAnError(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]any{
		"nan":     math.NaN(),
		"channel": map[string]any{"c": make(chan int)},
	} {
		if got, err := plainjson.Marshal(value, ""); err == nil {
			t.Errorf("Marshal(%s) = %q, want an error", name, got)
		}
	}
}
