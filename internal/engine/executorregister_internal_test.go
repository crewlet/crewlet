package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// fakeExecutor is the executor's reader and writer as the reaper's source and a
// seat's retirement reach them.
type fakeExecutor struct {
	expired, unended []executordomain.Expiry
	unendedBefore    time.Time
	reads            int

	deleted, forgotten, lost []string
	lostBefore               time.Time
	notDue                   map[string]bool

	handles      []string
	handlesErr   error
	handlesLevel statelog.ReadLevel

	removed    []string
	removedOps []string
	removeErr  error
	outcome    statelog.Outcome
}

func (f *fakeExecutor) Expired(_ context.Context, _ time.Time, limit int) ([]executordomain.Expiry, error) {
	f.reads++
	return f.expired[:min(limit, len(f.expired))], nil
}

func (f *fakeExecutor) Unended(_ context.Context, before time.Time, limit int) ([]executordomain.Expiry, error) {
	f.reads++
	f.unendedBefore = before
	return f.unended[:min(limit, len(f.unended))], nil
}

func (f *fakeExecutor) DeleteExpired(_ context.Context, _ executordomain.Call, _, _, turn string,
	_ time.Time) (statelog.Result, error) {

	if f.notDue[turn] {
		return statelog.Result{}, executordomain.ErrNotDue
	}
	f.deleted = append(f.deleted, turn)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (f *fakeExecutor) ExpireRemoval(_ context.Context, _ executordomain.Call, handle string,
	_ time.Time) (statelog.Result, error) {

	f.forgotten = append(f.forgotten, handle)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (f *fakeExecutor) EndLost(_ context.Context, _ executordomain.Call, _, _, turn string,
	before time.Time) (statelog.Result, error) {

	f.lost, f.lostBefore = append(f.lost, turn), before
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (f *fakeExecutor) Handles(_ context.Context, fresh statelog.Freshness) ([]string, error) {
	f.handlesLevel = fresh.Level
	return f.handles, f.handlesErr
}

func (f *fakeExecutor) DeleteSeat(_ context.Context, c executordomain.Call, handle string) (
	statelog.Result, error) {

	f.removed, f.removedOps = append(f.removed, handle), append(f.removedOps, c.OpID)
	outcome := f.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	return statelog.Result{Outcome: outcome, OpID: c.OpID}, f.removeErr
}

// executorClock is the instant the cases' sources judge a lost end on.
var executorClock = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// sourceOver is the executor's reaper source over fake, with live as the turns
// whose runs the fleet holds.
func sourceOver(fake *fakeExecutor, live ...string) executorTrash {
	return executorTrash{reader: fake, writer: fake,
		term: func(context.Context) (time.Duration, error) { return 30 * 24 * time.Hour, nil },
		live: func(context.Context) (map[string]bool, error) {
			out := map[string]bool{}
			for _, turn := range live {
				out[turn] = true
			}
			return out, nil
		},
		now: func() time.Time { return executorClock }}
}

// THE EXECUTOR SOURCE NEVER ENDS OR AGES OUT A LIVE RUN, and nominates a lost
// end before anything past the retention: a turn whose end was lost holds its
// session against the seat's budget until it is ended. A lost end is judged a
// day past the last capture (executordomain.LostAfter), on the source's clock.
//
// Mutation: drop the live check from either listing and a turn parked on a
// person's answer is ended, or aged out, under its run.
func TestTheExecutorSourceNeverEndsOrAgesOutALiveRun(t *testing.T) {
	t.Parallel()
	parked, lost, idle := uuid.NewString(), uuid.NewString(), uuid.NewString()
	fake := &fakeExecutor{
		unended: []executordomain.Expiry{
			{Handle: "ana", Turn: parked, ConversationKey: "chat:a"},
			{Handle: "ana", Turn: lost, ConversationKey: "owner/repo#7"},
		},
		expired: []executordomain.Expiry{
			{Handle: "ben"},
			{Handle: "ana", Turn: parked, ConversationKey: "chat:a"},
			{Handle: "ana", Turn: idle, ConversationKey: "chat:b"},
		},
	}
	src := sourceOver(fake, parked)
	got, err := src.TrashedBefore(t.Context(), executorClock.Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("TrashedBefore: %v", err)
	}
	want := []string{lostItem + "ana/" + lost + "/owner/repo#7", removalItem + "ben",
		"ana/" + idle + "/chat:b"}
	if !slices.Equal(got, want) {
		t.Fatalf("the source nominates %v, want %v", got, want)
	}
	if !fake.unendedBefore.Equal(executorClock.Add(-executordomain.LostAfter)) {
		t.Errorf("a lost end was judged on turns untouched since %s, want a day before %s",
			fake.unendedBefore, executorClock)
	}
	if got, _ := src.TrashedBefore(t.Context(), executorClock, 1); !slices.Equal(got, want[:1]) {
		t.Errorf("a listing of one nominates %v, want the lost end alone", got)
	}
}

// A PASS THAT CANNOT READ THE RUN RECORDS NOMINATES NOTHING: unknown is never
// "nothing is running".
//
// Mutation: treat the read's failure as no live runs and every parked turn's
// session is aged out on the first coordination outage.
func TestTheExecutorSourceNominatesNothingWithoutTheRuns(t *testing.T) {
	t.Parallel()
	fake := &fakeExecutor{expired: []executordomain.Expiry{
		{Handle: "ana", Turn: uuid.NewString(), ConversationKey: "chat:a"}}}
	src := sourceOver(fake)
	src.live = func(context.Context) (map[string]bool, error) {
		return nil, coord.ErrUnavailable
	}
	got, err := src.TrashedBefore(t.Context(), executorClock, 10)
	if err == nil || len(got) != 0 || fake.reads != 0 {
		t.Fatalf("a pass that could not read the runs nominated %v (%v) after %d reads, "+
			"want nothing and an error", got, err, fake.reads)
	}
}

// fakeRuns is the fleet's run records as the live-turn read reaches them.
type fakeRuns struct{ records []coord.Record }

func (f fakeRuns) SandboxRuns(context.Context) ([]coord.Record, error) { return f.records, nil }

// EVERY RUN RECORD IS A LIVE TURN, WHATEVER IT HOLDS: a record this build cannot
// decode and one of a status it does not know are runs nobody has finished as
// much as any other, since a finished run's record is deleted — and the
// sandbox's own listings skip both, which is why the retention reads the keys.
//
// Mutation: read the live turns through sandbox's ListActive and the garbled
// record and the newer build's are aged out under their runs.
func TestEveryRunRecordIsALiveTurn(t *testing.T) {
	t.Parallel()
	running, garbled, newer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	runs := fakeRuns{records: []coord.Record{
		{Key: running, Value: []byte(`{"status":"` + sandbox.StatusRunning + `"}`), Version: 1},
		{Key: garbled, Value: []byte(`{"status":`), Version: 1},
		{Key: newer, Value: []byte(`{"status":"a_status_a_newer_build_writes"}`), Version: 1},
	}}
	live, err := liveTurns(runs)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, turn := range map[string]string{"a running run": running,
		"a record this build cannot decode": garbled, "a newer build's status": newer} {
		if !live[turn] {
			t.Errorf("%s is not live, so its session could be aged out under it", name)
		}
	}
}

// THE REAPER'S ITEMS ROUND-TRIP EVERY KIND, a conversation key holding a slash
// included: a turn past its retention, a lost end and a removed seat's removal
// each reach their own write, and a turn touched since its listing is not
// eligible rather than a failure.
//
// Mutation: split the item on every slash and a conversation key holding one
// is cut, so the delete names another conversation's subject and is refused.
func TestTheReapersItemsRoundTripEveryKind(t *testing.T) {
	t.Parallel()
	turn, busy := uuid.NewString(), uuid.NewString()
	fake := &fakeExecutor{notDue: map[string]bool{busy: true}}
	src := sourceOver(fake)
	key := "owner/repo#7"
	item := expiryItem(executordomain.Expiry{Handle: "ana", Turn: turn, ConversationKey: key})
	if handle, gotTurn, gotKey, ok := parseExpiryItem(item); !ok || handle != "ana" ||
		gotTurn != turn || gotKey != key {
		t.Fatalf("%q parses as (%q, %q, %q, %v)", item, handle, gotTurn, gotKey, ok)
	}
	for _, id := range []string{item, lostItem + item, removalItem + "ben"} {
		if err := src.PurgeTrashed(t.Context(), id, executorClock, "op"); err != nil {
			t.Errorf("purge %s: %v", id, err)
		}
	}
	if !slices.Equal(fake.deleted, []string{turn}) || !slices.Equal(fake.lost, []string{turn}) ||
		!slices.Equal(fake.forgotten, []string{"ben"}) {
		t.Errorf("deleted %v, ended lost %v and forgot %v", fake.deleted, fake.lost, fake.forgotten)
	}
	if !fake.lostBefore.Equal(executorClock.Add(-executordomain.LostAfter)) {
		t.Errorf("the lost end was decided against %s, want a day before %s", fake.lostBefore,
			executorClock)
	}
	busyItem := expiryItem(executordomain.Expiry{Handle: "ana", Turn: busy, ConversationKey: key})
	if err := src.PurgeTrashed(t.Context(), busyItem, executorClock, "op"); !errors.Is(err,
		trashreap.ErrNotEligible) {
		t.Errorf("a turn captured since it was listed answers %v, want not eligible", err)
	}
	for _, malformed := range []string{"ana", lostItem + "ana"} {
		if err := src.PurgeTrashed(t.Context(), malformed, executorClock, "op"); !errors.Is(err,
			trashreap.ErrNotEligible) {
			t.Errorf("the unreadable item %q answers %v", malformed, err)
		}
	}
}

// A RETIRED SEAT'S SESSIONS ARE REMOVED BY THE SEAT, under the retirement's own
// instant: the handle the record names, never one the running company still
// gives a seat, and — for a record that lost its handle — every handle holding
// sessions whose agent id under the running organisation is the seat's, listed
// at `linearizable` so a seat whose rows this node has not applied is found.
//
// Mutation: list the handles at `stale` and a lagging node retires a seat
// whose rows it has not applied without removing them; drop the present-seat
// check and a renamed organisation's seat loses the sessions now its own.
func TestARetiredSeatsSessionsAreRemovedByTheSeat(t *testing.T) {
	t.Parallel()
	retiring := executorClock
	ana, _ := org.DeriveAgentID("Acme", "ana")
	present := []placement.Seat{{Handle: "ben"}}

	named := &fakeExecutor{}
	if err := removeSeatSessions(t.Context(), named, named, "Acme", present, ana, "ana",
		retiring); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(named.removed, []string{"ana"}) ||
		named.removedOps[0] != executordomain.SeatDeleteOpID(retiring, "ana") {
		t.Errorf("removed %v under %v, want ana under the retirement's own operation",
			named.removed, named.removedOps)
	}

	kept := &fakeExecutor{}
	ben, _ := org.DeriveAgentID("Acme", "ben")
	if err := removeSeatSessions(t.Context(), kept, kept, "Acme", present, ben, "ben",
		retiring); err != nil || len(kept.removed) != 0 {
		t.Errorf("a handle the company still gives a seat: removed %v (%v)", kept.removed, err)
	}

	unlabelled := &fakeExecutor{handles: []string{"ana", "ben", "cat"}}
	if err := removeSeatSessions(t.Context(), unlabelled, unlabelled, "Acme", present, ana, "",
		retiring); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(unlabelled.removed, []string{"ana"}) ||
		unlabelled.handlesLevel != statelog.ReadLinearizable {
		t.Errorf("a record with no handle removed %v, listing at %q; want ana, listed at "+
			"linearizable", unlabelled.removed, unlabelled.handlesLevel)
	}

	unread := &fakeExecutor{handlesErr: executordomain.ErrIncomplete}
	if err := removeSeatSessions(t.Context(), unread, unread, "Acme", present, ana, "",
		retiring); !errors.Is(err, executordomain.ErrIncomplete) || len(unread.removed) != 0 {
		t.Errorf("a listing this node cannot answer whole: removed %v (%v), want nothing "+
			"and the error", unread.removed, err)
	}
}

// A REMOVAL THE LOG WILL NOT TAKE LEAVES THE SESSIONS TO THE RETENTION: the
// retirement goes on, because the retention bounds the sessions and a mailbox
// held for as long as the log is unhealthy is bounded by nothing — while a
// retirement whose own budget ran out is undone whole.
//
// Mutation: return the removal's error and every retirement waits on the
// executor log's health.
func TestARemovalTheLogWillNotTakeLeavesTheSessionsToTheRetention(t *testing.T) {
	t.Parallel()
	ana, _ := org.DeriveAgentID("Acme", "ana")
	for name, fake := range map[string]*fakeExecutor{
		"refused":         {removeErr: errors.New("statelog: the executor log is full")},
		"of unknown fate": {outcome: statelog.OutcomeUnknown},
	} {
		if err := removeSeatSessions(t.Context(), fake, fake, "Acme", nil, ana, "ana",
			executorClock); err != nil {
			t.Errorf("a removal %s failed the retirement: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	spent := &fakeExecutor{removeErr: context.Canceled}
	if err := removeSeatSessions(ctx, spent, spent, "Acme", nil, ana, "ana",
		executorClock); !errors.Is(err, context.Canceled) {
		t.Errorf("a retirement whose budget is spent answered %v, want it undone", err)
	}
}

// THE RETENTION IS THE DOCUMENT'S, as its block states it: unset (or no
// document at all) is the block's thirty days, never a term of nothing, and a
// set value is kept.
//
// Mutation: read a zero as it stands and a company built in code ages out
// every session at once.
func TestTheExecutorRetentionIsTheDocumentsOwn(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	company := func(days int) *Company {
		return &Company{Config: &config.Company{TurnEngine: config.TurnEngine{
			ConversationSession: config.ConversationSession{RetentionDays: days}}}}
	}
	for _, c := range []struct {
		name    string
		company *Company
		want    time.Duration
	}{
		{"a company with no config", &Company{}, 30 * day},
		{"a company that sets nothing", company(0), 30 * day},
		{"a company that keeps a week", company(7), 7 * day},
	} {
		if got := executorRetentionOf(c.company); got != c.want {
			t.Errorf("%s keeps sessions %v, want %v", c.name, got, c.want)
		}
	}
}

// THE TERM AND THE BUDGET ARE THE FLEET REVISION'S, KNOWN ONLY ON A NODE THAT
// RUNS IT: a retention delete and a capture's budget are records every data
// node applies, so a node serving an activation the fleet replaced, one booted
// on an offer no activation named, one that cannot read the pointer, and a
// fleet that has activated nothing each leave them unknown.
//
// Mutation: read either off the node's own company again and every case but
// the first reads a value it cannot know.
func TestTheExecutorTermAndBudgetAreKnownOnlyOnANodeServingTheFleetsRevision(t *testing.T) {
	t.Parallel()
	theFleets := func(_, current coord.Activation) time.Time { return current.At }
	for _, tc := range []struct {
		name  string
		stamp func(earlier, current coord.Activation) time.Time
		down  bool
		known bool
	}{
		{name: "a node serving the fleet's activation", stamp: theFleets, known: true},
		{name: "a node serving an activation the fleet has replaced",
			stamp: func(earlier, _ coord.Activation) time.Time { return earlier.At }},
		{name: "a node booted on an offer no activation named",
			stamp: func(coord.Activation, coord.Activation) time.Time { return time.Time{} }},
		{name: "a node that cannot read the pointer", stamp: theFleets, down: true},
		{name: "a fleet that has activated nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
			company := &Company{Config: &config.Company{TurnEngine: config.TurnEngine{
				ConversationSession: config.ConversationSession{
					RetentionDays: 7, MaxBytesPerSeat: 40 << 20}}}}
			if tc.stamp != nil {
				earlier := activate(t, fleet, "rev-1")
				company.ActivatedAt = tc.stamp(earlier, activate(t, fleet, "rev-2"))
			}
			fleet.down = tc.down
			e := &Engine{backends: &Backends{Fleet: fleet}}
			e.epoch.current.Store(company)

			term, termErr := e.executorRetention(t.Context())
			budget, budgetErr := e.ExecutorSessionBudget(t.Context())
			if tc.known != (termErr == nil) || (tc.known && term != 7*24*time.Hour) {
				t.Errorf("the term reads %v (%v), want it known (%v) at a week only on a "+
					"node serving the fleet's activation", term, termErr, tc.known)
			}
			if tc.known != (budgetErr == nil) || (tc.known && budget != 40<<20) {
				t.Errorf("the budget reads %d (%v), want it known (%v) at 40 MiB only on a "+
					"node serving the fleet's activation", budget, budgetErr, tc.known)
			}
		})
	}
}
