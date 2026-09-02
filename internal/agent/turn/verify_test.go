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
		Catalogue:  []string{"slack_post", "slack_history", "reflect_and_persist", "tracker_do"},
		MCPTools:   []string{"slack_post", "slack_history", "tracker_do"},
		KnownReads: []string{"slack_history"},
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
	s := turn.Surface{MCPTools: []string{"slack_post"}}
	if turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}}, s) {
		t.Error("a failed call counted as a delivery")
	}
	if !turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}, {Name: "slack_post"}}, s) {
		t.Error("a retry that succeeded did not count")
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
