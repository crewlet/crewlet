// Package integrationtest is the ONE suite every [integration.Reconciler]
// passes, in the queuetest / coordtest / storetest tradition.
//
// # Why a shared suite rather than per-third-party app tests
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
		{"every finding is a kind this build knows", findingsAreKnown},
		{"a finding a person must act on says what to do", personFindingsAreActionable},
		{"a cancelled pass reports a fault rather than health", cancelledPassIsAFault},
	}
}

// Reconciler is one third-party app's entry into the suite.
type Reconciler struct {
	// New builds a reconciler against a world that is ALREADY CONVERGED:
	// every seat has the identity the company asks for, every credential
	// works, and nothing is outstanding.
	//
	// Converged rather than empty, because the clause that matters most is
	// about the steady state. A pass over a company that needs work is
	// allowed to write; a pass over one that does not is the state the
	// loop spends its life in, and the one where a stray write becomes a
	// credential rotated every ten minutes for ever.
	New func(t TB) integration.Reconciler

	// Mutations counts every write the third-party app has received since New.
	//
	// Required. See the package doc.
	Mutations func() int
}

// Run drives the suite.
func Run(t *testing.T, r Reconciler) {
	t.Helper()
	if r.New == nil {
		t.Fatalf("integrationtest: Reconciler.New is required")
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
	kind := r.New(t).Kind()
	if !kind.Valid() {
		t.Fatalf("Kind() is %q, which is not in integration.Kinds", kind)
	}
}

// A kind that moved between passes would leave the previous one's status
// behind for ever, describing a surface nothing writes to any more.
func kindIsStable(t TB, r Reconciler) {
	rec := r.New(t)
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
	rec := r.New(t)
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
	rec := r.New(t)
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
	findings, err := r.New(t).Reconcile(context.Background())
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
	findings, err := r.New(t).Reconcile(context.Background())
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

	findings, err := r.New(t).Reconcile(ctx)
	if err == nil && len(findings) == 0 {
		t.Fatalf("%s", "a cancelled pass reported a converged integration; the "+
			"loop reads that as ready and trusts it for a full settled interval")
	}
}
