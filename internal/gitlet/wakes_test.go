package gitlet_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM THE GATE'S RECORDS CONCERN (concerns.go, "Who hears what the gate
// does"): a head check's ending, a merge, a dequeue, a queue's ejection and
// landing and a broken landing each carry the routing their own writer
// stamped, and the wakes they cause are read from it alone.

// headAttempt asks for attempt key of change's revision's head queued for
// reason, under the target's checks file.
func headAttempt(key gitlet.CheckKey, change string, revision int, reason gitlet.QueueReason) gitlet.ChecksRequest {
	req := queueing(key, reason)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = gitlet.PurposeHead, change, revision
	req.Queued.DefsOID = oidOf("main-checks")
	return req
}

// runCheck queues, claims and ends attempt key of change's revision's head
// through w — ended in state for why — and is the ending's position.
func runCheck(t *testing.T, w *gitlet.ChecksWriter, key gitlet.CheckKey, change string, revision int,
	state gitlet.CheckState, why gitlet.CheckReason) statelog.Position {

	t.Helper()
	reason := gitlet.QueueDispatch
	if key.Attempt > 1 {
		reason = gitlet.QueueRequeue
	}
	var ended statelog.Position
	for _, req := range []gitlet.ChecksRequest{headAttempt(key, change, revision, reason),
		claiming(key, "git-a", ""), ending(key, state, why)} {
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, key.Name+" at the head", res, err)
		ended = res.Position
	}
	return ended
}

// heardSorted is heard, sorted: who a routing tells, whatever order its
// lists hold them in.
func heardSorted(concerns []gitlet.Concern) []string {
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
// the head the change has moved past measures code nobody is asked to fix. A
// failure a person's CI reports (M5-D24) is the same failure of the same head
// — the gate leaves the merge unmet on it alike — so it tells the same
// authors, the reporting person as its actor.
//
// Mutations: Reason.WakesActor false for checks_failed — ada, the actor, is
// not told, red; route every ending of a head check, whatever its state — the
// passing one tells the authors, red; drop the attempt bound from
// failsItsHead — the third errored attempt tells nobody, red; route no
// reported ending (decideChecks returns decideReport's decision as it is) —
// the CI's failure tells nobody, red.
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
	key := func(name, oid string, attempt int) gitlet.CheckKey {
		return gitlet.CheckKey{RepoID: repo, OID: oidOf(oid), Target: mainRef, Name: name, Attempt: attempt}
	}

	failed := runCheck(t, w, key("unit", "B2", 1), change, 2, gitlet.CheckStateFailed, gitlet.CheckReasonExit)
	routings := routingAt(t, n, failed)
	if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].ChecksFailed ||
		routings[0].Actor != "ada" {
		t.Fatalf("the failed check's record carries %+v, want web!1's checks failed, its actor ada", routings)
	}
	concerns := gitlet.Concerns(routings)
	if got := heardSorted(concerns); !slices.Equal(got, []string{"ada:checks_failed", "dev:checks_failed"}) {
		t.Fatalf("the failed check tells %v, want dev and ada — who pushed the head — that it failed", got)
	}

	for name, p := range map[string]statelog.Position{
		"a passing check": runCheck(t, w, key("lint", "B2", 1), change, 2, gitlet.CheckStatePassed,
			gitlet.CheckReasonExit),
		"an errored attempt the home queues again": runCheck(t, w, key("e2e", "B2", 1), change, 2,
			gitlet.CheckStateErrored, gitlet.CheckReasonRunnerLost),
		"a failed check of the head before": runCheck(t, w, key("unit", "B", 1), change, 1,
			gitlet.CheckStateFailed, gitlet.CheckReasonExit),
	} {
		if routings := routingAt(t, n, p); len(routings) != 0 {
			t.Fatalf("%s carries %+v, want no routing: it tells nobody", name, routings)
		}
	}
	runCheck(t, w, key("e2e", "B2", 2), change, 2, gitlet.CheckStateErrored, gitlet.CheckReasonRunnerLost)
	last := runCheck(t, w, key("e2e", "B2", gitlet.AutoRequeueAttempts), change, 2, gitlet.CheckStateErrored,
		gitlet.CheckReasonRunnerLost)
	if got := heardSorted(gitlet.Concerns(routingAt(t, n, last))); !slices.Equal(got,
		[]string{"ada:checks_failed", "dev:checks_failed"}) {
		t.Fatalf("the attempt the home's requeue stops at tells %v, want the authors and ada", got)
	}

	// A PERSON'S CI's FAILURE OF THE HEAD tells the same authors: its row
	// names no change, and the gate reads it as the engine's own, so the
	// change is the live one into main at B2 — and its actor is the person
	// whose CI reported it, founder, told as the head's pusher is. Their CI's
	// pass tells nobody, and its failure of the head the change moved past
	// measures code nobody is asked to fix.
	report := func(name, oid string, state gitlet.CheckState) statelog.Position {
		res, err := w.Publish(t.Context(), reporting(key(name, oid, 1), state, "https://ci.example.com/1"))
		requireChecked(t, name+" reported "+string(state), res, err)
		return res.Position
	}
	reported := routingAt(t, n, report("ci", "B2", gitlet.CheckStateFailed))
	if len(reported) != 1 || reported[0].ChangeID != change || !reported[0].ChecksFailed ||
		reported[0].Actor != "founder" {
		t.Fatalf("the failure founder's CI reported carries %+v, want web!1's checks failed, its actor founder",
			reported)
	}
	if got := heardSorted(gitlet.Concerns(reported)); !slices.Equal(got,
		[]string{"ada:checks_failed", "dev:checks_failed", "founder:checks_failed"}) {
		t.Fatalf("the failure founder's CI reported tells %v, want the authors and founder", got)
	}
	for name, p := range map[string]statelog.Position{
		"a reported pass": report("ci-lint", "B2", gitlet.CheckStatePassed),
		"a reported failure of the head the change moved past": report("ci", "B", gitlet.CheckStateFailed),
	} {
		if routings := routingAt(t, n, p); len(routings) != 0 {
			t.Fatalf("%s carries %+v, want no routing: it tells nobody", name, routings)
		}
	}
}

