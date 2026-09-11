// Package integrationtest is the ONE suite every [integration.Reconciler]
// passes, in the queuetest / coordtest / storetest tradition.
//
// # Why a shared suite rather than per-integration tests
//
// [integration.Reconciler]'s doc states a safety contract in four clauses,
// and until this package existed nothing anywhere enforced any of them. That
// is the worst shape a rule can be in: written down, believed, and checked by
// nobody. Seven third-party apps would each decide separately what "idempotent" and
// "must not rotate a working credential" mean, and the second reading is an
// outage on a timer, because the loop runs unattended for the life of the
// deployment.
//
// A twin that agrees only with itself proves nothing, which is why the
// memory backends elsewhere in this tree are certified by the same cases as
// the real ones. The same argument applies here with more force: the thing
// being certified is not a data structure but a promise about what a pass
// does to somebody's Slack workspace.
//
// # What the suite can and cannot see
//
// It drives a reconciler against a world the caller has already converged,
// and it asks that world how many writes it received. That second half is
// [Reconciler.Mutations] and it is REQUIRED rather than optional, because it
// is the one clause most likely to be wrong and a suite that quietly skipped
// it would certify everything except the thing worth certifying. A third-party app
// harness that genuinely cannot count writes has to grow the ability rather
// than be waived: a skip is not a pass.
//
// # What counts as a write, and what does not
//
// Anything a PERSON WOULD HAVE TO UNDO. That is wider than "a request to the
// third-party app": a pass that re-seals a seat's credential through the
// fleet's sealed store on every converged run is writing just as surely, and
// the harness has to see it.
//
// It is also NARROWER than "a non-GET request", and getting that wrong pushes
// in the dangerous direction. Some vendors model a listing as a POST —
// Atlassian's workspace discovery is one — so a counter keyed on HTTP method
// makes the clause impossible to satisfy, and a clause that cannot be
// satisfied is one somebody eventually weakens. Count by ROUTE, and say in the
// harness which routes those are and why.
package integrationtest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// TB is the subset of *testing.T the cases use.
//
// An interface rather than the concrete type, for exactly one reason: this
// suite's own tests have to prove that every case CAN fail, and a case that
// calls (*testing.T).Fatalf fails its parent too, so a deliberate violation
// could not be asserted on from inside the same run. With a TB the cases are
// drivable by a recorder. A third-party app passes a real *testing.T and never sees
// this type.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Case is one clause of the contract, drivable on its own.
//
// Exported so [Cases] can hand them to a recorder. A third-party app calls [Run].
type Case struct {
	Name string
	Fn   func(t TB, r Reconciler)
}

// Cases is the contract, in the order [Run] drives it.
func Cases() []Case {
	return []Case{
		{"the kind is one this build converges", kindIsValid},
		{"the kind does not change across passes", kindIsStable},
		{"a converged pass writes nothing", convergedPassWritesNothing},
		{"two passes over an unchanged world agree", passesAgree},
		{"an outstanding world actually reports something", outstandingWorldReports},
		{"every finding is a kind this build knows", findingsAreKnown},
		{"a finding a person must act on says what to do", personFindingsAreActionable},
		{"two passes over an outstanding world agree", outstandingPassesAgree},
		{"a cancelled pass reports a fault rather than health", cancelledPassIsAFault},
	}
}

