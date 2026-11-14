package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/secrets"
)

// WHAT A RUN KEEPS OF WHAT CARRIED ITS SESSIONS ON (leads.go, leadseal.go):
// each lead sealed once by the job whose brief first told it, taken into the
// run's leads part beside its row once, numbered as taken and kept as sealed
// through every relaunch, marked by the record of each job whose brief told
// it, and held to the part's share.

// rigLeads is the seal a rig's coordinator seals the leads its launches' briefs
// open with under ([CoordinatorOptions.Leads]): the rigs' one fleet keyring, as
// a fleet shares one.
var rigLeads = sync.OnceValue(func() *LeadSeal {
	seal, err := NewLeadSeal(rigAnswerRing)
	if err != nil {
		panic(err)
	}
	return seal
})

// answerLead is an answer lead of a run that asked which branch, answered words.
func answerLead(words string) Lead {
	return Lead{Kind: LeadKindAnswer, Round: 1, Answer: &LeadAnswer{Question: "which branch?", Words: words,
		By: "founder"}}
}

// sealedFor is lead sealed by seal for job job, the seq'th launch of turn t1.
func sealedFor(t *testing.T, seal *LeadSeal, lead Lead, job string, seq int64) KeptLead {
	t.Helper()
	lead.Job, lead.Seq = job, seq
	kept, err := seal.Seal("t1", lead)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return kept
}

// A LEAD'S SEAL OPENS ONLY ON THE TURN AND PLACE IT WAS SEALED FOR. A lead is an
// instruction a later brief hands a run as a person's words, so the row may not
// move it: carried onto another turn's record, or onto another job's place in
// this one's history, or named as another kind or round of the work, it does not
// open, and nor does one another fleet sealed. What the record names it by is
// compared, and everything a reader is handed comes out of the seal.
//
// Mutation: leave the turn, the job, its place, the kind or the round
// uncompared, and a case goes red.
func TestALeadSealOpensOnlyOnTheTurnAndPlaceItWasSealedFor(t *testing.T) {
	t.Parallel()
	kept := sealedFor(t, rigLeads(), answerLead("use main"), "job-2", 2)
	opened, err := rigLeads().Open("t1", kept)
	if err != nil || opened.Job != "job-2" || opened.Seq != 2 || opened.Answer == nil ||
		opened.Answer.Words != "use main" {
		t.Fatalf("the lead opens as %+v, %v; want job-2's answer, use main", opened, err)
	}
	elsewhere, err := NewLeadSeal(recordseal.OneKey(rigAnswerKey, "another fleet's key material"))
	if err != nil {
		t.Fatal(err)
	}
	anotherJobs := sealedFor(t, rigLeads(), answerLead("use main"), "job-9", 2).Sealed
	aReviews := sealedFor(t, rigLeads(), Lead{Kind: LeadKindReview, Round: 1}, "job-2", 2).Sealed
	for name, tc := range map[string]struct {
		turn string
		move func(*KeptLead)
		seal *LeadSeal
	}{
		"onto another turn":       {turn: "t2"},
		"onto another job":        {move: func(k *KeptLead) { k.Job = "job-3" }},
		"onto another launch":     {move: func(k *KeptLead) { k.Seq = 3 }},
		"as another kind":         {move: func(k *KeptLead) { k.Kind = LeadKindReview }},
		"as another round":        {move: func(k *KeptLead) { k.Round = 2 }},
		"sealed by another fleet": {seal: elsewhere},
		"its frame cut":           {move: func(k *KeptLead) { k.Sealed = k.Sealed[:len(k.Sealed)-1] }},
		"its seal another job's":  {move: func(k *KeptLead) { k.Sealed = anotherJobs }},
		"its seal a review's":     {move: func(k *KeptLead) { k.Sealed = aReviews }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			moved, turn := kept, "t1"
			moved.Sealed = bytes.Clone(kept.Sealed)
			if tc.seal != nil {
				moved = sealedFor(t, tc.seal, answerLead("use main"), "job-2", 2)
			}
			if tc.move != nil {
				tc.move(&moved)
			}
			if tc.turn != "" {
				turn = tc.turn
			}
			if got, err := rigLeads().Open(turn, moved); !errors.Is(err, ErrLeadUnopened) {
				t.Errorf("the lead moved %s opens as %+v, %v; want it refused, %v", name, got, err, ErrLeadUnopened)
			}
		})
	}
}

