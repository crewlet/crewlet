package httpjson_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// ONLY A BODY DECLARED JSON IS READ AS ONE, so a cross-site form's text/plain
// post never reaches a write. Mutation: accept any media type and the form
// rows pass.
func TestOnlyABodyDeclaredJSONIsRead(t *testing.T) {
	t.Parallel()
	for header, want := range map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"application/json; charset=UTF-8":   true,
		"application/json; charset=latin1":  false,
		"text/plain":                        false,
		"application/x-www-form-urlencoded": false,
		"multipart/form-data; boundary=x":   false,
		"":                                  false,
		"application/json;;":                false,
	} {
		if got := httpjson.DeclaresJSON(header); got != want {
			t.Errorf("DeclaresJSON(%q) = %v, want %v", header, got, want)
		}
	}
}

// A STRICT BODY IS ONE VALUE OF THE ROUTE'S OWN FIELDS: an unknown key and a
// second value after the first are refused, never ignored. Mutation: drop
// DisallowUnknownFields or the trailing-token check and its row passes.
func TestAStrictBodyIsOneValueOfTheRoutesOwnFields(t *testing.T) {
	t.Parallel()
	type body struct {
		Args map[string]any `json:"args"`
	}
	var b body
	if err := httpjson.DecodeStrict([]byte(`{"args":{"a":1}}`), &b); err != nil || b.Args["a"] != 1.0 {
		t.Fatalf("one object of the route's fields: %v %v", b, err)
	}
	if err := httpjson.DecodeStrict([]byte(`{"args":{},"op_id":"x"}`), &body{}); err == nil ||
		errors.Is(err, httpjson.ErrTrailingData) {
		t.Errorf("an unknown key answered %v, want the decoder's refusal", err)
	}
	if err := httpjson.DecodeStrict([]byte(`{"args":{}} {}`), &body{}); !errors.Is(err, httpjson.ErrTrailingData) {
		t.Errorf("a second object answered %v, want ErrTrailingData", err)
	}
	if err := httpjson.DecodeStrict([]byte(`not json`), &body{}); err == nil {
		t.Error("a body that is not JSON was read")
	}
}