// Reconciler is one integration's entry into the suite.
//
// TWO WORLDS, and the second one exists because the first cannot certify what
// half these cases claim to. A converged pass reports NO findings — that is
// what converged means — so every case that walks the findings list walks an
// empty one and passes whatever the vendor does. Three of them did, silently,
// and a reader counting green ticks would have read seven certified clauses
// where two carried weight.
type Reconciler struct {
	// Converged builds a reconciler against a world that is ALREADY
	// CONVERGED: every seat has the identity the company asks for, every
	// credential works, and nothing is outstanding.
	//
	// This is the steady state — the one the loop spends its life in, and
	// the one where a stray write becomes a credential rotated every ten
	// minutes for ever.
	Converged func(t TB) integration.Reconciler

	// Outstanding builds one against a world where something is genuinely
	// wrong in a way A PERSON has to act on: a credential the third-party
	// app refuses, an app nobody installed, a seat with no account, an
	// address deliveries cannot reach.
	//
	// REQUIRED, for the reason [Reconciler.Mutations] is. A suite that let a
	// vendor skip this would go on reporting the findings cases green while
	// certifying nothing about them, which is the exact shape this package
	// was written to remove and would be the second time it happened here.
	//
	// A pass over this world is ALLOWED to write — it has work to do, and
	// doing it is the point. Nothing counts mutations here.
	Outstanding func(t TB) integration.Reconciler

	// Mutations counts every write the pass has made since Converged — at
	// the third-party app, and into this deployment's own sealed store.
	//
	// Required, and the package doc says what a write is: anything a person
	// would have to undo, counted by ROUTE rather than by HTTP method.
	//
	// Only ever sampled around a pass over the CONVERGED world, as a delta,
	// so a harness may share one counter between both.
	Mutations func() int
}

// Run drives the suite.
func Run(t *testing.T, r Reconciler) {
	t.Helper()
	if r.Converged == nil {
		t.Fatalf("integrationtest: Reconciler.Converged is required")
	}
	if r.Outstanding == nil {
		t.Fatalf("%s", "integrationtest: Reconciler.Outstanding is required; a "+
			"converged world reports no findings, so without it every case "+
			"that walks the findings list passes over an empty one and "+
			"certifies nothing")
	}
	if r.Mutations == nil {
		t.Fatalf("%s", "integrationtest: Reconciler.Mutations is required; a harness "+
			"that cannot count writes cannot certify the one clause of the "+
			"safety contract most likely to be wrong")
	}

	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) { c.Fn(t, r) })
	}
}

// The kind is what the loop keys every status on, so one this build does not
// know is a status nothing will ever render.
func kindIsValid(t TB, r Reconciler) {
	kind := r.Converged(t).Kind()
	if !kind.Valid() {
		t.Fatalf("Kind() is %q, which is not in integration.Kinds", kind)
	}
}

// A kind that moved between passes would leave the previous one's status
// behind for ever, describing a surface nothing writes to any more.
func kindIsStable(t TB, r Reconciler) {
	rec := r.Converged(t)
	first := rec.Kind()
	if _, err := rec.Reconcile(context.Background()); err != nil &&
		!errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("Reconcile: %v", err)
	}
	if second := rec.Kind(); second != first {
		t.Fatalf("Kind() was %q before a pass and %q after", first, second)
	}
}

// THE LOAD-BEARING CLAUSE.
//
// The loop runs this every few minutes for the life of the deployment, so a
// converged pass that writes is not a small inefficiency: at the third-party apps
// here, a write is an account created, a membership set, or a credential
// minted. A credential minted every pass revokes the one every running agent
// is authenticating with, from a loop whose whole promise is that it is safe
// to leave switched on.
func convergedPassWritesNothing(t TB, r Reconciler) {
	rec := r.Converged(t)
	before := r.Mutations()
	if _, err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile over a converged world: %v", err)
	}
	if after := r.Mutations(); after != before {
		t.Fatalf("a pass over a converged world made %d write(s); the loop runs "+
			"this every few minutes for ever, so a write here is a third-party app "+
			"mutation on a timer", after-before)
	}
}

// A pass whose findings churn over an unchanged world makes the reported
// phase flap and resets the backoff every time, so an integration that needs
// nothing is re-read at the shortest interval the schedule has.
func passesAgree(t TB, r Reconciler) {
	rec := r.Converged(t)
	ctx := context.Background()

	first, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if !slices.Equal(first, second) {
		t.Fatalf("two passes over one world disagree:\n first: %+v\nsecond: %+v",
			first, second)
	}
	// And so do the reports they fold into, which is what an operator
	// actually reads. Asserted separately because two different finding
	// lists can classify the same, and a change nobody sees is still a
	// change that resets the attempt counter.
	if a, b := integration.Classify(first), integration.Classify(second); a != b {
		t.Fatalf("two passes classify differently:\n first: %+v\nsecond: %+v", a, b)
	}
}

