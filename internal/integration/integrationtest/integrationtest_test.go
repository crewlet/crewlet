package integrationtest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
)

// compliant is a reconciler that keeps every clause of the safety contract.
type compliant struct {
	writes   *int
	findings []integration.Finding
}

func (compliant) Kind() integration.Kind { return integration.KindGitLab }

func (c compliant) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.findings, nil
}

// world builds a reconciler that reports exactly these findings.
func world(findings ...integration.Finding) func(integrationtest.TB) integration.Reconciler {
	return func(integrationtest.TB) integration.Reconciler {
		return compliant{findings: findings}
	}
}

// outstanding is a world with something a PERSON owes, which is what the
// findings clauses need in front of them.
func outstanding() func(integrationtest.TB) integration.Reconciler {
	return world(integration.Finding{
		Kind: integration.FindingGrantShort, Subject: "ceo",
		Detail: "ceo needs maintainer on the platform group",
	})
}

// A reconciler that keeps the contract passes.
func TestACompliantReconcilerPasses(t *testing.T) {
	writes := 0
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged:   func(integrationtest.TB) integration.Reconciler { return compliant{writes: &writes} },
		Outstanding: outstanding(),
		Mutations:   func() int { return writes },
	})
}

// And a world carrying several findings passes too, including one nobody has
// to act on beside one somebody does.
func TestACompliantReconcilerWithFindingsPasses(t *testing.T) {
	writes := 0
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(integrationtest.TB) integration.Reconciler { return compliant{writes: &writes} },
		Outstanding: world(
			integration.Finding{
				Kind: integration.FindingGrantShort, Subject: "ceo",
				Detail: "ceo needs maintainer",
			},
			integration.Finding{Kind: integration.FindingGrantPending, Subject: "cto"},
		),
		Mutations: func() int { return writes },
	})
}

// violation is a reconciler that breaks ONE named clause.
type violation struct {
	name string
	// clause is the [integrationtest.Case] this reconciler must make fail,
	// by name.
	//
	// NAMING IT IS THE WHOLE POINT. Asserting only that "some case went
	// red" is satisfied whenever any OTHER clause catches the violator
	// first, and most of these violators trip more than one: a reconciler
	// that ignores cancellation fails the cancellation clause even when it
	// was written to break idempotence. So a clause that had quietly
	// stopped checking anything would still have looked proven, which is
	// precisely the failure mode this test exists to rule out.
	clause string
	rec    integrationtest.Reconciler
}