// A LEAD UNDER A KEY THIS NODE LACKS IS KEPT FOR A NODE THAT HOLDS IT. A fleet
// rotating its keyring has nodes that hold a newer key beside ones that do not:
// a lead a newer node sealed is, on an older one, a message that cannot be read
// here (a fact about the node, never the row), so it is told as such, and the
// run keeps it as it was, byte for byte, through every relaunch and every take
// of a later job's own leads beside it, whose attach marks it told again, never
// re-sealed or dropped: a later relaunch on a node that holds the key opens it
// and tells it.
//
// Mutation: drop the part's leads at a take, or let a reset name the run anew,
// and the newer node has nothing to open.
func TestALeadUnderAKeyThisNodeLacksIsKeptForANodeThatHoldsIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	newer := recordseal.Keyring{ActiveID: "leads-k2", Keys: []recordseal.Key{
		{ID: rigAnswerKey, Material: "the sandbox rigs' fleet key material"},
		{ID: "leads-k2", Material: "the key a newer node seals under"}}}
	newerSeal, err := NewLeadSeal(newer)
	if err != nil {
		t.Fatal(err)
	}
	store := NewCoordStore(memory.NewFleet())
	attach := func(row PendingRun, own []KeptLead, told []LeadRef) {
		t.Helper()
		if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-" + row.LaunchID,
			Brief: BriefContinued, Leads: BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
				Counted: row.LeadsAppended, Own: own, Told: told}}, Fence{}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	kept := sealedFor(t, newerSeal, answerLead("use main"), first.LaunchID, first.LaunchSeq)
	attach(first, []KeptLead{kept}, nil)
	// TWO RELAUNCHES, each taking its own lead in beside it, under the key this
	// node does hold, and telling the newer node's again.
	for range 2 {
		next, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", Continues: true}, Fence{})
		if err != nil {
			t.Fatal(err)
		}
		own := sealedFor(t, rigLeads(), answerLead("an answer of "+next.LaunchID), next.LaunchID, next.LaunchSeq)
		attach(next, []KeptLead{own}, []LeadRef{kept.Ref()})
	}
	got, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.Leads(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Kept) != 3 || !bytes.Equal(read.Kept[0].Sealed, kept.Sealed) || read.Cut != 0 ||
		!slices.Contains(got.Launch.LeadMarks, read.Kept[0].Appended) {
		t.Fatalf("two relaunches on, the run keeps %+v with %d cut and the job marks %v; want the newer node's "+
			"lead first, byte for byte, and marked told again", read.Kept, read.Cut, got.Launch.LeadMarks)
	}
	if _, err := rigLeads().Open("t1", read.Kept[0]); !errors.Is(err, ErrLeadKeyUnknown) {
		t.Errorf("an older node opens the lead with %v, want it told as sealed under a key it lacks, %v", err,
			ErrLeadKeyUnknown)
	}
	opened, err := newerSeal.Open("t1", read.Kept[0])
	if err != nil || opened.Answer == nil || opened.Answer.Words != "use main" {
		t.Errorf("a node holding the key opens the kept lead as %+v, %v; want the answer, use main", opened, err)
	}
}

