package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/provision"
)

// fakeReconciler answers with whatever the test set, and counts its passes.
type fakeReconciler struct {
	kind     Kind
	findings []Finding
	err      error

	// observe sees the context the pass was actually handed, for the case
	// whose subject is which context that is.
	observe func(context.Context)

	mu     sync.Mutex
	passes int
}

func (f *fakeReconciler) Kind() Kind { return f.kind }

func (f *fakeReconciler) Reconcile(ctx context.Context) ([]Finding, error) {
	if f.observe != nil {
		f.observe(ctx)
	}
	f.mu.Lock()
	f.passes++
	f.mu.Unlock()
	return f.findings, f.err
}

func (f *fakeReconciler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passes
}

// fakeStore is the coordination store as a map.
type fakeStore struct {
	mu        sync.Mutex
	rows      map[Kind]State
	forgot    []Kind
	loadErr   error
	saveErr   error
	forgetErr error
}

func newStore(rows ...State) *fakeStore {
	s := &fakeStore{rows: map[Kind]State{}}
	for _, row := range rows {
		s.rows[row.Kind] = row
	}
	return s
}

func (s *fakeStore) LoadIntegrations(context.Context) ([]State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make([]State, 0, len(s.rows))
	for _, row := range s.rows {
		out = append(out, row)
	}
	return out, nil
}

func (s *fakeStore) LoadIntegration(_ context.Context, kind Kind) (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return State{}, false, s.loadErr
	}
	row, ok := s.rows[kind]
	return row, ok, nil
}

func (s *fakeStore) SaveIntegration(_ context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.rows[state.Kind] = state
	return nil
}

func (s *fakeStore) ForgetIntegration(_ context.Context, kind Kind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forgetErr != nil {
		return s.forgetErr
	}
	delete(s.rows, kind)
	s.forgot = append(s.forgot, kind)
	return nil
}

// has reports presence without failing, for the cases whose subject is
// whether a row survived at all.
func (s *fakeStore) has(kind Kind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[kind]
	return ok
}

func (s *fakeStore) get(t *testing.T, kind Kind) State {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[kind]
	if !ok {
		t.Fatalf("no state was recorded for %s", kind)
	}
	return row
}

