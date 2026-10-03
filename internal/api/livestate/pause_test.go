package livestate_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/iam"
)

// A PAUSED SEAT SAYS WHO PAUSED IT, WHEN AND WHY — and a resume clears it.
//
// The pause is what the seat-state vocabulary reads a paused seat's state
// from, so the overlay has to carry it on every push and in the snapshot: who
// paused it in the three halves iam.ActorFor names (the seat a person's
// credential is bound to, kind human, and the credential beside it), the
// instant, the reason and whether they also stopped the turn. And `paused` is
// ALWAYS on the row, null when there is none, or a resumed seat merged into
// a client's row would keep wearing its old pause. Keyed by the seat's agent
// id, as the pause record itself is.
func TestAPausedSeatCarriesWhoPausedIt(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(env("seat_paused", map[string]any{
		"agent_id": "id-cto", "role": "CTO", "agent_handle": "cto",
		"paused_by": "jane", "paused_by_kind": "human", "operator_id": "session:l-1",
		"reason": "looping", "stop_running": true,
		"paused_at": "2026-06-14T11:59:00Z",
	}, at("2026-06-14T11:59:00Z"), id("p1")))
	if _, moved := change.Agents["id-cto"]; !moved {
		t.Fatal("a pause did not push the seat")
	}
	got := overlayOf(t, s, "id-cto").Paused
	want := livestate.Paused{By: "jane", ByKind: iam.ActorHuman, OperatorID: "session:l-1",
		At: "2026-06-14T11:59:00Z", Reason: "looping", StopRunning: true}
	if got == nil || *got != want {
		t.Fatalf("paused = %+v, want %+v", got, want)
	}
	if st := overlayOf(t, s, "id-cto"); st.Activity != livestate.ActivityStopped ||
		st.StoppedReason == nil || *st.StoppedReason != livestate.StoppedPaused {
		t.Errorf("activity = %q (%v), want stopped/paused", st.Activity, st.StoppedReason)
	}
	if s.AgentOverlay("CTO") != nil {
		t.Error("the pause filed an entry under the seat's role name")
	}

	// An older pause arriving after the resume must not put it back.
	s.Apply(env("seat_resumed", map[string]any{"agent_id": "id-cto", "resumed_by": "omar"},
		at("2026-06-14T12:05:00Z"), id("r1")))
	if p := overlayOf(t, s, "id-cto").Paused; p != nil {
		t.Fatalf("paused = %+v after a resume, want none", p)
	}
	s.Apply(env("seat_paused", map[string]any{"agent_id": "id-cto", "paused_by": "old"},
		at("2026-06-14T12:01:00Z"), id("p0")))
	if p := overlayOf(t, s, "id-cto").Paused; p != nil {
		t.Errorf("a pause older than the resume put the seat back to paused: %+v", p)
	}

	rows := s.OverlayRows([]string{"id-cto"})
	raw, _ := json.Marshal(rows[0])
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if v, present := decoded["paused"]; !present || v != nil {
		t.Errorf("a resumed seat's row carries paused=%v (present %v), want an explicit null",
			v, present)
	}
}

// A PAUSE TAKEN BEFORE THIS PROCESS STARTED IS STILL ON THE SEAT: it is seeded
// from the coordination record, which no event this process will hear
// mentions. And the stream wins over the seed when it already spoke.
func TestAPauseIsSeededFromTheRecord(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("seat_resumed", map[string]any{"agent_id": "id-ceo"}, at("2026-06-14T12:00:00Z")))
	change := s.SeedPauses([]livestate.SeedPause{
		{AgentID: "id-cto", Paused: livestate.Paused{By: "jane", ByKind: iam.ActorHuman,
			At: "2026-06-01T09:00:00Z"}},
		{AgentID: "id-ceo", Paused: livestate.Paused{By: "stale", At: "2026-06-01T09:00:00Z"}},
	})
	if _, moved := change.Agents["id-cto"]; !moved {
		t.Error("seeding a pause pushed nothing")
	}
	if p := overlayOf(t, s, "id-cto").Paused; p == nil || p.By != "jane" || p.ByKind != iam.ActorHuman {
		t.Errorf("CTO paused = %+v, want the seeded pause", p)
	}
	if p := overlayOf(t, s, "id-ceo").Paused; p != nil {
		t.Errorf("the seed overwrote a resume the stream had already delivered: %+v", p)
	}
}
