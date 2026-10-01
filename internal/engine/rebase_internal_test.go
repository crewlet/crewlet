package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turn"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mustDescribeTurn is [Engine.describeTurn], failing the test on an error.
func mustDescribeTurn(t *testing.T, e *Engine, company *Company, req Request) turnTelemetry {
	t.Helper()
	tel, err := e.describeTurn(context.Background(), company, req)
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	return tel
}

// mustDescribeResume is [Engine.describeResume], failing the test on an error.
func mustDescribeResume(t *testing.T, e *Engine, company *Company, in resumeInput) turnTelemetry {
	t.Helper()
	tel, err := e.describeResume(context.Background(), company, in)
	if err != nil {
		t.Fatalf("describeResume: %v", err)
	}
	return tel
}

// toolsMint is the instant the tools a telemetry hands its runner mint their
// operation ids at: the turn the runner is given — the frame the tools
// actually read — into the actor every derived id is built from.
func toolsMint(t *testing.T, company *Company, tel turnTelemetry) time.Time {
	t.Helper()
	tc := tel.runnerTurn(company, 0, nil, "the task", turn.ToolReply("")).Context
	if tc == nil {
		t.Fatal("the runner was handed no turn")
	}
	return builtin.Actor{TurnID: tc.RunID, WorkKey: tc.WorkKey,
		WorkSince: tc.WorkSince, RebasedTo: tc.RebasedTo}.OperationSince()
}

// rebaseRig is an engine whose coordination store is the fleet twin, behind a
// store that counts what it is asked and can be made to fail.
type rebaseRig struct {
	engine  *Engine
	company *Company
	store   *countingRebases
}

func newRebaseRig(t *testing.T) *rebaseRig {
	t.Helper()
	fleet := coordmem.NewFleet()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	return &rebaseRig{
		engine:  &Engine{backends: &Backends{Fleet: fleet}},
		company: &Company{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}},
		store:   &countingRebases{inner: fleet},
	}
}

// countingRebases is a [rebaseStore] that counts its reads and can fail them,
// or lose a race to a competing attempt.
type countingRebases struct {
	inner rebaseStore

	mu    sync.Mutex
	reads int
	fail  error
	// competitor, when set, is recorded by ANOTHER attempt between this
	// one's read and its first write, which that write then loses.
	competitor time.Time
}

func (c *countingRebases) Rebase(ctx context.Context, seed string) (time.Time, uint64, error) {
	c.mu.Lock()
	c.reads++
	fail := c.fail
	c.mu.Unlock()
	if fail != nil {
		return time.Time{}, 0, fail
	}
	return c.inner.Rebase(ctx, seed)
}

func (c *countingRebases) RecordRebase(ctx context.Context, seed string, at time.Time, version uint64) (bool, error) {
	c.mu.Lock()
	competitor := c.competitor
	c.competitor = time.Time{}
	c.mu.Unlock()
	if !competitor.IsZero() {
		if _, err := c.inner.RecordRebase(ctx, seed, competitor, version); err != nil {
			return false, err
		}
	}
	return c.inner.RecordRebase(ctx, seed, at, version)
}

func (c *countingRebases) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// keyed is the identity of a turn on a unit of work that began at began.
func keyed(began time.Time) builtin.Actor {
	return builtin.Actor{TurnID: statelog.NewOpID(began, ""), WorkKey: "wk-1", WorkSince: began}
}

