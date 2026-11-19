package runner

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// AN OPENCODE CALL IS KEYED BY ITS PLACE IN ITS LOG: its Appended in decimal,
// valid only beside an admission place and never for a call a person's
// approval held, which is then never asked; two calls of one place are both
// unasked. Claude Code's calls are keyed by their id as before.
//
// Mutation: leave Seq unchecked, key a held call, or read OpenCode's id, and a
// case goes red.
func TestAnOpenCodeCallIsKeyedByItsPlaceInItsLog(t *testing.T) {
	t.Parallel()
	call := func(appended, seq int) BridgedCall {
		return BridgedCall{Call: ledger.Call{Name: "echo", Digest: "d"}, Appended: appended, Seq: seq}
	}
	held := call(3, 3)
	held.Held = true
	withID := call(4, 4)
	withID.CallID = "toolu_x"
	for name, tc := range map[string]struct {
		call  BridgedCall
		key   string
		valid bool
	}{
		"placed":              {call(5, 2), "5", true},
		"no place in the log": {call(0, 2), "0", false},
		"no admission place":  {call(5, 0), "5", false},
		"held for a person":   {held, "3", false},
		"carrying an id":      {withID, "4", true},
	} {
		key, valid := callKey(sandbox.OpenCodeHarness, tc.call)
		if key != tc.key || valid != tc.valid {
			t.Errorf("%s: key %q, %t; want %q, %t", name, key, valid, tc.key, tc.valid)
		}
	}
	if key, valid := callKey(sandbox.ClaudeCodeHarness, withID); key != "toolu_x" || !valid {
		t.Errorf("Claude Code's key = %q, %t; want its id", key, valid)
	}
	twice := []BridgedCall{call(7, 1), call(7, 2), held}
	ask, asked := (&Runner{}).holdingsAsk(sandbox.OpenCodeHarness, roundCalls{bridged: twice}, nil, nil, "2.2.7")
	if len(asked) != 0 || slices.ContainsFunc(ask.Calls, func(c sandbox.LoggedCall) bool { return c.Asked }) {
		t.Errorf("calls of one place and a held call: asked %+v; want none", ask.Calls)
	}
}

// AN OPENCODE ASK KEYS EACH CALL BY ITS LOGGED TEXT: "" with a digest keys
// the empty object; "" with none, text the log cut, withheld arguments, ones
// the bridge could not read, or text that does not decode state no key. A
// Claude Code ask computes none.
//
// Mutation: key from Args, which is nil for "" and unreadable text alike, or
// key every harness's calls, and a case goes red.
func TestAnOpenCodeAskKeysEachCallByItsLoggedText(t *testing.T) {
	t.Parallel()
	empty, _ := toolargs.Key(nil)
	text, _ := toolargs.Key([]byte(`{"b":1,"a":"x"}`))
	for name, tc := range map[string]struct {
		call BridgedCall
		want sandbox.ArgsKey
	}{
		"text":                 {BridgedCall{ArgsText: `{"a":"x","b":1}`}, sandbox.ArgsKey{Whole: true, Sum: text}},
		"none, with a digest":  {BridgedCall{Call: ledger.Call{Digest: "d"}}, sandbox.ArgsKey{Whole: true, Sum: empty}},
		"none, with no digest": {BridgedCall{}, sandbox.ArgsKey{}},
		"cut":                  {BridgedCall{ArgsText: `{"a":"x"}`, ArgsBytes: 70000}, sandbox.ArgsKey{}},
		"withheld":             {BridgedCall{Call: ledger.Call{Withheld: true, Digest: "d"}}, sandbox.ArgsKey{}},
		"unread":               {BridgedCall{Call: ledger.Call{ArgsUnread: true, Digest: "d"}}, sandbox.ArgsKey{}},
		"no object":            {BridgedCall{ArgsText: `[1]`, Call: ledger.Call{Digest: "d"}}, sandbox.ArgsKey{}},
	} {
		tc.call.Name, tc.call.Appended, tc.call.Seq = "echo", 1, 1
		ask, _ := (&Runner{}).holdingsAsk(sandbox.OpenCodeHarness, roundCalls{bridged: []BridgedCall{tc.call}}, nil,
			nil, "2.2.7")
		if got := ask.Calls[0].Args; got != tc.want {
			t.Errorf("%s: key %+v; want %+v", name, got, tc.want)
		}
		claude, _ := (&Runner{}).holdingsAsk(sandbox.ClaudeCodeHarness, roundCalls{bridged: []BridgedCall{tc.call}},
			nil, nil, "2.1.322")
		if got := claude.Calls[0].Args; got != (sandbox.ArgsKey{}) {
			t.Errorf("%s: a Claude Code ask keys %+v; want none", name, got)
		}
	}
}

// AN ASK HOLDS EVERY LISTED CALL BUT THE APPROVED ONES: each with its place,
// tool, digest, outcome and held mark, asked where its telling would list it,
// a reported skill load among them (OpenCode's carries its place, and no part
// of its session names it); the calls in flight marked so; the log's dropped
// tally with OnlyFailed only where every call it counts failed and none is
// unfinished; and no entry for the late dropped count, which that tally
// holds. The approved call is no source of a part, so it is in neither list.
//
// Mutation: leave late calls out, put approved calls in, or add an unnamed
// entry for the late count, and a case goes red.
func TestAnAskHoldsEveryListedCallButTheApprovedOnes(t *testing.T) {
	t.Parallel()
	placed := func(name string, at int) BridgedCall {
		return BridgedCall{Call: ledger.Call{Name: name, Result: name + " done", Digest: "d-" + name}, Appended: at,
			Seq: at, ArgsText: "{}"}
	}
	late, approved, held, load := placed("late", 2), placed("approved", 3), placed("held", 4),
		placed("load_tool_skill", 5)
	late.Late, approved.Approved, held.Held = true, true, true
	round := roundCalls{
		bridged:    []BridgedCall{placed("echo", 1), late, approved, held, load},
		unfinished: []BridgedCall{{Call: ledger.Call{Name: "slow", Digest: "d-slow"}, Seq: 6}},
		elided: []ledger.Elided{{Name: "gone", Digest: "d-gone", Calls: 2, Failed: 2},
			{Name: "mixed", Calls: 2, Failed: 1}, {Name: "stuck", Calls: 1, Failed: 1, Unfinished: 1}},
		lateElided: 3,
	}
	ask, asked := (&Runner{}).holdingsAsk(sandbox.OpenCodeHarness, round, new(int), nil, "2.2.7")
	var names []string
	for _, c := range ask.Calls {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"echo", "late", "held", "load_tool_skill"}) {
		t.Errorf("the ask's calls are %q; want every listed call but the approved one", names)
	}
	if _, ok := asked["5"]; !ok || len(asked) != 2 {
		t.Errorf("asked %v; want the echo call and the skill load", asked)
	}
	if c := ask.Calls[2]; !c.Held || c.Asked || c.Digest != "d-held" || c.Appended != 4 {
		t.Errorf("the held call is %+v; want it marked, unasked, with its place and digest", c)
	}
	want := []sandbox.CallIdentity{{Name: "slow", Digest: "d-slow", InFlight: true},
		{Name: "gone", Digest: "d-gone", OnlyFailed: true}, {Name: "mixed"}, {Name: "stuck"}}
	if !slices.Equal(ask.Unlisted, want) {
		t.Errorf("the unlisted calls are %+v; want %+v", ask.Unlisted, want)
	}
}
