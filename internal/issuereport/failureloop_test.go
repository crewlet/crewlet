package issuereport

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// rebaseFor and turnLoopAt are two engine functions a failure can leave
// through, as the runtime spells them.
const (
	rebaseFor  = "github.com/crewlet/crewlet/internal/engine.rebaseFor"
	turnLoopAt = "github.com/crewlet/crewlet/internal/agent/turn.Run"
)

// stampedTurn is a seat's turn that failed unclassified, its error stamped at
// function.
func stampedTurn(function string) types.AgentTurnCompleted {
	return types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kindError,
		ErrorFunction: function, ErrorFile: "/srv/build/internal/x/file.go"}
}

// sentReport decodes the one report the relay received.
func sentReport(t *testing.T, r *rig) map[string]any {
	t.Helper()
	got := r.relay.received()
	if len(got) != 1 {
		t.Fatalf("%d reports, want 1", len(got))
	}
	var doc map[string]any
	if err := json.Unmarshal(got[0], &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A FAILURE LOOP IS COUNTED PER SEAT AND PLACE, and names the place: five
// failures at one function are one defect, four there and one at another are
// two causes that each started over, and a loop nothing stamped falls back to
// the class's own site, which tells a maintainer where to add the next stamp.
//
// Mutation: count per seat alone (drop the place from the kind) and the mixed
// row is reported; name the class's site for a stamped loop and the place row
// does; drop the fallback and the unstamped loop names nothing.
func TestAFailureLoopIsCountedPerPlaceAndNamesIt(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassFailureLoop)
	r := newRig(t)
	for range threshold - 1 {
		r.publish(stampedTurn(rebaseFor))
	}
	r.publish(stampedTurn(turnLoopAt))
	r.idle()
	if got := r.relay.received(); len(got) != 0 {
		t.Fatalf("a row two places shared was reported: %s", got)
	}
	for range threshold - 1 {
		r.publish(stampedTurn(turnLoopAt))
	}
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
	doc := sentReport(t, r)
	if doc["site"] != "internal/agent/turn.Run" || doc["component"] != "internal/agent/turn" ||
		doc["site_file"] != "internal/agent/turn/file.go" {
		t.Errorf("a loop at turn.Run was reported at %v in %v (%v)", doc["site"], doc["component"], doc["site_file"])
	}
	if facts, _ := doc["facts"].(map[string]any); facts["message_lost"] != false {
		t.Errorf("a turn loop says message_lost %v", facts["message_lost"])
	}

	unstamped := newRig(t)
	for range threshold {
		unstamped.publish(types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kindError})
	}
	waitFor(t, "the fallback report", func() bool { return len(unstamped.relay.received()) == 1 })
	doc = sentReport(t, unstamped)
	if entry, _ := entryOf(ClassFailureLoop); doc["site"] != string(entry.Site) || doc["site_file"] != nil {
		t.Errorf("an unstamped loop was reported at %v (%v), want the class's own site", doc["site"], doc["site_file"])
	}
}

// A LOST MESSAGE IS REPORTED AT ONCE, AT ITS PLACE. A delivery whose failures
// nothing classified spent its whole budget, so one is already every retry the
// threshold stands for; it is the same defect as a loop at the same place, so
// it shares the loop's fingerprint and says it lost a message.
//
// Mutation: drop Once and nothing is sent; give it a place of its own and the
// fingerprint differs from the loop's.
func TestALostMessageIsReportedAtOnceAtItsPlace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.report.MessageLost(context.Background(), "seat", rebaseFor, "/srv/build/internal/engine/rebase.go")
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
	doc := sentReport(t, r)
	if doc["site"] != "internal/engine.rebaseFor" || doc["site_file"] != "internal/engine/rebase.go" {
		t.Errorf("a lost message was reported at %v (%v)", doc["site"], doc["site_file"])
	}
	if facts, _ := doc["facts"].(map[string]any); facts["message_lost"] != true {
		t.Errorf("a lost message says message_lost %v", facts["message_lost"])
	}
	loop := newRig(t)
	for range entryThreshold(ClassFailureLoop) {
		loop.publish(types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kindError,
			ErrorFunction: rebaseFor, ErrorFile: "rebase.go"})
	}
	waitFor(t, "the loop's report", func() bool { return len(loop.relay.received()) == 1 })
	if got := sentReport(t, loop)["fingerprint"]; got != doc["fingerprint"] {
		t.Errorf("a loop and a lost message at one place are two fingerprints: %v, %v", got, doc["fingerprint"])
	}
}