// AN ATTEMPT MINTS AT ITS WORK'S START WHILE THE LEDGER CAN VOUCH FOR IT TO THE
// ATTEMPT'S END, AND ASKS NOBODY; PAST THAT IT REBASES ONTO ITSELF AND RECORDS
// IT.
//
// Every node's operation ledger deletes the rows it applied thirty days ago and
// records how far back it deleted, and an operation minted before that point
// whose row is gone is answered `unknown` and never published, on every node.
// Short of the horizon the start is kept, because a write an earlier attempt
// made and this one repeats — the retry an `unknown` asks for — collapses onto
// the first copy only under the same id: a rule that always rebased is red
// there. Past it every write would be lost: a rule that never rebased is red
// there. Inside the retention's last day it rebases too, since the attempt goes
// on deciding writes while a sweep moves past its start: a horizon at the bare
// retention is red on that row.
func TestAnAttemptRebasesOntoItselfOnlyPastTheHorizon(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		after   time.Duration
		rebased bool
	}{
		{"an attempt a week on", 7 * 24 * time.Hour, false},
		{"an attempt at the horizon", statelog.MintHorizon, false},
		{"an attempt just past the horizon", statelog.MintHorizon + time.Millisecond, true},
		{"an attempt a minute short of the retention", statelog.OpsRetention - time.Minute, true},
		{"an attempt just past the retention", statelog.OpsRetention + time.Millisecond, true},
		{"an attempt forty days on", 40 * 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newRebaseRig(t)
			at := began.Add(tc.after)
			got, err := rebaseFor(t.Context(), rig.store, keyed(began), at)
			if err != nil {
				t.Fatalf("rebaseFor: %v", err)
			}
			if !tc.rebased {
				if !got.IsZero() {
					t.Fatalf("an attempt %s after its work began rebased onto %s — a "+
						"repeat of an earlier attempt's write would be a second write",
						tc.after, got)
				}
				if n := rig.store.readCount(); n != 0 {
					t.Fatalf("an attempt inside the horizon read the coordination "+
						"store %d time(s): no attempt at the work can have rebased yet, "+
						"and every ordinary turn would pay for the read", n)
				}
				return
			}
			if !got.Equal(at) {
				t.Fatalf("an attempt %s after its work began mints at %s, want its own "+
					"instant %s — minted at the start, no node could vouch for its writes",
					tc.after, got, at)
			}
			if recorded, _, _ := rig.store.inner.Rebase(t.Context(), "wk-1"); !recorded.Equal(at) {
				t.Fatalf("the rebase was not recorded (the store holds %s): the attempt "+
					"after this one would mint anew and write again what this one wrote",
					recorded)
			}
		})
	}
}

// EVERY ATTEMPT IS JUDGED AGAINST ITS OWN CLOCK — so an attempt that ran inside
// the horizon holds nothing over one that runs past it.
//
// The shape the reviewers found: a resume's first attempt runs at twenty-eight
// days and fails without acting; the person answers again at forty and that
// attempt succeeds. Judged at the first attempt's instant, the second minted
// at the start and every write it made was answered `unknown` on every node for
// good. Judged at its own, it rebases.
func TestALaterAttemptIsJudgedAtItsOwnClock(t *testing.T) {
	t.Parallel()
	rig := newRebaseRig(t)
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if got, err := rebaseFor(t.Context(), rig.store, keyed(began), began.Add(28*24*time.Hour)); err != nil || !got.IsZero() {
		t.Fatalf("the first attempt at 28 days = (%s, %v), want the start", got, err)
	}
	later := began.Add(40 * 24 * time.Hour)
	got, err := rebaseFor(t.Context(), rig.store, keyed(began), later)
	if err != nil {
		t.Fatalf("rebaseFor: %v", err)
	}
	if !got.Equal(later) {
		t.Fatalf("the attempt at 40 days mints at %s, want its own %s", got, later)
	}
}

