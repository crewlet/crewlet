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
)

// fakeReconciler answers with whatever the test set, and counts its passes.
type fakeReconciler struct {
	kind     Kind
	findings []Finding
	err      error

	mu     sync.Mutex
	passes int
}

func (f *fakeReconciler) Kind() Kind { return f.kind }

func (f *fakeReconciler) Reconcile(context.Context) ([]Finding, error) {
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

// A surface the company document no longer declares is forgotten, so its last
// status does not sit in the fleet's view forever describing an integration
// that is gone.
func TestDepartedSurfaceIsForgotten(t *testing.T) {
	now := time.Now().UTC()
	live := &fakeReconciler{kind: KindGitLab}
	store := newStore(
		State{Kind: KindGitLab},
		State{Kind: KindSlack, Report: Ready()},
	)

	at(t, now, store, nil, Registration{Reconciler: live}).Tick(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if !slices.Contains(store.forgot, KindSlack) {
		t.Fatalf("slack was not forgotten; forgot %v", store.forgot)
	}
	if _, still := store.rows[KindSlack]; still {
		t.Fatal("slack's state survived being forgotten")
	}
	if _, gone := store.rows[KindGitLab]; !gone {
		t.Fatal("the live surface was forgotten too")
	}
}

// Surfaces are converged in the canonical order however the config listed
// them, so two nodes with one company agree and a status listing does not
// reshuffle when the duty moves between them.
func TestOrderIsCanonicalNotRegistrationOrder(t *testing.T) {
	w, err := New(Options{
		Store: newStore(),
		Registrations: []Registration{
			{Reconciler: &fakeReconciler{kind: KindDatadog}},
			{Reconciler: &fakeReconciler{kind: KindSlack}},
			{Reconciler: &fakeReconciler{kind: KindGitHub}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []Kind{KindSlack, KindGitHub, KindDatadog}
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

	mu          sync.Mutex
	calls       int
	removeSeats bool
}

func (f *fakeDisconnector) Disconnect(_ context.Context, removeSeats bool) error {
	f.mu.Lock()
	f.calls++
	f.removeSeats = removeSeats
	f.mu.Unlock()
	return f.err
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
