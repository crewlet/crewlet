package turn_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// SERVER-BACKED AND NOT A KNOWN READ. A delivery to a shared surface only ever
// comes from an MCP server, so a first-party builtin never counts however much
// it writes — and "not a known read" is POSITIVE, so an unannotated tool
// counts, which is the fail-closed direction.
func TestDeliverableIsServerBackedAndNotAKnownRead(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{"slack_post", "slack_history", "reflect_and_persist", "tracker_do"},
		// The registry computed this: slack_history is annotated read-only
		// so it is absent, and reflect_and_persist is a first-party tool
		// registered without tools.Delivers().
		Deliverables: []string{"slack_post", "tracker_do"},
		KnownReads:   []string{"slack_history"},
	}
	for name, want := range map[string]bool{
		"slack_post":          true,  // server-backed write
		"tracker_do":          true,  // server-backed, unannotated — fail closed
		"slack_history":       false, // positively read-only
		"reflect_and_persist": false, // a builtin, however much it writes
		"never_registered":    false,
	} {
		if got := turn.Deliverable(name, s); got != want {
			t.Errorf("Deliverable(%q) = %v, want %v", name, got, want)
		}
	}
}

// SUCCESSFUL CALLS ONLY. A failed post did not post, and counting it would
// close the check on exactly the turn that needs to iterate.
func TestDeliveredIgnoresFailedCalls(t *testing.T) {
	t.Parallel()
	s := turn.Surface{Deliverables: []string{"slack_post"}}
	if turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}}, s) {
		t.Error("a failed call counted as a delivery")
	}
	if !turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}, {Name: "slack_post"}}, s) {
		t.Error("a retry that succeeded did not count")
	}
}

// PROOF, NOT SUSPICION. Acted decides whether a broken turn's trigger is
// given up rather than redelivered, so a true answer SPENDS the work — and the
// tools that must not trip it are the ones a turn calls on every single round.
//
// This is the case that kills the obvious implementation. `submit_work`, the
// discovery pair and the sub-agent spawner are all registered with NO
// annotations at all (internal/agent/runner/phases.go), so any predicate of
// the form "not proven read-only" reads true for the executor's own
// terminator — and every turn that closed a round would then be abandoned.
func TestActedCountsOnlyWhatIsPROVENToHaveLeftTheEngine(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{
			"slack_post", "slack_history", "tracker_do", "submit_work",
			"activate_tool", "reflect_and_persist", "a2a_ask", "run_sandbox",
		},
		Deliverables:   []string{"slack_post", "tracker_do"},
		KnownReads:     []string{"slack_history"},
		KnownOpenWorld: []string{"a2a_ask", "run_sandbox"},
	}
	for name, want := range map[string]bool{
		"slack_post":    true,  // a declared deliverable reached a person
		"tracker_do":    true,  // declared a deliverable at registration
		"a2a_ask":       true,  // a builtin, but it woke a colleague
		"run_sandbox":   true,  // a builtin, but it started a billed box
		"slack_history": false, // positively read-only
		// The three that make this predicate different from
		// !ReadOnlyProven, and the reason it is not written that way:
		// every one of them is unannotated in-engine plumbing that a
		// healthy turn calls constantly.
		"submit_work":         false,
		"activate_tool":       false,
		"reflect_and_persist": false, // an agent's own diary is not the world
		"never_registered":    false,
	} {
		if got := turn.Acted([]ledger.Call{{Name: name}}, s); got != want {
			t.Errorf("Acted(%q) = %v, want %v", name, got, want)
		}
	}
}

// SUCCESSFUL CALLS ONLY, for the same reason Delivered ignores them — and here
// the consequence is sharper: abandoning a turn over a post that did not post
// discards work for a write that never happened.
func TestActedIgnoresFailedCalls(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Deliverables:   []string{"slack_post"},
		KnownOpenWorld: []string{"a2a_ask"},
	}
	if turn.Acted([]ledger.Call{{Name: "slack_post", Failed: true}}, s) {
		t.Error("a failed post counted as having reached the world")
	}
	if turn.Acted([]ledger.Call{{Name: "a2a_ask", Failed: true}}, s) {
		t.Error("a failed ask counted as having woken a colleague")
	}
	if !turn.Acted([]ledger.Call{{Name: "slack_post", Failed: true}, {Name: "slack_post"}}, s) {
		t.Error("a retry that succeeded did not count")
	}
}

// Acted and Delivered answer DIFFERENT questions and neither contains the
// other. Kept as its own case because merging them is the tempting
// simplification and it is wrong in both directions.
func TestActedAndDeliveredDisagreeOnPurpose(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue:      []string{"a2a_ask", "tracker_do"},
		Deliverables:   []string{"tracker_do"},
		KnownOpenWorld: []string{"a2a_ask"},
	}
	ask := []ledger.Call{{Name: "a2a_ask"}}
	// An ask is not a DELIVERY: nobody waiting on this turn got an answer,
	// which is what the delivery gate is for.
	if turn.Delivered(ask, s) {
		t.Error("a colleague ask counted as a delivery to the person waiting")
	}
	// It IS an act: a redelivery would wake that colleague a second time
	// and spend another of their turns.
	if !turn.Acted(ask, s) {
		t.Error("a colleague ask counted as nothing having happened")
	}
}

func TestAwaitedIsEveryReplyButNone(t *testing.T) {
	t.Parallel()
	for r, want := range map[turn.Reply]bool{
		turn.ReplyNone: false, turn.ReplyTool: true, turn.ReplyEngine: true,
	} {
		if got := r.Awaited(); got != want {
			t.Errorf("%s.Awaited() = %v, want %v", r, got, want)
		}
	}
}

// THE ENGINE'S CORRECTION GOES LAST because it is the one the next round must
// act on: on the override path the reviewer said done and wrote no correction
// of its own, so there it is the only instruction there is.
func TestAppendCorrectionPutsTheEnginesWordLast(t *testing.T) {
	t.Parallel()
	got := turn.AppendCorrection("the tone is off", "nothing was delivered")
	if !strings.HasSuffix(got, "nothing was delivered") {
		t.Errorf("joined = %q, want the engine's correction last", got)
	}
	if !strings.Contains(got, "the tone is off") {
		t.Errorf("joined = %q, want the reviewer's notes kept", got)
	}
	// Either side missing leaves the other alone rather than a blank line.
	if got := turn.AppendCorrection("notes", ""); got != "notes" {
		t.Errorf("with no correction = %q", got)
	}
	if got := turn.AppendCorrection("   ", "fix it"); got != "fix it" {
		t.Errorf("with blank notes = %q", got)
	}
}