// A LATER ATTEMPT INHERITS THE REBASE WHILE IT CAN, AND REBASES AGAIN ONCE IT
// CANNOT — a crash re-run, a retried resume and the next half of the turn
// alike, since each is only another attempt at the same work.
//
// While the recorded instant is within the horizon, a later attempt mints
// there, so a write the earlier attempt made and this one repeats collapses: a
// rule that judged every attempt against the work's start alone rebased each
// half onto its own instant and wrote the repeat twice (half 2 rebased onto R1,
// half 3 two hours later onto R1+2h). Once the recorded instant is itself past
// the horizon, the rule that ended the first loss ends this one: a retry thirty
// days after a rebase rebases again, and the record moves with it.
func TestALaterAttemptInheritsTheRebaseWhileItCan(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	first := began.Add(35 * 24 * time.Hour)
	for _, tc := range []struct {
		name  string
		after time.Duration
		// inherits is whether the attempt `after` the first rebase mints
		// where the first did.
		inherits bool
	}{
		{"the next half two hours on", 2 * time.Hour, true},
		{"a retried resume a week on", 7 * 24 * time.Hour, true},
		{"an attempt at the horizon of the rebase", statelog.MintHorizon, true},
		{"an attempt past the horizon of the rebase", statelog.MintHorizon + time.Millisecond, false},
		{"an attempt thirty-five days after the rebase", 35 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newRebaseRig(t)
			if got, err := rebaseFor(t.Context(), rig.store, keyed(began), first); err != nil || !got.Equal(first) {
				t.Fatalf("the first rebase = (%s, %v), want %s", got, err, first)
			}
			at := first.Add(tc.after)
			got, err := rebaseFor(t.Context(), rig.store, keyed(began), at)
			if err != nil {
				t.Fatalf("rebaseFor: %v", err)
			}
			want := at
			if tc.inherits {
				want = first
			}
			if !got.Equal(want) {
				t.Fatalf("an attempt %s after the first rebase mints at %s, want %s",
					tc.after, got, want)
			}
			if recorded, _, _ := rig.store.inner.Rebase(t.Context(), "wk-1"); !recorded.Equal(want) {
				t.Fatalf("the record holds %s after the attempt, want %s — the "+
					"attempt after it would inherit an instant this one did not mint at",
					recorded, want)
			}
		})
	}
}

// A RUN WITH NO WORK KEY IS JUDGED AND RECORDED BY ITS OWN RUN.
//
// Its ids are seeded from the run and minted at the run's start, which a resume
// forty days on is exactly as far past the ledger as a keyed run's work — and a
// run id an older build minted with no instant in it is further still. So the
// rule is judged against the instant the ids WOULD carry, whichever identity it
// is the start of, and the record is kept under the seed they are derived
// from: a rule reading WorkSince alone would never rebase these.
func TestARunWithNoWorkKeyIsRebasedByItsRun(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		runID string
		after time.Duration
		want  func(at time.Time) time.Time
	}{
		{"a run a week on", statelog.NewOpID(started, ""), 7 * 24 * time.Hour,
			func(time.Time) time.Time { return time.Time{} }},
		{"a run forty days on", statelog.NewOpID(started, ""), 40 * 24 * time.Hour,
			func(at time.Time) time.Time { return at }},
		{"a run whose id carries no instant, an hour on", "run-from-an-older-build", time.Hour,
			func(at time.Time) time.Time { return at }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newRebaseRig(t)
			at := started.Add(tc.after)
			got, err := rebaseFor(t.Context(), rig.store, builtin.Actor{TurnID: tc.runID}, at)
			if err != nil {
				t.Fatalf("rebaseFor: %v", err)
			}
			if !got.Equal(tc.want(at)) {
				t.Fatalf("the keyless run mints at %s, want %s", got, tc.want(at))
			}
			if !got.IsZero() {
				if recorded, _, _ := rig.store.inner.Rebase(t.Context(), tc.runID); !recorded.Equal(got) {
					t.Fatalf("the rebase is not recorded under the run (%s)", recorded)
				}
			}
		})
	}
}

