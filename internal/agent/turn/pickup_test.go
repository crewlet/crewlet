package turn_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
)

// pickupReply is the obligation of a pickup that handed the item w-1.
func pickupReply() turn.Reply {
	r := turn.ToolReply("")
	r.Item = "w-1"
	return r
}

// A PICKUP'S TURN IS NOT DELIVERED BY A COMMENT ALONE (ADR-0122): with only a
// comment on the record, a delivery or a block the executor reports is sent
// back before the reviewer, and the reviewer's `done` is overturned, each
// naming the five typed answers; with a typed answer on the item every check
// passes, and an assignment's turn, which owes no typed answer, is answered by
// its comment as before.
//
// Mutation: drop the pickup arm from Check, and the comment-only round reaches
// the reviewer, whose `done` the override alone would catch.
func TestACommentAloneDoesNotAnswerAPickup(t *testing.T) {
	t.Parallel()
	s := turn.Surface{Deliveries: map[string]string{
		"comment_on_work_item": "worklet", "update_work_item": "worklet"}}
	commented := []ledger.Call{{Name: "comment_on_work_item"}}
	for _, outcome := range []turn.Outcome{turn.OutcomeDelivered, turn.OutcomeBlocked} {
		w := turn.Work{Outcome: outcome, Calls: commented, Evidence: "waits on the vendor"}
		v := turn.Check(w, nil, pickupReply(), s)
		if v.Correction != turn.PickupUntyped {
			t.Errorf("%s over a comment alone: correction %q, want the pickup's", outcome, v.Correction)
		}
		if override, correction := turn.OverrideDone(w, nil, pickupReply(), s); !override ||
			correction != turn.PickupUntyped {
			t.Errorf("%s over a comment alone: the reviewer's done stood (%v, %q)", outcome, override, correction)
		}
		w.Typed = true
		if v := turn.Check(w, nil, pickupReply(), s); v.Correction != "" {
			t.Errorf("%s over a typed answer was sent back: %q", outcome, v.Correction)
		}
		if override, _ := turn.OverrideDone(w, nil, pickupReply(), s); override {
			t.Errorf("%s over a typed answer had its done overturned", outcome)
		}
	}
	assigned := turn.Work{Outcome: turn.OutcomeDelivered, Calls: commented}
	if v := turn.Check(assigned, nil, turn.ToolReply(""), s); v.Correction != "" {
		t.Errorf("an assignment answered by a comment was sent back: %q", v.Correction)
	}
	for _, want := range []string{"a comment alone", "update_work_item", "`ask`", "`waiting_on`",
		"`wait_until`", "`assignee`"} {
		if !strings.Contains(turn.PickupUntyped, want) && !strings.Contains(types.PickupAnswers, want) {
			t.Errorf("the pickup's correction does not name %q:\n%s", want, turn.PickupUntyped)
		}
	}
	if !strings.Contains(turn.PickupUntyped, types.PickupAnswers) {
		t.Error("the correction names other answers than the brief does")
	}
}

// A PICKUP'S ITEM RIDES ITS STORED OBLIGATION, so the turn that resumes from a
// parked run is held to the same typed answer; a build that does not know the
// mark reads a kind it refuses rather than a plain tool obligation.
func TestAPickupsItemRoundTripsThroughTheStoredReply(t *testing.T) {
	t.Parallel()
	want := pickupReply()
	if got := turn.ParseReply(want.String()); got != want || !got.Valid() {
		t.Errorf("ParseReply(%q) = %+v, want %+v", want.String(), got, want)
	}
	// AN OLDER BUILD'S PARSE: everything before the first `:` is the kind.
	kind, _, _ := strings.Cut(want.String(), ":")
	if (turn.Reply{Kind: turn.ReplyKind(kind)}).Valid() {
		t.Errorf("an older build would read %q as an obligation it knows", want.String())
	}
	if (turn.Reply{Kind: turn.ReplyNone, Item: "w-1"}).Valid() {
		t.Error("an item on an obligation nobody waits on reads as valid")
	}
}
