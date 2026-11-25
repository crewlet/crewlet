package sandbox

import (
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// freshTeller is a caller's [LaunchRequest.FreshFor] that tells each verdict
// in its own words and keeps every verdict it was asked to tell under.
type freshTeller struct {
	mu   sync.Mutex
	told []*executorkeep.NotContinued
}

func (f *freshTeller) tell(verdict *executorkeep.NotContinued) FreshBrief {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.told = append(f.told, verdict)
	if verdict == nil {
		return FreshBrief{Text: "the fresh brief, nothing lost", Handover: HandoverNone}
	}
	return FreshBrief{Text: "the fresh brief: " + verdict.Sentence(), Handover: HandoverHanded}
}

func (f *freshTeller) asked() []*executorkeep.NotContinued {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.told
}

// THE FRESH BRIEF A RUN MAY READ NAMES ITS OWN LOSS. Beside a session the launch
// continues, the fresh brief its box falls back to is told under the one loss
// that fallback stamps, restore_failed, and recorded with what it hands; a
// session refused before the run starts hands the run the fresh brief told
// under the launch's own verdict, never the one built before. Each brief's
// hand-over is on the job's record, the one its run reads counted at
// collection.
//
// Mutation: hand the fresh brief as built beside a continuation, tell a refused
// launch's under the caller's verdict, or record no hand-over, and a case goes
// red.
func TestTheFreshBriefARunMayReadNamesItsOwnLoss(t *testing.T) {
	t.Parallel()
	t.Run("beside a continuation", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			teller := &freshTeller{}
			req := continuingKept(kept)
			req.FreshFor, req.Session.Handover = teller.tell, HandoverUnread
			if _, err := launchRestoring(t, rig, restore, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			started := rig.runner.Started()
			last := started[len(started)-1]
			told := teller.asked()
			if len(told) != 1 || told[0] == nil || told[0].Loss != executorkeep.LossRestoreFailed ||
				told[0].Instead != executorkeep.InsteadFresh || told[0].LostTurn != "" {
				t.Fatalf("the fresh brief was told under %+v, want once under restore_failed, of the turn's own", told)
			}
			if last.FreshBrief != "the fresh brief: "+told[0].Sentence() || last.Brief == last.FreshBrief {
				t.Fatalf("the job's fresh brief is %q beside %q, want the one told under restore_failed",
					last.FreshBrief, last.Brief)
			}
			if rec := rig.get("t1").Launch; rec.Handover != HandoverUnread || rec.FreshHandover != HandoverHanded {
				t.Errorf("the record says the briefs hand %q and %q, want the continuation's and the fresh one's",
					rec.Handover, rec.FreshHandover)
			}
		})
	})
	t.Run("a session refused before the run", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			restore.unreadable = true
			teller := &freshTeller{}
			req := continuingKept(kept)
			req.FreshFor = teller.tell
			req.NotContinued = executorkeep.Lost(executorkeep.LossCaptureLate, executorkeep.InsteadOlder, "")
			if _, err := launchRestoring(t, rig, restore, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			started := rig.runner.Started()
			last := started[len(started)-1]
			told := teller.asked()
			if len(told) != 1 || told[0] == nil || told[0].Loss != executorkeep.LossRestoreFailed {
				t.Fatalf("the fresh brief was told under %+v, want the launch's own restore_failed", told)
			}
			if last.Resume != "" || last.Brief != "the fresh brief: "+told[0].Sentence() {
				t.Fatalf("the job resumes %q on %q, want a session of its own on the brief told under its loss",
					last.Resume, last.Brief)
			}
			if rec := rig.get("t1").Launch; rec.Handover != HandoverHanded || rec.NotContinued == nil ||
				rec.NotContinued.Loss != executorkeep.LossRestoreFailed {
				t.Errorf("the record says %+v handing %q, want restore_failed, its brief's hand-over",
					rec.NotContinued, rec.Handover)
			}
		})
	})
}