// AN ATTEMPT THAT CANNOT LEARN WHETHER AN EARLIER ONE REBASED DOES NOT RUN.
//
// Either instant it might choose is wrong for one of the two cases it cannot
// tell apart: the start loses every write past the horizon, and its own instant
// writes again whatever an earlier attempt wrote. So an unreadable store — or
// none at all — is an error, and the attempt is handed back to be retried.
func TestAnAttemptThatCannotReadTheRebaseDoesNotRun(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	at := began.Add(40 * 24 * time.Hour)

	rig := newRebaseRig(t)
	outage := errors.New("the coordination store is unreachable")
	rig.store.fail = outage
	if got, err := rebaseFor(t.Context(), rig.store, keyed(began), at); !errors.Is(err, outage) {
		t.Fatalf("rebaseFor over an unreachable store = (%s, %v), want the outage", got, err)
	}

	_, err := rebaseFor(t.Context(), nil, keyed(began), at)
	if err == nil || !strings.Contains(err.Error(), "no coordination store") {
		t.Fatalf("rebaseFor with no store = %v, want the missing store named", err)
	}

	// INSIDE THE HORIZON NEITHER MATTERS: nothing is read.
	if got, err := rebaseFor(t.Context(), nil, keyed(began), began.Add(time.Hour)); err != nil || !got.IsZero() {
		t.Fatalf("an attempt inside the horizon with no store = (%s, %v), want the start", got, err)
	}
}

// AN ATTEMPT THAT LOSES THE RACE TO RECORD INHERITS THE WINNER'S INSTANT.
//
// Two attempts at the same work past the horizon — a redelivery while the
// first is still running, on another node — each want their own instant. The
// loser's write is refused; it reads what the winner recorded, which is within
// the horizon of its own clock, and mints there, so the two write under one set
// of ids. A loser that went on to mint at its own instant would write every
// write the winner makes a second time.
func TestAnAttemptThatLosesTheRaceInheritsTheWinner(t *testing.T) {
	t.Parallel()
	rig := newRebaseRig(t)
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	winner := began.Add(40 * 24 * time.Hour)
	rig.store.competitor = winner
	got, err := rebaseFor(t.Context(), rig.store, keyed(began), winner.Add(time.Second))
	if err != nil {
		t.Fatalf("rebaseFor: %v", err)
	}
	if !got.Equal(winner) {
		t.Fatalf("the losing attempt mints at %s, want the winner's %s", got, winner)
	}
}

// A DISPATCH OF A TRIGGER OLDER THAN THE HORIZON MINTS AT ITS ATTEMPT, AND ITS
// RE-RUN AT THE SAME INSTANT.
//
// A seat's mailbox retains what is published while nothing consumes it, so a
// seat nobody placed for a month — or a fleet that was down — is handed a
// backlog whose work began before every ledger's horizon. Minted at the
// trigger, every write of that turn was answered `unknown` on every node for
// good. The rebase is decided where the dispatch builds the turn its tools
// read from, so a dispatch path that skipped it is red here; and a re-run of
// the same trigger (the crash re-run a redelivery is) inherits what the first
// run recorded, or every write the first run made would be written twice.
func TestADispatchPastTheHorizonMintsAtItsAttempt(t *testing.T) {
	t.Parallel()
	rig := newRebaseRig(t)
	began := time.Now().UTC().Add(-31 * 24 * time.Hour)
	req := Request{Handle: "swe", RunID: newRunID(), WorkKey: "wk-backlog", WorkSince: began}

	before := time.Now().UTC().Truncate(time.Millisecond)
	first := mustDescribeTurn(t, rig.engine, rig.company, req)
	after := time.Now().UTC()
	if first.rebasedTo.Before(before) || first.rebasedTo.After(after) {
		t.Fatalf("a dispatch 31 days after its trigger mints at %s, want its own "+
			"attempt (%s..%s)", first.rebasedTo, before, after)
	}
	if got := toolsMint(t, rig.company, first); !got.Equal(first.rebasedTo) {
		t.Fatalf("the dispatched turn's tools mint at %s, want the rebase %s", got, first.rebasedTo)
	}

	// THE RE-RUN, a new run of the same trigger: it inherits.
	req.RunID = newRunID()
	time.Sleep(2 * time.Millisecond)
	rerun := mustDescribeTurn(t, rig.engine, rig.company, req)
	if !rerun.rebasedTo.Equal(first.rebasedTo) {
		t.Fatalf("the re-run mints at %s, the first run at %s — every write the "+
			"first run made would be written a second time", rerun.rebasedTo, first.rebasedTo)
	}
}