// A LAUNCH MARKS THE LEADS THE BRIEF IT HANDED TOLD. Which brief a launch hands
// its run is the launch's to decide, once it has its box, so its request
// carries what each brief opened with (its own leads, and the earlier ones it
// told again), and the attach keeps the picked brief's: its own leads, sealed
// for the job and taken into the run's leads part, and on the job's record the
// numbers of those and of the earlier ones it told again. The copy in the
// asking job's box, a session of its own where that box was reclaimed, the
// turn's kept capture in a box that replaced its own, and a send-back's
// continuation of it each record theirs; a copy whose resume could not read
// the run's leads marks the earlier one by the number its brief handed on; and
// a turn's first launch, whose fresh brief opens with nothing a session lacks,
// takes and marks none.
//
// Mutation: hand every launch the fresh brief's leads, take no lead of the
// brief's own, mark none the brief told again, or drop the numbers it handed
// on, and a case goes red.
func TestALaunchMarksTheLeadsTheBriefItHandedTold(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// seed takes into t1's leads part an earlier lead, as an earlier job's
	// attach took it in: what the relaunch finds beside its row.
	seed := func(t *testing.T, store *CoordStore) KeptLead {
		t.Helper()
		row, _, err := store.Get(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		earlier := sealedFor(t, rigLeads(), answerLead("an earlier answer"), "job-earlier", 1)
		if took, err := store.takeLeads(ctx, "t1", BriefLeads{Run: row.LeadsRun, Job: "job-earlier",
			Own: []KeptLead{earlier}}, Fence{}); err != nil || !took.landed {
			t.Fatalf("seeding the run's leads: %+v, %v", took, err)
		}
		return earlier
	}
	// check reads t1's leads beside its row and the job's marks of them: the
	// earlier lead marked told again or not, and the job's own, the one whose
	// words are want, marked.
	check := func(t *testing.T, store *CoordStore, earlier KeptLead, toldAgain bool, want string) {
		t.Helper()
		got, _, err := store.Get(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		read, err := store.Leads(ctx, got)
		if err != nil {
			t.Fatal(err)
		}
		leads := read.Kept
		if len(leads) != 2 || leads[0].Ref() != earlier.Ref() {
			t.Fatalf("the run keeps %+v; want the earlier lead first, then the job's own", leads)
		}
		marks := []int{leads[1].Appended}
		if toldAgain {
			marks = []int{leads[0].Appended, leads[1].Appended}
		}
		if !slices.Equal(got.Launch.LeadMarks, marks) {
			t.Errorf("the job marks %v, want %v: its own lead, and the earlier one if told again (%v)",
				got.Launch.LeadMarks, marks, toldAgain)
		}
		own, err := rigLeads().Open("t1", leads[1])
		if err != nil || own.Job != got.LaunchID || own.Answer == nil || own.Answer.Words != want {
			t.Errorf("the job's own lead is %+v, %v; want %q sealed for job %s", own, err, want, got.LaunchID)
		}
	}
	for name, tc := range map[string]struct {
		reap      bool
		unread    bool
		toldAgain bool
		want      string
	}{
		"the asking job's own copy, in its box":   {toldAgain: true, want: "carried on in place"},
		"a session of its own, the box reclaimed": {reap: true, want: "carried on afresh"},
		// ITS RESUME COULD NOT READ THE RUN'S LEADS: the brief hands the
		// earlier lead on by its number, the part's first, not by name.
		"the asking job's own copy, the leads unread": {unread: true, toldAgain: true,
			want: "carried on in place"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			var earlier KeptLead
			askAndAnswer(t, rig, tc.reap, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
				earlier = seed(t, rig.pending)
				req := continuing(asked)
				req.Continue.Told = BriefTold{Own: []Lead{answerLead("carried on in place")},
					Told: []LeadRef{earlier.Ref()}}
				if tc.unread {
					req.Continue.Told = BriefTold{Own: []Lead{answerLead("carried on in place")},
						ToldUnread: []int{1}}
				}
				req.Continue.Fresh = BriefTold{Own: []Lead{answerLead("carried on afresh")}}
				if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
					t.Errorf("relaunch: %v", err)
				}
			})
			check(t, rig.pending, earlier, tc.toldAgain, tc.want)
		})
	}
	for name, send := range map[string]bool{"a round carried on": false, "a send-back": true} {
		t.Run("the turn's kept capture, "+name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				earlier := seed(t, rig.pending)
				req := continuingKept(kept)
				req.Session.Told = BriefTold{Own: []Lead{answerLead("carried on from the capture")},
					Told: []LeadRef{earlier.Ref()}}
				if !send {
					req.Continue = &Continuation{Fresh: BriefTold{Own: []Lead{answerLead("carried on afresh")}}}
				}
				if _, err := launchRestoring(t, rig, restore, req); err != nil {
					t.Fatalf("relaunch: %v", err)
				}
				check(t, rig.pending, earlier, true, "carried on from the capture")
			})
		})
	}
	t.Run("a turn's first launch", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		if _, err := rig.launchVia(ctx, rig.manager, launchReq("t1")); err != nil {
			t.Fatalf("a first launch: %v", err)
		}
		row := rig.get("t1")
		read, err := rig.pending.Leads(ctx, row)
		if err != nil || len(read.Kept) != 0 || read.Cut != 0 || len(row.Launch.LeadMarks) != 0 ||
			row.LeadsAppended != 0 {
			t.Errorf("a first launch's fresh brief kept leads %+v with %d cut, marked %v and counted %d, %v; want "+
				"none", read.Kept, read.Cut, row.Launch.LeadMarks, row.LeadsAppended, err)
		}
	})
}