// at builds a worker whose clock is frozen at now.
func at(t *testing.T, now time.Time, store Store, claim DutyFunc, regs ...Registration) *Worker {
	t.Helper()
	w, err := New(Options{
		Registrations: regs, Store: store, ClaimDuty: claim,
		Now: func() time.Time { return now },
		// PINNED, so a case can assert an exact instant. What the real
		// spread does is asserted on its own, below.
		Spread: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// configuredAt is [at] with a document test installed, for the cases whose
// subject is what the loop forgets.
func configuredAt(
	t *testing.T, now time.Time, store Store, configured func(Kind) bool,
	regs ...Registration,
) *Worker {
	t.Helper()
	w, err := New(Options{
		Registrations: regs, Store: store, Configured: configured,
		Now:    func() time.Time { return now },
		Spread: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// guardedAt is [at] with a guard installed, for the cases whose subject is the
// guard itself.
func guardedAt(
	t *testing.T, now time.Time, store Store, reg Registration, guard Guard,
) *Worker {
	t.Helper()
	w, err := New(Options{
		Registrations: []Registration{reg}, Store: store, Guard: guard,
		Now:    func() time.Time { return now },
		Spread: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// A surface nothing has ever reconciled is due at once, so a fresh company
// starts converging on the first tick rather than after one interval.
func TestFirstSightIsDue(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	if r.count() != 1 {
		t.Fatalf("a surface with no recorded state ran %d passes, want 1", r.count())
	}
	row := store.get(t, KindGitLab)
	if row.Report.Phase != PhaseReady {
		t.Fatalf("a pass that found nothing recorded %s, want ready", row.Report.Phase)
	}
	if !row.SettledAt.Equal(now) {
		t.Fatalf("SettledAt is %s, want %s", row.SettledAt, now)
	}
	if !row.NextAttemptAt.Equal(now.Add(DefaultSchedule.Settled)) {
		t.Fatalf("the next attempt is %s, want %s",
			row.NextAttemptAt, now.Add(DefaultSchedule.Settled))
	}
}

// A surface whose next attempt is in the future is left alone. Without this
// every surface would be reconciled every fifteen seconds, which is the one
// design that makes the loop too expensive to leave switched on.
func TestNotDueIsNotReconciled(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore(State{Kind: KindGitLab, NextAttemptAt: now.Add(time.Minute)})

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("a surface due in a minute ran %d passes", r.count())
	}
}

// A DOCUMENT THAT CHANGED OUTRANKS THE CADENCE.
//
// The wait is for asking a third-party app again, and it is right: nobody
// wants a timer hammering GitHub every fifteen seconds. It is wrong the
// moment the answer changes HERE. An operator who installs an agent's app is
// redirected straight back to a card still holding the previous pass's
// finding, "this agent has no app of its own", printed above the same card's
// roster reporting that agent installed and ready: one screen, two answers,
// for as long as the settled cadence had left to run.
func TestAChangedConfigurationMakesEverySurfaceDue(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore(State{Kind: KindGitLab, NextAttemptAt: now.Add(10 * time.Minute)})
	w := at(t, now, store, nil, Registration{Reconciler: r})

	// Settled ten minutes out, so nothing runs.
	w.Tick(context.Background())
	if r.count() != 0 {
		t.Fatalf("a surface due in ten minutes ran %d passes", r.count())
	}

	w.MarkStale()
	w.Tick(context.Background())
	if r.count() != 1 {
		t.Fatalf("an applied revision left the surface on its old cadence: %d passes", r.count())
	}

	// AND ONE APPLY IS ONE SWEEP. Left set, the flag would make every
	// later tick ignore the cadence for ever, which is the fifteen-second
	// hammering the wait exists to prevent.
	w.Tick(context.Background())
	if r.count() != 1 {
		t.Fatalf("the loop kept ignoring the cadence after one apply: %d passes", r.count())
	}
}

// THE SINGLETON'S POINT. A coordination store that could not say whether this
// node holds the duty must not be read as "it is mine". Two nodes reconciling
// one surface both see a seat with no account and both create one, and the
// third-party app ends up with two identities for one agent that no later pass can
// detect or repair.
func TestUnknownDutyDoesNotReconcile(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeReconciler{kind: KindGitLab}
	unknown := func(context.Context) (bool, error) {
		return false, errors.New("the coordination store could not be reached")
	}

	at(t, now, newStore(), unknown, Registration{Reconciler: r}).Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("a tick with an unknown duty ran %d passes", r.count())
	}
}

// A node that does not hold the duty does nothing, quietly.
func TestDutyNotHeldDoesNotReconcile(t *testing.T) {
	r := &fakeReconciler{kind: KindGitLab}
	refuse := func(context.Context) (bool, error) { return false, nil }

	at(t, time.Now().UTC(), newStore(), refuse, Registration{Reconciler: r}).
		Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("a node without the duty ran %d passes", r.count())
	}
}

// A store that cannot be read reconciles NOTHING, rather than reconciling
// everything as though it had never run. Treating an unreadable store as an
// empty one would re-run every surface on every tick for as long as the
// outage lasted.
func TestUnreadableStoreReconcilesNothing(t *testing.T) {
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()
	store.loadErr = errors.New("coordination store unavailable")

	at(t, time.Now().UTC(), store, nil, Registration{Reconciler: r}).
		Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("a tick with an unreadable store ran %d passes", r.count())
	}
}

// A fault is a wait, and it does not leave the previous pass's findings
// standing under this pass's timestamp: a pass that failed did not observe
// the world, and keeping what an earlier one saw ages a stale answer into a
// current one.
func TestAFaultIsAWaitAndDropsStaleFindings(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab, err: errors.New("gitlab: 502 from the instance")}
	store := newStore(State{
		Kind:      KindGitLab,
		Findings:  []Finding{{Kind: FindingGrantShort, Subject: "ceo"}},
		SettledAt: now.Add(-time.Hour),
	})

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	row := store.get(t, KindGitLab)
	if row.Findings != nil {
		t.Fatalf("a failed pass kept %d findings from an earlier one", len(row.Findings))
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts is %d, want 1", row.Attempts)
	}
	if !strings.Contains(row.LastError, "502") {
		t.Fatalf("LastError is %q, want the third-party app's own message", row.LastError)
	}
	if row.Outcome != OutcomeWaiting {
		t.Fatalf("a fault recorded outcome %q, want %q", row.Outcome, OutcomeWaiting)
	}
	// SettledAt is a fact about when it last WAS settled, so a later
	// failure must not erase it.
	if row.SettledAt.IsZero() {
		t.Fatal("a failed pass cleared the record of when the surface last settled")
	}
}

// A pass that settles resets the backoff and clears the previous fault. Left
// standing, an old error would make a healthy integration read as broken for
// as long as nobody looked at the phase beside it.
func TestSettlingResetsAttemptsAndClearsTheFault(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore(State{
		Kind: KindGitLab, Attempts: 7, LastError: "gitlab: 502 from the instance",
	})

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	row := store.get(t, KindGitLab)
	if row.Attempts != 0 {
		t.Fatalf("attempts is %d after settling, want 0", row.Attempts)
	}
	if row.LastError != "" {
		t.Fatalf("LastError is %q after settling, want it cleared", row.LastError)
	}
}

// Consecutive unsettled passes stretch the wait out rather than retrying at a
// fixed rate.
func TestAttemptsAccumulateAcrossPasses(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{
		kind:     KindGitLab,
		findings: []Finding{{Kind: FindingGrantPending, Subject: "ceo"}},
	}
	store := newStore()

	var last time.Duration
	for pass := 1; pass <= 4; pass++ {
		// The clock stands still, so each tick has to be made due by the
		// previous one's own record rather than by time passing.
		store.mu.Lock()
		row := store.rows[KindGitLab]
		row.NextAttemptAt = time.Time{}
		store.rows[KindGitLab] = row
		store.mu.Unlock()

		at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

		got := store.get(t, KindGitLab)
		if got.Attempts != pass {
			t.Fatalf("pass %d recorded %d attempts", pass, got.Attempts)
		}
		wait := got.NextAttemptAt.Sub(now)
		if pass > 1 && wait <= last {
			t.Fatalf("pass %d waits %s, no longer than the previous %s", pass, wait, last)
		}
		last = wait
	}
}

// The advisory settles. An integration whose only finding is excess access is
// working, so it is re-read on the settled cadence rather than retried as
// though something were in flight.
func TestAnAdvisorySettles(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{
		kind: KindGitLab,
		findings: []Finding{{
			Kind: FindingGrantExcess, Subject: "ceo",
			Detail: "ceo holds owner on api-gateway", ActionURL: "https://example.com/x",
		}},
	}
	store := newStore()

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	row := store.get(t, KindGitLab)
	if row.Outcome != OutcomeSettled {
		t.Fatalf("an advisory recorded outcome %q, want %q", row.Outcome, OutcomeSettled)
	}
	if row.Report.Detail == "" || row.Report.ActionURL == "" {
		t.Fatalf("the advisory lost its sentence or its link: %+v", row.Report)
	}
	if !row.NextAttemptAt.Equal(now.Add(DefaultSchedule.Settled)) {
		t.Fatalf("the next attempt is %s, want the settled cadence", row.NextAttemptAt)
	}
}

// A TEARDOWN-ONLY surface the company document no longer declares is
// forgotten, so its last row does not sit in the fleet's view describing an
// integration that is gone — and, because that row carries an ADDRESS, so a
// later reconnect does not inherit the one from the company before it.
//
// Slack is the class and today the only member: no pass converges it, so
// nothing can ever return ErrNotConfigured for it, and until this existed its
// row outlived the block for the life of the deployment.
func TestADepartedTeardownOnlySurfaceIsForgotten(t *testing.T) {
	now := time.Now().UTC()
	store := newStore(State{Kind: KindSlack, Endpoint: "https://old.example.com"})

	w := configuredAt(t, now, store, func(Kind) bool { return false },
		Registration{Only: KindSlack, Disconnector: &fakeDisconnector{}})
	w.Tick(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if !slices.Contains(store.forgot, KindSlack) {
		t.Fatalf("slack was not forgotten; forgot %v", store.forgot)
	}
	if _, still := store.rows[KindSlack]; still {
		t.Fatal("slack's row survived being forgotten")
	}
}

// AND ONE THE DOCUMENT STILL DECLARES IS KEPT, which is the whole difference
// and the direction that fails silently: deleting it takes away the one
// warning an operator ever gets that a moved public base has stranded Slack's
// Request URL.
func TestAConfiguredTeardownOnlySurfaceKeepsItsRow(t *testing.T) {
	now := time.Now().UTC()
	store := newStore(State{Kind: KindSlack, Endpoint: "https://now.example.com"})

	w := configuredAt(t, now, store, func(Kind) bool { return true },
		Registration{Only: KindSlack, Disconnector: &fakeDisconnector{}})
	w.Tick(context.Background())

	if !store.has(KindSlack) {
		t.Fatal("a surface the document still declares was forgotten")
	}
}

// A SURFACE WITH A RECONCILER IS NEVER FORGOTTEN BY THIS PATH, however the
// document-shaped test answers. Its own pass owns the question and answers it
// better — every pass reads `enabled: false` as not configured, where a block
// test sees a block and says yes — so two authorities here would make a paused
// integration flap between them.
func TestAReconciledSurfaceIsNotForgottenByTheDocumentTest(t *testing.T) {
	now := time.Now().UTC()
	store := newStore(State{Kind: KindGitLab, Report: Ready()})

	w := configuredAt(t, now, store, func(Kind) bool { return false },
		Registration{Reconciler: &fakeReconciler{kind: KindGitLab}})
	w.Tick(context.Background())

	if !store.has(KindGitLab) {
		t.Fatal("the document test forgot a surface whose own pass owns that answer")
	}
}

// AND A TEARDOWN IN FLIGHT IS NEVER FORGOTTEN, even once the block is gone.
//
// A disconnect removes the block BEFORE its last retry can succeed, and a
// teardown that failed leaves the row disconnecting on purpose. Forgetting it
// here abandons an unfinished teardown: the third-party app's webhooks stay
// live, the card disappears from the screen, and nothing says so.
func TestATeardownInFlightIsNotForgotten(t *testing.T) {
	now := time.Now().UTC()
	store := newStore(State{Kind: KindSlack, Disconnecting: true})

	w := configuredAt(t, now, store, func(Kind) bool { return false },
		Registration{Only: KindSlack, Disconnector: &fakeDisconnector{
			err: errors.New("slack refused the delete"),
		}})
	w.Tick(context.Background())

	if !store.has(KindSlack) {
		t.Fatal("an unfinished teardown's row was deleted, abandoning the disconnect")
	}
}

// A NODE THAT CANNOT SAY WHAT THE DOCUMENT DECLARES FORGETS NOTHING, because
// every wrong answer here deletes a row somebody needs.
func TestNothingIsForgottenWithoutADocumentTest(t *testing.T) {
	now := time.Now().UTC()
	store := newStore(State{Kind: KindSlack, Endpoint: "https://old.example.com"})

	at(t, now, store, nil, Registration{Only: KindSlack, Disconnector: &fakeDisconnector{}}).
		Tick(context.Background())

	if !store.has(KindSlack) {
		t.Fatal("a node with no document test deleted a row on a guess")
	}
}

// Surfaces are converged in the canonical order however the config listed
// them, so two nodes with one company agree and a status listing does not
// reshuffle when the duty moves between them.
//
// THE CANONICAL ORDER IS [ConvergeOrder], NOT [Kinds], and the fixture has to
// contain a pair that tells them apart or this asserts nothing. It did not:
// Slack, GitHub and Datadog sort identically under both, so the whole change
// from one to the other left this green.
//
// Atlassian before Jira is the pair that differs, and it is a real dependency
// rather than a preference. Atlassian is where an agent's service account is
// CREATED; Jira is a product that account works in, and its pass checks for the
// account with the credential Atlassian minted. In reading order Jira went
// first, so over a reconnect the tracker found the seat still mapped to the
// account the disconnect had deleted and showed "Action required — you, at the
// third-party app" with a 401 for about thirty-five seconds, until Atlassian's
// own pass made the new one. Nobody had anything to do.
func TestOrderIsCanonicalNotRegistrationOrder(t *testing.T) {
	w, err := New(Options{
		Store: newStore(),
		Registrations: []Registration{
			{Reconciler: &fakeReconciler{kind: KindSlack}},
			{Reconciler: &fakeReconciler{kind: KindJira}},
			{Reconciler: &fakeReconciler{kind: KindDatadog}},
			{Reconciler: &fakeReconciler{kind: KindAtlassian}},
			{Reconciler: &fakeReconciler{kind: KindGitHub}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []Kind{KindAtlassian, KindJira, KindSlack, KindGitHub, KindDatadog}
	if !slices.Equal(w.order, want) {
		t.Fatalf("order is %v, want %v", w.order, want)
	}
}

// Two reconcilers for one surface would each overwrite the other's status
// every tick, and the last writer would decide what an operator sees.
func TestNewRefusesADuplicateSurface(t *testing.T) {
	_, err := New(Options{
		Store: newStore(),
		Registrations: []Registration{
			{Reconciler: &fakeReconciler{kind: KindSlack}},
			{Reconciler: &fakeReconciler{kind: KindSlack}},
		},
	})
	if err == nil {
		t.Fatal("two reconcilers for slack were accepted")
	}
	if !strings.Contains(err.Error(), "slack") {
		t.Fatalf("the error does not name the surface: %v", err)
	}
}

// A kind this build does not converge is refused at construction rather than
// skipped silently at every tick, where nothing would ever say why.
func TestNewRefusesAnUnknownSurface(t *testing.T) {
	_, err := New(Options{
		Store:         newStore(),
		Registrations: []Registration{{Reconciler: &fakeReconciler{kind: Kind("pagerduty")}}},
	})
	if err == nil {
		t.Fatal("an unknown surface was accepted")
	}
	if !strings.Contains(err.Error(), "pagerduty") {
		t.Fatalf("the error does not name the surface: %v", err)
	}
}

// Reconcilers with nowhere to record what they find are refused up front. A
// worker that ran passes and dropped every result would look exactly like one
// whose third-party apps were all healthy.
func TestNewRefusesReconcilersWithNoStore(t *testing.T) {
	_, err := New(Options{
		Registrations: []Registration{{Reconciler: &fakeReconciler{kind: KindSlack}}},
	})
	if err == nil {
		t.Fatal("a worker with reconcilers and no store was accepted")
	}
}

// No reconcilers is a valid company, not an error: a founder who wires no
// provisioning still runs an engine.
func TestNewAcceptsNoReconcilers(t *testing.T) {
	w, err := New(Options{})
	if err != nil {
		t.Fatalf("New with no registrations: %v", err)
	}
	// Start is a no-op rather than a goroutine that ticks over nothing.
	w.Start(context.Background())
	w.Stop()
}

// A store that cannot record the result does not stop the pass, and does not
// wedge the loop. The work at the third-party app is already durable; what is lost is
// the record of it, so the next tick re-runs a pass with nothing left to do.
func TestASaveFailureDoesNotStopTheLoop(t *testing.T) {
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()
	store.saveErr = errors.New("coordination store unavailable")

	w := at(t, time.Now().UTC(), store, nil, Registration{Reconciler: r})
	w.Tick(context.Background())
	w.Tick(context.Background())

	if r.count() != 2 {
		t.Fatalf("the loop ran %d passes across two ticks, want 2", r.count())
	}
}

// Stop waits for a tick already in flight, and a second Stop is harmless.
func TestStartAndStopAreIdempotent(t *testing.T) {
	w := at(t, time.Now().UTC(), newStore(), nil,
		Registration{Reconciler: &fakeReconciler{kind: KindGitLab}})

	ctx := context.Background()
	w.Start(ctx)
	w.Start(ctx)
	w.Stop()
	w.Stop()
}

// fakeDisconnector records what a teardown was asked to do.
type fakeDisconnector struct {
	err error
	// removed is the end state this teardown reports, for the cases about
	// what a disconnect records.
	removed provision.Removed

	mu          sync.Mutex
	calls       int
	removeSeats bool
}

func (f *fakeDisconnector) Disconnect(
	_ context.Context, removeSeats bool,
) (provision.Removed, error) {
	f.mu.Lock()
	f.calls++
	f.removeSeats = removeSeats
	f.mu.Unlock()
	return f.removed, f.err
}

func (f *fakeDisconnector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A SURFACE BEING TAKEN AWAY IS NOT RECONCILED.
//
// Its block stays in the company document for the whole teardown, because
// that block carries the credential the teardown authenticates with. A
// reconcile over it would therefore find it configured, converge it, and
// report it healthy while somebody was waiting for it to go, and on a
// third-party app whose pass registers hooks, it would put back exactly what
// the teardown was removing.
func TestATearingDownSurfaceIsTornDownRatherThanReconciled(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindJira}
	d := &fakeDisconnector{}
	store := newStore(State{Kind: KindJira, Disconnecting: true, RemoveSeats: true})

	w := at(t, now, store, nil, Registration{Reconciler: r, Disconnector: d})
	w.Tick(context.Background())

	if r.count() != 0 {
		t.Errorf("the reconciler ran %d times over a surface being removed", r.count())
	}
	if d.count() != 1 {
		t.Fatalf("the teardown ran %d times, want 1", d.count())
	}
	if !d.removeSeats {
		t.Error("the operator's answer about removing accounts did not reach the third-party app")
	}
	// It finished, so the row is gone: nothing is left to reconcile.
	if store.has(KindJira) {
		t.Error("a finished teardown left its status row behind")
	}
}

// A TEARDOWN THAT FAILS HOLDS THE SURFACE and is retried, rather than letting
// it fall back to looking connected.
func TestAFailedTeardownKeepsTheRowAndRetries(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindJira}
	d := &fakeDisconnector{err: errors.New("jira: 403 removing the webhook")}
	store := newStore(State{Kind: KindJira, Disconnecting: true})

	w := at(t, now, store, nil, Registration{Reconciler: r, Disconnector: d})
	w.Tick(context.Background())

	if !store.has(KindJira) {
		t.Fatal("a failed teardown forgot the row; the third-party app still holds what it registered")
	}
	row := store.get(t, KindJira)
	if row.Report.Phase != PhaseDisconnecting {
		t.Errorf("phase = %q, want %q", row.Report.Phase, PhaseDisconnecting)
	}
	if !row.Disconnecting {
		t.Error("the intent was dropped, so the next tick would reconcile it as connected")
	}
	if row.NextAttemptAt.IsZero() || !row.NextAttemptAt.After(now) {
		t.Error("no retry was scheduled, so the teardown would never be attempted again")
	}
	if r.count() != 0 {
		t.Error("the reconciler ran over a surface being removed")
	}
}

// A NODE THAT CANNOT DISCONNECT WRITES NOTHING. Its roles may leave the
// passes unwired while the loop still runs for every other surface, and the
// disconnect waits for a node that can rather than being recorded as stuck.
func TestANodeWithNoDisconnectorLeavesTheRowAlone(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindJira}
	store := newStore(State{Kind: KindJira, Disconnecting: true})

	w := at(t, now, store, nil, Registration{Reconciler: r})
	w.Tick(context.Background())

	if !store.has(KindJira) {
		t.Fatal("the disconnect intent was lost on a node that cannot act on it")
	}
	row := store.get(t, KindJira)
	if !row.Disconnecting {
		t.Fatal("the disconnect intent was lost on a node that cannot act on it")
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d: a node that did nothing counted an attempt, which "+
			"backs the retry off for a node that could have acted", row.Attempts)
	}
	if r.count() != 0 {
		t.Error("the reconciler ran over a surface being removed")
	}
}

// A NODE THAT CANNOT DISCONNECT *YET* WRITES NOTHING EITHER.
//
// The loop is armed when the engine is constructed and the surface a
// disconnect removes a block through is installed when the API is wired,
// several hundred milliseconds later. A tick in that window is early, not
// broken: recording an attempt would back off the retry that was about to
// work, and recording a fault would put an error on the screen for a
// disconnect nobody has failed to do.
func TestATeardownThatIsMerelyEarlyIsNotRecordedAsAFailure(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindJira}
	d := &fakeDisconnector{err: fmt.Errorf("%w: no config surface yet", ErrDisconnectUnavailable)}
	store := newStore(State{Kind: KindJira, Disconnecting: true})

	w := at(t, now, store, nil, Registration{Reconciler: r, Disconnector: d})
	w.Tick(context.Background())

	row := store.get(t, KindJira)
	if row.Attempts != 0 {
		t.Errorf("attempts = %d: a tick that was early backed off the retry", row.Attempts)
	}
	if row.LastError != "" {
		t.Errorf("last_error = %q: an early tick put a fault on the screen", row.LastError)
	}
	if !row.Disconnecting {
		t.Error("the intent was lost")
	}
	// And it WAS attempted, so the moment the surface is wired the next
	// tick completes it.
	if d.count() != 1 {
		t.Errorf("the disconnector was called %d times", d.count())
	}
}

// A SAVE DOES NOT WAIT OUT THE CADENCE.
//
// The stale flag alone only promised that the NEXT tick would reconsider, and
// the next tick is up to a full interval away. An operator who pressed Save
// watched the card go on describing the configuration they had just replaced,
// and read that as the save not working. The cadence is for asking a
// third-party app again; this is the answer changing here.
func TestAnAppliedRevisionBringsTheTickForward(t *testing.T) {
	r := &fakeReconciler{kind: KindGitLab}
	// AN INTERVAL NO TEST COULD WAIT OUT, so a pass inside this test can
	// only have come from the wake.
	w, err := New(Options{
		Registrations: []Registration{{Reconciler: r}},
		Store:         newStore(),
		Interval:      time.Hour,
		WakeSettle:    5 * time.Millisecond,
		Now:           func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Start(context.Background())
	defer w.Stop()

	// The loop's own first pass, which every start runs.
	waitForPasses(t, r, 1)

	w.MarkStale()
	waitForPasses(t, r, 2)

	// AND ONE OPERATOR ACTION IS ONE PASS. The setup dialog writes one
	// request per surface, so saving Atlassian applies three revisions in a
	// row; a tick each would ask three third-party apps three times for one
	// press of Save.
	before := r.count()
	w.MarkStale()
	w.MarkStale()
	w.MarkStale()
	waitForPasses(t, r, before+1)
	// PAST THE WINDOW BEFORE COUNTING, or a second tick still on its way
	// would read as a burst that folded.
	time.Sleep(20 * 5 * time.Millisecond)
	if got := r.count(); got != before+1 {
		t.Errorf("three applies ran %d passes, want the burst folded into one", got-before)
	}
}

// waitForPasses waits for the loop to have run at least n passes.
func waitForPasses(t *testing.T, r *fakeReconciler, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.count() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the loop ran %d passes, want %d: the tick never came forward", r.count(), n)
}

// A PASS STAMPS THE ADDRESS ONLY WHERE IT IS THE THING KEEPING IT CURRENT.
//
// Slack's Request URL is a field a person typed into a settings page. A pass
// that stamped the base it ran against would be claiming the third-party app
// had been told about an address nobody has told it about, which turns the
// one warning an operator gets about a moved public base into a card
// reporting Connected.
func TestOnlyAnEngineRegisteredAddressIsStampedByAPass(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const base = "https://now.example.com"
	const stale = "https://old.example.com"

	// The address a person set Slack up against, already on the row.
	store := newStore(
		State{Kind: KindSlack, Endpoint: stale},
		// A row an earlier build stamped on a surface with no inbound
		// address at all.
		State{Kind: KindMattermost, Endpoint: stale},
	)
	w, err := New(Options{
		Registrations: []Registration{
			{Reconciler: &fakeReconciler{kind: KindGitLab}},
			{Reconciler: &fakeReconciler{kind: KindSlack}},
			{Reconciler: &fakeReconciler{kind: KindMattermost}},
		},
		Store:    store,
		Now:      func() time.Time { return now },
		Endpoint: func() string { return base },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	if got := store.get(t, KindGitLab).Endpoint; got != base {
		t.Errorf("gitlab endpoint = %q, want %q: its pass registers the hook", got, base)
	}
	if got := store.get(t, KindSlack).Endpoint; got != stale {
		t.Errorf("slack endpoint = %q, want %q left alone: only a person "+
			"can move an address they typed at the third-party app", got, stale)
	}
	if got := store.get(t, KindMattermost).Endpoint; got != "" {
		t.Errorf("mattermost endpoint = %q, want it cleared: nothing delivers "+
			"to an address for this surface, so a stale one is a false alarm "+
			"waiting for the next base change", got)
	}
}

// A SURFACE SOMEBODY ELSE IS WRITING AT IS NOT EVEN READ.
//
// The provisioning guard is shared with the pass an operator runs from the
// dashboard and with a disconnect's teardown, so a tick can find it held. That
// is not a result: nothing was observed, so nothing may be recorded. An attempt
// counted here would back the cadence off for a pass that never ran, and a
// fault written here would describe as broken a surface that is at that moment
// being provisioned successfully by somebody else.
//
// The reconciler must not RUN either, which is the half a sentinel returned
// out of the pass could never give: by the time a pass can say "somebody else
// holds this", it has already reached the third-party app.
func TestABusySurfaceIsNotTouchedAtAll(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()

	w := guardedAt(t, now, store, Registration{Reconciler: r},
		func(context.Context, Kind) (context.Context, func(), bool, error) {
			return nil, nil, false, nil
		})
	w.Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("the reconciler ran %d times behind a held guard, want 0", r.count())
	}
	if store.has(KindGitLab) {
		t.Errorf("a tick that reconciled nothing wrote a status row: %+v",
			store.get(t, KindGitLab))
	}
}

// AND A GUARD THAT COULD NOT ANSWER IS THE SAME OUTCOME, for the opposite
// reason: a coordination store that failed has not said the surface is idle,
// and acting on that guess is what creates the duplicate account the guard
// exists to prevent. Three-valued, and the third value must not collapse into
// either of the other two.
func TestAnUnreadableGuardTouchesNothing(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()

	w := guardedAt(t, now, store, Registration{Reconciler: r},
		func(context.Context, Kind) (context.Context, func(), bool, error) {
			return nil, nil, false, errors.New("coordination store unreachable")
		})
	w.Tick(context.Background())

	if r.count() != 0 {
		t.Fatalf("the reconciler ran %d times on an unreadable guard, want 0", r.count())
	}
	if store.has(KindGitLab) {
		t.Errorf("an unreadable guard wrote a status row: %+v", store.get(t, KindGitLab))
	}
}

// THE GUARD IS GIVEN BACK however the pass went, or the first surface to fail
// would lock every later writer out for the whole lease TTL.
func TestTheGuardIsReleasedAfterAFailedPass(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab, err: errors.New("vendor unreachable")}
	store := newStore()

	var taken, released int
	w := guardedAt(t, now, store, Registration{Reconciler: r},
		func(ctx context.Context, _ Kind) (context.Context, func(), bool, error) {
			taken++
			return ctx, func() { released++ }, true, nil
		})
	w.Tick(context.Background())

	if taken != 1 || released != 1 {
		t.Fatalf("guard taken %d and released %d times, want 1 and 1", taken, released)
	}
}

// AND AN ORDINARY FAULT STILL IS RECORDED, or the arm above would be a way to
// lose every real failure.
func TestAnOrdinaryFaultIsStillRecorded(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab, err: errors.New("the vendor refused")}
	store := newStore()

	at(t, now, store, nil, Registration{Reconciler: r}).Tick(context.Background())

	if !store.has(KindGitLab) {
		t.Fatal("a failed pass recorded nothing")
	}
	if got := store.get(t, KindGitLab); got.LastError == "" {
		t.Error("the recorded row carries no fault")
	}
}

// THE ENDPOINT RULE IS ONE RULE, and it is three-way rather than a stamp.
//
// Two writers share these rows: this loop's tick and the pass an operator runs
// from the dashboard. The rule lived inside the loop and the dashboard's pass
// stamped every surface unconditionally, so one row meant different things
// depending on which writer touched it last.
func TestStampEndpointFollowsTheSurfacesIngress(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind  Kind
		start string
		want  string
	}{
		// A pass registers the delivery, so where it ran IS where the
		// registration points.
		"engine-registered takes the current address": {
			kind: KindGitLab, start: "https://old.example.com", want: "https://now.example.com",
		},
		// A person typed the address at the third-party app. Stamping it
		// would turn the one warning about a moved address into a green row.
		"operator-typed is left exactly as it was": {
			kind: KindSlack, start: "https://typed.example.com", want: "https://typed.example.com",
		},
		// Nothing delivers to an address, so carrying one is a false alarm
		// waiting for the public base to move. Cleared, not merely skipped,
		// so a row an earlier build stamped converges.
		"a surface with no inbound is cleared": {
			kind: KindAtlassian, start: "https://stale.example.com", want: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := State{Kind: tc.kind, Endpoint: tc.start}
			StampEndpoint(&state, tc.kind, "https://now.example.com")
			if state.Endpoint != tc.want {
				t.Errorf("Endpoint = %q, want %q", state.Endpoint, tc.want)
			}
		})
	}
}

// A PEER'S SURFACE IS SKIPPED, NOT DELETED.
//
// The status rows are the FLEET's, written by whichever build ran the last
// pass, and a rolling upgrade puts a newer node's kind in front of an older
// reader. [Kind.Valid]'s own doc promises what happens then: "an unknown kind
// is SKIPPED by the worker and rendered as-is by the API."
//
// It was deleted instead, and this build cannot tell a peer's kind from a
// departed one by looking at its own registrations — both are simply absent.
// So an older node erased the newer node's status on every tick and the newer
// node wrote it back on every pass, for the length of the upgrade.
//
// The sweep now walks this build's own canonical order rather than the rows it
// found, so a kind it has never heard of is not merely spared but never
// considered — which is the same promise made structurally instead of by a
// guard somebody could drop.
func TestAKindThisBuildDoesNotKnowIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	peer := Kind("a-surface-from-a-newer-build")
	store := newStore(State{Kind: peer, Endpoint: "https://engine.example.com"})

	w := configuredAt(t, now, store,
		// The harshest answer available: this document declares NOTHING.
		func(Kind) bool { return false },
		Registration{Reconciler: &fakeReconciler{kind: KindGitLab}})
	w.Tick(context.Background())

	if !store.has(peer) {
		t.Error("a surface only a newer build knows was deleted from the fleet's status")
	}
}

// THE DUTY IS RE-CLAIMED BEFORE EACH SURFACE, not sampled once per tick.
//
// The claim's TTL is a small multiple of the tick interval, and the sweep
// makes network calls to every configured surface in turn — so a tick across
// eight vendors outlives it easily, and a duty that lapsed mid-tick means a
// second node is already reconciling the surfaces this one has not reached.
//
// The re-claim sits past the not-due check, so a tick with nothing to do
// makes none of them.
func TestTheDutyIsReclaimedBeforeEachSurface(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var claims int
	// Held for the first two asks — the tick's own, and the first
	// surface's — then lost, as a lapsed TTL looks from here.
	claim := func(context.Context) (bool, error) {
		claims++
		return claims <= 2, nil
	}
	first := &fakeReconciler{kind: KindJira}
	second := &fakeReconciler{kind: KindGitLab}

	at(t, now, newStore(), claim,
		Registration{Reconciler: first}, Registration{Reconciler: second}).
		Tick(context.Background())

	if first.count() != 1 {
		t.Errorf("the first surface ran %d times, want 1", first.count())
	}
	if second.count() != 0 {
		t.Errorf("the second surface ran after the duty was lost: %d passes",
			second.count())
	}
}

// AND A TICK WITH NOTHING DUE ASKS ONCE. The re-claim is a round trip, so it
// belongs on the work rather than on the sweep.
func TestATickWithNothingDueClaimsOnce(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var claims int
	claim := func(context.Context) (bool, error) { claims++; return true, nil }

	// Recorded as settled and not due again until well after now.
	store := newStore(State{
		Kind: KindJira, Report: Ready(), NextAttemptAt: now.Add(time.Hour),
	})
	at(t, now, store, claim, Registration{Reconciler: &fakeReconciler{kind: KindJira}}).
		Tick(context.Background())

	if claims != 1 {
		t.Errorf("a tick with nothing due claimed the duty %d times, want 1", claims)
	}
}

// A RENAMED REGISTRATION IS REPORTED, NOT DELETED.
//
// A Datadog webhook definition is addressed by NAME, that name is a live
// config field, and Datadog serves no listing — a GET on the collection
// answers 405. So renaming it creates a second definition and abandons the
// first, which goes on delivering correctly to this same engine for every
// monitor still naming it, and nothing can ever find it again. Deleting it
// would silence exactly those monitors; saying nothing left the operator with
// two definitions and no way to learn it.
func TestARenamedRegistrationIsReportedAsAnOrphan(t *testing.T) {
	t.Parallel()
	state := State{Registration: "crewlet"}

	got := StampRegistration(&state, "crewlet-prod")
	if len(got) != 1 {
		t.Fatalf("a rename produced %d finding(s), want one", len(got))
	}
	if got[0].Kind != FindingRegistrationOrphaned {
		t.Errorf("kind = %q, want %q", got[0].Kind,
			FindingRegistrationOrphaned)
	}
	// BOTH NAMES, because the operator needs to know which one to repoint
	// monitors at and which one to delete.
	if !strings.Contains(got[0].Detail, "crewlet-prod") ||
		!strings.Contains(got[0].Detail, `"crewlet"`) {
		t.Errorf("the finding names only one of the two definitions: %q", got[0].Detail)
	}
	// AND IT IS AN ADVISORY. Nothing is broken — both definitions deliver —
	// so a company carrying one is READY with a note rather than degraded.
	if report := Classify(got); report.Phase != PhaseReady {
		t.Errorf("an orphan classified %s; nothing about it is broken", report.Phase)
	}
	if state.Registration != "crewlet-prod" {
		t.Errorf("the recorded name is %q, so the next pass reports the rename "+
			"again for ever", state.Registration)
	}
}

// AND AN UNCHANGED NAME SAYS NOTHING, or every pass would report a rename.
func TestAnUnchangedRegistrationIsSilent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		previous, current string
	}{
		{"unchanged", "crewlet", "crewlet"},
		// A FIRST PASS is not a rename: there is nothing recorded to have
		// been left behind.
		{"first pass", "", "crewlet"},
		// AND A SURFACE WITH NO SUCH NAME never reports one. Every kind
		// but Datadog is in this case, so the alternative would be one
		// spurious advisory per surface the moment a row is written.
		{"no name at this surface", "crewlet", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := State{Registration: tc.previous}
			if got := StampRegistration(&state, tc.current); len(got) != 0 {
				t.Errorf("reported %+v", got)
			}
		})
	}
}

