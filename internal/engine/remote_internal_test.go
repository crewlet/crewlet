package engine

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRIM AND THE GATE COUNT ONLY THE NODES THAT HOLD DATA. A node without it
// applies no log and publishes no position, so a trim that counted it at zero
// would never advance again — and a presence row whose roles this build
// cannot read (a role a newer build added) reads as every role, data
// included, so it still counts: leaving it out would trim past its rows.
func TestTheTrimAndTheGateCountOnlyDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	for id, meta := range map[string]map[string]any{
		"data-a":  {"roles": []string{"data", "seats"}},
		"agent-1": {"roles": []string{"seats"}},
		"newer":   {"roles": []string{"data", "seats", "a-future-role"}},
	} {
		if _, _, err := backend.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	presences, err := livePresences(ctx, backend)
	if err != nil {
		t.Fatalf("livePresences: %v", err)
	}
	var got []string
	for _, p := range presences {
		got = append(got, p.NodeID)
	}
	slices.Sort(got)
	if want := []string{"data-a", "newer"}; !slices.Equal(got, want) {
		t.Fatalf("counted %v, want %v", got, want)
	}
}

// A DATA NODE IN A MAINTENANCE MODE TAKES NO WRITE FOR ANYBODY. The mode is
// the evidence a capacity operation is established from — no publisher on
// this node — and a write it took on another node's behalf, or its own seats',
// would be the publish the mode rules out. Its reads are still served.
//
// THE CORE'S HALVES AS MUCH AS THE NATIVE ONES: the core runs its logs in every
// mode, so its readers are served there, and its writers were handed out beside
// them — a peer's artifacts write asked of a node in a maintenance mode was
// published by it. Each writer is held to the mode on its own, so a node that
// withheld one and offered another is named.
//
// Mutation: hand the artifacts writer out whatever the mode and the maintenance
// rows go red naming it.
func TestAMaintenanceModeDataNodeServesNoWriter(t *testing.T) {
	t.Parallel()
	for mode, wantWriters := range map[statelog.MaintenanceMode]bool{
		statelog.ModeNormal:      true,
		statelog.ModeMaintenance: false,
		statelog.ModeSeal:        false,
	} {
		e := &Engine{mode: mode, backends: &Backends{}}
		n := &native{
			writer: &worklet.Writer{}, pages: &doclet.Store{},
			trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{},
		}
		e.native.Store(n)
		c := &core{
			artifactsReader: &artifactsdomain.Reader{}, artifactsWriter: &artifactsdomain.Writer{},
			artifactDataReader: &artifactdata.Reader{}, artifactDataWriter: &artifactdata.Writer{},
			executorReader: &executordomain.Reader{}, executorWriter: &executordomain.Writer{},
		}
		b := e.estateBackend(c, n)
		for name, offered := range map[string]bool{
			"tracker":       b.Writer != nil,
			"pages":         b.PageWriter != nil,
			"artifacts":     b.Artifacts.Writer != nil,
			"artifact data": b.ArtifactData.Writer != nil,
			"executor":      b.Executor.Writer != nil,
		} {
			if offered != wantWriters {
				t.Errorf("%s: the %s writer offered = %v, want %v", mode, name, offered, wantWriters)
			}
		}
		for name, served := range map[string]bool{
			"tracker":       b.Tracker != nil,
			"pages":         b.Pages != nil,
			"artifacts":     b.Artifacts.Reader != nil,
			"artifact data": b.ArtifactData.Reader != nil,
			"executor":      b.Executor.Reader != nil,
		} {
			if !served {
				t.Errorf("%s: the %s reader is missing, and a mode that publishes "+
					"nothing still answers reads", mode, name)
			}
		}
	}
}

// A TOOL'S WRITE CROSSES TO ANOTHER NODE WITH ITS WHOLE PROVENANCE.
//
// It once carried the turn and the chain alone, written before a person's
// credential, a task's chat origin and the turn's set of written items were
// part of a provenance — and every write a router took to a peer lost the
// three, with nothing to say so: an operator's gesture lost the credential it
// was made under, a task filed from a thread forgot the thread, and a turn
// that wrote one item was charged to none. So the
// fixture sets EVERY field a provenance has, and a field added to it later is
// one this test makes its author carry across or explain.
func TestAToolsActorCrossesWithItsWholeProvenance(t *testing.T) {
	t.Parallel()
	written := &turnctx.Written{}
	actor := builtin.Actor{
		Handle: "ops-token", Kind: worklet.AuthorOperator,
		OperatorID: "tok-1", TurnID: "run-1", Chain: []string{"cto", "swe"},
		Thread:  &worklet.Origin{Surface: "slack", Conversation: "C0FOUNDER/1700000000.000100"},
		Written: written,
	}
	got := remoteActor(actor)
	if got.Handle != actor.Handle || got.Kind != actor.Kind || got.Records {
		t.Fatalf("the actor crossed as %+v, want %s (%s) and no request to record — the "+
			"router asks for that itself", got, actor.Handle, actor.Kind)
	}
	provenance := reflect.ValueOf(got.Provenance)
	for i := range provenance.NumField() {
		if provenance.Field(i).IsZero() {
			t.Errorf("the provenance crossed without its %s", provenance.Type().Field(i).Name)
		}
	}
	want := worklet.Provenance{OperatorID: "tok-1", TurnID: "run-1",
		Chain: []string{"cto", "swe"}, Origin: actor.Thread, Written: written}
	if !reflect.DeepEqual(got.Provenance, want) {
		t.Errorf("the provenance crossed as %+v, want %+v", got.Provenance, want)
	}

	// A TURN WITH NO SET CROSSES WITH NONE: a nil set in the interface would
	// be a log reporting into nothing, and the holder would collect every
	// item it wrote only for the asker to drop them.
	actor.Written = nil
	if got := remoteActor(actor); got.Provenance.Written != nil {
		t.Errorf("a turn holding no set crossed with %#v", got.Provenance.Written)
	}
}
