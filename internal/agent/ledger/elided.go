package ledger

import (
	"slices"
	"strings"
)

// Elided is calls a record did not keep, by identity: how many of a run's calls
// to Name asked for Digest, and how many of those failed.
//
// A RECORD BOUNDED IN COUNT KEEPS THE IDENTITIES OF WHAT IT DROPS. An executor
// run's calls are kept in the log beside its row, which drops a long run's
// middle (internal/sandbox's MaxBridgeCalls), and each call it drops still
// asked for something — which the run's call log counts every later call
// against, so an operation id derived after the drop is the one the run would
// have derived without it (internal/agent/turnctx's CallLog). So the drop is a
// tally rather than a loss: the tool, the digest of the arguments the call was
// admitted with, how many calls had both, and how many failed — which is also
// all a delivery check reads of a call.
//
// AN IDENTITY THE RECORD COULD NOT KEEP is said by what is missing: an empty
// Digest is calls to Name whose arguments are not known (an over-count of Name
// when seeded), and an empty Name calls whose tool is not known either (an
// over-count of every tool). Both are the safe direction, and both are a
// record that grew past what its tally holds.
//
// AN OUTCOME NOBODY KNOWS IS COUNTED APART: Unfinished is how many of the
// calls were [Call.Unfinished] — begun, and not finished as their run's
// collection stopped waiting for them — each counted in Failed too, as the
// call itself is failed. A parked turn's closed round condensed past its
// calls keeps them in its tally (internal/agent/execstate), and a count of
// failures alone would tell the next round a call that may have taken effect
// did nothing, which it would make again, and tell the turn's end that a reply
// still running was never sent.
//
// THE SAME SHAPE COUNTS THE CALLS A ROUND'S OWN CALLS MADE — a delegated
// worker's, which the run's call log absorbed ([Iteration.Absorbed]) — with
// Failed and Unfinished unset: identities a later segment seeds, which no
// delivery check reads.
//
// JSON TAGS ARE A WIRE FORMAT: a closed round carries these across a park
// ([Iteration.Elided], [Iteration.Absorbed]).
type Elided struct {
	Name       string `json:"name,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Calls      int    `json:"calls"`
	Failed     int    `json:"failed,omitempty"`
	Unfinished int    `json:"unfinished,omitempty"`
}

// Called is what a delivery check reads of calls a record did not keep: one
// successful call per tool that succeeded at least once among them, and one
// failed call per tool none of whose calls succeeded — names and outcomes,
// which is everything [Call] a delivery check reads.
//
// FOR A CHECK, NEVER FOR A RENDER: a call here carries no arguments, and drawn
// as a ledger line it would read as a call made with none. [FormatElided] is
// how they are shown.
//
// A count with no name is no call of any tool, and is left out: what it means
// for a delivery is the reader's to decide (an incomplete record).
func Called(elided []Elided) []Call {
	succeeded := map[string]bool{}
	var names []string
	for _, e := range elided {
		if e.Name == "" || e.Calls <= 0 {
			continue
		}
		if _, seen := succeeded[e.Name]; !seen {
			names = append(names, e.Name)
		}
		succeeded[e.Name] = succeeded[e.Name] || e.Calls > e.Failed
	}
	out := make([]Call, 0, len(names))
	for _, name := range names {
		out = append(out, Call{Name: name, Failed: !succeeded[name]})
	}
	return out
}

// Count is how many calls a tally counts.
func Count(elided []Elided) int {
	n := 0
	for _, e := range elided {
		n += max(e.Calls, 0)
	}
	return n
}

// FormatElided is the line that says what a record did not keep of a run's
// calls, or "" for none: how many, and by tool how many, how many of those
// failed and how many have an outcome nobody knows — the evidence a count bound
// costs, or a store that did not take a call's record, said rather than hidden,
// because a log that silently skips is a log that lies about what the run did.
//
// A CALL WHOSE OUTCOME IS UNKNOWN IS NEVER SAID TO HAVE FAILED: it is counted
// apart from the failures it is also counted among ([Elided.Unfinished]), and
// the line says what [Call.Unfinished]'s own answer says of it — that it may
// have taken effect, to be checked before it is made again — because a run
// told it failed makes it again.
//
// skip are the names that never render ([FormatOptions.Skip]); they are still
// counted in the total, since the run did make them.
func FormatElided(elided []Elided, skip []string) string {
	return tallyLine(elided, skip, furtherCalls)
}

// furtherCalls opens [FormatElided]'s line.
func furtherCalls(total int) string {
	return "- (+" + itoa(total) + " further call(s) the run made, which its record did not keep"
}

// tallyLine is the line counting the calls of a tally, or "" for none: opening
// says what they are, handed their total, and the rest is [FormatElided]'s —
// by tool, the failures and the unknown outcomes apart.
func tallyLine(elided []Elided, skip []string, opening func(total int) string) string {
	type tally struct{ calls, failed, unfinished int }
	byName := map[string]*tally{}
	var names []string
	total, unnamed, unfinished := 0, 0, 0
	for _, e := range elided {
		if e.Calls <= 0 {
			continue
		}
		total += e.Calls
		unfinished += max(e.Unfinished, 0)
		switch {
		case e.Name == "":
			unnamed += e.Calls
			continue
		case slices.Contains(skip, e.Name):
			continue
		}
		t, ok := byName[e.Name]
		if !ok {
			t = &tally{}
			byName[e.Name] = t
			names = append(names, e.Name)
		}
		t.calls += e.Calls
		t.failed += e.Failed
		t.unfinished += max(e.Unfinished, 0)
	}
	if total == 0 {
		return ""
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names)+1)
	for _, name := range names {
		t := byName[name]
		var outcomes []string
		if failed := t.failed - t.unfinished; failed > 0 {
			outcomes = append(outcomes, itoa(failed)+" failed")
		}
		if t.unfinished > 0 {
			outcomes = append(outcomes, itoa(t.unfinished)+" whose outcome is unknown")
		}
		part := name + " ×" + itoa(t.calls)
		if len(outcomes) > 0 {
			part += " (" + strings.Join(outcomes, ", ") + ")"
		}
		parts = append(parts, part)
	}
	if unnamed > 0 {
		parts = append(parts, itoa(unnamed)+" to tools the record did not keep either")
	}
	line := opening(total)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	if unfinished > 0 {
		line += "; " + unknownOutcome
	}
	return line + ")"
}

// unknownOutcome is what [FormatElided] says of the calls it counts whose
// outcome nobody knows, in the words a listed one answers with
// (internal/agent/runner's UnfinishedNote).
const unknownOutcome = "a call whose outcome is unknown may have taken effect: check whether it did " +
	"before making it again"
