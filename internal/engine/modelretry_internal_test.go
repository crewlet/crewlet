package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// retryNode is one node of a fleet sharing fleet: its own revision, its own
// sign-in plans, and the retry watch running.
func retryNode(t *testing.T, fleet coord.Fleet) (*Engine, *Company) {
	t.Helper()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	// THE ENTRY AS THE REVISION WRITES IT, on a harness that is not there: a
	// "retry now" builds its probe from the revision, and a case must never
	// start a real harness.
	spec := c.Config.Providers.LLM["plan"]
	spec.StateDir = t.TempDir()
	spec.Overrides = map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")}
	c.Config.Providers.LLM["plan"] = spec
	e := epochEngine(c)
	e.backends = &Backends{Fleet: fleet}
	e.startModelRetries(t.Context())
	t.Cleanup(e.stopModelRetries)
	return e, c
}

// spend sits c's plan entry out on a report made at `at`.
func spend(t *testing.T, c *Company, at time.Time) {
	t.Helper()
	if !planOf(t, c).RunEnded(t.Context(), harness.RunEnd{Answer: limitReport(at), At: at}) {
		t.Fatal("the premise: the entry sits out")
	}
}

// limitReport is Claude Code's usage-limit sentence as a report made at `at`
// says it: its window resets on the hour, in UTC, two to three hours later.
// It is relative to the report because a fixed hour fails at that hour.
// "resets 5pm" reported at 16:59 is a window over by 17:00, and the harness
// reads one that passed less than five minutes ago as no reset at all
// (clockResetGrace). Every case naming it then found no sit-out for five
// minutes of each day. The zone is named so that the sentence means the same
// instant under whatever TZ the suite runs.
func limitReport(at time.Time) string {
	return "Usage limit reached · resets " + at.UTC().Add(3*time.Hour).Format("3pm") + " (UTC)"
}

var person = iam.Actor{Name: "jane-doe", Kind: iam.ActorHuman, OperatorID: "session-1"}

// A RETRY CLEARS EVERY NODE'S SIT-OUT, FOR THE REPORTS BEFORE IT AND NONE AFTER.
// A person clears through one node; every node hears the record and clears its
// own memory of the limit, which no request to one node could reach. A report
// the vendor made after the request stands, and hearing the record again (a
// re-watch, a restart) clears nothing more: the rule is the stamp, not the
// delivery.
//
// Mutation: clear without the stamp (everything), apply on the taking node
// alone, or clear again on a re-delivered record, and this fails.
func TestARetryClearsEveryNodesSitOut(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	one, c1 := retryNode(t, fleet)
	_, c2 := retryNode(t, fleet)
	before := time.Now().Add(-time.Minute)
	spend(t, c1, before)
	spend(t, c2, before)

	result, err := one.RetryModel(t.Context(), "plan", coord.RetryClear, person)
	if err != nil || !result.Cleared {
		t.Fatalf("RetryModel(clear) = %+v, %v; want the record written", result, err)
	}
	for name, c := range map[string]*Company{"the taking node": c1, "the other node": c2} {
		deadline := time.Now().Add(5 * time.Second)
		for planOf(t, c).SittingOut() != nil {
			if time.Now().After(deadline) {
				t.Fatalf("%s still sits the entry out after the clear", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	record, found, err := fleet.ModelRetry(t.Context(), "plan")
	if err != nil || !found || record.By != person.Name || record.Mode != coord.RetryClear {
		t.Fatalf("the record = %+v (found %v, %v), want jane's clear", record, found, err)
	}
	spend(t, c2, record.At.Add(time.Second))
	two := epochEngine(c2)
	two.applyModelRetry(t.Context(), record)
	if planOf(t, c2).SittingOut() == nil {
		t.Error("a record heard again cleared a limit the vendor reported after the request")
	}
}

// "RETRY NOW" ASKS FIRST AND TELLS NOBODY UNLESS IT ANSWERED: on an entry no
// node sits out there is nothing to retry; a probe that fails writes no record,
// so no node clears a limit nothing has shown is over.
//
// Mutation: write the record before the probe, or regardless of its answer,
// and this fails.
func TestRetryNowWritesNothingUnlessTheProbeAnswered(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	e, c := retryNode(t, fleet)

	if _, err := e.RetryModel(t.Context(), "plan", coord.RetryProbe, person); !errors.Is(err, ErrNotSittingOut) {
		t.Fatalf("retrying an entry nobody sits out = %v, want ErrNotSittingOut", err)
	}
	if _, err := e.RetryModel(t.Context(), "nobody", coord.RetryClear, person); !errors.Is(err, ErrNoModel) {
		t.Fatalf("retrying an unknown entry = %v, want ErrNoModel", err)
	}

	spend(t, c, time.Now().Add(-time.Minute))
	// The entry's harness does not exist, so the probe fails for a reason that is
	// not a limit.
	result, err := e.RetryModel(t.Context(), "plan", coord.RetryProbe, person)
	if err != nil || result.Cleared || result.Failed == "" {
		t.Fatalf("RetryModel(probe) on a harness that cannot answer = %+v, %v; want nothing "+
			"cleared, saying why", result, err)
	}
	if _, found, _ := fleet.ModelRetry(t.Context(), "plan"); found {
		t.Error("a probe that did not answer wrote the record, so every node clears a limit nothing showed is over")
	}
}
