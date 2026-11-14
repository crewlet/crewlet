package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

// A DROPPED CALL IS STILL A CALL TO A DELIVERY CHECK. What a run's record kept
// only as identities reads as calls by name and outcome: a tool that succeeded
// at least once among them as a successful call, one none of whose calls
// succeeded as a failed one, and a count whose tool was not kept as no call
// of any tool — never a call a renderer could draw with no arguments.
//
// Mutation: read a tool as failed when any of its dropped calls failed, and a
// reply posted once among two attempts reads as never posted.
func TestADroppedCallIsStillACallToADeliveryCheck(t *testing.T) {
	t.Parallel()
	got := Called([]Elided{
		{Name: "post_message", Digest: "a", Calls: 2, Failed: 1},
		{Name: "read_file", Digest: "b", Calls: 3},
		{Name: "jira_comment", Digest: "c", Calls: 2, Failed: 2},
		{Name: "jira_comment", Digest: "d", Calls: 1, Failed: 1},
		{Calls: 9},
	})
	want := []Call{{Name: "post_message"}, {Name: "read_file"}, {Name: "jira_comment", Failed: true}}
	if len(got) != len(want) {
		t.Fatalf("Called = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Failed != want[i].Failed || got[i].Args != nil {
			t.Errorf("call %d = %+v, want %+v with no arguments", i, got[i], want[i])
		}
	}
}

// WHAT A RECORD DID NOT KEEP IS SAID, NOT HIDDEN. A reviewer handed a long run's
// record, and a later round handed its ledger, are told how many calls it
// dropped and by tool how many and how many failed — the skipped meta-tools
// counted in the total and not named — and a round that dropped nothing says
// nothing more; a round's ledger block carries the line under its calls.
//
// Mutation: return "" for a tally, and the reviewer judges an elision as the
// run; name a skipped meta-tool, and the line carries noise the ledger drops.
func TestWhatARecordDidNotKeepIsSaid(t *testing.T) {
	t.Parallel()
	elided := []Elided{
		{Name: "read_file", Digest: "a", Calls: 100},
		{Name: "read_file", Digest: "b", Calls: 20, Failed: 3},
		{Name: "post_message", Digest: "c", Calls: 1},
		{Name: "activate_tool", Calls: 4},
		{Calls: 5},
	}
	line := FormatElided(elided, []string{"activate_tool"})
	for _, want := range []string{"+130 further call(s)", "read_file ×120 (3 failed)", "post_message ×1",
		"5 to tools the record did not keep either"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line %q does not say %q", line, want)
		}
	}
	if strings.Contains(line, "activate_tool") {
		t.Errorf("the line names a skipped meta-tool: %q", line)
	}
	if FormatElided(nil, nil) != "" {
		t.Error("a record that dropped nothing says something")
	}
	block := RenderIterations([]Iteration{{Iteration: 1, Calls: []Call{{Name: "post_message"}},
		Elided: elided}}, []string{"activate_tool"}, nil)
	if !strings.Contains(block, line) {
		t.Errorf("the round's ledger block does not carry its tally:\n%s", block)
	}
}

// A CALL WHOSE OUTCOME IS UNKNOWN IS NEVER SAID TO HAVE FAILED. A tally counts
// such a call among its failures and apart from them (Elided.Unfinished), and
// its line counts it apart and says what the call's own answer would — that it
// may have taken effect, to be checked before it is made again — because a run
// told it failed makes it again: the duplicate post the unknown outcome exists
// to prevent. A tally holding none says nothing of it.
//
// Mutation: render the failures whole, and the call is said to have failed;
// drop the sentence, and the next round is not told to check.
func TestACallWhoseOutcomeIsUnknownIsNeverSaidToHaveFailed(t *testing.T) {
	t.Parallel()
	line := FormatElided([]Elided{
		{Name: "post_message", Digest: "a", Calls: 2, Failed: 1, Unfinished: 1},
		{Name: "post_message", Digest: "b", Calls: 1, Failed: 1},
		{Name: "read_file", Calls: 4, Failed: 1},
	}, nil)
	for _, want := range []string{"post_message ×3 (1 failed, 1 whose outcome is unknown)", "read_file ×4 (1 failed)",
		"may have taken effect: check whether it did before making it again"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line %q does not say %q", line, want)
		}
	}
	if known := FormatElided([]Elided{{Name: "read_file", Calls: 4, Failed: 1}}, nil); strings.Contains(known,
		"outcome") {
		t.Errorf("a tally of known outcomes says %q", known)
	}
}

