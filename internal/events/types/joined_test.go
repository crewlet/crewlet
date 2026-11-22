package types

import "testing"

// A JOINED RECORD SAYS WHAT BECAME OF THE WAKE, AND NO MORE. Each outcome this
// build knows reads as what happened to the message, and an outcome a newer
// peer recorded reads as no more than the message having been handed over:
// claimed as read or as left, it would say what nobody recorded.
//
// Mutation: read an unknown outcome as delivered, and its row says the run read
// a message nobody says it read.
func TestAJoinedRecordSaysWhatBecameOfTheWakeAndNoMore(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[JoinOutcome]string{
		JoinDelivered:   "swe read a message from bob about its work",
		JoinUnconfirmed: "swe was handed a message from bob and never confirmed reading it",
		JoinUnread:      "swe ended its job before reading a message from bob",
		"parked":        "swe was handed a message from bob",
	} {
		if got := (AgentTurnJoined{Outcome: outcome, WrittenBy: "bob"}).SummaryFor("swe"); got != want {
			t.Errorf("%s: %q, want %q", outcome, got, want)
		}
		if outcome.Valid() != (outcome != "parked") {
			t.Errorf("%s: Valid is %v", outcome, outcome.Valid())
		}
	}
}
