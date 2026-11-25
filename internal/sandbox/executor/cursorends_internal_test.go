package executor

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// cursorFixture is a file of testdata/cursor at the measured release.
func cursorFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/cursor/" + cursorSupported + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A CURSOR RUN'S END IS WHAT ITS OWN RECORD SAYS: each stream as the real
// cursor-agent of the measured release printed it, with what the run's script
// adds after it. A finished run ends on its result; a refused one on the
// turn_ended its transcript recorded, failed in Cursor's words; one stopped at
// its cap on the script's cap line, failed and capped; one interrupted,
// terminated or killed, which Cursor records no end of, never ends.
//
// Mutation: read a turn_ended with an error as no end, take the cap line for
// a success, or end a run on a line that ends nothing: a row goes red.
func TestCursorsEndIsWhatItsOwnRecordSays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                   string
		ended, success, capped bool
		says                   string
	}{
		{"finished", true, true, false, ""},
		{"refused", true, false, false, "Unauthorized (HTTP 401)"},
		{"capped", true, false, true, "turn cap of 1"},
		{"interrupted", false, false, false, "stopped before it reported how its run ended"},
		{"sigterm", false, false, false, "stopped before it reported how its run ended"},
		{"sigkill", false, false, false, "stopped before it reported how its run ended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dec := (CursorAgent{}).Events()
			if err := eachLine(bytes.NewReader(cursorFixture(t, "ends/"+c.name+".stream.jsonl")), dec,
				maxLineBytes); err != nil {
				t.Fatal(err)
			}
			res := dec.Result()
			if dec.Ended() != c.ended || res.Success != c.success || res.RoundCapped != c.capped ||
				!strings.Contains(res.Error, c.says) {
				t.Fatalf("ended %t, success %t, capped %t (%q); want ended %t, success %t, capped %t naming %q",
					dec.Ended(), res.Success, res.RoundCapped, res.Error, c.ended, c.success, c.capped, c.says)
			}
		})
	}
}

// A FINISHED CURSOR RUN ANSWERS WITH ITS LAST WORDS AND IS CHARGED ITS RESULT'S
// USAGE: the prompt not served from the cache, its cached share and the reply
// as the result line states them (measured: 160 sent, 40 of them cached),
// and its turns as the model responses it made, one calling a tool and the
// answer.
//
// Mutation: charge the cached share as uncached, or count a call's start and
// completion as two responses: this goes red.
func TestAFinishedCursorRunIsChargedItsResultsUsage(t *testing.T) {
	t.Parallel()
	res := (CursorAgent{}).Parse(cursorFixture(t, "ends/finished.stream.jsonl"))
	if !res.Success || res.Text != "STUB-DONE" || res.SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("result %+v, want the answer STUB-DONE of the session", res)
	}
	if res.CacheReadTokens != 40 || res.OutputTokens != 10 || res.Turns != 2 {
		t.Errorf("charged %d cached, %d out over %d turns; want 40, 10 over 2 (%+v)", res.CacheReadTokens,
			res.OutputTokens, res.Turns, res)
	}
	if !strings.Contains(res.Transcript, "echo TOOL-RAN") {
		t.Errorf("the transcript does not name the shell call:\n%s", res.Transcript)
	}
}

// A CURSOR RUN'S RESPONSES ARE COUNTED EXACTLY AT ANY LENGTH: each model call
// once, however many calls it started or how late their ends arrived, and past
// the bound a set of the calls seen would hold.
//
// Mutation: count every start, count a model call again when its call ends
// after the next response's, or hold the count to a bound, and a row differs.
func TestACursorRunsResponsesAreCountedExactlyAtAnyLength(t *testing.T) {
	t.Parallel()
	call := func(sub, model string) string {
		return `{"type":"tool_call","subtype":"` + sub + `","call_id":"c","model_call_id":"` + model + `","tool_call":{}}`
	}
	var many []string
	for i := range maxPendingCalls + 5 {
		m := fmt.Sprintf("m%d", i)
		many = append(many, call("started", m), call("completed", m))
	}
	for name, c := range map[string]struct {
		lines []string
		want  int
	}{
		"two calls of one response": {[]string{call("started", "a"), call("started", "a"), call("completed", "a"),
			call("completed", "a"), call("started", "b")}, 2},
		"an end after the next response's start": {[]string{call("started", "a"), call("started", "b"),
			call("completed", "a"), call("completed", "b")}, 2},
		"past a set's bound": {many, maxPendingCalls + 5},
	} {
		dec := &cursorEvents{}
		for _, l := range c.lines {
			dec.Line([]byte(l))
		}
		if dec.Requests() != c.want {
			t.Errorf("%s: %d responses, want %d", name, dec.Requests(), c.want)
		}
	}
}