// violations is one reconciler per clause of the contract.
//
// A function rather than a package-level slice because several entries carry
// per-run state (a write counter, a pass counter), and a table shared between
// two tests would carry one test's passes into the other's.
func violations() []violation {
	return []violation{
		{
			name:   "a kind this build does not converge",
			clause: "the kind is one this build converges",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return unknownKind{} },
				Outstanding: outstanding(),
				Mutations:   func() int { return 0 },
			},
		},
		{
			name:   "a kind that moves between passes",
			clause: "the kind does not change across passes",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return &drifter{} },
				Outstanding: outstanding(),
				Mutations:   func() int { return 0 },
			},
		},
		{
			name:   "a converged pass that writes",
			clause: "a converged pass writes nothing",
			rec: func() integrationtest.Reconciler {
				writes := 0
				return integrationtest.Reconciler{
					Converged:   func(integrationtest.TB) integration.Reconciler { return &writer{writes: &writes} },
					Outstanding: outstanding(),
					Mutations:   func() int { return writes },
				}
			}(),
		},
		{
			name:   "findings that churn between passes",
			clause: "two passes over an unchanged world agree",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return &churner{} },
				Outstanding: outstanding(),
				Mutations:   func() int { return 0 },
			},
		},
		{
			// THE ONE THAT MAKES THE THREE BELOW MEAN ANYTHING. A world
			// that reports nothing is converged, and every clause that
			// walks a findings list then walks an empty one.
			name:   "an outstanding world with nothing outstanding",
			clause: "an outstanding world actually reports something",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return compliant{} },
				Outstanding: world(),
				Mutations:   func() int { return 0 },
			},
		},
		{
			// AND THE OTHER HALF OF IT. Findings alone are not enough:
			// a world whose only finding is one nobody has to act on
			// leaves the actionability clause with nothing to check.
			name:   "an outstanding world nobody has to act on",
			clause: "an outstanding world actually reports something",
			rec: integrationtest.Reconciler{
				Converged: func(integrationtest.TB) integration.Reconciler { return compliant{} },
				Outstanding: world(
					integration.Finding{Kind: integration.FindingGrantPending, Subject: "cto"},
				),
				Mutations: func() int { return 0 },
			},
		},
		{
			name:   "a finding kind this build cannot read",
			clause: "every finding is a kind this build knows",
			rec: integrationtest.Reconciler{
				Converged: func(integrationtest.TB) integration.Reconciler { return compliant{} },
				// Carries a detail, so the only clause it breaks is the
				// one it is here for: an unknown kind is owed by the
				// operator, and a detail-less one would trip the
				// actionability clause as well.
				Outstanding: world(integration.Finding{
					Kind: integration.FindingKind("pager_rota_stale"), Subject: "ceo",
					Detail: "read the peer's logs",
				}),
				Mutations: func() int { return 0 },
			},
		},
		{
			name:   "a person's finding with nothing to act on",
			clause: "a finding a person must act on says what to do",
			rec: integrationtest.Reconciler{
				Converged: func(integrationtest.TB) integration.Reconciler { return compliant{} },
				Outstanding: world(integration.Finding{
					Kind: integration.FindingGrantShort, Subject: "ceo",
				}),
				Mutations: func() int { return 0 },
			},
		},
		{
			name:   "findings that churn over an outstanding world",
			clause: "two passes over an outstanding world agree",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return compliant{} },
				Outstanding: func(integrationtest.TB) integration.Reconciler { return &churner{} },
				Mutations:   func() int { return 0 },
			},
		},
		{
			name:   "a cancelled pass reported as health",
			clause: "a cancelled pass reports a fault rather than health",
			rec: integrationtest.Reconciler{
				Converged:   func(integrationtest.TB) integration.Reconciler { return swallower{} },
				Outstanding: outstanding(),
				Mutations:   func() int { return 0 },
			},
		},
	}
}

// THE SUITE HAS TO BE ABLE TO FAIL, on every clause, or it is a claim rather
// than coverage. Each entry breaks exactly one clause of the contract, and
// THAT clause must go red for it.
//
// The violation's own failure output goes to the subtest's log, where a
// person reading a failing run sees it. What this asserts is only that the
// named case failed, because that is the property that matters: a suite
// which quietly stopped checking a clause would look exactly like one where
// every third-party app complied.
func TestTheSuiteRejectsEachViolation(t *testing.T) {
	for _, v := range violations() {
		t.Run(v.name, func(t *testing.T) {
			if !caseFails(t, v.clause, v.rec) {
				t.Fatalf("%q passed a reconciler with %s, so that clause is a "+
					"claim rather than coverage", v.clause, v.name)
			}
		})
	}
}

// EVERY CLAUSE NEEDS A VIOLATOR. A clause with none is checked by nothing:
// it passes every reconciler in the tree and would go on passing if its body
// were deleted, which is exactly how a suite stops meaning anything while
// still reporting seven green subtests.
func TestEveryClauseIsShownFalsifiable(t *testing.T) {
	vs := violations()
	for _, c := range integrationtest.Cases() {
		if !slices.ContainsFunc(vs, func(v violation) bool { return v.clause == c.Name }) {
			t.Errorf("no violation breaks %q, so nothing shows that clause can fail", c.Name)
		}
	}
}

