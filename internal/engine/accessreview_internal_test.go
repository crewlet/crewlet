package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// AN AGENT WHOSE ACCESS CHANGED REVIEWS ITS OPEN WORK ONCE (ADR-0111): what
// the node holding a seat measures after an apply, what it asks for, and what
// the dispatcher does with a review that has nothing left to tell.

// sreAccess is a company with Cloudflare connected and its SRE agent on it at
// level, the shape of an administrator lowering an agent's level.
func sreAccess(level string) *config.Company {
	on := true
	return &config.Company{
		Name: "Example", Mission: "Ship it.",
		Integrations:       config.Integrations{Cloudflare: &config.Cloudflare{}},
		SeatAccessMigrated: []string{"cloudflare"},
		Roles: []config.Role{{
			Name: "Site Reliability Engineer", Handle: "sre", Kind: org.KindAgent,
			Integrations: config.RoleIntegrations{
				Cloudflare: &config.RoleCloudflare{Enabled: &on, Level: level},
			},
		}},
	}
}

// sreObserved is the SRE's Cloudflare account, made and on.
var sreObserved = map[seataccess.Lane]seataccess.Observed{
	seataccess.LaneCloudflare: {Exists: true, Enabled: true},
}

// sreSnapshot is the SRE's access with Cloudflare at level.
func sreSnapshot(c *config.Company) capability.Snapshot {
	return capability.Take(capability.Input{Company: c, Handle: "sre", Observed: sreObserved})
}

// toldAs is a seat's access log with snap told by turn as its baseline.
func toldAs(turn string, snap capability.Snapshot) accessdomain.Recent {
	return accessdomain.Recent{Complete: true, Baseline: &accessdomain.ToldRow{
		Handle: "sre", Turn: turn, Digest: snap.Digest(), Snapshot: snap.JSON(), Latest: true,
	}}
}

// someWork is open work to review: one question asked, one held call.
func someWork(context.Context, *Company, string) (accessreview.Work, error) {
	return accessreview.Work{Asks: []string{"ENG-12"},
		Runs: []accessreview.Run{{Turn: "t-held", Tool: "ssh_operate", Server: "ssh"}}, Open: 2}, nil
}

// noWork is a seat with nothing open.
func noWork(context.Context, *Company, string) (accessreview.Work, error) {
	return accessreview.Work{}, nil
}

// captured is a publisher that keeps what it was asked to publish.
type captured struct {
	mu   sync.Mutex
	reqs []accessreview.Request
}

func (c *captured) publish(_ context.Context, req accessreview.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return nil
}

func (c *captured) got() []accessreview.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]accessreview.Request(nil), c.reqs...)
}

// withReviews arms e's review debounce with a timer that never fires on its
// own, and answers the timers it armed.
func withReviews(t *testing.T, e *Engine) *[]func() {
	t.Helper()
	var armed []func()
	var mu sync.Mutex
	r := newAccessReviews(t.Context(), e.checkAccessReviews)
	r.after = func(_ time.Duration, f func()) func() bool {
		mu.Lock()
		defer mu.Unlock()
		armed = append(armed, f)
		return func() bool { return true }
	}
	e.accessReviews.Store(r)
	return &armed
}

// clock is a seam's clock that moves a second on every read.
func clock() func() time.Time {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

// seamsOf are e's review seams with the work, publisher and clock a case states.
func seamsOf(e *Engine, work func(context.Context, *Company, string) (accessreview.Work, error),
	pub *captured) reviewSeams {

	s := e.reviewSeams()
	s.work, s.publish, s.now = work, pub.publish, clock()
	return s
}

// A LEVEL CHANGE ASKS FOR EXACTLY ONE REVIEW. A lowered level is a change the
// closed list counts, measured from the told record, and a seat checked twice
// for one change is asked once, under one wake id another node derives too.
//
// Mutation: drop the asked memo and the second check publishes again.
func TestALevelChangeAsksForExactlyOneReview(t *testing.T) {
	t.Parallel()
	m, owed := measured(sreSnapshot(sreAccess("read")), toldAs("t-told", sreSnapshot(sreAccess("operate"))))
	if !owed || m.base.Turn != "t-told" || len(m.seatLines) != 1 ||
		!strings.Contains(m.seatLines[0], "Cloudflare: your level is now") ||
		!strings.Contains(m.personLines[0], "Cloudflare: its level is now") {
		t.Fatalf("a lowered level measures %+v (%v), want one line in each voice", m, owed)
	}

	e, _ := aRunningNode(t)
	withReviews(t, e)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	seams := seamsOf(e, someWork, pub)
	for range 2 {
		if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
			t.Fatalf("reviewSeat: %v", err)
		}
	}
	got := pub.got()
	if len(got) != 1 || got[0].Baseline != turnID || got[0].Open != 2 || len(got[0].Lines) != 1 {
		t.Fatalf("two checks of one change asked for %+v, want one measured from %s", got, turnID)
	}
	pending, found, err := e.PendingAccessReview(t.Context(), "ceo")
	if err != nil || !found || accessreview.WakeID(accessreview.RequestFor("ceo", pending, nil, 0)) !=
		accessreview.WakeID(got[0]) {
		t.Errorf("the recorded change %+v (%v, %v) derives another wake than the one asked", pending, found, err)
	}
}

