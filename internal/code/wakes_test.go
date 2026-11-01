package code_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM THE GATE'S RECORDS CONCERN (concerns.go, "Who hears what the gate
// does"): a head check's ending, a merge, a dequeue, a queue's ejection and
// landing and a broken landing each carry the routing their own writer
// stamped, and the wakes they cause are read from it alone.

// headAttempt asks for attempt key of change's revision's head queued for
// reason, under the target's checks file.
func headAttempt(key code.CheckKey, change string, revision int, reason code.QueueReason) code.ChecksRequest {
	req := queueing(key, reason)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, change, revision
	req.Queued.DefsOID = oidOf("main-checks")
	return req
}

// runCheck queues, claims and ends attempt key of change's revision's head
// through w — ended in state for why — and is the ending's position.
func runCheck(t *testing.T, w *code.ChecksWriter, key code.CheckKey, change string, revision int,
	state code.CheckState, why code.CheckReason) statelog.Position {

	t.Helper()
	reason := code.QueueDispatch
	if key.Attempt > 1 {
		reason = code.QueueRequeue
	}
	var ended statelog.Position
	for _, req := range []code.ChecksRequest{headAttempt(key, change, revision, reason),
		claiming(key, "git-a", ""), ending(key, state, why)} {
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, key.Name+" at the head", res, err)
		ended = res.Position
	}
	return ended
}

// heardSorted is heard, sorted: who a routing tells, whatever order its
// lists hold them in.
func heardSorted(concerns []code.Concern) []string {
	out := heard(concerns)
	slices.Sort(out)
	return out
}

// A FAILED CHECK WAKES ITS AUTHORS AND ITS ACTOR (M5-D26): ada moves dev's
// change's head to B2, its revision 2, and a check of B2 that fails tells the
// change's authors — dev, its owner, and ada, who pushed its head — that it
// failed, ada AS THE RECORD'S ACTOR: the one code reason that is news to who
// made the record (Reason.WakesActor), since nothing else tells a pusher that
// their commit fails its checks. An attempt that errored on its
// infrastructure, which the home queues again, tells nobody until the attempt
// the home's own requeue stops at; a passing check tells nobody — readiness is
// the home's next mergeability record's to announce; and a failed check of
// the head the change has moved past measures code nobody is asked to fix.
//
// Mutations: Reason.WakesActor false for checks_failed — ada, the actor, is
// not told, red; route every ending of a head check, whatever its state — the
// passing one tells the authors, red; drop the attempt bound from
// failsItsHead — the third errored attempt tells nobody, red.
func TestAFailedCheckWakesItsAuthorsAndItsActor(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	res, err := c.Propose(t.Context(), proposal(repo, update(featureRef, oidOf("B"), oidOf("B2"))))
	if err != nil || res.Position.IsZero() {
		t.Fatalf("ada's head move answered %+v, %v", res, err)
	}
	w, _, _, _ := checksWriterOn(t, n)
	key := func(name, oid string, attempt int) code.CheckKey {
		return code.CheckKey{RepoID: repo, OID: oidOf(oid), Target: mainRef, Name: name, Attempt: attempt}
	}

	failed := runCheck(t, w, key("unit", "B2", 1), change, 2, code.CheckStateFailed, code.CheckReasonExit)
	routings := routingAt(t, n, failed)
	if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].ChecksFailed ||
		routings[0].Actor != "ada" {
		t.Fatalf("the failed check's record carries %+v, want web!1's checks failed, its actor ada", routings)
	}
	concerns := code.Concerns(routings)
	if got := heardSorted(concerns); !slices.Equal(got, []string{"ada:checks_failed", "dev:checks_failed"}) {
		t.Fatalf("the failed check tells %v, want dev and ada — who pushed the head — that it failed", got)
	}

	for name, p := range map[string]statelog.Position{
		"a passing check": runCheck(t, w, key("lint", "B2", 1), change, 2, code.CheckStatePassed,
			code.CheckReasonExit),
		"an errored attempt the home queues again": runCheck(t, w, key("e2e", "B2", 1), change, 2,
			code.CheckStateErrored, code.CheckReasonRunnerLost),
		"a failed check of the head before": runCheck(t, w, key("unit", "B", 1), change, 1,
			code.CheckStateFailed, code.CheckReasonExit),
	} {
		if routings := routingAt(t, n, p); len(routings) != 0 {
			t.Fatalf("%s carries %+v, want no routing: it tells nobody", name, routings)
		}
	}
	runCheck(t, w, key("e2e", "B2", 2), change, 2, code.CheckStateErrored, code.CheckReasonRunnerLost)
	last := runCheck(t, w, key("e2e", "B2", code.AutoRequeueAttempts), change, 2, code.CheckStateErrored,
		code.CheckReasonRunnerLost)
	if got := heardSorted(code.Concerns(routingAt(t, n, last))); !slices.Equal(got,
		[]string{"ada:checks_failed", "dev:checks_failed"}) {
		t.Fatalf("the attempt the home's requeue stops at tells %v, want the authors and ada", got)
	}
}

