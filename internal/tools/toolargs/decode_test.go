package toolargs_test

import (
	"encoding/json"
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
func TestACallsArgumentsAreExactlyOneObject(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"a": 1} {"a": 2}`, `{"a": 1}null`, `{"a": 1} x`,
		`[1, 2]`, `"text"`, `7`, `{"a": 1`} {
		if args, err := toolargs.Decode([]byte(raw)); err == nil {
			t.Errorf("Decode(%q) = %#v, want it refused", raw, args)
		}
	}
	if args, err := toolargs.Decode([]byte("{\"a\": 1}\n")); err != nil || len(args) != 1 {
		t.Errorf("a document ending in a newline was refused: %#v, %v", args, err)
	}
}