// A SPAN OF DROPPED ROUNDS RENDERS ITS ROUNDS AND THEIR CALLS BY TOOL. An entry
// that stands for rounds a park dropped (Iteration.Through) is drawn as those
// rounds, saying that what they set out to do, produced and were told is not
// kept — never as one round that said nothing, which a later round takes for
// work never done — and counting their calls by tool with their outcomes, an
// unknown one apart, which is what the next round must not repeat. Rounds that
// called nothing say "(none)", and a span of one round says all of it of one.
//
// Mutation: draw a span as its first round, or a span of one as a round, and
// the rounds it stands for are a round that set out to do nothing; draw
// "(none)" whatever its tallies, and the calls they made are gone from the
// next round's view.
func TestASpanOfDroppedRoundsRendersItsRoundsAndTheirCallsByTool(t *testing.T) {
	t.Parallel()
	block := RenderIterations([]Iteration{
		{Iteration: 1, Intent: "draft the reply", Calls: []Call{{Name: "read_file"}}},
		{Iteration: 2, Through: 40, Elided: []Elided{
			{Name: "post_message", Calls: 3, Failed: 1, Unfinished: 1},
			{Name: "read_file", Calls: 10},
			{Name: "activate_tool", Calls: 2},
		}},
		{Iteration: 41, Through: 42},
		{Iteration: 43, Through: 43, Elided: []Elided{{Name: "read_file", Calls: 2, Failed: 2}}},
	}, []string{"activate_tool"}, nil)
	for _, want := range []string{
		"### Iteration 1\nSet out to: draft the reply",
		"### Iterations 2–40\n" + spanNotKept + "\nCalled:\n- (15 call(s) these rounds' runs made, which the " +
			"turn's record counts by tool alone: post_message ×3 (1 whose outcome is unknown), read_file ×10; " +
			unknownOutcome + ")",
		"### Iterations 41–42\n" + spanNotKept + "\nCalled:\n" + noCalls,
		"### Iteration 43\n" + roundNotKept + "\nCalled:\n- (2 call(s) its run made, which the turn's record " +
			"counts by tool alone: read_file ×2 (2 failed))",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the ledger block does not say\n%s\n\nin\n%s", want, block)
		}
	}
	if strings.Contains(block, "### Iteration 2\n") || strings.Contains(block, "activate_tool") {
		t.Errorf("the ledger block draws a span as one round, or names a skipped tool:\n%s", block)
	}
}

// A ROUND WHOSE CALLS ARE ALL TALLIED IS NOT SAID TO HAVE CALLED NOTHING. A
// parked turn's closed round condensed past its calls keeps them as a tally
// alone, and its ledger block says what the tally says, rather than "(none)" —
// which a later round reads as a round that took no action, and does the work
// again. A round that called nothing at all still says so.
//
// Mutation: render "(none)" for an empty list whatever the tally holds, and a
// round of two hundred posts reads as one that posted nothing.
func TestARoundWhoseCallsAreAllTalliedIsNotSaidToHaveCalledNothing(t *testing.T) {
	t.Parallel()
	tallied := RenderIterations([]Iteration{{Iteration: 1,
		Elided: []Elided{{Name: "post_message", Calls: 200, Failed: 100}}}}, nil, nil)
	if strings.Contains(tallied, noCalls) || !strings.Contains(tallied, "post_message ×200 (100 failed)") {
		t.Errorf("a round whose calls are tallied renders:\n%s", tallied)
	}
	if idle := RenderIterations([]Iteration{{Iteration: 1}}, nil, nil); !strings.Contains(idle, noCalls) {
		t.Errorf("a round that called nothing renders:\n%s", idle)
	}
}

// A CLOSED ROUND CARRIES ITS CALLS' IDENTITIES AND ITS DROPPED ONES ACROSS A
// PARK. The round rides the parked turn's state as JSON, and a later segment
// seeds its call log from it: a call's digest and the round's tally must come
// back as they went, and a call with none writes no key a build before them
// would not have written.
//
// Mutation: drop either tag's omitempty, or the field itself, and the round
// comes back without what a seed counts.
func TestAClosedRoundCarriesItsIdentitiesAcrossAPark(t *testing.T) {
	t.Parallel()
	in := Iteration{Iteration: 2, Calls: []Call{{Name: "set_status", Digest: "abc"}, {Name: "read_file"}},
		Elided: []Elided{{Name: "set_status", Digest: "def", Calls: 2, Failed: 1}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Iteration
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Calls[0].Digest != "abc" || len(out.Elided) != 1 || out.Elided[0] != in.Elided[0] {
		t.Errorf("the round came back %+v from %s", out, raw)
	}
	if strings.Count(string(raw), `"Digest"`) != 1 {
		t.Errorf("a call with no identity wrote one: %s", raw)
	}
}

// A WITHHELD CALL'S ARGUMENTS ARE SAID TO BE SEALED. A call held for a person's
// approval and the one they approved carry no arguments on any record — they
// ride the run's row sealed — and drawn as `tool()` the line reads as the tool
// called with none: a different call, which the reviewer and the prior-work
// ledger would both take for what the run did. The line says the arguments
// are sealed instead; any other call with none still reads as one.
//
// Mutation: render a withheld call's arguments as the empty map they are, and
// the approved deletion reads as `gh_delete_repo() → success`.
func TestAWithheldCallsArgumentsAreSaidToBeSealed(t *testing.T) {
	t.Parallel()
	got := FormatCalls([]Call{
		{Name: "gh_delete_repo", Withheld: true},
		{Name: "list_repos"},
	}, FormatOptions{})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || lines[0] != "- gh_delete_repo("+withheldArgs+") → success" ||
		lines[1] != "- list_repos() → success" {
		t.Errorf("the calls render:\n%s", got)
	}
}