// A FAILED CHECK WAKES FOR WHOM ITS HEAD WAS PUSHED (ADR-0109): a failed check
// of a change's head is news about the push that made the head, whose pusher
// the routing names (TestAFailedCheckWakesItsAuthorsAndItsActor), so where a
// seat pushed that head in a turn it worked for somebody, the failure is that
// somebody's errand. The seat cto moves dev's change's head to B2, its
// revision 2, in a turn founder woke: a check of B2 that fails tells the
// change's authors — dev, and cto, who pushed the head and hears of it as its
// actor — through a routing carrying founder beside cto, at version 2, and
// both wakes are written by cto for founder, cto's own included. The same
// failure reported by founder's CI is the person's own act, made for nobody
// but themselves: its routing carries nobody, at version 1, whatever the head
// was pushed for.
//
// Mutation: revisionPusher reading no carrier — the failure's routing carries
// nobody and its wakes are written for nobody, red.
func TestAFailedCheckWakesForWhomItsHeadWasPushed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	moved := proposal(repo, update(featureRef, oidOf("B"), oidOf("B2")))
	moved.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto",
		Run: gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String()}}
	moved.Via, moved.WrittenFor = gitlet.ViaEngine, types.ForTurn(founder)
	res, err := c.Propose(t.Context(), moved)
	requireOK(t, answer{res, err})
	w, _, _, _ := checksWriterOn(t, n)
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("B2"), Target: mainRef, Name: "unit", Attempt: 1}

	failed := runCheck(t, w, key, change, 2, gitlet.CheckStateFailed, gitlet.CheckReasonExit)
	routings := routingAt(t, n, failed)
	if len(routings) != 1 || routings[0].Actor != "cto" || !routings[0].ChecksFailed {
		t.Fatalf("the failed check's record carries %+v, want web!1's checks failed, its actor cto", routings)
	}
	if f, v := routings[0].WorkingFor, n.recordAt(failed).V; !reflect.DeepEqual(f, founder) ||
		v != gitlet.WorkingForVersion {
		t.Errorf("the failed check's routing carries %+v at version %d, want founder, for whom cto pushed the "+
			"head, at %d", f, v, gitlet.WorkingForVersion)
	}
	_, relayed, routed := wakesOf(t, feedRecord(t, n, failed))
	if got := woken(routed); !relayed || !slices.Equal(slices.Sorted(slices.Values(got)),
		[]string{"cto:checks_failed", "dev:checks_failed"}) {
		t.Fatalf("the failed check wakes %v (relayed %v), want dev, its owner, and cto, who pushed its head",
			got, relayed)
	}
	byCTOForFounder := types.NotificationWriter{Kind: iam.ActorAgent, Seat: "cto", WorkingFor: founder}
	for _, r := range routed {
		if r.Writer == nil || !reflect.DeepEqual(*r.Writer, byCTOForFounder) {
			t.Errorf("%s's wake is written by %s, want %s: the head failing is founder's errand", r.To.Handle,
				writtenBy(r.Writer), writtenBy(&byCTOForFounder))
		}
	}

	// THE SAME FAILURE FOUNDER'S CI REPORTS: a person's own act.
	reportKey := key
	reportKey.Name = "ci"
	reported, err := w.Publish(t.Context(), reporting(reportKey, gitlet.CheckStateFailed, "https://ci.example.com/1"))
	requireChecked(t, "ci reported failed", reported, err)
	routings = routingAt(t, n, reported.Position)
	switch {
	case len(routings) != 1 || routings[0].Actor != "founder" || !routings[0].ChecksFailed:
		t.Fatalf("the failure founder's CI reported carries %+v, want web!1's checks failed, its actor founder",
			routings)
	case routings[0].WorkingFor != nil || n.recordAt(reported.Position).V != 1:
		t.Errorf("the failure founder's CI reported carries %+v at version %d, want nobody at 1: a person "+
			"reports for nobody but themselves", routings[0].WorkingFor, n.recordAt(reported.Position).V)
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	override := merging(repo, change, ada, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	landed := merged.Position

	n.checked(repo, oidOf("M"), mainRef, "unit", 1, gitlet.CheckStateFailed)
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Broken = &gitlet.Broken{RepoID: repo, TargetRef: mainRef, Position: landed, Ordinal: 1, OID: oidOf("M"),
			Checks: []string{"unit"}}
	}))
	broken := requireRecorded(t, res, err)
	routings := routingAt(t, n, broken)
	if len(routings) != 1 || routings[0].ChangeID != change || !routings[0].Broken || routings[0].Actor != "" ||
		routings[0].Enqueuer != "" || routings[0].Lead != "cto" {
		t.Fatalf("the broken record carries %+v, want web!1 broken, no enqueuer, its lead cto, made by nobody",
			routings)
	}
	if got := heard(gitlet.Concerns(routings)); !slices.Equal(got,
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
	for mergeBy, want := range map[gitlet.MergeBy]string{
		gitlet.MergeByWriters:     "dev:ready_to_merge",
		gitlet.MergeByMaintainers: "cto:ready_to_merge",
	} {
		n := newNode(t)
		n.chart = testChart()
		c, _ := committerOn(t, n, n.pub)
		w, _ := writerOn(t, n, n.pub)
		cw, _, _, _ := checksWriterOn(t, n)
		s := suiteSettings()
		s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
		gate := queueGate()
		gate.MergeBy = mergeBy
		s.Protections[0].Gate = gate
		repo := n.ensure("web", s, t1).RepoID
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
			set(featureRef, gitlet.ZeroOID, oidOf("B"))))
		opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
		requireChanged(t, opened, err)
		change := opened.ChangeID
		converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
		converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil))
		converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
		if n.storedReadiness(change).ready {
			t.Fatalf("merge by %s: a change its required check has not passed on is stored ready", mergeBy)
		}

		passed := runCheck(t, cw, gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit",
			Attempt: 1}, change, 1, gitlet.CheckStatePassed, gitlet.CheckReasonExit)
		if routings := routingAt(t, n, passed); len(routings) != 0 {
			t.Fatalf("merge by %s: the passing check's record carries %+v, want no routing: only the home's "+
				"mergeability record announces a change ready", mergeBy, routings)
		}
		refresh := converse(t, w, gitlet.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
		if !n.storedReadiness(change).ready {
			t.Fatalf("merge by %s: the refresh after the passing check left the change not ready", mergeBy)
		}
		if got := heard(gitlet.Concerns(routingAt(t, n, refresh.Position))); !slices.Equal(got, []string{want}) {
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
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ENG-7")).String(),
		Key: "ENG-7", Project: "ENG"}
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID

	records := map[string]statelog.Position{}
	wants := map[string][]string{}
	records["a failed head check"] = runCheck(t, cw, gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef,
		Name: "lint", Attempt: 1}, change, 1, gitlet.CheckStateFailed, gitlet.CheckReasonExit)
	wants["a failed head check"] = []string{"dev:checks_failed"}
	ready(t, n, w, repo, change)

	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, enqueue.Enqueue.EntryID, ada))
	records["a dequeue"] = requireRecorded(t, res, err)
	wants["a dequeue"] = []string{"dev:dequeued"}

	enqueue = enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: enqueue.Enqueue.EntryID,
			Reason: gitlet.EjectConflict}}}
	}))
	records["an ejection"] = requireRecorded(t, res, err)
	wants["an ejection"] = []string{"dev:conflicted"}

	enqueue = enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.passed(repo, oidOf("M"), mainRef, "unit")
	res, err = c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")}))
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
			if r.Metadata[gitlet.MetaWorkID] != item.ID || r.Metadata[gitlet.MetaWorkKey] != item.Key {
				t.Errorf("%s wakes %s naming work item %q (%q), want %s", name, r.To.Handle,
					r.Metadata[gitlet.MetaWorkID], r.Metadata[gitlet.MetaWorkKey], item.Key)
			}
		}
	}
}

