package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/iam"
)

// A PAUSE AND A RESUME, THROUGH THE ROUTE A PERSON PRESSES THEM ON.
//
// `pause_seat` and `resume_seat` reach a node three ways — the dashboard's act
// route, `crewlet seats`, and an operator's assistant over MCP — and all three
// are one dispatch. This drives the act route as `crewlet seats` does, under
// the deployment's Tier A token with an operation key in `Idempotency-Key`,
// and reads back what each half of the engine recorded:
//
//   - the answer names the author as [iam.ActorFor] does — the token's own
//     login, of the operator kind, through itself — because nobody is bound
//     to it;
//   - the coordination record is keyed by the seat's IDENTITY, its agent id,
//     never its handle, so a rename keeps the pause and a hire on a freed
//     handle inherits nothing;
//   - the live projection puts the seat in `stopped`, for `paused`, and a
//     resume takes it out again.
//
// Mutations: key the record by handle and the identity read finds nothing;
// record the author as the bound seat's name and the receipt names the wrong
// party; drop the projection's pause fold and the seat is never drawn stopped.
func TestASeatIsPausedAndResumedThroughTheActRoute(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	ceo := seatAgentID(t, n, "ceo")

	conn := n.dial(t)
	frames := &capture{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go read(ctx, conn, frames)
	waitFor(t, "the snapshot", func() bool {
		return slices.Contains(frames.kinds(t), "snapshot")
	})

	act := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		status, raw := bearer(t, n, http.MethodPost, "/operator/act/"+tool,
			map[string]any{"args": args},
			map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
		var answer struct {
			Outcome string         `json:"outcome"`
			Receipt map[string]any `json:"receipt"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
			t.Fatalf("POST /operator/act/%s answered %d: %s", tool, status, raw)
		}
		if answer.Outcome != "applied" {
			t.Fatalf("%s came to %q, want applied: %s", tool, answer.Outcome, raw)
		}
		return answer.Receipt
	}

	// --- the pause ---------------------------------------------------- //
	login := iam.TokenLogin("e2e")
	paused := act("pause_seat", map[string]any{"handle": "ceo", "reason": "the e2e pause"})
	if paused["changed"] != true || paused["paused_by"] != login ||
		paused["paused_by_kind"] != string(iam.ActorOperator) ||
		paused["operator_id"] != login {
		t.Errorf("the pause's receipt is %v, want a change by %s, of the operator "+
			"kind, through itself", paused, login)
	}
	record, found, err := n.engine.Backends().Fleet.SeatPause(t.Context(), uuid.MustParse(ceo))
	if err != nil || !found {
		t.Fatalf("no pause recorded under the seat's agent id %s (found %v, err %v)",
			ceo, found, err)
	}
	if record.By != login || record.ByKind != iam.ActorOperator ||
		record.Reason != "the e2e pause" {
		t.Errorf("the pause record is %+v, want %s (operator) with the reason given",
			record, login)
	}
	waitFor(t, "the seat to be drawn as paused", func() bool {
		return seatShown(frames, t, ceo, "stopped", "paused")
	})

	// --- and the resume ------------------------------------------------ //
	if resumed := act("resume_seat", map[string]any{"handle": "ceo"}); resumed["changed"] != true {
		t.Errorf("the resume's receipt is %v, want a change", resumed)
	}
	if _, found, err := n.engine.Backends().Fleet.SeatPause(t.Context(), uuid.MustParse(ceo)); err != nil || found {
		t.Errorf("the pause record outlived the resume (found %v, err %v)", found, err)
	}
	waitFor(t, "the seat to be drawn as no longer paused", func() bool {
		return seatShown(frames, t, ceo, "idle", "")
	})
}

// seatShown reports whether the newest `agents` push about a seat put it in an
// activity, stopped for a reason where one is named.
func seatShown(frames *capture, t *testing.T, agentID, activity, reason string) bool {
	t.Helper()
	var last map[string]any
	for _, row := range frames.agentRows(t) {
		if row["agent_id"] == agentID && row["activity"] != nil {
			last = row
		}
	}
	if last == nil || last["activity"] != activity {
		return false
	}
	if reason == "" {
		return true
	}
	got, _ := last["stopped_reason"].(string)
	return got == reason
}