// A DISPATCH INSIDE THE HORIZON MINTS AT ITS TRIGGER, AS EVERY TURN ALWAYS HAS.
func TestADispatchInsideTheHorizonMintsAtItsTrigger(t *testing.T) {
	t.Parallel()
	rig := newRebaseRig(t)
	began := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Millisecond)
	tel := mustDescribeTurn(t, rig.engine, rig.company, Request{
		Handle: "swe", RunID: newRunID(), WorkKey: "wk-recent", WorkSince: began,
	})
	if !tel.rebasedTo.IsZero() {
		t.Fatalf("a dispatch a week after its trigger was rebased onto %s", tel.rebasedTo)
	}
	if got := toolsMint(t, rig.company, tel); !got.Equal(began) {
		t.Fatalf("the dispatched turn's tools mint at %s, want the trigger's %s", got, began)
	}
}

// A DISPATCH THAT CANNOT READ WHERE ITS WORK IS MINTED IS NOT DESCRIBED, and so
// never runs: the error reaches the dispatcher, which hands the delivery back.
func TestADispatchThatCannotReadItsRebaseIsRefused(t *testing.T) {
	t.Parallel()
	outage := errors.New("the coordination store is unreachable")
	e := &Engine{backends: &Backends{Fleet: failingFleet{memFleet: coordmem.NewFleet(), err: outage}}}
	rig := newRebaseRig(t)
	_, err := e.describeTurn(context.Background(), rig.company, Request{
		Handle: "swe", RunID: newRunID(), WorkKey: "wk-backlog",
		WorkSince: time.Now().UTC().Add(-31 * 24 * time.Hour),
	})
	if !errors.Is(err, outage) {
		t.Fatalf("describeTurn over an unreachable store = %v, want the outage", err)
	}
}

// AND THE TURN IT WOULD HAVE DESCRIBED DOES NOT RUN: runTurn returns the error
// before a runner exists, which is what the dispatcher NAKs — nothing acted, so
// the redelivery runs it cleanly once the store answers.
func TestADispatchThatCannotReadItsRebaseDoesNotRun(t *testing.T) {
	t.Parallel()
	outage := errors.New("the coordination store is unreachable")
	e := &Engine{backends: &Backends{Fleet: failingFleet{memFleet: coordmem.NewFleet(), err: outage}}}
	e.epoch.current.Store(newRebaseRig(t).company)
	res, err := e.runTurn(context.Background(), Request{
		Handle: "swe", RunID: newRunID(), WorkKey: "wk-backlog",
		WorkSince: time.Now().UTC().Add(-31 * 24 * time.Hour),
	})
	if !errors.Is(err, outage) {
		t.Fatalf("runTurn over an unreachable store = %v, want the outage", err)
	}
	if reason, abandon := turn.Abandon(res, err); abandon {
		t.Fatalf("the refused turn would be abandoned (%s) rather than redelivered", reason)
	}
}

// failingFleet is the fleet twin with its rebase reads failing.
type failingFleet struct {
	*memFleet
	err error
}

// memFleet names the twin for embedding, since a field called Fleet would hide
// the contract's own Fleet method.
type memFleet = coordmem.Fleet

func (f failingFleet) Rebase(context.Context, string) (time.Time, uint64, error) {
	return time.Time{}, 0, f.err
}

