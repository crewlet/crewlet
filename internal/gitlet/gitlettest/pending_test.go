package gitlettest_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// pendingOnce is a fleet whose committer answers ONE push pending, as it
// answers one whose record a loaded runner's applier has not applied within
// the committer's own wait: durable at its position, which is the record's
// own, and unresolved where it was proposed. The answer carries no commands,
// so a setup that took a pending answer's commands for the outcome fails
// here: only the push's row says what became of it.
type pendingOnce struct {
	gitlettest.Fleet
	forced *atomic.Bool
}

func (f pendingOnce) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	res, err := f.Fleet.Propose(ctx, p)
	if err != nil || res.Outcome != statelog.OutcomeApplied || !f.forced.CompareAndSwap(false, true) {
		return res, err
	}
	res.Outcome, res.Answer.Commands, res.Answer.Collapsed = statelog.OutcomePending, nil, false
	return res, nil
}

// A BULK PACK ANSWERED PENDING IS RESOLVED AT ITS POSITION, not failed:
// statelog answers pending a record durable there, and documents the answer
// as never a failure. C5's setup read one as a failure and went red whenever
// a loaded runner's applier lagged a bulk pack past the committer's wait
// (#799). Here one pack is answered pending, and the setup must read what
// became of it and go on with every pack laid.
func TestABulkPackAnsweredPendingIsResolvedAtItsPosition(t *testing.T) {
	var forced atomic.Bool
	c := gitlettest.NewCase(t, func(t *testing.T, s gitlettest.Shape) gitlettest.Fleet {
		return pendingOnce{Fleet: gitlettest.Solo(gitlettest.SoloOptions{}, nil)(t, s), forced: &forced}
	}, gitlettest.Shape{}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	tip := gitlettest.PushEach(c, p, work, 1, 1<<10)
	const packs = 3
	gitlettest.BulkPacks(c, p, c.Rows().Repo.ID, tip, packs)
	if !forced.Load() {
		t.Fatal("no bulk pack was answered pending, so the case held the setup to nothing")
	}
	refs := c.Rows().Refs
	for i := range packs {
		if ref := fmt.Sprintf("refs/heads/bulk/%04d", i); refs[ref] != tip {
			t.Errorf("%s is at %q, want the tip %s every bulk pack lays", ref, refs[ref], tip)
		}
	}
}

// AN OPERATOR'S GESTURE ANSWERED PENDING IS WAITED FOR, not failed, for the
// same reason: the eviction is durable at its position, and the gesture
// answers once the data node holds it, at the position it landed.
func TestAGestureAnsweredPendingIsWaitedForNotFailed(t *testing.T) {
	f := gitlettest.SoloNode(t, gitlettest.SoloOptions{})
	var landed statelog.Position
	evictPending := func(s *gitlet.Store, ctx context.Context, a gitlet.Actor, opID, node string) (statelog.Result,
		error) {

		res, err := s.EvictNode(ctx, a, opID, node)
		if err == nil && res.Outcome == statelog.OutcomeApplied {
			landed, res.Outcome = res.Position, statelog.OutcomePending
		}
		return res, err
	}
	at := f.Data().Gesture(t, "gitlettest-departed", evictPending, "evict")
	if landed.IsZero() {
		t.Fatal("the eviction was never answered pending, so the case held the gesture to nothing")
	}
	if at != landed {
		t.Errorf("the gesture answered %s, want %s, where its record landed", at, landed)
	}
}
