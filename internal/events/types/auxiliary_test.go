package types

import (
	"encoding/json"
	"testing"
)

// AN AUXILIARY RECORD CARRIES NO `failed` KEY, and its type is not a failure.
//
// The store promotes a top-level `failed: true` into the tag the turn list
// marks a whole turn failed by, and the live projection derives a turn's
// failure from the same key — so a record of spend that carried one would
// paint a turn red because its memory filter timed out. Failure travels as a
// COUNT, which no reader takes for the turn's outcome.
func TestAnAuxiliaryRecordCarriesNoFailureMark(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(filled(AuxiliarySpend{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if _, present := keys["failed"]; present {
		t.Fatal("auxiliary_spend carries a `failed` key, which marks the turn it " +
			"names failed in the turn list and the live view")
	}
	if Failed(AuxiliarySpend{}.EventType(), false, false) {
		t.Fatal("auxiliary_spend is in the failure set, which marks every turn it names failed")
	}
}

// EVERY PURPOSE AND STAGE THIS BUILD PUBLISHES IS ONE IT KNOWS, and an unknown
// one off the wire is a value rather than a match.
func TestAuxiliaryPurposesAndStagesAreClosedSets(t *testing.T) {
	t.Parallel()
	for _, p := range AuxPurposes() {
		if !p.Valid() {
			t.Errorf("purpose %q is published and not valid", p)
		}
	}
	for _, s := range AuxStages {
		if !s.Valid() {
			t.Errorf("stage %q is published and not valid", s)
		}
	}
	for _, unknown := range []AuxPurpose{"", "condense_", "condense_novel", "a_later_worker"} {
		if unknown.Valid() {
			t.Errorf("purpose %q reads as valid", unknown)
		}
	}
	for _, unknown := range []AuxStage{"", "a_later_stage"} {
		if unknown.Valid() {
			t.Errorf("stage %q reads as valid", unknown)
		}
	}
	if got := AuxCondense("thread"); got != "condense_thread" || !got.Valid() {
		t.Errorf("AuxCondense(thread) = %q, valid %v", got, got.Valid())
	}
}

// THE SUMMARY SAYS WHAT WAS SPENT, ON WHAT, IN HOW MANY CALLS, and a person's
// spend leads with the person rather than with nobody.
func TestAnAuxiliarySummaryNamesTheSpendAndWhoItWasFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rec  AuxiliarySpend
		want string
	}{{
		name: "a seat's",
		rec: AuxiliarySpend{RoleName: "Dev", Purpose: AuxMemoryFilter, Calls: 1,
			TotalTokens: 420, Model: "claude-haiku-4-5"},
		want: "Dev spent 420 tokens on memory filter (1 call, claude-haiku-4-5)",
	}, {
		name: "a person's, with a failed call",
		rec: AuxiliarySpend{ActorSeat: "alice", Purpose: AuxCondense("source"), Calls: 3,
			FailedCalls: 1, TotalTokens: 9000, ProviderKey: "cheap"},
		want: "alice spent 9000 tokens on condense source (3 calls, 1 failed, cheap)",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actor := tc.rec.Actor()
			if actor == "" {
				actor = tc.rec.Role()
			}
			if got := tc.rec.SummaryFor(actor); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}
