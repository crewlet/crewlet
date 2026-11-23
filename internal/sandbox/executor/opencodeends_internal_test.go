package executor

import (
	"bytes"
	"os"
	"testing"
)

// OPENCODE'S END IS WHAT ITS OWN STREAM SAYS, read off streams OpenCode 2.3.2
// printed under the runner's invocation (`opencode serve`, then `opencode run
// --server --format json --title crewlet --session --model`) against a stub
// model: a finished run ends on its `stop` step and succeeds; a refused
// request and a session interrupted as the run's end interrupts it end on an
// `error` event and fail; a client killed mid-turn, its last step one that
// called a tool, never ended. A client killed before its first step prints
// nothing, which every runner's table case covers.
//
// Mutation: read a `tool-calls` step as an end, or an `error` event as none,
// and its case fails.
func TestOpenCodesEndIsWhatItsStreamSays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name           string
		ended, success bool
	}{
		{"finished", true, true},
		{"refused", true, false},
		{"interrupted", true, false},
		{"midturn", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile("testdata/opencode/2.3.2/ends/" + c.name + ".stream.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			dec := (OpenCode{}).Events()
			if err := eachLine(bytes.NewReader(raw), dec, maxLineBytes); err != nil {
				t.Fatal(err)
			}
			res := dec.Result()
			if dec.Ended() != c.ended || res.Success != c.success {
				t.Fatalf("ended %t, success %t (%q); want ended %t, success %t",
					dec.Ended(), res.Success, res.Error, c.ended, c.success)
			}
		})
	}
}