// A CHANGE MADE, REVERTED AND MADE AGAIN IS REVIEWED AGAIN, with no turn in
// between: the revert drops the untold record, and the redo is measured afresh
// under a wake id of its own.
//
// Mutation: keep the record across the revert and the redo is never asked.
func TestAChangeRevertedAndMadeAgainIsReviewedAgain(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	withReviews(t, e)
	before := capabilityCompany(t, "Ship small changes.")
	first, turnID := dispatchTurn(t, e, before)
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	seams := seamsOf(e, someWork, pub)
	for _, c := range []*Company{after, before, after} {
		if err := e.reviewSeat(t.Context(), c, "ceo", seams); err != nil {
			t.Fatalf("reviewSeat: %v", err)
		}
	}
	got := pub.got()
	if len(got) != 2 || accessreview.WakeID(got[0]) == accessreview.WakeID(got[1]) {
		t.Fatalf("a change, its revert and the change again asked for %d reviews, want 2 under two wakes", len(got))
	}
}

// THREE QUICK REVISIONS ASK FOR ONE REVIEW: every apply restarts the quiet
// window, so a run of saves is measured once.
//
// Mutation: arm a timer per kick without stopping the one before.
func TestThreeQuickRevisionsAskForOneReview(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		checks  []bool
		armed   []func()
		stopped = map[int]bool{}
	)
	r := newAccessReviews(t.Context(), func(_ context.Context, _ []string, all bool) []string {
		mu.Lock()
		defer mu.Unlock()
		checks = append(checks, all)
		return nil
	})
	r.after = func(_ time.Duration, f func()) func() bool {
		mu.Lock()
		defer mu.Unlock()
		armed = append(armed, f)
		i := len(armed) - 1
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			stopped[i] = true
			return true
		}
	}
	for range 3 {
		r.kick()
	}
	mu.Lock()
	if len(armed) != 3 || !stopped[0] || !stopped[1] || stopped[2] {
		mu.Unlock()
		t.Fatalf("three applies armed %d windows with %v stopped, want the last one standing",
			len(armed), stopped)
	}
	last := armed[2]
	mu.Unlock()
	last()
	mu.Lock()
	defer mu.Unlock()
	if len(checks) != 1 || !checks[0] {
		t.Errorf("the quiet window ran %v checks, want one of every seat held", checks)
	}
}

