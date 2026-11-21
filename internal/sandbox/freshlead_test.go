package sandbox

import (
	"context"
	"slices"
	"testing"
)

// A SESSION THE RUN STARTS OF ITS OWN KEEPS ITS TASK (protocol.go). A fresh
// brief opens a session with the turn's task, and a later continuation of that
// session that may be a summary may lack it: every launch whose run may start a
// session on its fresh brief keeps that brief's task beside the run's record.

// freshTask is the lead a fresh brief opens with: the turn's task, its brief
// having rendered closed rounds through round 2.
func freshTask() Lead {
	return Lead{Kind: LeadKindFresh, Round: 3, Task: &LeadTask{Rounds: 2}}
}

// keptLeads is t1's leads beside its row and the job's marks of them.
func keptLeads(t *testing.T, store *CoordStore) ([]KeptLead, PendingRun) {
	t.Helper()
	ctx := t.Context()
	row, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.Leads(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	return read.Kept, row
}

// openedFresh is the one fresh lead among kept, opened, and whether there was
// exactly one, sealed for job.
func openedFresh(t *testing.T, kept []KeptLead, job string) (KeptLead, bool) {
	t.Helper()
	var fresh []KeptLead
	for _, lead := range kept {
		if lead.Kind == LeadKindFresh {
			fresh = append(fresh, lead)
		}
	}
	if len(fresh) != 1 {
		return KeptLead{}, false
	}
	opened, err := rigLeads().Open("t1", fresh[0])
	if err != nil || opened.Job != job || opened.Task == nil || *opened.Task != (LeadTask{Rounds: 2}) {
		t.Errorf("the fresh lead opens as %+v, %v; want the task through round 2, sealed for job %s", opened,
			err, job)
	}
	return fresh[0], true
}

// A FRESH LAUNCH KEEPS ITS TASK LEAD. A turn's first launch, whose request
// names no continuation, and a relaunch handed its fresh brief, each keep the
// task that brief opened with, sealed for the job and marked told by it: the
// brief the run read told it.
//
// Mutation: keep no lead of a first launch's own, or refuse to seal a fresh
// lead's shape, and a case goes red.
func TestAFreshLaunchKeepsItsTaskLead(t *testing.T) {
	t.Parallel()
	t.Run("a turn's first launch", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		req := launchReq("t1")
		req.FreshOwn = []Lead{freshTask()}
		if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
			t.Fatalf("a first launch: %v", err)
		}
		kept, row := keptLeads(t, rig.pending)
		fresh, ok := openedFresh(t, kept, row.LaunchID)
		if !ok || len(kept) != 1 || !slices.Equal(row.Launch.LeadMarks, []int{fresh.Appended}) {
			t.Errorf("a first launch keeps %+v and marks %v; want its fresh brief's task, marked", kept,
				row.Launch.LeadMarks)
		}
	})
	t.Run("a relaunch handed its fresh brief", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		askAndAnswer(t, rig, true, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
			req := continuing(asked)
			req.Continue.Told = BriefTold{Own: []Lead{answerLead("carried on in place")}}
			req.Continue.Fresh = BriefTold{Own: []Lead{freshTask(), answerLead("carried on afresh")}}
			if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
				t.Errorf("relaunch: %v", err)
			}
		})
		kept, row := keptLeads(t, rig.pending)
		fresh, ok := openedFresh(t, kept, row.LaunchID)
		if !ok || len(kept) != 2 || row.Launch.Brief != BriefFresh ||
			!slices.Contains(row.Launch.LeadMarks, fresh.Appended) || len(row.Launch.LeadMarks) != 2 {
			t.Errorf("the relaunch records brief %q, keeps %+v and marks %v; want the fresh brief handed, its "+
				"task and its answer kept and marked", row.Launch.Brief, kept, row.Launch.LeadMarks)
		}
	})
}

// A FAILED RESTORE KEEPS THE FRESH BRIEF'S TASK. A launch that hands its run a
// brief continuing a session hands its box the fresh brief beside it, which the
// box's script starts a session of its own on where it cannot restore the one
// continued: the task that brief opened with is kept beside the handed brief's
// leads, so that session's task is kept too. Unmarked, since the brief the run
// read did not tell it; and a lead both briefs open with is kept once, as the
// handed brief's.
//
// Mutation: keep only the handed brief's leads, or mark the fresh brief's
// among them, and a case goes red.
func TestAFailedRestoreKeepsTheFreshBriefsTask(t *testing.T) {
	t.Parallel()
	// check reads t1's leads: the handed brief's answer, kept and marked
	// alone, and the fresh brief's task, kept beside it unmarked.
	check := func(t *testing.T, store *CoordStore, handed string) {
		t.Helper()
		kept, row := keptLeads(t, store)
		fresh, ok := openedFresh(t, kept, row.LaunchID)
		var answer KeptLead
		for _, lead := range kept {
			if lead.Kind == LeadKindAnswer {
				answer = lead
			}
		}
		opened, err := rigLeads().Open("t1", answer)
		switch {
		case !ok || len(kept) != 2:
			t.Errorf("the job keeps %+v; want the handed brief's answer and the fresh brief's task", kept)
		case err != nil || opened.Answer == nil || opened.Answer.Words != handed:
			t.Errorf("the answer kept is %+v, %v; want the handed brief's, %q", opened, err, handed)
		case !slices.Equal(row.Launch.LeadMarks, []int{answer.Appended}) ||
			slices.Contains(row.Launch.LeadMarks, fresh.Appended):
			t.Errorf("the job marks %v; want the handed brief's answer alone, %d", row.Launch.LeadMarks,
				answer.Appended)
		}
	}
	t.Run("the asking job's own copy, in its box", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		askAndAnswer(t, rig, false, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
			req := continuing(asked)
			req.Continue.Told = BriefTold{Own: []Lead{answerLead("carried on in place")}}
			req.Continue.Fresh = BriefTold{Own: []Lead{freshTask(), answerLead("carried on afresh")}}
			if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
				t.Errorf("relaunch: %v", err)
			}
		})
		check(t, rig.pending, "carried on in place")
	})
	for name, send := range map[string]bool{"a round carried on": false, "a send-back": true} {
		t.Run("the turn's kept capture, "+name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				req := continuingKept(kept)
				req.Session.Told = BriefTold{Own: []Lead{answerLead("carried on from the capture")}}
				if send {
					req.FreshOwn = []Lead{freshTask(), answerLead("carried on afresh")}
				} else {
					req.Continue = &Continuation{Fresh: BriefTold{Own: []Lead{freshTask(),
						answerLead("carried on afresh")}}}
				}
				if _, err := launchRestoring(t, rig, restore, req); err != nil {
					t.Fatalf("relaunch: %v", err)
				}
				check(t, rig.pending, "carried on from the capture")
			})
		})
	}
}