// A TAKE KEEPS EACH LEAD ONCE, NUMBERED AS IT WAS TAKEN. The attach that names a
// job's command takes the leads its brief opened with of its own into the run's
// leads part, each by its identity: a new one is numbered one past every lead
// the part has taken, so a number names one lead for the run's whole life,
// whatever the bound drops; one the part keeps already is left as it is, so an
// attach written again takes nothing twice. And the job is marked with the
// numbers of every lead its brief told that the part keeps then, its own and
// the earlier ones it told again, never one the bound has just dropped.
//
// Mutation: take a lead whatever the part keeps, number one by the part's
// length, mark the leads before the bound drops any, or leave the brief's own
// unmarked, and a check goes red.
func TestATakeKeepsEachLeadOnceNumberedAsItWasTaken(t *testing.T) {
	t.Parallel()
	small := func(job string, n int) KeptLead {
		return KeptLead{Kind: LeadKindAnswer, Job: job, Seq: 1, Round: 1, Appended: n, Sealed: []byte("sealed")}
	}
	numbered := func(leads []KeptLead) (jobs []string, numbers []int) {
		for _, kept := range leads {
			jobs, numbers = append(jobs, kept.Job), append(numbers, kept.Appended)
		}
		return jobs, numbers
	}
	part := leadsPart{Run: "run-1", Leads: []KeptLead{small("job-1", 1), small("job-2", 2)}, Appends: 2}
	own := small("job-3", 0)
	next, marks, grew := part.took([]KeptLead{own},
		[]LeadRef{{Job: "job-1", Kind: LeadKindAnswer}, {Job: "job-9", Kind: LeadKindAnswer}}, nil)
	if jobs, numbers := numbered(next.Leads); !slices.Equal(jobs, []string{"job-1", "job-2", "job-3"}) ||
		!slices.Equal(numbers, []int{1, 2, 3}) || next.Appends != 3 || !grew || !slices.Equal(marks, []int{1, 3}) {
		t.Fatalf("the take keeps %v numbered %v (%d taken, grew %v) and marks %v; want job-3 numbered 3, and "+
			"marks for job-1, told again, and job-3, its own, and none for job-9, which the part does not keep",
			jobs, numbers, next.Appends, grew, marks)
	}
	again, marks, grew := next.took([]KeptLead{own}, nil, nil)
	if jobs, numbers := numbered(again.Leads); len(jobs) != 3 || !slices.Equal(numbers, []int{1, 2, 3}) ||
		again.Appends != 3 || grew || !slices.Equal(marks, []int{3}) {
		t.Errorf("the same lead taken again keeps %v numbered %v (%d taken, grew %v) and marks %v; want nothing "+
			"taken, its number kept and marked", jobs, numbers, again.Appends, grew, marks)
	}
	// PAST THE BOUND: a third of the share each, as the part holds them, so a
	// third drops the oldest, and the next is numbered past every one taken.
	big := func(job string, n int) KeptLead {
		lead := small(job, n)
		lead.Sealed = bytes.Repeat([]byte("s"), MaxKeptLeadBytes/3)
		return lead
	}
	full := leadsPart{Run: "run-1", Leads: []KeptLead{big("job-1", 1), big("job-2", 2)}, Appends: 2}
	dropped, marks, _ := full.took([]KeptLead{big("job-3", 0)}, []LeadRef{{Job: "job-1", Kind: LeadKindAnswer}},
		nil)
	if jobs, numbers := numbered(dropped.Leads); !slices.Equal(jobs, []string{"job-2", "job-3"}) ||
		!slices.Equal(numbers, []int{2, 3}) || !slices.Equal(marks, []int{3}) {
		t.Fatalf("past the bound the part keeps %v numbered %v and marks %v; want job-1 dropped and unmarked "+
			"though told again, job-3 numbered 3 and marked", jobs, numbers, marks)
	}
	later, _, _ := dropped.took([]KeptLead{big("job-4", 0)}, nil, nil)
	if jobs, numbers := numbered(later.Leads); !slices.Equal(jobs, []string{"job-3", "job-4"}) ||
		!slices.Equal(numbers, []int{3, 4}) || later.Appends != 4 {
		t.Errorf("the next take keeps %v numbered %v with %d taken; want job-4 numbered 4, past every lead "+
			"taken, the dropped among them", jobs, numbers, later.Appends)
	}
}

// A LEAD A BRIEF COULD NOT READ IS MARKED BY ITS NUMBER. A resume whose read of
// the run's leads failed knows the leads its job's brief told by their numbers
// alone, off the job's record: the brief it builds says them as not kept and
// hands them on by number, beside the ones it told again by name. The take
// marks each one the part keeps, as a lead told again is marked, so the job the
// relaunch opens names it for a continuation of its session to tell once a
// read answers; a number the part does not keep, dropped by the bound or never
// taken, is not marked; and numbers alone take nothing in, so the part is not
// written for them.
//
// Mutation: ignore the numbers, mark them whether the part keeps them or not,
// or count them as taken, and a check goes red.
func TestALeadABriefCouldNotReadIsMarkedByItsNumber(t *testing.T) {
	t.Parallel()
	small := func(job string, n int) KeptLead {
		return KeptLead{Kind: LeadKindAnswer, Job: job, Seq: 1, Round: 1, Appended: n, Sealed: []byte("sealed")}
	}
	// THE PART AS A TAKE PAST ITS BOUND LEFT IT: the first lead dropped.
	part := leadsPart{Run: "run-1", Leads: []KeptLead{small("job-2", 2), small("job-3", 3)}, Appends: 3}
	next, marks, grew := part.took([]KeptLead{small("job-4", 0)}, nil, []int{1, 3, 9})
	if !slices.Equal(marks, []int{3, 4}) || !grew || next.Appends != 4 || len(next.Leads) != 3 {
		t.Errorf("the take marks %v (grew %v, %d taken, %d kept); want 3, which the brief could not read and the "+
			"part keeps, and 4, its own, and neither 1, which the bound dropped, nor 9, which it never took",
			marks, grew, next.Appends, len(next.Leads))
	}
	alone, marks, grew := part.took(nil, nil, []int{2})
	if !slices.Equal(marks, []int{2}) || grew || alone.Appends != 3 || len(alone.Leads) != 2 {
		t.Errorf("a take of numbers alone marks %v (grew %v, %d taken, %d kept); want 2 marked and nothing taken",
			marks, grew, alone.Appends, len(alone.Leads))
	}
}