// A third-party app reports what it observed in the shared vocabulary and does not get
// to invent an eighth kind: an unknown one classifies as degraded with a
// sentence nobody can act on.
func findingsAreKnown(t TB, r Reconciler) {
	findings, err := r.Outstanding(t).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range findings {
		phase, actor := f.Kind.Verdict()
		unknownPhase, unknownActor := integration.FindingKind("no such kind").Verdict()
		if phase == unknownPhase && actor == unknownActor &&
			f.Kind != integration.FindingUnknownTier {
			t.Errorf("finding kind %q is not one this build knows", f.Kind)
		}
	}
}

// A finding whose actor is a person is the whole reason this subsystem
// exists: it is the sentence that saves somebody reading logs. One with no
// detail renders as a bare phase name and sends them there anyway.
func personFindingsAreActionable(t TB, r Reconciler) {
	findings, err := r.Outstanding(t).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range findings {
		_, actor := f.Kind.Verdict()
		if !actor.WaitsOnAPerson() {
			continue
		}
		if strings.TrimSpace(f.Detail) == "" {
			t.Errorf("%s finding on %q is owed by %s and says nothing about what "+
				"to do", f.Kind, f.Subject, actor)
		}
	}
}

// THE ANTI-VACUITY CLAUSE, and the reason the two cases after it mean
// anything.
//
// Every case that walks a findings list is satisfied by an empty one. So a
// harness whose "outstanding" world is quietly converged — a credential that
// resolves after all, a seat that does have an account, a fixture that
// short-circuits before the walk that would have noticed — puts three green
// ticks on a vendor nothing was checked about. That is not hypothetical: it is
// what this suite did for every vendor before this case existed, and what the
// suite it replaced did for all seven.
//
// So the world has to prove itself first. At least one finding, and at least
// one a PERSON owes, because the cases below are about what an operator is
// told and a world whose only finding is "the third-party app is still
// applying a grant" tells them nothing.
func outstandingWorldReports(t TB, r Reconciler) {
	findings, err := r.Outstanding(t).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("a pass over the outstanding world: %v", err)
	}
	if len(findings) == 0 {
		t.Fatalf("%s", "the outstanding world reported nothing, so it is "+
			"converged: every case that walks the findings list then walks an "+
			"empty one and certifies nothing about this integration")
	}
	for _, f := range findings {
		if _, actor := f.Kind.Verdict(); actor.WaitsOnAPerson() {
			return
		}
	}
	t.Fatalf("the outstanding world reported %d finding(s) and none is owed by "+
		"a person, so the case that checks what an operator is told has "+
		"nothing to check", len(findings))
}

// A pass whose findings churn makes the reported phase flap and resets the
// backoff every time — and over the world that HAS findings, which is where
// churn is actually reachable. The converged twin of this case compares two
// empty lists, so on its own it cannot see a vendor that returns its seats in
// map order.
func outstandingPassesAgree(t TB, r Reconciler) {
	rec := r.Outstanding(t)
	ctx := context.Background()

	first, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !slices.Equal(first, second) {
		t.Fatalf("two passes over one outstanding world disagree:\n first: %+v\nsecond: %+v",
			first, second)
	}
	if a, b := integration.Classify(first), integration.Classify(second); a != b {
		t.Fatalf("two passes over one outstanding world classify differently:\n first: %+v\nsecond: %+v",
			a, b)
	}
}

// A cancelled pass has NOT observed the world, so it must raise rather than
// answer with findings.
//
// The two are opposite claims to the loop: an error is a fault it retries,
// and an empty findings list is a statement that everything is fine. A third-party app
// that swallowed cancellation would have a node shutting down record every
// integration as ready on its way out, and the next node to hold the duty
// would trust that for a full settled interval.
func cancelledPassIsAFault(t TB, r Reconciler) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	findings, err := r.Converged(t).Reconcile(ctx)
	if err == nil && len(findings) == 0 {
		t.Fatalf("%s", "a cancelled pass reported a converged integration; the "+
			"loop reads that as ready and trusts it for a full settled interval")
	}
}
