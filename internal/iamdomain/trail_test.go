package iamdomain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A GESTURE MADE THROUGH A TOKEN IS RECORDED AS ONE ON THE TRAIL.
//
// `GET /iam/audit` reads iam_history, and the trail named its actor alone. A
// machine token acts as its owner, so its gesture's actor is the owner — and
// what somebody's token did to the directory read there as done by them. The
// record names the credential now, at the version that defines it, and the row
// carries it beside the actor.
//
// TWO THINGS STAY WHERE THEY WERE, and each is a row below. A GATE is pinned at
// version 1 for ever, so a removal through the same token names its actor alone
// — the event announcing it carries the credential. And a writer that acted
// through nothing — the node's own, which writes every sign-in and every duty —
// writes at the base, so a rolling upgrade defers none of those on an older
// node. Mutation: drop the column from the apply, the field from the record, or
// write the version at the base, and a row goes red.
func TestAGestureMadeThroughATokenIsRecordedAsOneOnTheTrail(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.New().String()
	enrolSarah(t, rig, id)

	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	owner := principalNamed("ana.admin", iam.KindPerson, iam.AllGrants)
	owner.Via = via
	party := rig.writer.As(owner)

	for _, step := range []struct {
		reason  string
		gesture func() error
		version int
	}{
		{"left the building", func() error {
			_, err := party.SetStage(t.Context(), id, iam.StageSuspended,
				"op-suspend", "left the building")
			return err
		}, iamdomain.OperatorRecordVersion},
		{"came back", func() error {
			_, err := rig.writer.SetStage(t.Context(), id, iam.StageActive,
				"op-restore", "came back")
			return err
		}, iamdomain.BaseRecordVersion},
		{"left for good", func() error {
			_, err := party.Remove(t.Context(), id, "op-remove", "left for good")
			return err
		}, iamdomain.GateRecordVersion},
	} {
		if err := step.gesture(); err != nil {
			t.Fatalf("%s: %v", step.reason, err)
		}
		rig.drain()
		if env := rig.lastEnvelope(); env.V != step.version {
			t.Errorf("%q was written at version %d, want %d", step.reason,
				env.V, step.version)
		}
	}

	page, err := reader.History(t.Context(), iamdomain.HistoryQuery{Person: id})
	if err != nil {
		t.Fatalf("read the trail: %v", err)
	}
	rows := map[string]iamdomain.HistoryRow{}
	for _, row := range page.Entries {
		rows[row.Reason] = row
	}
	for reason, want := range map[string]string{
		"left the building": via,
		"came back":         "",
		"left for good":     "",
	} {
		row, ok := rows[reason]
		if !ok {
			t.Errorf("the trail holds no row for %q: %+v", reason, page.Entries)
			continue
		}
		if row.Actor != "ana.admin" || row.OperatorID != want {
			t.Errorf("%q is on the trail as %q through %q, want ana.admin "+
				"through %q", reason, row.Actor, row.OperatorID, want)
		}
	}
}