// THE ORPHAN REACHES THE REPORT, which is the half a unit test of
// StampRegistration cannot see.
//
// It did not: the stamp ran AFTER Observe had already folded the findings, so
// the appended finding went into a variable nothing read again. Nothing about
// the row looked wrong — the name was recorded correctly and the rename was
// simply never reported.
func TestARenamedRegistrationReachesTheStatusRow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore()
	store.rows[KindDatadog] = State{Kind: KindDatadog, Registration: "crewlet"}

	w, err := New(Options{
		Registrations: []Registration{{Reconciler: &fakeReconciler{kind: KindDatadog}}},
		Store:         store,
		Now:           func() time.Time { return now },
		Spread:        func(d time.Duration) time.Duration { return d },
		Registration:  func(Kind) string { return "crewlet-prod" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	row := store.get(t, KindDatadog)
	if row.Registration != "crewlet-prod" {
		t.Errorf("the recorded name is %q, so the rename is reported again "+
			"on every tick for ever", row.Registration)
	}
	var found bool
	for _, f := range row.Findings {
		if f.Kind == FindingRegistrationOrphaned {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rename reached no finding on the row: %+v", row.Findings)
	}
	// AND IT DOES NOT BREAK THE SURFACE. Both definitions deliver, so a
	// company carrying an orphan is connected with a note.
	if row.Report.Phase != PhaseReady {
		t.Errorf("phase = %s; an orphan is an advisory, not a fault", row.Report.Phase)
	}
}

// THE LOST DISCONNECT — the regression this whole guard exists for.
//
// A tick reads every row once to decide what is due. An operator pressing
// Disconnect writes the intent under the surface's own guard, on whichever
// node served the request, and that write can land after this tick's bulk read
// and before it reaches this surface. Folding the pass's outcome into the row
// it read FIRST puts a connected integration back over the top: the card flips
// from Disconnecting to connected and the operator presses the button again.
//
// coord.Integrations says this cannot happen because "every writer takes the
// surface's own provisioning lease first". Every writer did — and the loop then
// released it and wrote afterwards, from a snapshot taken before the pass. The
// fix is not a lock that was missing, it is holding the one that was already
// there across the read, the pass and the write alike.
//
// The disconnect is landed from inside the guard callback, which is exactly
// where a real one can land: markDisconnecting must wait for this guard, so the
// last instant it can get in is just before the loop takes it.
func TestADisconnectLandingBeforeTheGuardIsNotOverwritten(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore(State{Kind: KindGitLab, Report: Ready()})
	r := &fakeReconciler{kind: KindGitLab}

	w := guardedAt(t, now, store, Registration{Reconciler: r},
		func(ctx context.Context, kind Kind) (context.Context, func(), bool, error) {
			// The operator got in first, under this same guard.
			if err := store.SaveIntegration(ctx, State{
				Kind: kind, Report: Ready(), Disconnecting: true, RemoveSeats: true,
			}); err != nil {
				t.Fatalf("stage the disconnect: %v", err)
			}
			return ctx, func() {}, true, nil
		})
	w.Tick(context.Background())

	row := store.get(t, KindGitLab)
	if !row.Disconnecting {
		t.Fatalf("the tick overwrote an operator's disconnect: %+v", row)
	}
	if r.count() != 0 {
		t.Fatalf("a surface being taken away was converged %d times, want 0 — a "+
			"reconcile here finds the block still in the document, converges it, "+
			"and reports it healthy while somebody waits for it to go", r.count())
	}
}

// A NODE WHOSE CONFIGURATION IS BEHIND THE FLEET'S CONVERGES NOTHING.
//
// Every reconciler reads the live company document, so on a shedding node a
// pass converges a third-party app to the epoch the fleet has already replaced:
// an account a removed seat should no longer have is kept, a webhook is
// re-registered at the previous address, and the row says ready.
//
// ASKED BEFORE THE DUTY IS CLAIMED, which is the half that matters for the
// fleet: declining without claiming lets this node's lease lapse so a peer
// holding the current revision takes the loop over. Claiming and then doing
// nothing would hold the duty hostage on the node least able to do it.
func TestASheddingNodeNeitherClaimsNorConverges(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()

	var claims int
	w, err := New(Options{
		Registrations: []Registration{{Reconciler: r}},
		Store:         store,
		Admits:        func() bool { return false },
		ClaimDuty: func(context.Context) (bool, error) {
			claims++
			return true, nil
		},
		Now:    func() time.Time { return now },
		Spread: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	if claims != 0 {
		t.Errorf("a shedding node claimed the duty %d times; its lease has to "+
			"lapse so a current peer can take the loop over", claims)
	}
	if r.count() != 0 {
		t.Errorf("a shedding node ran %d passes against a stale company", r.count())
	}
	if store.has(KindGitLab) {
		t.Errorf("a shedding node wrote a status row: %+v", store.get(t, KindGitLab))
	}
}

// AND IT RESUMES ON ITS OWN once the node is current again, because the gate is
// read per tick rather than sampled when the loop was wired. Sampling it at
// construction is the shape that looks right and is permanently wrong: the
// control plane that answers this is built after the engine, so at wiring time
// the answer is always "admit".
func TestANodeThatCatchesUpResumes(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := &fakeReconciler{kind: KindGitLab}
	store := newStore()

	current := false
	w, err := New(Options{
		Registrations: []Registration{{Reconciler: r}},
		Store:         store,
		Admits:        func() bool { return current },
		Now:           func() time.Time { return now },
		Spread:        func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.Tick(context.Background())
	if r.count() != 0 {
		t.Fatalf("a shedding node ran %d passes", r.count())
	}

	current = true
	w.Tick(context.Background())
	if r.count() != 1 {
		t.Fatalf("a node that caught up ran %d passes, want 1", r.count())
	}
}

// THE PASS RUNS ON THE CONTEXT THE GUARD BOUNDED, not on the tick's own.
//
// The lease is taken once and never renewed, so the deadline carried by that
// context is the only thing making "the lease protects this pass" true rather
// than hoped for. A worker that passed its own context through would leave the
// reconcile loop's passes unbounded — which is where they were, while the
// dashboard's identical pass had been bounded for exactly this reason.
func TestThePassRunsOnTheGuardsContext(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore()

	type key struct{}
	var sawMark bool
	r := &fakeReconciler{kind: KindGitLab}
	r.observe = func(ctx context.Context) { sawMark = ctx.Value(key{}) == "bounded" }

	w := guardedAt(t, now, store, Registration{Reconciler: r},
		func(ctx context.Context, _ Kind) (context.Context, func(), bool, error) {
			return context.WithValue(ctx, key{}, "bounded"), func() {}, true, nil
		})
	w.Tick(context.Background())

	if !sawMark {
		t.Fatal("the pass ran on the tick's context rather than the one the " +
			"guard bounded, so nothing stops it outliving its own lease")
	}
}

// THE COMPANY'S OWN CHECK INTERVAL IS WHAT A SETTLED SURFACE WAITS.
//
// The settled cadence is the ONLY thing that finds access somebody revoked by
// hand at the third-party app — nothing tells this engine — so how long it is
// decides how long a card claims Connected over an agent that has been cut
// off. Measured: eight minutes, on a deployment that would have been happy to
// spend a read per seat every minute.
func TestASettledSurfaceWaitsTheCompanysOwnCheckInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore()
	w, err := New(Options{
		Registrations:   []Registration{{Reconciler: &fakeReconciler{kind: KindGitLab}}},
		Store:           store,
		Now:             func() time.Time { return now },
		Spread:          func(d time.Duration) time.Duration { return d },
		SettledInterval: func() time.Duration { return time.Minute },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	row := store.get(t, KindGitLab)
	if !row.NextAttemptAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("the next attempt is %s, want the company's own interval at %s",
			row.NextAttemptAt, now.Add(time.Minute))
	}
}

// AND ONLY THE SETTLED ONE. The other three waits are retries of something
// already known to be wrong, and their cost does not scale with the company —
// substituting this into them would turn a company that shortened its check
// interval into one that also hammers a third-party app it is waiting on.
func TestTheCheckIntervalDoesNotChangeTheOtherWaits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore()
	w, err := New(Options{
		Registrations: []Registration{{Reconciler: &fakeReconciler{
			kind: KindGitLab,
			findings: []Finding{{
				Kind: FindingApprovalRequired, Subject: "nimbus",
			}},
		}}},
		Store:           store,
		Now:             func() time.Time { return now },
		Spread:          func(d time.Duration) time.Duration { return d },
		SettledInterval: func() time.Duration { return time.Minute },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	row := store.get(t, KindGitLab)
	if row.Report.Phase == PhaseReady {
		t.Fatalf("the fixture settled, so this case asserts nothing: %+v", row.Report)
	}
	if !row.NextAttemptAt.Equal(now.Add(DefaultSchedule.AdminBase)) {
		t.Fatalf("the next attempt is %s, want the admin cadence at %s",
			row.NextAttemptAt, now.Add(DefaultSchedule.AdminBase))
	}
}

// AND AN UNSET ONE IS THE DEFAULT, never zero. A settled surface that came
// due immediately would reconcile every tick, which is the one design that
// makes this loop too expensive to leave switched on.
func TestAnUnsetCheckIntervalTakesTheDefault(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := newStore()
	w, err := New(Options{
		Registrations:   []Registration{{Reconciler: &fakeReconciler{kind: KindGitLab}}},
		Store:           store,
		Now:             func() time.Time { return now },
		Spread:          func(d time.Duration) time.Duration { return d },
		SettledInterval: func() time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Tick(context.Background())

	row := store.get(t, KindGitLab)
	if !row.NextAttemptAt.Equal(now.Add(DefaultSchedule.Settled)) {
		t.Fatalf("the next attempt is %s, want the default settled cadence",
			row.NextAttemptAt)
	}
}
