package code_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT A SEAT'S DAY COUNTS OF ITS CHANGES AND REVIEWS IS ANNOUNCED ONCE, BY THE
// NODE THAT LANDED IT: an opened change (`change_opened`) by the committer whose
// record opened it, from a change tool's open or a push's options alike, and a
// review (`review_submitted`) by the change writer whose record landed it —
// each naming every seat it is counted on by the agent id the chart gives it,
// a person by none — and never again for a retry its operation ledger
// collapsed onto the record.

// agentIDs is the grant tests' chart as the engine resolves it for an
// announcement: "agent-<handle>" for each agent seat it holds, and nothing for
// a person or a handle it does not hold.
func agentIDs(handle string) (string, bool) {
	s, ok := testChart().Seat(handle)
	if !ok || s.Human {
		return "", false
	}
	return "agent-" + handle, true
}

// announcing tunes a committer to announce on stream.
func announcing(stream *announced) func(*code.CommitterOptions) {
	return func(o *code.CommitterOptions) { o.Events, o.AgentIDs = stream, agentIDs }
}

// requireOnce fails t unless stream holds exactly one event of eventType once
// a retry's would have arrived.
func requireOnce(t *testing.T, stream *announced, eventType string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if got := len(stream.of(eventType)); got != 1 {
		t.Fatalf("%s was announced %d times, want once: a retry the ledger collapsed is its landing's", eventType,
			got)
	}
}

// AN OPENED CHANGE IS ANNOUNCED ONCE, NAMING ITS OWNER'S SEAT: dev's open of
// feature into main announces web!1 from this node, owned by dev and named by
// dev's agent id; the same open asked again collapses onto it and announces
// nothing; and a person's open names no agent id, since a person's day is no
// seat's.
//
// Mutation: announce a call the ledger collapsed (drop `!res.Collapsed`) —
// the open asked again announces a second time, red; name the owner without
// resolving it (Agent "") — dev's day counts nothing, red.
func TestAnOpenedChangeIsAnnouncedOnceOnItsOwnersSeat(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := &announced{}
	c, _ := committerOn(t, n, n.pub, announcing(stream))
	repo := changeRepo(t, n, "web")

	p := opening(repo, featureRef, mainRef, dev)
	res, err := c.ProposeChange(t.Context(), p)
	requireChanged(t, res, err)
	waitAnnouncedOf(t, stream, "change_opened", 1)
	ev := stream.of("change_opened")[0]
	if got, ok := ev.Data.(*types.ChangeOpened); !ok || ev.Source != n.id || *got != (types.ChangeOpened{
		RepoID: repo, RepoName: "web", ChangeID: res.ChangeID, Number: 1, Target: mainRef,
		Title: "Retry the upload", Owner: "dev", OwnerKind: "agent", Agent: "agent-dev"}) {
		t.Fatalf("the open was announced as %+v from %q, want dev's web!1 into main named by dev's agent id, "+
			"from %s", ev.Data, ev.Source, n.id)
	}

	again, err := c.ProposeChange(t.Context(), p)
	requireChanged(t, again, err)
	requireOnce(t, stream, "change_opened")

	person, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, ada))
	requireChanged(t, person, err)
	waitAnnouncedOf(t, stream, "change_opened", 2)
	if got := stream.of("change_opened")[1].Data.(*types.ChangeOpened); got.Owner != "founder" ||
		got.OwnerKind != "person" || got.Agent != "" || got.Number != 2 {
		t.Fatalf("a person's open was announced as %+v, want founder's web!2 named by no agent id", got)
	}
}

// A PUSH THAT OPENS A CHANGE ANNOUNCES IT ONCE, as the push's committer lands
// it: dev's push asking for a change announces the change it opened, by push,
// named by dev's agent id; the same push proposed again is answered from the
// row its first record wrote — collapsed — and announces nothing.
//
// Mutation: announce a push answered collapsed (drop `r.Answer.Collapsed`
// from pushOpenings) — the push proposed again announces a second time, red.
func TestAPushThatOpensAChangeAnnouncesItOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := &announced{}
	c, _ := committerOn(t, n, n.pub, announcing(stream))
	repo := changeRepo(t, n, "web")

	p, res := pushOpening(t, c, repo, dev, &code.ChangeOption{Title: "Retry the upload"},
		update(featureRef, oidOf("B"), oidOf("C")))
	requireOK(t, answer{res, nil})
	waitAnnouncedOf(t, stream, "change_opened", 1)
	if got := stream.of("change_opened")[0].Data.(*types.ChangeOpened); got.ChangeID != code.PushChangeID(p.PushID) ||
		!got.ByPush || got.Owner != "dev" || got.Agent != "agent-dev" || got.Number != 1 {
		t.Fatalf("the push's change was announced as %+v, want dev's web!1, opened by its push", got)
	}

	if again, err := c.Propose(t.Context(), p); err != nil || !again.Answer.Collapsed {
		t.Fatalf("the push proposed again answered %+v, %v; want its first record's row, collapsed", again, err)
	}
	requireOnce(t, stream, "change_opened")
}