// A LEDGER WRITE MEASURES THE SEAT IT NAMES: access a sync or a vendor step
// changes, with no revision, is measured too.
//
// Mutation: hand out the ledger unwrapped and the write asks for nothing.
func TestALedgerWriteMeasuresTheSeatItNames(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	armed := withReviews(t, e)
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatalf("SeatAccessLedger: %v", err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{Kind: integration.KindCloudflare,
		Handle: "sre"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := e.accessReviews.Load()
	r.mu.Lock()
	named, timers := r.handles["sre"], len(*armed)
	r.mu.Unlock()
	if !named || timers != 1 {
		t.Errorf("a ledger write named sre %v and armed %d windows, want the seat measured once", named, timers)
	}
}

// A PAUSED SEAT REVIEWS WHEN IT RESUMES: its review wake waits on the inbox
// like any mail, parked while paused and dispatched as a review once resumed.
//
// Mutation: drop a review addressed to a paused seat.
func TestAPausedSeatReviewsWhenItResumes(t *testing.T) {
	t.Parallel()
	ready := inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
	paused := ready
	paused.Paused = true
	held := inbox.Screen(paused, []*events.Event{reviewWake()})
	if held.Action != inbox.ActionPauseAndPark || len(held.Events) != 1 {
		t.Fatalf("a review reaching a paused seat is %s with %d events, want parked whole",
			held.Action, len(held.Events))
	}
	resumed := inbox.Screen(ready, held.Events)
	if resumed.Action != inbox.ActionProceed || !accessReviewsOnly(resumed.Events) {
		t.Fatalf("the parked review on the seat's resume is %s, want dispatched as a review", resumed.Action)
	}
	ran := 0
	d := &Dispatcher{
		Completions:   ledgerstore.NewMemoryCompletions(),
		ReviewSettled: func(context.Context, string) bool { return false },
		Turn: func(context.Context, Request) (turn.Result, error) {
			ran++
			return turn.Result{}, nil
		},
	}
	if res := d.Dispatch(t.Context(), "sre", resumed.Events); res.Outcome != queue.OutcomeAck || ran != 1 {
		t.Errorf("the resumed seat's review ran %d turns (%v), want 1", ran, res.Outcome)
	}
}

// A SETTLED REVIEW IS ACKED WITHOUT A TURN; one with a change to tell runs.
//
// Mutation: drop the settled check and a seat told twice reviews twice.
func TestASettledReviewIsAckedWithoutATurn(t *testing.T) {
	t.Parallel()
	ran := 0
	settled := true
	d := &Dispatcher{
		Completions:   ledgerstore.NewMemoryCompletions(),
		ReviewSettled: func(context.Context, string) bool { return settled },
		Turn: func(context.Context, Request) (turn.Result, error) {
			ran++
			return turn.Result{}, nil
		},
	}
	if res := d.Dispatch(t.Context(), "sre", []*events.Event{reviewWake()}); res.Outcome != queue.OutcomeAck || ran != 0 {
		t.Fatalf("a settled review ran %d turns (%v), want it acked unrun", ran, res.Outcome)
	}
	settled = false
	if d.Dispatch(t.Context(), "sre", []*events.Event{reviewWake()}); ran != 1 {
		t.Errorf("a review with a change to tell ran %d turns, want 1", ran)
	}
}

// reviewWake is a review's wake as the notify spine delivers it.
func reviewWake() *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: accessreview.Source, SourceEventType: accessreview.EventType,
		Subject: accessreview.Subject, Body: accessreview.Task,
	}, events.NewTrace())
}

// THE REVIEW TURN'S TOLD RECORD STOPS THE NEXT TURN REPEATING THE CHANGE: from
// its launch the seat owes no review and its next turn is told nothing.
//
// Mutation: tell the review turn the change without writing its told record.
func TestAReviewTurnsToldRecordStopsTheNextTurnRepeatingIt(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	if _, owed, err := e.measureAccess(t.Context(), after, "ceo"); err != nil || !owed {
		t.Fatalf("the premise: the changed seat owes no review (%v)", err)
	}
	if e.reviewSettled(t.Context(), after, "ceo") {
		t.Fatal("the premise: a review with a change to tell reads as settled")
	}
	review, reviewTurn := dispatchTurn(t, e, after)
	if !strings.Contains(review.changes, "Only the SRE operates production.") {
		t.Fatalf("the review turn is told:\n%s", review.changes)
	}
	launchTurn(t, e, review, reviewTurn)
	if _, owed, err := e.measureAccess(t.Context(), after, "ceo"); err != nil || owed {
		t.Errorf("after the review turn launched the seat owes a review (%v)", err)
	}
	if !e.reviewSettled(t.Context(), after, "ceo") {
		t.Error("after the review turn launched a second review of the change is not settled")
	}
	if next, _ := dispatchTurn(t, e, after); next.changes != "" {
		t.Errorf("the turn after the review is told:\n%s", next.changes)
	}
}

// AN ASK RAISED BEFORE A CHANGE CARRIES THE NOTE UNTIL THE REVIEW ENDS. A
// question asked before the change is stale from the change's record on; one
// asked after the review turn was told it is not; and once the turn told it
// ends, the record goes and no question is noted. Another turn's end, or a
// later check, leaves it.
//
// Mutation: drop the record at the told record's write rather than the turn's
// end and the note goes while the agent is still reviewing.
func TestAnAskRaisedBeforeAChangeCarriesTheNoteUntilAskedAgain(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	withReviews(t, e)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	asked := time.Now().UTC()
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	if err := e.reviewSeat(t.Context(), after, "ceo", seamsOf(e, someWork, pub)); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	pending, found, err := e.PendingAccessReview(t.Context(), "ceo")
	if err != nil || !found || !pending.Stale(asked) || len(pending.Lines) != 1 ||
		!strings.Contains(pending.Lines[0], "Policy added") {
		t.Fatalf("the change's record is %+v (%v, %v), want a pending review noting the earlier ask",
			pending, found, err)
	}
	review, reviewTurn := dispatchTurn(t, e, after)
	launchTurn(t, e, review, reviewTurn)
	if pending, _, _ = e.PendingAccessReview(t.Context(), "ceo"); pending.ToldTurn != reviewTurn ||
		!pending.Stale(asked) || pending.Stale(pending.ToldAt.Add(time.Second)) {
		t.Fatalf("after the review turn launched the record is %+v, want it told to %s", pending, reviewTurn)
	}
	e.endReviewOnTurnEnd(t.Context(), "ceo", turnID)
	if err := e.reviewSeat(t.Context(), after, "ceo", seamsOf(e, someWork, pub)); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if _, found, _ := e.PendingAccessReview(t.Context(), "ceo"); !found {
		t.Fatal("another turn's end, or a check while the review runs, dropped the record")
	}
	e.endReviewOnTurnEnd(t.Context(), "ceo", reviewTurn)
	if _, found, _ := e.PendingAccessReview(t.Context(), "ceo"); found {
		t.Error("the review turn ended and its change is still noted")
	}
}

