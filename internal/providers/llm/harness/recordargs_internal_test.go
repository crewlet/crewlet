package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// A RECORDED CALL'S ARGUMENTS ARE READ AS THE MODEL WROTE THEM, and arguments
// that are not an object say so rather than reading as none. Claude Code keeps
// the array its model wrote in its record and calls the server with the
// array's elements as an object (measured on 2.1.310: `["a","b"]` arrives as
// `{"0":"a","1":"b"}`), so the record is the one witness that the call's
// arguments did not parse; read as `{}`, the host ran the call. An absent value
// — null, an empty string, as a harness records a call with no arguments — is
// none, and is no error.
func TestARecordedCallsArgumentsSayWhenTheyAreNotAnObject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		value   any
		want    string
		problem string
	}{
		{name: "an object", value: map[string]any{"text": "x"}, want: `{"text":"x"}`},
		{name: "an object as a string", value: `{"text":"x"}`, want: `{"text":"x"}`},
		{name: "null", value: nil, want: `{}`},
		{name: "an empty string", value: " ", want: `{}`},
		{name: "a null string", value: "null", want: `{}`},
		{name: "an array", value: []any{"a", "b"}, want: `{}`, problem: "a JSON array"},
		{name: "a number", value: json.Number("4"), want: `{}`, problem: "a JSON number"},
		{name: "an unterminated object", value: `{"text": "x`, want: `{}`, problem: "not one JSON object"},
		{name: "an array as a string", value: `["a","b"]`, want: `{}`, problem: "not one JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, problem := argumentsOf(tc.value)
			got, _ := json.Marshal(args)
			if string(got) != tc.want {
				t.Errorf("arguments = %s, want %s", got, tc.want)
			}
			if tc.problem == "" && problem != "" {
				t.Errorf("an absent or object value was faulted: %q", problem)
			}
			if tc.problem != "" && !strings.Contains(problem, tc.problem) {
				t.Errorf("problem = %q, want it to say %q", problem, tc.problem)
			}
		})
	}
}