// A REVIEW IS ANNOUNCED ONCE, NAMING THE SEAT THAT GAVE IT AND THE SEAT THAT
// RECEIVED ITS FINDINGS: plat-lead's review of dev's web!1 raising two findings
// announces them both by their agent ids, the revision it judged and its
// verdict; the same review asked again collapses and announces nothing; and a
// person's review names no reviewer agent, while its findings still count on
// the change's owner.
//
// Mutation: announce a collapsed write (drop `!res.Collapsed` from the
// writer's fresh) — the review asked again announces twice, red; name the
// owner's agent from the reviewer (OwnerAgent: e.Agent) — the owner seat's
// findings are counted on the reviewer, red.
func TestAReviewIsAnnouncedOnceOnItsReviewerAndOwner(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	stream := &announced{}
	w, _ := writerOn(t, n, n.pub, func(o *code.ChangeWriterOptions) { o.Events, o.AgentIDs = stream, agentIDs })

	cw := reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 1, "No backoff"), raising(code.SeverityMinor, 2, "No jitter"))
	res := converse(t, w, cw)
	waitAnnouncedOf(t, stream, "review_submitted", 1)
	ev := stream.of("review_submitted")[0]
	if got, ok := ev.Data.(*types.ReviewSubmitted); !ok || ev.Source != n.id || *got != (types.ReviewSubmitted{
		RepoID: repo, RepoName: "web", ChangeID: change, Number: 1, ReviewID: res.ID, Revision: 1,
		Reviewer: "plat-lead", ReviewerKind: "agent", Agent: "agent-plat-lead", Verdict: "request_changes",
		Findings: 2, Owner: "dev", OwnerKind: "agent", OwnerAgent: "agent-dev"}) {
		t.Fatalf("the review was announced as %+v from %q, want plat-lead's two findings on dev's web!1, each "+
			"seat by its agent id, from %s", ev.Data, ev.Source, n.id)
	}

	if _, err := w.Converse(t.Context(), cw); err != nil {
		t.Fatalf("the review asked again: %v", err)
	}
	requireOnce(t, stream, "review_submitted")

	converse(t, w, reviewing(repo, change, ada, 1, code.VerdictComment, nil, raising(code.SeverityNit, 3, "Naming")))
	waitAnnouncedOf(t, stream, "review_submitted", 2)
	if got := stream.of("review_submitted")[1].Data.(*types.ReviewSubmitted); got.Reviewer != "founder" ||
		got.ReviewerKind != "person" || got.Agent != "" || got.OwnerAgent != "agent-dev" || got.Findings != 1 {
		t.Fatalf("a person's review was announced as %+v, want founder's, named by no agent id, its finding "+
			"on dev's", got)
	}
}

// A WRITER THAT ANNOUNCES NAMES ITS SEATS: a committer, a change writer or a
// checks writer handed an event stream and no way to resolve a seat's agent
// id is refused, since every opening, review, ejection and failed head check
// it announced would be counted on no seat's day.
func TestAnAnnouncingWriterResolvesItsSeats(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	o := code.CommitterOptions{Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id, Limits: code.DefaultLimits,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{}, false }, Events: &announced{}}
	if _, err := code.NewCommitter(o); err == nil {
		t.Fatal("a committer announcing with no AgentIDs was built")
	}
	wo := code.ChangeWriterOptions{Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{}, false }, Events: &announced{}}
	if _, err := code.NewChangeWriter(wo); err == nil {
		t.Fatal("a change writer announcing with no AgentIDs was built")
	}
	co := code.ChecksWriterOptions{Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{}, false }, Events: &announced{}}
	if _, err := code.NewChecksWriter(co); err == nil {
		t.Fatal("a checks writer announcing with no AgentIDs was built")
	}
}

// A FAILED HEAD CHECK IS ANNOUNCED ON ITS OWNER'S SEAT: the ending of a check
// of dev's change's head that fails it names dev — the change's owner, by the
// agent id its day is filed under — and so does an errored attempt past the
// home's own requeue; an ending that passes, and an errored one the home
// queues again, name nobody, since no day counts them (`checks_failed`).
//
// Mutation: name the owner on every head ending — the passing one names dev,
// red.
func TestAFailedHeadCheckIsAnnouncedOnItsOwnersSeat(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _, stream, _ := checksWriterOn(t, n)
	key := func(name string, attempt int) code.CheckKey {
		return code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: name, Attempt: attempt}
	}
	runCheck(t, w, key("unit", 1), change, 1, code.CheckStateFailed, code.CheckReasonExit)
	runCheck(t, w, key("lint", 1), change, 1, code.CheckStatePassed, code.CheckReasonExit)
	for attempt := 1; attempt <= code.AutoRequeueAttempts; attempt++ {
		runCheck(t, w, key("e2e", attempt), change, 1, code.CheckStateErrored, code.CheckReasonRunnerLost)
	}
	waitAnnouncedOf(t, stream, "code_check_finished", 2+code.AutoRequeueAttempts)
	for _, ev := range stream.of("code_check_finished") {
		p := ev.Data.(*types.CodeCheckFinished)
		fails := p.State == "failed" || p.Attempt == code.AutoRequeueAttempts
		owned := p.Owner == "dev" && p.OwnerKind == "agent" && p.Agent == "agent-dev"
		if owned != fails || (!fails && p.Owner+p.Agent != "") {
			t.Errorf("%s attempt %d (%s) names owner %q, agent %q: want dev's seat named exactly where the "+
				"ending fails the head", p.Name, p.Attempt, p.State, p.Owner, p.Agent)
		}
	}
}