// THE SAVE'S DRY RUN COUNTS DISTINCT OPEN ITEMS. A draft lowering the SRE's
// level names it with its open items and the line, in a person's words; a draft
// that changes nothing, or a seat with nothing open, names nobody.
//
// Mutation: measure the draft without the save's edits and no seat is named.
func TestTheSaveDryRunCountsOpenItems(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatalf("SeatAccessLedger: %v", err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{Kind: integration.KindCloudflare, Handle: "sre",
		Lanes: map[seataccess.Lane]seataccess.LaneRecord{
			seataccess.LaneCloudflare: {Observed: sreObserved[seataccess.LaneCloudflare]},
		}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	cfg := sreAccess("operate")
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	current := seataccess.Answer{Seats: []seataccess.SeatAnswer{{Handle: "sre",
		Lanes: map[seataccess.Lane]seataccess.LaneAnswer{
			seataccess.LaneCloudflare: {Desired: seataccess.Desired{Enabled: true, Level: "operate"}},
		}}}}
	draftOf := func(level string) seataccess.Draft {
		return seataccess.Draft{Seats: map[string]map[seataccess.Lane]seataccess.Desired{
			"sre": {seataccess.LaneCloudflare: {Enabled: true, Level: level}},
		}}
	}
	reviews, err := e.draftReviews(t.Context(), cfg, roster, current, draftOf("read"), someWork)
	if err != nil {
		t.Fatalf("draftReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Handle != "sre" || reviews[0].Open != 2 ||
		len(reviews[0].Changes) != 1 || !strings.Contains(reviews[0].Changes[0], "its level is now") {
		t.Fatalf("the dry run of a lowered level names %+v, want the SRE with 2 open items", reviews)
	}
	if reviews, err := e.draftReviews(t.Context(), cfg, roster, current, draftOf("read"), noWork); err != nil || len(reviews) != 0 {
		t.Errorf("a seat with nothing open is named %+v (%v)", reviews, err)
	}
	if reviews, err := e.draftReviews(t.Context(), cfg, roster, current, draftOf("operate"), someWork); err != nil || len(reviews) != 0 {
		t.Errorf("a draft that changes nothing names %+v (%v)", reviews, err)
	}
}

// A CHANGE OUTSIDE THE CLOSED LIST ASKS FOR NO REVIEW: the mission, the vision
// and the seat's availability are not what a seat is told about its access.
//
// Mutation: compare whole documents rather than snapshots.
func TestAChangeOutsideTheClosedListAsksForNoReview(t *testing.T) {
	t.Parallel()
	before := sreAccess("operate")
	after := sreAccess("operate")
	after.Mission = "Ship it faster."
	after.Vision = "Every release is boring."
	after.Roles[0].Availability = "Weekdays."
	if m, owed := measured(sreSnapshot(after), toldAs("t-told", sreSnapshot(before))); owed {
		t.Errorf("a mission, vision and availability change owe a review %+v", m)
	}
}

// A SEAT WITH NOTHING OPEN IS ASKED FOR NO REVIEW; once it has open work, the
// same change is asked for.
//
// Mutation: ask for a review whatever the open work.
func TestASeatWithNothingOpenIsAskedForNoReview(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	withReviews(t, e)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	if err := e.reviewSeat(t.Context(), after, "ceo", seamsOf(e, noWork, pub)); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if got := pub.got(); len(got) != 0 {
		t.Errorf("a seat with nothing open was asked for %+v", got)
	}
	if err := e.reviewSeat(t.Context(), after, "ceo", seamsOf(e, someWork, pub)); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if got := pub.got(); len(got) != 1 {
		t.Errorf("the seat with open work was asked for %d reviews, want 1", len(got))
	}
}