// brokenWorkItemLanding is the position of the broken record marking the
// landing of a change dev opened for item, on a repository of its own whose
// main queues every merge on its check unit: ada's override merge lands it
// ahead of that check, which then fails on the landed tip.
func brokenWorkItemLanding(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter,
	item *types.WorkItem) statelog.Position {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo := n.ensure("api", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	converse(t, w, n.revising(repo, opened.ChangeID, 1, oidOf("B"), mainRef, nil))
	override := merging(repo, opened.ChangeID, ada, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	n.checked(repo, oidOf("M"), mainRef, "unit", 1, gitlet.CheckStateFailed)
	res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Broken = &gitlet.Broken{RepoID: repo, TargetRef: mainRef, Position: merged.Position, Ordinal: 1,
			OID: oidOf("M"), Checks: []string{"unit"}}
	}))
	return requireRecorded(t, res, err)
}

// mergedWorkItemChange is the position of cto's merge of a change dev opened
// for item, on a repository of its own whose main asks one approval and no
// check.
func mergedWorkItemChange(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter,
	item *types.WorkItem) statelog.Position {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, mergeGate()
	repo := n.ensure("site", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(seatRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, seatRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	approved(t, n, w, repo, opened.ChangeID)
	res, err := c.ProposeMerge(t.Context(), merging(repo, opened.ChangeID, cto, gitlet.MergeMethodMerge, oidOf("A"),
		oidOf("B"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))
	return res.Position
}