// A BROKEN LANDING WAKES ITS AUTHORS AND LEAD (D-QUEUE): dev's change, queued
// by ada, lands ahead of its checks by her override merge past main's queue,
// and its required check then fails on the landed tip: the broken record
// marking that landing tells dev, its author, and cto, the effective lead of
// Engineering, which owns the repository — whoever can put the branch right —
// that the landing broke it. The broken record is the home's own, so none of
// them made it and both hear. Ada's seat founder, who queued the change, is
// told nothing: her override landed it, not the queue, whose own landings
// land green and so are never broken.
//
// Mutations: stamp no lead on a broken record's routing — cto is not told,
// red; route the broken record's reason as conflicted — the reason is wrong,
// red.
func TestABrokenLandingWakesItsAuthorsAndLead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	override := merging(repo, change, ada, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	landed := merged.Position

	n.checked(repo, oidOf("M"), mainRef, "unit", 1, code.CheckStateFailed)
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Broken = &code.Broken{RepoID: repo, TargetRef: mainRef, Position: landed, Ordinal: 1, OID: oidOf("M"),
			Checks: []string{"unit"}}
	}))
	broken := requireRecorded(t, res, err)
	routings := routingAt(t, n, broken)
	if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].Broken || routings[0].Actor != "" ||
		routings[0].Enqueuer != "" || routings[0].Lead != "cto" {
		t.Fatalf("the broken record carries %+v, want web!1 broken, no enqueuer, its lead cto, made by nobody",
			routings)
	}
	if got := heard(code.Concerns(routings)); !slices.Equal(got,
		[]string{"dev:target_broken", "cto:target_broken"}) {
		t.Fatalf("the broken landing tells %v, want its author and Engineering's lead", got)
	}
}

// A PASSING CHECK CAN MAKE A CHANGE READY (M5-D26), and it is never the
// check's record that says so: dev's change into a queue target is approved
// and merges cleanly but waits on its required check, so it is stored not
// ready. The check passes at its head — a record that tells nobody — and the
// home's readiness refresh that follows, its head's checks having moved,
// stores the change ready and announces it to whoever may merge it: under a
// gate any writer merges by, dev, its author; under one only maintainers
// merge by, Engineering's lead cto, since no author may.
//
// Mutations: announce ready from the check record (route a passed head
// check's ending to the change's authors as ready_to_merge) — the check's
// record tells somebody, red; announce to the owner whatever the gate — the
// maintainers' case tells dev, red.
func TestAPassingCheckCanMakeAChangeReady(t *testing.T) {
	t.Parallel()
	for mergeBy, want := range map[code.MergeBy]string{
		code.MergeByWriters:     "dev:ready_to_merge",
		code.MergeByMaintainers: "cto:ready_to_merge",
	} {
		n := newNode(t)
		n.chart = testChart()
		c, _ := committerOn(t, n, n.pub)
		w, _ := writerOn(t, n, n.pub)
		cw, _, _, _ := checksWriterOn(t, n)
		s := suiteSettings()
		s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
		gate := queueGate()
		gate.MergeBy = mergeBy
		s.Protections[0].Gate = gate
		repo := n.ensure("web", s, t1).RepoID
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
			set(featureRef, code.ZeroOID, oidOf("B"))))
		opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
		requireChanged(t, opened, err)
		change := opened.ChangeID
		converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
		converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
		converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
		if n.storedReadiness(change).ready {
			t.Fatalf("merge by %s: a change its required check has not passed on is stored ready", mergeBy)
		}

		passed := runCheck(t, cw, code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit",
			Attempt: 1}, change, 1, code.CheckStatePassed, code.CheckReasonExit)
		if routings := routingAt(t, n, passed); len(routings) != 0 {
			t.Fatalf("merge by %s: the passing check's record carries %+v, want no routing: only the home's "+
				"mergeability record announces a change ready", mergeBy, routings)
		}
		refresh := converse(t, w, code.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &code.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
		if !n.storedReadiness(change).ready {
			t.Fatalf("merge by %s: the refresh after the passing check left the change not ready", mergeBy)
		}
		if got := heard(code.Concerns(routingAt(t, n, refresh.Position))); !slices.Equal(got, []string{want}) {
			t.Fatalf("merge by %s: the refresh tells %v, want %s", mergeBy, got, want)
		}
	}
}

