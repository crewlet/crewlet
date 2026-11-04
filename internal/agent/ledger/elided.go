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
// THE SAME SHAPE COUNTS THE CALLS A ROUND'S OWN CALLS MADE — a delegated
// worker's, which the run's call log absorbed ([Iteration.Absorbed]) — with
// Failed unset: identities a later segment seeds, which no delivery check reads.
//
// JSON TAGS ARE A WIRE FORMAT: a closed round carries these across a park
// ([Iteration.Elided], [Iteration.Absorbed]).
type Elided struct {
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest,omitempty"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed,omitempty"`
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
// calls, or "" for none: how many, and by tool how many and how many of those
// failed — the evidence a count bound costs, or a store that did not take a
// call's record, said rather than hidden, because a log that silently skips is
// a log that lies about what the run did.
//
// skip are the names that never render ([FormatOptions.Skip]); they are still
// counted in the total, since the run did make them.
func FormatElided(elided []Elided, skip []string) string {
	type tally struct{ calls, failed int }
	byName := map[string]*tally{}
	var names []string
	total, unnamed := 0, 0
	for _, e := range elided {
		if e.Calls <= 0 {
			continue
		}
		total += e.Calls
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
	}
	if total == 0 {
		return ""
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names)+1)
	for _, name := range names {
		t := byName[name]
		part := name + " ×" + itoa(t.calls)
		if t.failed > 0 {
			part += " (" + itoa(t.failed) + " failed)"
		}
		parts = append(parts, part)
	}
	if unnamed > 0 {
		parts = append(parts, itoa(unnamed)+" to tools the record did not keep either")
	}
	line := "- (+" + itoa(total) + " further call(s) the run made, which its record did not keep"
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return line + ")"
}
