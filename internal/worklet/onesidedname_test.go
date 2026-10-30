package worklet_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A ONE-SIDED EDGE'S REPAIR IS NAMED IN ONE STEP OF ITS OPERATION ID.
//
// The grammar is `<uuidv7>[.<name>][.<step>…]`, so a dot begins a step: the
// repair's name joined the dependent and the blocker with one, and its id read
// as the name `onesided-<dependent>` with a step `<blocker>` — an id the
// engine's own caller rule calls malformed.
//
// Mutation: join the pair with a dot again, and the name holds one.
func TestAOneSidedRepairIsNamedInOneStep(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"dep-a", "blk-a"} {
		filedTask(t, r, id)
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep-a", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relate: &worklet.RelationIntent{Add: []worklet.Relation{
				{Kind: worklet.RelationWaitingOn, Other: "blk-a"},
			}},
		}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()
	before := r.logEnd(t)
	worker, err := maintenance.New(maintenance.Options{
		Jobs: worklet.Jobs(worklet.DutyDeps{
			DB: r.db, Writer: r.writer, NodeID: "node-a",
			Logger: slog.New(slog.DiscardHandler),
		}),
		Now: func() time.Time { return wednesday.AddDate(1, 0, 0) },
	})
	if err != nil {
		t.Fatalf("build the tracker's maintenance worker: %v", err)
	}
	if swept, err := worker.Tick(t.Context()); err != nil || swept["tracker_one_sided"] != 1 {
		t.Fatalf("Tick = (%v, %v), want the edge repaired", swept, err)
	}
	repairs := 0
	for seq := before + 1; seq <= r.logEnd(t); seq++ {
		op := r.recordAt(t, seq).OpID
		if len(op) <= 37 || !strings.HasPrefix(op[37:], "onesided-") {
			continue
		}
		repairs++
		if name := op[37:]; strings.Contains(name, ".") {
			t.Errorf("the repair's operation %q is named %q, which the grammar "+
				"reads as a name and a step", op, name)
		}
		if err := statelog.CheckCallerOpID(op); err != nil {
			t.Errorf("the repair's operation %q is not one the engine's own rule "+
				"admits: %v", op, err)
		}
	}
	if repairs != 1 {
		t.Errorf("found %d repair records, want the one", repairs)
	}
}
