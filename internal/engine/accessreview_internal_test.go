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
		Runs: []accessreview.Run{{Turn: "t-held", Tool: "ssh_operate", Server: "ssh"}}}, nil
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

// withReviews arms e's review debounce where the node did not.
func withReviews(t *testing.T, e *Engine) {
	t.Helper()
	if e.accessReviews.Load() == nil {
		e.accessReviews.Store(newAccessReviews(t.Context(), e.checkAccessReviews))
	}
}

// A LEVEL CHANGE ASKS FOR EXACTLY ONE REVIEW. The SRE's Cloudflare level
// lowered from Operate to Read is a change the closed list counts, measured
// from the told record, and a seat checked twice for the same change, on an
// apply that changed nothing further, is asked once: its wake's id is derived
// from the seat, the told record and the new digest, so a republish by another
// node collapses into the same wake.
//
// Mutation: drop the asked memo and the second check publishes again; derive
// the request from the previous revision rather than the told record and the
// baseline is not the told turn.
func TestALevelChangeAsksForExactlyOneReview(t *testing.T) {
	t.Parallel()
	req, owed := reviewOf(sreSnapshot(sreAccess("read")), toldAs("t-told", sreSnapshot(sreAccess("operate"))), "sre")
	if !owed || req.Baseline != "t-told" || len(req.Lines) != 1 ||
		!strings.Contains(req.Lines[0], "Cloudflare: your level is now") {
		t.Fatalf("a lowered level owes %+v (%v), want one review measured from the told turn", req, owed)
	}

	e, _ := aRunningNode(t)
	withReviews(t, e)
	before := capabilityCompany(t, "Ship small changes.")
	first, turnID := dispatchTurn(t, e, before)
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	seams := reviewSeams{work: someWork, publish: pub.publish}
	for range 2 {
		if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
			t.Fatalf("reviewSeat: %v", err)
		}
	}
	got := pub.got()
	if len(got) != 1 {
		t.Fatalf("two checks of one change asked for %d reviews, want 1: %+v", len(got), got)
	}
	if got[0].Baseline != turnID || got[0].Open != 2 || len(got[0].Lines) != 1 {
		t.Errorf("the review asked for is %+v, want it measured from %s with 2 open items", got[0], turnID)
	}
	again, owedAgain, err := e.reviewOwed(t.Context(), after, "ceo")
	if err != nil || !owedAgain || accessreview.WakeID(again) != accessreview.WakeID(got[0]) {
		t.Errorf("the same change measured again wakes %v (%v, %v), want the same wake %v",
			accessreview.WakeID(again), owedAgain, err, accessreview.WakeID(got[0]))
	}
}

// THREE QUICK REVISIONS ASK FOR ONE REVIEW. Every apply restarts the quiet
// window, so a person's run of saves is measured once, after the last of them.
//
// Mutation: arm a timer per kick without stopping the one before and three
// checks run.
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

// A PAUSED SEAT REVIEWS WHEN IT RESUMES. A review is asked for whatever the
// seat's pause, and its wake waits on the seat's inbox like any mail: parked
// while the seat is paused, delivered once it is resumed, and dispatched as a
// review there.
//
// Mutation: drop a review addressed to a paused seat and nothing reaches it on
// its resume.
func TestAPausedSeatReviewsWhenItResumes(t *testing.T) {
	t.Parallel()
	wake := reviewWake()
	ready := inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
	paused := ready
	paused.Paused = true
	held := inbox.Screen(paused, []*events.Event{wake})
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

// A SETTLED REVIEW IS ACKED WITHOUT A TURN. A review whose seat was already
// told its access as it stands (by its review, or a turn that told the change
// first) runs nothing; one that still has a change to tell runs.
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

// reviewWake is a review's wake as the notify spine delivers it to the seat.
func reviewWake() *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: accessreview.Source, SourceEventType: accessreview.EventType,
		Subject: accessreview.Subject, Body: accessreview.Task,
	}, events.NewTrace())
}

