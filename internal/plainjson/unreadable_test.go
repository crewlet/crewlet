package plainjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// A DOCUMENT THAT DID NOT DECODE IS TOLD IN WORDS, NEVER IN GO'S.
//
// Every surface that decodes a caller's JSON answered the decoder's error as
// its detail, which names Go's types and struct fields and the time package's
// layout. Each row decodes a real document through a strict decoder into a
// real struct, as the surfaces do, and holds the sentence to what it should
// say and to none of the decoder's words. Mutations: answer err.Error() and
// every row goes red; drop the field from a type's sentence and the field
// rows do; read a too-large whole number as a wrong type and its row does.
func TestADocumentThatDidNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	type share struct {
		Role string `json:"role"`
	}
	type body struct {
		Grants []string  `json:"grants"`
		Limit  int       `json:"limit"`
		Ratio  float64   `json:"ratio"`
		At     time.Time `json:"at"`
		Shares []share   `json:"shares"`
		Draft  bool      `json:"draft"`
		Count  uint      `json:"count"`
	}
	for _, tc := range []struct {
		doc, want string
	}{
		{``, "the body is empty"},
		{`{"grants":`, "the body isn't valid JSON: it ends too soon"},
		{`{grants}`, "the body isn't valid JSON: it goes wrong at byte 2"},
		{`[1]`, "the body is a list where an object belongs"},
		{`{"grants":"admin"}`, `in the body, "grants" holds text where a list belongs`},
		{`{"limit":1.5}`, `in the body, "limit" holds 1.5 where a whole number belongs`},
		{`{"limit":"ten"}`, `in the body, "limit" holds text where a whole number belongs`},
		{`{"limit":99999999999999999999}`,
			`in the body, "limit" is 99999999999999999999, which is too large`},
		{`{"limit":-99999999999999999999}`,
			`in the body, "limit" is -99999999999999999999, which is too small`},
		{`{"count":-1}`, `in the body, "count" is -1, which is too small`},
		{`{"ratio":true}`, `in the body, "ratio" holds true or false where a number belongs`},
		{`{"draft":{}}`, `in the body, "draft" holds an object where true or false belongs`},
		{`{"shares":[{"role":5}]}`, `in the body, "shares.0.role" holds a number where text belongs`},
		{`{"at":"yesterday"}`, `in the body, the time "yesterday" isn't an RFC 3339 time ` +
			`like 2026-04-16T09:00:00Z`},
		{`{"owner":"jane"}`, `in the body, "owner" isn't a field this takes`},
		{`{"at":5}`, `in the body, "at" holds a number where an RFC 3339 time like ` +
			`2026-04-16T09:00:00Z belongs`},
	} {
		dec := json.NewDecoder(bytes.NewReader([]byte(tc.doc)))
		dec.DisallowUnknownFields()
		var into body
		err := dec.Decode(&into)
		if err == nil {
			t.Fatalf("%s decoded", tc.doc)
		}
		got := plainjson.Unreadable("the body", err)
		if got != tc.want {
			t.Errorf("%s: told %q, want %q", tc.doc, got, tc.want)
		}
		for _, goWords := range []string{"json:", "Go ", "unmarshal", "2006-01-02",
			"[]string", "UnmarshalJSON", "invalid character"} {
			if strings.Contains(got, goWords) {
				t.Errorf("%s: told %q, which carries %q", tc.doc, got, goWords)
			}
		}
	}
	// AND THROUGH json.Unmarshal, which says a decoder's two ends as one
	// SyntaxError: an empty document is empty and a cut one ends too soon,
	// exactly as a decoder's are told, never "goes wrong at byte 0".
	for doc, want := range map[string]string{
		``:          "the body is empty",
		`{`:         "the body isn't valid JSON: it ends too soon",
		`{"limit":`: "the body isn't valid JSON: it ends too soon",
		`{x}`:       "the body isn't valid JSON: it goes wrong at byte 2",
	} {
		var into body
		if got := plainjson.Unreadable("the body", json.Unmarshal([]byte(doc), &into)); got != want {
			t.Errorf("Unmarshal %q: told %q, want %q", doc, got, want)
		}
	}
	if got := plainjson.Unreadable("the q parameter", errors.New("anything")); got !=
		"the q parameter holds a value this doesn't take" {
		t.Errorf("an error nobody recognises is told %q", got)
	}
}