// THE LEADS THE RUN KEEPS ARE HELD TO THEIR SHARE, WITH THE CUT SAID. A long
// turn carried on by many answers keeps every one of them only while they fit
// the part's share for them ([MaxKeptLeadBytes]) and its count of them
// ([MaxKeptLeads]): past either the OLDEST go first, since the newest are the
// ones a session cut off most likely lacks, and each one dropped is still
// counted, the part's count of every lead it took running on, so a brief that
// tells the rest again says how many it no longer can.
//
// Mutation: drop the newest rather than the oldest, lower the count on a drop,
// or keep leads past their count, and a check goes red.
func TestTheLeadsTheRunKeepsAreHeldToTheirShareWithTheCutSaid(t *testing.T) {
	t.Parallel()
	third := MaxKeptLeadBytes / 3
	lead := func(job string, n int) KeptLead {
		return KeptLead{Kind: LeadKindAnswer, Job: job, Seq: 1, Round: 1, Appended: n,
			Sealed: bytes.Repeat([]byte("a"), third)}
	}
	part := leadsPart{Run: "run-1", Leads: []KeptLead{lead("job-1", 3), lead("job-2", 4)}, Appends: 4}
	got, _, _ := part.took([]KeptLead{lead("job-3", 0)}, nil, nil)
	var jobs []string
	for _, kept := range got.Leads {
		jobs = append(jobs, kept.Job)
	}
	if !slices.Equal(jobs, []string{"job-2", "job-3"}) || got.Appends != 5 {
		t.Errorf("past its share the part keeps %v with %d taken; want the oldest dropped, [job-2 job-3], and "+
			"the count run on to 5, three of them not kept", jobs, got.Appends)
	}
	if raw, _ := json.Marshal(got.Leads); len(raw) > MaxKeptLeadBytes {
		t.Errorf("the part's leads weigh %d, past their share %d", len(raw), MaxKeptLeadBytes)
	}
	// PAST ITS COUNT, as a build that seals smaller leads would fill it.
	many := leadsPart{Run: "run-1", Appends: MaxKeptLeads}
	for i := range MaxKeptLeads {
		many.Leads = append(many.Leads, KeptLead{Kind: LeadKindAnswer, Job: fmt.Sprintf("job-%d", i), Seq: 1,
			Appended: i + 1, Sealed: []byte("s")})
	}
	past, _, _ := many.took([]KeptLead{{Kind: LeadKindAnswer, Job: "job-newest", Seq: 2, Sealed: []byte("s")}},
		nil, nil)
	if n := len(past.Leads); n != MaxKeptLeads || past.Leads[0].Job != "job-1" ||
		past.Leads[n-1].Job != "job-newest" || past.Appends != MaxKeptLeads+1 {
		t.Errorf("past its count the part keeps %d leads from %s to %s with %d taken; want %d, the oldest "+
			"dropped and the newest kept, %d taken", n, past.Leads[0].Job, past.Leads[n-1].Job, past.Appends,
			MaxKeptLeads, MaxKeptLeads+1)
	}
}

// A RECORD WHOSE BOX STARTED ITS OWN SESSION TELLS NO LEAD AGAIN. A session of
// the run's own holds the brief it opened with wherever it can be continued, so
// the leads that brief told are in it: a job whose launch handed a fresh brief,
// or whose box started a session of its own in a continued one's place, its
// restore having failed, names none to tell again. A job that continued its
// session, or whose box never said or said it in a word this build does not
// know, names the ones its brief told: told again rather than never.
//
// Mutation: read the told leads off the record whatever its box started, and a
// case goes red.
func TestARecordWhoseBoxStartedItsOwnSessionTellsNoLead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		brief BriefKind
		start SessionStart
		told  bool
	}{
		"a fresh brief":                   {brief: BriefFresh},
		"a box that started its own":      {brief: BriefContinued, start: SessionStartFresh},
		"a box that continued":            {brief: BriefContinued, start: SessionStartContinued, told: true},
		"a box that never said":           {brief: BriefContinuedNewBox, told: true},
		"a word this build does not know": {brief: BriefContinued, start: "resumed", told: true},
	} {
		got := LaunchRecord{Brief: tc.brief, SessionStart: tc.start, LeadMarks: []int{1}}.ToldLeads()
		if want := map[bool]int{true: 1, false: 0}[tc.told]; len(got) != want || (want == 1 && got[0] != 1) {
			t.Errorf("%s: the record tells again %v, want %d of its marks, the one its brief told", name, got, want)
		}
	}
}

