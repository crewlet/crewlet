package integrationtest_test

import (
	"context"
	"errors"
	"fmt"
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

// A reconciler that keeps the contract passes.
func TestACompliantReconcilerPasses(t *testing.T) {
	writes := 0
	integrationtest.Run(t, integrationtest.Reconciler{
		New:       func(integrationtest.TB) integration.Reconciler { return compliant{writes: &writes} },
		Mutations: func() int { return writes },
	})
}

// And one carrying findings passes too, so the suite certifies a company with
// something outstanding rather than only an empty one.
func TestACompliantReconcilerWithFindingsPasses(t *testing.T) {
	writes := 0
	findings := []integration.Finding{
		{Kind: integration.FindingGrantShort, Subject: "ceo", Detail: "ceo needs maintainer"},
		{Kind: integration.FindingGrantPending, Subject: "cto"},
	}
	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(integrationtest.TB) integration.Reconciler {
			return compliant{writes: &writes, findings: findings}
		},
		Mutations: func() int { return writes },
	})
}

// THE SUITE HAS TO BE ABLE TO FAIL, on every clause, or it is a claim rather
// than coverage. Each entry below breaks exactly one clause of the contract,
// and the suite must go red for it.
//
// The violation's own failure output goes to the subtest's log, where a
// person reading a failing run sees it. What this asserts is only that the
// subtest failed at all, because that is the property that matters: a suite
// which quietly stopped checking a clause would look exactly like one where
// every vendor complied.
func TestTheSuiteRejectsEachViolation(t *testing.T) {
	cases := []struct {
		name string
		rec  integrationtest.Reconciler
	}{
		{
			name: "a kind this build does not converge",
			rec: integrationtest.Reconciler{
				New:       func(integrationtest.TB) integration.Reconciler { return unknownKind{} },
				Mutations: func() int { return 0 },
			},
		},
		{
			name: "a converged pass that writes",
			rec: func() integrationtest.Reconciler {
				writes := 0
				return integrationtest.Reconciler{
					New:       func(integrationtest.TB) integration.Reconciler { return &writer{writes: &writes} },
					Mutations: func() int { return writes },
				}
			}(),
		},
		{
			name: "findings that churn between passes",
			rec: integrationtest.Reconciler{
				New:       func(integrationtest.TB) integration.Reconciler { return &churner{} },
				Mutations: func() int { return 0 },
			},
		},
		{
			name: "a person's finding with nothing to act on",
			rec: integrationtest.Reconciler{
				New: func(integrationtest.TB) integration.Reconciler {
					return compliant{findings: []integration.Finding{
						{Kind: integration.FindingGrantShort, Subject: "ceo"},
					}}
				},
				Mutations: func() int { return 0 },
			},
		},
		{
			name: "a cancelled pass reported as health",
			rec: integrationtest.Reconciler{
				New:       func(integrationtest.TB) integration.Reconciler { return swallower{} },
				Mutations: func() int { return 0 },
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !anyCaseFails(tc.rec) {
				t.Fatalf("every case passed a reconciler with %s, so the clause "+
					"that forbids it is a claim rather than coverage", tc.name)
			}
		})
	}
}

// anyCaseFails drives every case against a recorder and reports whether one
// of them went red.
//
// A RECORDER rather than a nested subtest, because a failing subtest fails
// its parent whatever t.Run returns, so a deliberate violation could not be
// asserted on from inside this file. That constraint is the whole reason
// integrationtest.TB exists.
func anyCaseFails(rec integrationtest.Reconciler) bool {
	for _, c := range integrationtest.Cases() {
		if runCase(c, rec).failed {
			return true
		}
	}
	return false
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
		New: func(integrationtest.TB) integration.Reconciler {
			return compliant{writes: &writes, findings: []integration.Finding{
				{Kind: integration.FindingGrantShort, Subject: "ceo", Detail: "needs maintainer"},
			}}
		},
		Mutations: func() int { return writes },
	}
	for _, c := range integrationtest.Cases() {
		if got := runCase(c, rec); got.failed {
			t.Errorf("%q failed a compliant reconciler: %v", c.Name, got.log)
		}
	}
}

type unknownKind struct{}

func (unknownKind) Kind() integration.Kind { return integration.Kind("pagerduty") }
func (unknownKind) Reconcile(context.Context) ([]integration.Finding, error) {
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
