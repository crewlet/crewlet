package livestate_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// A PAUSED SEAT SAYS WHO PAUSED IT, WHEN AND WHY — and a resume clears it.
//
// The pause is what the seat-state vocabulary reads a paused seat's state
// from, so the overlay has to carry it on every push and in the snapshot: the
// person (the seat their token is bound to, before the token itself), the
// instant, the reason and whether they also stopped the turn. And `paused` is
// ALWAYS on the row, null when there is none, or a resumed seat merged into
// a client's row would keep wearing its old pause.
func TestAPausedSeatCarriesWhoPausedIt(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(env("seat_paused", map[string]any{
		"role": "CTO", "agent_handle": "cto", "paused_by": "jane-token",
		"paused_by_seat": "jane", "reason": "looping", "stop_running": true,
		"paused_at": "2026-06-14T11:59:00Z",
	}, at("2026-06-14T11:59:00Z"), id("p1")))
	if _, moved := change.Agents["CTO"]; !moved {
		t.Fatal("a pause did not push the seat")
	}
	got := overlayOf(t, s, "CTO").Paused
	want := livestate.Paused{By: "jane", At: "2026-06-14T11:59:00Z", Reason: "looping", StopRunning: true}
	if got == nil || *got != want {
		t.Fatalf("paused = %+v, want %+v", got, want)
	}

	// An older pause arriving after the resume must not put it back.
	s.Apply(env("seat_resumed", map[string]any{"role": "CTO", "resumed_by": "omar-token"},
		at("2026-06-14T12:05:00Z"), id("r1")))
	if p := overlayOf(t, s, "CTO").Paused; p != nil {
		t.Fatalf("paused = %+v after a resume, want none", p)
	}
	s.Apply(env("seat_paused", map[string]any{"role": "CTO", "paused_by": "old-token"},
		at("2026-06-14T12:01:00Z"), id("p0")))
	if p := overlayOf(t, s, "CTO").Paused; p != nil {
		t.Errorf("a pause older than the resume put the seat back to paused: %+v", p)
	}

	rows := s.OverlayRows([]string{"CTO"})
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
	s.Apply(env("seat_resumed", map[string]any{"role": "CEO"}, at("2026-06-14T12:00:00Z")))
	change := s.SeedPauses([]livestate.SeedPause{
		{Role: "CTO", Paused: livestate.Paused{By: "jane", At: "2026-06-01T09:00:00Z"}},
		{Role: "CEO", Paused: livestate.Paused{By: "stale", At: "2026-06-01T09:00:00Z"}},
	})
	if _, moved := change.Agents["CTO"]; !moved {
		t.Error("seeding a pause pushed nothing")
	}
	if p := overlayOf(t, s, "CTO").Paused; p == nil || p.By != "jane" {
		t.Errorf("CTO paused = %+v, want the seeded pause", p)
	}
	if p := overlayOf(t, s, "CEO").Paused; p != nil {
		t.Errorf("the seed overwrote a resume the stream had already delivered: %+v", p)
	}
}