// A HELD RESULT KEPT FOR A LEAD OPENS WITH ITS CALL'S SEAL. What a held call
// returned is sealed on its run's row under the fleet keyring's AEAD, bound to
// the run, the job, the call and the server's name for it; the lead that told it
// keeps it as sealed, with those fields ([HeldKept]), and a later node opens it
// under the very binding it was sealed with, for that call alone.
//
// Mutation: open a kept result under fields other than the ones it keeps, and
// it does not open.
func TestAHeldResultKeptForALeadOpensWithItsCallsSeal(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := NewCallSeal(cipher)
	held := HeldCall{ID: "held-1", LaunchID: "job-1", Server: "vendor", Tool: "vendor_post", Raw: "post",
		Digest: "sha256:d1"}
	sealed, err := seal.SealResult("t1", held, HeldResult{Output: "posted"})
	if err != nil {
		t.Fatal(err)
	}
	held.Result = sealed
	kept := held.Kept()
	if kept == nil || *kept != KeptAs(held, sealed) {
		t.Fatalf("the held call keeps %+v, want its result with the fields its seal binds", kept)
	}
	if result, err := seal.OpenKept("t1", *kept); err != nil || result.Output != "posted" {
		t.Errorf("the kept result opens as %+v, %v; want posted", result, err)
	}
	other := *kept
	other.ID = "held-2"
	if result, err := seal.OpenKept("t1", other); err == nil {
		t.Errorf("a result kept for held-1 opens as held-2's: %+v", result)
	}
	if _, err := seal.OpenKept("t2", *kept); err == nil {
		t.Error("a result kept for run t1 opens for run t2")
	}
}