// THE REVIEW TURN'S TOLD RECORD STOPS THE NEXT TURN REPEATING THE CHANGE. The
// review turn is told the change and the seat's open work through the one
// dispatch every turn takes, its launch writes the told record, and from then
// the seat owes no review and its next turn is told nothing.
//
// Mutation: tell the review turn the change without writing its told record
// and the next turn repeats it.
func TestAReviewTurnsToldRecordStopsTheNextTurnRepeatingIt(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	if _, owed, err := e.reviewOwed(t.Context(), after, "ceo"); err != nil || !owed {
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
	if _, owed, err := e.reviewOwed(t.Context(), after, "ceo"); err != nil || owed {
		t.Errorf("after the review turn launched the seat owes a review (%v)", err)
	}
	if !e.reviewSettled(t.Context(), after, "ceo") {
		t.Error("after the review turn launched a second review of the change is not settled")
	}
	if next, _ := dispatchTurn(t, e, after); next.changes != "" {
		t.Errorf("the turn after the review is told:\n%s", next.changes)
	}
}

// AN ASK RAISED BEFORE A CHANGE CARRIES THE NOTE UNTIL IT IS ASKED AGAIN. A
// question asked under Operate, with the level lowered to Read since, notes
// the change; once the review turn is told Read and asks again, the new
// question notes nothing, and a revert to Operate clears the old one's note.
//
// Mutation: measure against the next told record instead of the access now
// and the reverted question still carries a note.
func TestAnAskRaisedBeforeAChangeCarriesTheNoteUntilAskedAgain(t *testing.T) {
	t.Parallel()
	operate, read := sreSnapshot(sreAccess("operate")), sreSnapshot(sreAccess("read"))
	t1 := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	rows := []accessdomain.ToldRow{{Turn: "t-1", At: t1, Digest: operate.Digest(), Snapshot: operate.JSON()}}
	asked := t1.Add(time.Minute)
	lines, changed := changedSince(rows, read, asked)
	if !changed || len(lines) != 1 || !strings.Contains(lines[0], "level") {
		t.Fatalf("a question asked under Operate, with Read now, notes %v (%v)", lines, changed)
	}
	rows = append([]accessdomain.ToldRow{{Turn: "t-review", At: t1.Add(10 * time.Minute),
		Digest: read.Digest(), Snapshot: read.JSON()}}, rows...)
	if lines, changed := changedSince(rows, read, t1.Add(11*time.Minute)); changed {
		t.Errorf("the question asked again after the review notes %v", lines)
	}
	if lines, changed := changedSince(rows, operate, asked); changed {
		t.Errorf("a question asked under Operate, with Operate back now, notes %v", lines)
	}
	if lines, changed := changedSince(rows, read, t1.Add(-time.Hour)); changed {
		t.Errorf("a question older than every told record notes %v, want nothing said", lines)
	}
}

// THE SAVE'S DRY RUN COUNTS OPEN ITEMS. A draft lowering the SRE's Cloudflare
// level names the SRE with its open items and the line it will be told; a draft
// that changes nothing the seat is told, or a seat with nothing open, names
// nobody.
//
// Mutation: measure the draft without writing its settings into the copy and
// no seat is named.
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
	lower := seataccess.Draft{Seats: map[string]map[seataccess.Lane]seataccess.Desired{
		"sre": {seataccess.LaneCloudflare: {Enabled: true, Level: "read"}},
	}}
	reviews, err := e.draftReviews(t.Context(), cfg, roster, lower, someWork)
	if err != nil {
		t.Fatalf("draftReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Handle != "sre" || reviews[0].Open != 2 ||
		len(reviews[0].Changes) != 1 || !strings.Contains(reviews[0].Changes[0], "level") {
		t.Fatalf("the dry run of a lowered level names %+v, want the SRE with 2 open items", reviews)
	}
	if reviews, err := e.draftReviews(t.Context(), cfg, roster, lower, noWork); err != nil || len(reviews) != 0 {
		t.Errorf("a seat with nothing open is named %+v (%v)", reviews, err)
	}
	same := seataccess.Draft{Seats: map[string]map[seataccess.Lane]seataccess.Desired{
		"sre": {seataccess.LaneCloudflare: {Enabled: true, Level: "operate"}},
	}}
	if reviews, err := e.draftReviews(t.Context(), cfg, roster, same, someWork); err != nil || len(reviews) != 0 {
		t.Errorf("a draft that changes nothing names %+v (%v)", reviews, err)
	}
}

// A CHANGE OUTSIDE THE CLOSED LIST ASKS FOR NO REVIEW. The mission, the vision
// and the seat's availability are not what a seat is told about its access, so a
// revision that changes them leaves the seat's snapshot as it was told, and
// nothing is asked.
//
// Mutation: compare whole documents rather than snapshots and the mission's
// change asks for a review.
func TestAChangeOutsideTheClosedListAsksForNoReview(t *testing.T) {
	t.Parallel()
	before := sreAccess("operate")
	after := sreAccess("operate")
	after.Mission = "Ship it faster."
	after.Vision = "Every release is boring."
	after.Roles[0].Availability = "Weekdays."
	if req, owed := reviewOf(sreSnapshot(after), toldAs("t-told", sreSnapshot(before)), "sre"); owed {
		t.Errorf("a mission, vision and availability change owe a review %+v", req)
	}
}

// A SEAT WITH NOTHING OPEN IS ASKED FOR NO REVIEW. Its change is told at its
// next turn, as every change is; a turn now would review nothing.
//
// Mutation: ask for a review whatever the open work and this seat is woken.
func TestASeatWithNothingOpenIsAskedForNoReview(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	withReviews(t, e)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	pub := &captured{}
	if err := e.reviewSeat(t.Context(), after, "ceo", reviewSeams{work: noWork, publish: pub.publish}); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if got := pub.got(); len(got) != 0 {
		t.Errorf("a seat with nothing open was asked for %+v", got)
	}
	// AND ONCE IT HAS SOMETHING OPEN, the same change is asked for.
	if err := e.reviewSeat(t.Context(), after, "ceo", reviewSeams{work: someWork, publish: pub.publish}); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if got := pub.got(); len(got) != 1 {
		t.Errorf("the seat with open work was asked for %d reviews, want 1", len(got))
	}
}
