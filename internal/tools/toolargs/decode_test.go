package toolargs_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// A CALL'S NUMBERS ARRIVE EXACTLY, whichever transport carried it. Every
// reader of a tool's arguments reads a json.Number — a seat's arrive through
// its tool session, which decodes them here — and a transport that decoded to
// float64 first rounded an integer past 2^53 before any tool saw it: a
// 19-digit vendor id named another entity, and nothing reported an error.
func TestACallsArgumentsKeepAWideNumberExact(t *testing.T) {
	t.Parallel()
	const past = "9007199254740993" // 2^53 + 1, the first integer a float64 rounds
	args, err := toolargs.Decode([]byte(`{"issue_id": ` + past + `}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, ok := args["issue_id"].(json.Number); !ok || got.String() != past {
		t.Errorf("issue_id read as %#v, want json.Number(%s)", args["issue_id"], past)
	}
}

// NOTHING AT ALL IS A CALL WITH NO ARGUMENTS — a tool that takes none is
// sent no body, an empty one, or a literal null — and the map is never nil,
// since a caller writes the path's names into it.
func TestACallWithNoArgumentsIsAnEmptyMap(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "  \n", "null", " null "} {
		args, err := toolargs.Decode([]byte(raw))
		if err != nil || args == nil || len(args) != 0 {
			t.Errorf("Decode(%q) = %#v, %v; want an empty map", raw, args, err)
		}
	}
}

// EXACTLY ONE JSON OBJECT, or the call is refused: a second value after it is
// a call nobody can say which half of to run, and a decoder that stops at the
// first value would run the first in silence.
//
// AND THE REFUSAL IS IN WORDS ([plainjson.Unreadable]), which a model fixes
// its call from and a person reads on the act route: it was "the arguments
// are not a JSON object: invalid character 'x' looking for beginning of
// value" and "json: cannot unmarshal array into Go value of type
// map[string]interface {}". Mutation: wrap the decoder's error again and the
// word rows go red.
func TestACallsArgumentsAreExactlyOneObject(t *testing.T) {
	t.Parallel()
	for raw, says := range map[string]string{
		`{"a": 1} {"a": 2}`: "the arguments object has more after its JSON value, which is all it may hold",
		`{"a": 1}null`:      "the arguments object has more after its JSON value, which is all it may hold",
		`{"a": 1} x`:        "the arguments object has more after its JSON value, which is all it may hold",
		// THE STRAY BRACE AND COMMA a model leaves, which are no second
		// value: told in words true of anything that follows.
		`{"a": 1}}`: "the arguments object has more after its JSON value, which is all it may hold",
		`{"a": 1},`: "the arguments object has more after its JSON value, which is all it may hold",
		`[1, 2]`:    "the arguments object is a list where an object belongs",
		`"text"`:    "the arguments object is text where an object belongs",
		`7`:         "the arguments object is a number where an object belongs",
		`{"a": 1`:   "the arguments object isn't valid JSON: it ends too soon",
	} {
		args, err := toolargs.Decode([]byte(raw))
		if err == nil {
			t.Errorf("Decode(%q) = %#v, want it refused", raw, args)
			continue
		}
		if !strings.HasPrefix(err.Error(), says) {
			t.Errorf("Decode(%q) is told %q, want %q", raw, err, says)
		}
		for _, goWords := range []string{"json:", "invalid character", "Go value",
			"interface {}"} {
			if strings.Contains(err.Error(), goWords) {
				t.Errorf("Decode(%q) is told %q, which carries %q", raw,
					err, goWords)
			}
		}
	}
	if args, err := toolargs.Decode([]byte("{\"a\": 1}\n")); err != nil || len(args) != 1 {
		t.Errorf("a document ending in a newline was refused: %#v, %v", args, err)
	}
}