// caseFails drives ONE named case against a recorder and reports whether it
// went red.
//
// A RECORDER rather than a nested subtest, because a failing subtest fails
// its parent whatever t.Run returns, so a deliberate violation could not be
// asserted on from inside this file. That constraint is the whole reason
// integrationtest.TB exists.
func caseFails(t *testing.T, clause string, rec integrationtest.Reconciler) bool {
	t.Helper()
	for _, c := range integrationtest.Cases() {
		if c.Name == clause {
			return runCase(c, rec).failed
		}
	}
	// A renamed case, which would otherwise silently stop being exercised.
	t.Fatalf("no case is named %q; integrationtest.Cases() has %q",
		clause, caseNames())
	return false
}

func caseNames() []string {
	names := make([]string, 0, len(integrationtest.Cases()))
	for _, c := range integrationtest.Cases() {
		names = append(names, c.Name)
	}
	return names
}

// runCase drives one case and reports what it said.
func runCase(c integrationtest.Case, rec integrationtest.Reconciler) *recorder {
	got := &recorder{}
	func() {
		// Fatalf ends the case by unwinding, exactly as (*testing.T).Fatalf
		// ends a test, so the panic is expected rather than a failure of
		// this harness.
		defer func() { _ = recover() }()
		c.Fn(got, rec)
	}()
	return got
}

// recorder is an integrationtest.TB that remembers rather than reports.
type recorder struct {
	failed bool
	log    []string
}

func (*recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failed = true
	r.log = append(r.log, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	// Unwinds the case, which is what Fatalf means. Recovered by runCase.
	panic(errStop)
}

// errStop unwinds a case that called Fatalf.
var errStop = errors.New("integrationtest: the case stopped")

// EVERY CASE MUST PASS A COMPLIANT RECONCILER. Without this a case that
// failed unconditionally would satisfy the violation tests above while
// certifying nothing, which is the way a suite quietly stops meaning
// anything.
func TestEveryCasePassesACompliantReconciler(t *testing.T) {
	writes := 0
	rec := integrationtest.Reconciler{
		// CONVERGED MEANS NO FINDINGS, which is the whole reason the
		// outstanding world exists beside it: a compliant reconciler that
		// reported findings over a converged world would be describing a
		// company with work to do as one with none.
		Converged: func(integrationtest.TB) integration.Reconciler {
			return compliant{writes: &writes}
		},
		Outstanding: outstanding(),
		Mutations:   func() int { return writes },
	}
	for _, c := range integrationtest.Cases() {
		if got := runCase(c, rec); got.failed {
			t.Errorf("%q failed a compliant reconciler: %v", c.Name, got.log)
		}
	}
}

type unknownKind struct{}

func (unknownKind) Kind() integration.Kind { return integration.Kind("pagerduty") }
func (unknownKind) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	return nil, ctx.Err()
}

// drifter answers to one kind before a pass and another after, which would
// strand the first kind's status row describing a surface nothing writes to
// any more.
type drifter struct{ passes int }

func (d *drifter) Kind() integration.Kind {
	if d.passes == 0 {
		return integration.KindGitLab
	}
	return integration.KindGitHub
}

func (d *drifter) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.passes++
	return nil, nil
}

// writer mints something on every pass, which is the outage this contract
// exists to prevent.
type writer struct{ writes *int }

func (writer) Kind() integration.Kind { return integration.KindGitLab }
func (w *writer) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	*w.writes++
	return nil, nil
}

// churner reports something different every pass, which makes the phase flap
// and resets the backoff every time.
type churner struct{ n int }

func (churner) Kind() integration.Kind { return integration.KindGitLab }
func (c *churner) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.n++
	if c.n%2 == 0 {
		return []integration.Finding{{Kind: integration.FindingGrantPending, Subject: "ceo"}}, nil
	}
	return nil, nil
}

// swallower treats a cancelled context as a converged world, so a node
// shutting down records every integration as ready on its way out.
type swallower struct{}

func (swallower) Kind() integration.Kind { return integration.KindGitLab }
func (swallower) Reconcile(context.Context) ([]integration.Finding, error) {
	return nil, nil
}