// AN OLDER BUILD'S WRITE CARRIES A JOB'S LEAD MARKS. A fleet mid-upgrade
// rewrites the job's record on a build that does not know the marks (a status,
// a lease, a bridged call, the park the answer waits on), and the relaunch may
// run on this build: so the marks ride the job's record under their own key,
// which a build that does not declare it keeps verbatim and writes back
// (wirekeys.go); and the run the leads are kept for and how many the part has
// taken ride the top of the row the same way.
//
// Mutation: leave the marks off the wire, or drop the carried keys from the
// record's encode, and a check goes red.
func TestAnOlderBuildsWriteCarriesAJobsLeadMarks(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(LaunchRecord{Model: "m", LeadMarks: []int{2, 5}})
	if err != nil {
		t.Fatal(err)
	}
	declared := wireKeys(reflect.TypeFor[LaunchRecord]())
	if !declared["lead_marks"] {
		t.Fatalf("the job's record declares no key lead_marks: %v", declared)
	}
	older := maps.Clone(declared)
	delete(older, "lead_marks")
	type plain LaunchRecord
	var read plain
	extra, err := unmarshalCarrying(raw, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	read.LeadMarks, read.Model = nil, "m2"
	written, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back LaunchRecord
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if back.Model != "m2" || !slices.Equal(back.LeadMarks, []int{2, 5}) {
		t.Errorf("after the older build's write the job's record reads marks %v and model %q, want [2 5] and "+
			"m2: %s", back.LeadMarks, back.Model, written)
	}
	// AND THE ROW'S OWN: the run its leads are kept for and their count.
	row, err := encodeRun(PendingRun{TurnID: "t1", Status: StatusLaunching, LeadsRun: "job-1", LeadsAppended: 7})
	if err != nil {
		t.Fatal(err)
	}
	keys := pendingRunKeys()
	if !keys["leads_run"] || !keys["leads_appended"] {
		t.Fatal("the row declares no key leads_run or leads_appended")
	}
	olderRow := maps.Clone(keys)
	delete(olderRow, "leads_run")
	delete(olderRow, "leads_appended")
	var run PendingRun
	carried, err := unmarshalCarrying(row, &run, olderRow)
	if err != nil {
		t.Fatal(err)
	}
	run.LeadsRun, run.LeadsAppended, run.Status = "", 0, StatusRunning
	rewritten, err := marshalCarrying(run, olderRow, carried)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decodeRun(coord.Record{Key: "t1", Value: rewritten})
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != StatusRunning || again.LeadsRun != "job-1" || again.LeadsAppended != 7 {
		t.Errorf("after the older build's write the row reads %q, run %q and %d leads taken; want running, "+
			"job-1 and 7: %s", again.Status, again.LeadsRun, again.LeadsAppended, rewritten)
	}
}

// A LATER BUILD'S KEYS IN THE LEADS PART SURVIVE A TAKE. Every take of a job's
// leads rewrites the run's leads part whole, on whichever build runs the
// attach: so a key a later build added at the part's top, or to a lead it
// keeps, is carried by this build's decode and written back by its encode, and
// a kept lead's sealed bytes go back as they came (wirekeys.go).
//
// Mutation: drop the carried keys from the part's encode, or from a lead's, and
// a check goes red.
func TestALaterBuildsKeysInTheLeadsPartSurviveATake(t *testing.T) {
	t.Parallel()
	written := []byte(`{"run":"run-1","appends":1,"weight":7,"leads":[{"kind":"answer","job":"job-1","seq":1,` +
		`"round":1,"appended":1,"sealed":"c2VhbGVk","signer":"a later build's"}]}`)
	var part leadsPart
	if err := json.Unmarshal(written, &part); err != nil {
		t.Fatal(err)
	}
	next, _, grew := part.took([]KeptLead{{Kind: LeadKindAnswer, Job: "job-2", Seq: 2, Round: 1,
		Sealed: []byte("sealed")}}, nil, nil)
	if !grew {
		t.Fatal("the take took nothing in")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	var back leadsPart
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Extra["weight"]) != "7" || len(back.Leads) != 2 ||
		string(back.Leads[0].Extra["signer"]) != `"a later build's"` ||
		!bytes.Equal(back.Leads[0].Sealed, []byte("sealed")) {
		t.Errorf("the part written back after a take is %s; want the later build's key at its top and in its "+
			"lead, and the lead's sealed bytes as they came", raw)
	}
}

// THE COUNT OF KEPT LEADS NEVER DROPS A LEAD THEIR BYTES WOULD KEEP. A run's
// leads part keeps at most [MaxKeptLeads] leads beside their bytes, so the
// numbers a job's record marks them by are bounded by construction; the count
// is set so that it never drops a lead of this build's that the bytes would
// keep: the least any lead this build seals weighs as the part holds it (a
// review, which carries no source, of a one-byte turn under a one-byte key, its
// job a launch id, at the first place and the first number), times the count,
// is at least the bytes' bound, and that least is at least the 256 bytes the
// count is reckoned at (leads.go).
//
// Mutation: halve the count, or set it to a round thousand, and the count
// drops leads the bytes would keep.
func TestTheCountOfKeptLeadsNeverDropsALeadTheirBytesWouldKeep(t *testing.T) {
	t.Parallel()
	seal, err := NewLeadSeal(recordseal.OneKey("k", "the count's key material"))
	if err != nil {
		t.Fatal(err)
	}
	least, err := seal.Seal("t", Lead{Kind: LeadKindReview, Job: uuid.NewString(), Seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	least.Appended = 1
	raw, err := json.Marshal(least)
	if err != nil {
		t.Fatal(err)
	}
	weight := len(raw) + len(",")
	t.Logf("the least lead this build seals weighs %d as the part holds it, its comma counted: %d of them "+
		"weigh %d beside the %d the part keeps", weight, MaxKeptLeads, MaxKeptLeads*weight, MaxKeptLeadBytes)
	if MaxKeptLeads*weight < MaxKeptLeadBytes || weight < 256 {
		t.Errorf("%d of the least lead weigh %d, under the %d the part's bytes keep, or the least weighs %d, "+
			"under the 256 the count is reckoned at: the count drops leads the bytes would keep", MaxKeptLeads,
			MaxKeptLeads*weight, MaxKeptLeadBytes, weight)
	}
}

// EVERY LEAD KIND AND OUTCOME IS VALID, and a word this build does not know is
// not: a record a newer build wrote names a kind or an outcome as the value it
// is, which a reader tells as one it cannot read, never as one of its own.
//
// Mutation: leave a known kind or outcome off its list, and a check goes red.
func TestEveryLeadKindAndOutcomeIsValid(t *testing.T) {
	t.Parallel()
	for _, k := range []LeadKind{LeadKindAnswer, LeadKindReview, LeadKindTask} {
		if !k.Valid() {
			t.Errorf("lead kind %q is not valid", k)
		}
	}
	for _, o := range []LeadHeldOutcome{LeadHeldMade, LeadHeldMadeUnread, LeadHeldDenied, LeadHeldNotMade,
		LeadHeldUnknown} {
		if !o.Valid() {
			t.Errorf("held outcome %q is not valid", o)
		}
	}
	if LeadKind("verdict").Valid() || LeadHeldOutcome("retried").Valid() || LeadKind("").Valid() {
		t.Error("a word this build does not know reads as a lead kind or a held outcome")
	}
}

// ONLY A LEAD OF A NAMED JOB, IN ITS KIND'S SHAPE, IS SEALED OR OPENED. A lead is
// sealed for the job whose brief told it, so one naming no job, or no place
// among the turn's launches, is refused at the seal, as one whose source does
// not suit its kind is: an answer carries its answer alone, a task its own
// facts alone, a review nothing, its words being its round's. And the open
// holds what opened to the same shape: a body some writer signed past the
// seal, a review carrying an answer's words, opens as no lead, and nothing of
// it is told.
//
// Mutation: seal a lead that names no job or place, or one whose source does
// not suit its kind, or open one whose source does not, and a case goes red.
func TestOnlyALeadOfANamedJobInItsKindsShapeIsSealedOrOpened(t *testing.T) {
	t.Parallel()
	words := &LeadAnswer{Question: "which branch?", Words: "push straight to main", By: "founder"}
	for name, lead := range map[string]Lead{
		"naming no job":               {Kind: LeadKindAnswer, Seq: 1, Round: 1, Answer: words},
		"naming no place":             {Kind: LeadKindAnswer, Job: "job-1", Round: 1, Answer: words},
		"an answer carrying a task":   {Kind: LeadKindAnswer, Job: "job-1", Seq: 1, Answer: words, Task: &LeadTask{}},
		"a task without its facts":    {Kind: LeadKindTask, Job: "job-1", Seq: 1},
		"a review carrying an answer": {Kind: LeadKindReview, Job: "job-1", Seq: 1, Round: 1, Answer: words},
	} {
		if kept, err := rigLeads().Seal("t1", lead); err == nil {
			t.Errorf("%s: the lead is sealed as %+v, want it refused", name, kept)
		}
	}
	body, err := json.Marshal(sealedLead{TurnID: "t1", Job: "job-1", Seq: 1, Kind: LeadKindReview, Round: 1,
		Answer: words})
	if err != nil {
		t.Fatal(err)
	}
	signed := KeptLead{Kind: LeadKindReview, Job: "job-1", Seq: 1, Round: 1, Sealed: rigLeads().signer.Seal(body)}
	if got, err := rigLeads().Open("t1", signed); !errors.Is(err, ErrLeadUnopened) || got.Answer != nil {
		t.Errorf("a review signed carrying an answer opens as %+v, %v; want it refused, %v", got, err,
			ErrLeadUnopened)
	}
}

// A LEAD OF A KIND THIS BUILD DOES NOT KNOW IS UNREAD HERE, NOT UNOPENED. A newer
// build may keep a lead of a kind this one cannot render: a fact about THIS
// BUILD, the disposition a key this node lacks gets, so it is told as a message
// that cannot be read here and kept for a build that reads it, logged at
// warning; never refused as a seal that does not open, the record's fact,
// logged at error (internal/engine's resumeLeads tells the two apart by the
// error).
//
// Mutation: open a lead whatever its kind, and the newer build's lead is
// refused as the record's fault.
func TestALeadOfAKindThisBuildDoesNotKnowIsUnreadHereNotUnopened(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(sealedLead{TurnID: "t1", Job: "job-1", Seq: 1, Kind: "verdict", Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	newer := KeptLead{Kind: "verdict", Job: "job-1", Seq: 1, Round: 1, Sealed: rigLeads().signer.Seal(body)}
	if _, err := rigLeads().Open("t1", newer); !errors.Is(err, ErrLeadKindUnknown) || errors.Is(err, ErrLeadUnopened) {
		t.Errorf("a lead of a kind this build does not know opens with %v; want %v, and not %v", err,
			ErrLeadKindUnknown, ErrLeadUnopened)
	}
}

// A LEAD THE PART CANNOT WRITE IS DROPPED AND COUNTED. A lead carrying a key
// the part's encoder cannot write would refuse every write of the part it rode,
// so the take that holds the part's leads to their share drops it wherever it
// stands; it stays counted among every lead the part took, so a brief that
// tells the rest again says one more is missing, never that they are every
// one; it is never marked; and the part writes.
//
// Mutation: keep such a lead, and the part no longer writes.
func TestALeadThePartCannotWriteIsDroppedAndCounted(t *testing.T) {
	t.Parallel()
	broken := KeptLead{Kind: LeadKindAnswer, Job: "job-1", Seq: 1, Round: 1, Appended: 1, Sealed: []byte("sealed"),
		Extra: map[string]json.RawMessage{"weight": json.RawMessage("not json")}}
	if _, err := json.Marshal(broken); err == nil {
		t.Fatal("the encoder writes a lead whose carried key is not JSON; the case needs one it cannot")
	}
	own := KeptLead{Kind: LeadKindAnswer, Job: "job-2", Seq: 2, Round: 1, Sealed: []byte("sealed")}
	got, marks, _ := leadsPart{Run: "run-1", Leads: []KeptLead{broken}, Appends: 1}.took([]KeptLead{own},
		[]LeadRef{broken.Ref()}, nil)
	if len(got.Leads) != 1 || got.Leads[0].Job != "job-2" || got.Appends != 2 || !slices.Equal(marks, []int{2}) {
		t.Errorf("the part keeps %+v with %d taken and marks %v; want the lead it can write, job-2, numbered 2 "+
			"and marked, and the count run on to 2", got.Leads, got.Appends, marks)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("the part does not write: %v", err)
	}
}