// A RESUME PAST THE HORIZON MINTS AT ITS ATTEMPT; ONE AFTER AN EARLIER HALF
// REBASED MINTS WHERE THAT HALF DID.
//
// The resume builds its turn in describeResume, the frame that hands the
// runner the identity its tools derive every id from — so a resume path that
// skipped the rule is red on the first case. The second is the one the
// reviewers found: half 2 of a turn rebased onto R1 calls run_sandbox again,
// and half 3's resume two hours on, judged against the work's start alone, was
// rebased onto its own instant and wrote twice whatever half 2 wrote and half 3
// repeated. It inherits R1 instead.
func TestAResumePastTheHorizonMintsAtItsAttemptOrItsHalfsRebase(t *testing.T) {
	t.Parallel()
	began := time.Now().UTC().Add(-40 * 24 * time.Hour)
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	run := sandbox.PendingRun{
		TurnID: statelog.NewOpID(began, ""), WorkKey: "wk-parked", WorkSince: began,
		AgentHandle: "swe",
	}
	resume := func(rig *rebaseRig) turnTelemetry {
		return mustDescribeResume(t, rig.engine, rig.company, resumeInput{
			Run: run, Turn: resumedTurn(run, seat, rig.company.Org),
		})
	}

	t.Run("the first resume past the horizon", func(t *testing.T) {
		t.Parallel()
		rig := newRebaseRig(t)
		before := time.Now().UTC().Truncate(time.Millisecond)
		tel := resume(rig)
		if tel.rebasedTo.Before(before) || tel.rebasedTo.After(time.Now().UTC()) {
			t.Fatalf("a resume forty days on mints at %s, want its own attempt", tel.rebasedTo)
		}
		if got := toolsMint(t, rig.company, tel); !got.Equal(tel.rebasedTo) {
			t.Fatalf("the resumed half's tools mint at %s, want the rebase %s", got, tel.rebasedTo)
		}
	})
	t.Run("a resume after a half that rebased", func(t *testing.T) {
		t.Parallel()
		rig := newRebaseRig(t)
		earlier := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
		if _, err := rig.engine.backends.Fleet.RecordRebase(t.Context(), "wk-parked", earlier, 0); err != nil {
			t.Fatalf("RecordRebase: %v", err)
		}
		tel := resume(rig)
		if !tel.rebasedTo.Equal(earlier) {
			t.Fatalf("the next half mints at %s, want the rebase the half before it "+
				"recorded, %s", tel.rebasedTo, earlier)
		}
		if got := toolsMint(t, rig.company, tel); !got.Equal(earlier) {
			t.Fatalf("the next half's tools mint at %s, want %s", got, earlier)
		}
	})
}

// A RESUME INSIDE THE HORIZON MINTS AT ITS WORK'S START, so a first-half write
// the resumed half repeats collapses onto the first copy.
func TestAResumeInsideTheHorizonMintsAtItsWorksStart(t *testing.T) {
	t.Parallel()
	rig := newRebaseRig(t)
	began := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Millisecond)
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	run := sandbox.PendingRun{
		TurnID: statelog.NewOpID(began, ""), WorkKey: "wk-parked", WorkSince: began,
		AgentHandle: "swe",
	}
	tel := mustDescribeResume(t, rig.engine, rig.company, resumeInput{
		Run: run, Turn: resumedTurn(run, seat, rig.company.Org),
	})
	if !tel.rebasedTo.IsZero() {
		t.Fatalf("a resume a week on was rebased onto %s", tel.rebasedTo)
	}
	if got := toolsMint(t, rig.company, tel); !got.Equal(began) {
		t.Fatalf("the resumed half's tools mint at %s, want the work's start %s", got, began)
	}
}

// A RESUME THAT CANNOT READ WHERE ITS WORK IS MINTED DOES NOT RUN — resumeTurn
// returns before building a runner, and the coordinator hands its claim back
// for a retry, which judges the rebase again.
func TestAResumeThatCannotReadItsRebaseIsHandedBack(t *testing.T) {
	t.Parallel()
	outage := errors.New("the coordination store is unreachable")
	e := &Engine{backends: &Backends{Fleet: failingFleet{memFleet: coordmem.NewFleet(), err: outage}}}
	rig := newRebaseRig(t)
	began := time.Now().UTC().Add(-40 * 24 * time.Hour)
	seat := rig.company.Org.Roles[0]
	run := sandbox.PendingRun{
		TurnID: statelog.NewOpID(began, ""), WorkKey: "wk-parked", WorkSince: began,
		AgentHandle: "swe",
	}
	err := e.resumeTurn(context.Background(), resumeInput{
		Company: rig.company, Run: run, Turn: resumedTurn(run, seat, rig.company.Org),
	})
	if !errors.Is(err, outage) {
		t.Fatalf("resumeTurn over an unreachable store = %v, want the outage", err)
	}
}