// EVERY M5 WAKE NAMES ITS WORK ITEM (D-WAKES): a change carrying a work item
// is checked, queued, put out of its queue twice — a dequeue its enqueuer
// asked, an ejection for a conflicting merge — queued again and landed by its
// queue; a second change lands ahead of its checks by a person's override
// and that landing is then broken; and a third is merged by its record:
// every one of those records wakes the seats it concerns under its own reason, each wake
// naming the change's work item, so the turn it starts is charged to it
// (ADR-0022). A person is told in their inbox, never woken: ada's seat
// founder, who queued the change, has no wake of their own.
//
// Mutations: stamp no work item on the gate's routings (routingFor) — every
// wake names none, red; route a conflicting ejection as dequeued alone — dev
// is woken under dequeued, red.
func TestEveryM5WakeNamesItsWorkItem(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	cw, _, _, _ := checksWriterOn(t, n)
	item := &types.WorkItem{Backend: types.WorkNative, ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ENG-7")).String(),
		Key: "ENG-7", Project: "ENG"}
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID

	records := map[string]statelog.Position{}
	wants := map[string][]string{}
	records["a failed head check"] = runCheck(t, cw, code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef,
		Name: "lint", Attempt: 1}, change, 1, code.CheckStateFailed, code.CheckReasonExit)
	wants["a failed head check"] = []string{"dev:checks_failed"}
	ready(t, n, w, repo, change)

	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, enqueue.Enqueue.EntryID, ada))
	records["a dequeue"] = requireRecorded(t, res, err)
	wants["a dequeue"] = []string{"dev:dequeued"}

	enqueue = enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: repo, Entries: []code.Ejection{{EntryID: enqueue.Enqueue.EntryID,
			Reason: code.EjectConflict}}}
	}))
	records["an ejection"] = requireRecorded(t, res, err)
	wants["an ejection"] = []string{"dev:conflicted"}

	enqueue = enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.passed(repo, oidOf("M"), mainRef, "unit")
	res, err = c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
		code.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")}))
	records["a landing"], wants["a landing"] = requireRecorded(t, res, err), []string{"dev:merged"}

	records["a broken landing"] = brokenWorkItemLanding(t, n, c, w, item)
	wants["a broken landing"] = []string{"cto:target_broken", "dev:target_broken"}

	merged := mergedWorkItemChange(t, n, c, w, item)
	records["a merge"], wants["a merge"] = merged, []string{"dev:merged"}

	for name, p := range records {
		_, relayed, routed := wakesOf(t, feedRecord(t, n, p))
		got := woken(routed)
		slices.Sort(got)
		if !relayed || !slices.Equal(got, wants[name]) {
			t.Errorf("%s wakes %v (relayed %v), want %v", name, got, relayed, wants[name])
			continue
		}
		for _, r := range routed {
			if r.Metadata[code.MetaWorkID] != item.ID || r.Metadata[code.MetaWorkKey] != item.Key {
				t.Errorf("%s wakes %s naming work item %q (%q), want %s", name, r.To.Handle,
					r.Metadata[code.MetaWorkID], r.Metadata[code.MetaWorkKey], item.Key)
			}
		}
	}
}

// brokenWorkItemLanding is the position of the broken record marking the
// landing of a change dev opened for item, on a repository of its own whose
// main queues every merge on its check unit: ada's override merge lands it
// ahead of that check, which then fails on the landed tip.
func brokenWorkItemLanding(t *testing.T, n *node, c *code.Committer, w *code.ChangeWriter,
	item *types.WorkItem) statelog.Position {

	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo := n.ensure("api", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	converse(t, w, n.revising(repo, opened.ChangeID, 1, oidOf("B"), mainRef, nil))
	override := merging(repo, opened.ChangeID, ada, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	n.checked(repo, oidOf("M"), mainRef, "unit", 1, code.CheckStateFailed)
	res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Broken = &code.Broken{RepoID: repo, TargetRef: mainRef, Position: merged.Position, Ordinal: 1,
			OID: oidOf("M"), Checks: []string{"unit"}}
	}))
	return requireRecorded(t, res, err)
}

// mergedWorkItemChange is the position of cto's merge of a change dev opened
// for item, on a repository of its own whose main asks one approval and no
// check.
func mergedWorkItemChange(t *testing.T, n *node, c *code.Committer, w *code.ChangeWriter,
	item *types.WorkItem) statelog.Position {

	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = code.DirectPersons, mergeGate()
	repo := n.ensure("site", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(seatRef, code.ZeroOID, oidOf("B"))))
	open := opening(repo, seatRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	approved(t, n, w, repo, opened.ChangeID)
	res, err := c.ProposeMerge(t.Context(), merging(repo, opened.ChangeID, cto, code.MergeMethodMerge, oidOf("A"),
		oidOf("B"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))
	return res.Position
}
