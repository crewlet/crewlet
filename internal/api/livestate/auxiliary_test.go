package livestate_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events/types"
)

// auxiliaryEnv is an auxiliary_spend envelope carrying the payload as the wire
// does — the record's own keys, marshalled — so a test of what the projection
// does with it is a test of the record the engine publishes.
func auxiliaryEnv(t *testing.T, rec types.AuxiliarySpend, opts ...func(*livestate.Envelope)) *livestate.Envelope {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("remap: %v", err)
	}
	return env(rec.EventType(), payload, opts...)
}

// AN AUXILIARY RECORD MOVES NO SEAT.
//
// It is the one spend record a seat's reflection files AFTER its turn ended,
// and the phase record it replaced the plan for would have reopened that turn
// — the projection takes any phase event newer than a turn's end as the turn
// going on — and cleared a held provider stop, on every node. This record is
// no state event: arriving after the end, with a
// failed call on it, it leaves the turn ended, the hold held and the last
// turn as it was.
func TestAnAuxiliaryRecordMovesNoSeat(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("llm_unavailable", turnPayload("tn-1", map[string]any{
		"kind": "provider_down", "detail": "429 forever",
	}), at("2026-06-14T12:00:08Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:09Z")))
	before := overlayOf(t, s, "Lead")
	if before.Turn != nil || before.Activity != livestate.ActivityStopped {
		t.Fatalf("set-up: overlay = %+v, want an ended turn on a stopped seat", before)
	}

	change := s.Apply(auxiliaryEnv(t, types.AuxiliarySpend{
		Agent: "a-1", AgentHandle: "lead", RoleName: "Lead",
		Stage: types.AuxStageReflection, Purpose: types.AuxPersistDecider,
		TurnID: "tn-1", Model: "claude-haiku-4-5", ProviderKey: "cheap",
		Day: "2026-06-14", Calls: 2, FailedCalls: 1, InputTokens: 900, OutputTokens: 60,
		TotalTokens: 960, StartedAt: time.Date(2026, 6, 14, 12, 0, 10, 0, time.UTC),
		EndedAt: time.Date(2026, 6, 14, 12, 0, 14, 0, time.UTC), DurationMS: 3000,
	}, at("2026-06-14T12:00:14Z"), id("aux-1")))

	if _, moved := change.Agents["Lead"]; moved {
		t.Error("an auxiliary record moved its seat, so a screen is pushed a change nothing made")
	}
	after := overlayOf(t, s, "Lead")
	if after.Turn != nil {
		t.Errorf("turn = %+v: an auxiliary record after the end reopened the turn", after.Turn)
	}
	if after.Activity != livestate.ActivityStopped || after.StoppedReason == nil ||
		*after.StoppedReason != livestate.StoppedProvider {
		t.Errorf("activity = %q (%v): an auxiliary record cleared the provider hold",
			after.Activity, after.StoppedReason)
	}
	if !reflect.DeepEqual(before.LastTurn, after.LastTurn) {
		t.Errorf("last turn = %+v, was %+v: a failed auxiliary call rewrote the turn's outcome",
			after.LastTurn, before.LastTurn)
	}
	if !reflect.DeepEqual(before.LastError, after.LastError) {
		t.Errorf("last error = %+v, was %+v", after.LastError, before.LastError)
	}
}

// AN AUXILIARY RECORD IS NOT A LINE OF THE FEED. It is stored, and the
// activity feed — the whole company's last few hundred events — does not carry
// it, so a burst of them cannot push the turns a reader is watching out.
func TestAnAuxiliaryRecordIsNotALineOfTheFeed(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(auxiliaryEnv(t, types.AuxiliarySpend{
		Agent: "a-1", RoleName: "Lead", Stage: types.AuxStageTurn,
		Purpose: types.AuxMemoryFilter, TurnID: "tn-1", Calls: 1, TotalTokens: 100,
	}, id("aux-1")))
	if change.Events {
		t.Error("an auxiliary record was pushed as an `event` frame")
	}
	for _, row := range s.RecentEvents(livestate.EventFeedLimit) {
		if row.ID == "aux-1" {
			t.Fatal("an auxiliary record took a row of the activity feed")
		}
	}
	// And a placed type beside it still is one, which is what makes the
	// case above a test of the class rather than of a feed that records
	// nothing.
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), id("done-1")))
	if rows := s.RecentEvents(livestate.EventFeedLimit); len(rows) != 1 || rows[0].ID != "done-1" {
		t.Errorf("feed = %+v, want the turn's completion alone", rows)
	}
}
