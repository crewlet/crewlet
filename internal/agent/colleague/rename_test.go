package colleague_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/colleague"
)

// A COLLEAGUE IS REACHABLE BY THE NAME AN AGENT REMEMBERS.
//
// This package's premise is that a model types what it remembers, and what it
// remembers after a rename is the old handle: it is in the conversation
// ledger, in a page, in a channel topic, in the seat's own diary. Resolved
// against live handles alone, a renamed colleague answered "no such
// colleague" over a seat sitting right there — and a lookup that cannot find
// somebody is an ask that never happens.
func TestARenamedColleagueAnswersToTheNameAnAgentRemembers(t *testing.T) {
	t.Parallel()
	seats := []colleague.Seat{
		{Handle: "head-reliability", Name: "Head of Reliability", Kind: "agent",
			Former: []string{"sre-lead"}},
		{Handle: "cto", Name: "CTO", Kind: "agent"},
	}
	found := colleague.Resolve("sre-lead", seats)
	if len(found) != 1 {
		t.Fatalf("resolving a retired handle gave %d candidates, want exactly "+
			"one — %v", len(found), found)
	}
	if found[0].Seat.Handle != "head-reliability" {
		t.Errorf("resolved to %q, want the seat that used to answer to it",
			found[0].Seat.Handle)
	}
	// AND IT SAYS SO. The answer is a seat the query does not name, so a
	// reader who is not told will type the retired handle again.
	if got := found[0].Method; got != colleague.MethodFormerHandle {
		t.Errorf("method = %q, want the former-handle tier", got)
	}
	if found[0].Method.Label() == "" {
		t.Error("the former-handle tier renders no label, so an ambiguity " +
			"list would show this candidate with no reason beside it")
	}
}

// AND A LIVE HANDLE IS NEVER AMBIGUOUS WITH SOMEBODY ELSE'S RETIRED ONE.
//
// The rank this tier exists at, asserted: one seat renamed AWAY from a name
// another seat now holds. Matched in the same tier as a live handle the two
// would come back as two candidates, and two candidates means the caller must
// not pick — so that name would become unusable for both seats at once.
func TestALiveHandleBeatsAnotherSeatsRetiredOne(t *testing.T) {
	t.Parallel()
	seats := []colleague.Seat{
		{Handle: "head-reliability", Name: "Head of Reliability", Kind: "agent",
			Former: []string{"sre-lead"}},
		{Handle: "sre-lead", Name: "SRE Lead", Kind: "agent"},
	}
	found := colleague.Resolve("sre-lead", seats)
	if len(found) != 1 {
		t.Fatalf("%d candidates for a name one seat holds and another used to "+
			"hold, want the live one alone — %v", len(found), found)
	}
	if found[0].Seat.Handle != "sre-lead" {
		t.Errorf("resolved to %q, want the seat holding that handle today",
			found[0].Seat.Handle)
	}
	if got := found[0].Method; got != colleague.MethodExactHandle {
		t.Errorf("method = %q, want the live exact tier", got)
	}
}

// AND A LIVE HANDLE STILL WINS WHEN IT IS ONLY THE FOLD THAT MATCHES IT.
//
// The case above is decided by tier 1 and never reaches the fold, so it does
// not pin where this tier sits relative to tier 2 — which is the placement
// that matters, because a live handle a query matches only after folding is
// exactly as live as one it matches exactly. Folded in the SAME pass as the
// retired addresses, this query comes back as two candidates, and two means
// the caller must not pick: the name becomes unusable for both seats.
func TestALiveHandleWinsEvenWhenOnlyTheFoldReachesIt(t *testing.T) {
	t.Parallel()
	seats := []colleague.Seat{
		{Handle: "head-reliability", Name: "Head of Reliability", Kind: "agent",
			Former: []string{"sre-lead"}},
		{Handle: "sre-lead", Name: "SRE Lead", Kind: "agent"},
	}
	// Underscored and mixed case, so tier 1 misses both seats and tier 2 is
	// what finds the live one.
	found := colleague.Resolve("SRE_Lead", seats)
	if len(found) != 1 {
		t.Fatalf("%d candidates, want the live seat alone — a retired address "+
			"folded in the same pass as a live one makes the name ambiguous "+
			"and therefore unusable: %v", len(found), found)
	}
	if found[0].Seat.Handle != "sre-lead" {
		t.Errorf("resolved to %q, want the seat holding that handle today",
			found[0].Seat.Handle)
	}
	if got := found[0].Method; got != colleague.MethodCaseInsensitive {
		t.Errorf("method = %q, want the live fold", got)
	}
}

// AND A RETIRED NAME BEATS A SUBSTRING OF SOMEBODY ELSE'S.
//
// The other half of the rank. A retired handle is a name somebody wrote down
// exactly; run after the approximate tiers it would lose to any seat whose
// name happens to contain the query, which is how an ask reaches the wrong
// colleague rather than nobody.
func TestARetiredNameBeatsAnApproximateMatch(t *testing.T) {
	t.Parallel()
	seats := []colleague.Seat{
		{Handle: "head-reliability", Name: "Head of Reliability", Kind: "agent",
			Former: []string{"sre"}},
		{Handle: "sre-tooling", Name: "SRE Tooling", Kind: "agent"},
	}
	found := colleague.Resolve("sre", seats)
	if len(found) != 1 || found[0].Seat.Handle != "head-reliability" {
		t.Fatalf("resolved %v, want the seat that answered to that exact name", found)
	}
	if got := found[0].Method; got != colleague.MethodFormerHandle {
		t.Errorf("method = %q, want the former-handle tier", got)
	}
}

// AND THE FOLD REACHES IT, on the same terms a live handle gets.
//
// A retired handle is stored folded, and what a model types is whatever it
// read — "SRE_Lead" out of a Slack message, "SRE Lead" out of prose. A tier
// that compared raw would answer for one spelling of a name and not the
// others, which is the inconsistency tier 2 exists to remove for live ones.
func TestARetiredNameIsFoldedLikeALiveOne(t *testing.T) {
	t.Parallel()
	seats := []colleague.Seat{
		{Handle: "head-reliability", Name: "Head of Reliability", Kind: "agent",
			Former: []string{"sre-lead"}},
	}
	for _, query := range []string{"SRE-Lead", "SRE_Lead", "sre lead"} {
		found := colleague.Resolve(query, seats)
		if len(found) != 1 || found[0].Seat.Handle != "head-reliability" {
			t.Errorf("%q resolved %v, want the renamed seat", query, found)
		}
	}
}
