package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// retryNode is one node sharing fleet, with its own revision and sign-in plans
// and the retry watch running.
func retryNode(t *testing.T, fleet coord.Fleet) (*Engine, *Company) {
	t.Helper()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	// A harness binary that is not there: "retry now" builds its probe from the
	// revision, and a case must never start a real harness.
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

// limitReport is Claude Code's usage-limit sentence as reported at `at`: a reset
// on the hour, in UTC, two to three hours later. Relative, because a fixed hour
// reported just before it reads as a reset already passed (clockResetGrace) and
// no sit-out; the zone is named so the instant holds under any TZ.
func limitReport(at time.Time) string {
	return "Usage limit reached · resets " + at.UTC().Add(3*time.Hour).Format("3pm") + " (UTC)"
}

var person = iam.Actor{Name: "jane-doe", Kind: iam.ActorHuman, OperatorID: "session-1"}

// A RETRY CLEARS EVERY NODE'S SIT-OUT, FOR THE REPORTS BEFORE IT AND NONE AFTER.
// A clear through one node reaches every node's own memory of the limit; a
// report after the request stands, and hearing the record again (a re-watch,
// a restart) clears nothing more: the rule is the stamp, not the delivery.
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

// "RETRY NOW" ASKS FIRST AND TELLS NOBODY UNLESS IT ANSWERED: an entry no node
// sits out has nothing to retry, and a failed probe writes no record, so no
// node clears a limit nothing has shown is over.
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
	// The harness does not exist, so the probe fails for a reason not a limit.
	result, err := e.RetryModel(t.Context(), "plan", coord.RetryProbe, person)
	if err != nil || result.Cleared || result.Failed == "" {
		t.Fatalf("RetryModel(probe) on a harness that cannot answer = %+v, %v; want nothing "+
			"cleared, saying why", result, err)
	}
	if _, found, _ := fleet.ModelRetry(t.Context(), "plan"); found {
		t.Error("a probe that did not answer wrote the record, so every node clears a limit nothing showed is over")
	}
}

// planNode is [retryNode] with a harness that reads its plan: a script that
// prints Claude Code's usage screen with the week at weekPercent, fenced as the
// shipped read is, and answers no model call.
func planNode(t *testing.T, fleet coord.Fleet, weekPercent int) (*Engine, *Company) {
	t.Helper()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus", subscription: true})
	script := filepath.Join(t.TempDir(), "claude")
	screen := fmt.Sprintf(`{"num_turns":0,"result":"Current session: 4%% used · resets 6:30am (UTC)\n`+
		`Current week (all models): %d%% used · resets Mon 9am (UTC)"}`, weekPercent)
	body := "#!/bin/sh\ncase \"$*\" in *usage*) [ \"$ANTHROPIC_BASE_URL\" = \"http://127.0.0.1:9\" ] || exit 3; " +
		"printf '%s' '" + screen + "';; *) exit 1;; esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := c.Config.Providers.LLM["plan"]
	spec.StateDir = t.TempDir()
	spec.Overrides = map[string]any{"binary": script}
	c.Config.Providers.LLM["plan"] = spec
	rebuilt, err := buildProviders(c.Config, config.NewResolver(config.MapSource{}), harness.NewPlans(nil), nil)
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	c.Models = rebuilt
	if !planOf(t, c).ReadsPlan() {
		t.Fatal("the premise: the entry signs in with a login and reads its plan")
	}
	e := epochEngine(c)
	e.backends = &Backends{Fleet: fleet}
	e.startModelRetries(t.Context())
	t.Cleanup(e.stopModelRetries)
	return e, c
}

// CHECK NOW READS THE PLAN, NO MODEL CALL: a plan with room clears every node's
// sit-out; a week still used up writes nothing and answers the window, now
// held until the week's reset rather than the sentence's.
//
// Mutation: probe with a call where the entry reads its plan, and the script
// (which answers no call) fails both cases.
func TestCheckNowReadsThePlanInsteadOfCallingTheModel(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		week    int
		cleared bool
	}{
		"the plan has room":         {week: 3, cleared: true},
		"the week is still used up": {week: 100},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := coordmemory.NewFleet()
			e, c := planNode(t, fleet, tc.week)
			spend(t, c, time.Now().Add(-time.Minute))
			result, err := e.RetryModel(t.Context(), "plan", coord.RetryProbe, person)
			if err != nil || result.Failed != "" || result.Cleared != tc.cleared {
				t.Fatalf("Check now = %+v, %v; want cleared %v", result, err, tc.cleared)
			}
			if _, found, _ := fleet.ModelRetry(t.Context(), "plan"); found != tc.cleared {
				t.Errorf("the record was written = %v, want %v", found, tc.cleared)
			}
			if !tc.cleared && (result.Still == nil || result.Still.Window != "weekly") {
				t.Errorf("a used-up week answers %+v, want the weekly window still held", result.Still)
			}
		})
	}
}

// A WEEKLY LIMIT SITS THE ENTRY OUT TO ITS RESET ON THE NODE'S OWN ROW, days
// away, from the vendor's own sentence ("resets Nov 30 at 7am"), so the alarm
// and the Models page read one window and no hourly end.
//
// Mutation: cap the bench again, or read "at" as no instant, and the row's
// window ends within the hour.
func TestAWeeklyLimitSitsOutToItsResetOnTheRow(t *testing.T) {
	t.Parallel()
	_, c := retryNode(t, coordmemory.NewFleet())
	at := time.Date(2026, time.November, 24, 18, 31, 0, 0, time.UTC)
	event := `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"result":"You've hit your weekly limit · resets Nov 30 at 7am (UTC)"}`
	if !planOf(t, c).RunEnded(t.Context(), harness.RunEnd{Event: event, Failed: true, At: at}) {
		t.Fatal("the premise: the entry sits out")
	}
	state := planOf(t, c).Plan().State(at.Add(2 * time.Hour))
	reset := time.Date(2026, time.November, 30, 7, 0, 0, 0, time.UTC)
	if len(state.SittingOut) != 1 || !state.SittingOut[0].Until.Equal(reset) ||
		!state.SittingOut[0].Since.Equal(at) {
		t.Errorf("two hours on, the entry sits out %+v; want one window from %v to %v", state.SittingOut, at, reset)
	}
}

// stoppedAlarm is an alarm that records nothing and stops at once.
type stoppedAlarm struct{}

func (stoppedAlarm) Stop() bool { return true }

// A READING THAT ENDS A SIT-OUT TRIES PARKED SEATS AGAIN NOW: a seat parked on a
// chain naming an entry on the sign-in waits for a probe armed at the reset it
// was told, so the probe is re-armed at once, as a person's Clear does.
//
// Mutation: match no entry in rearmProbesOn, and the probe still waits days.
func TestAReadingThatEndsASitOutTriesParkedSeatsNow(t *testing.T) {
	t.Parallel()
	e, c := planNode(t, coordmemory.NewFleet(), 3)
	var waits []time.Duration
	e.providerParks.after = func(d time.Duration, _ func()) alarm {
		waits = append(waits, d)
		return stoppedAlarm{}
	}
	basis := providerBasis{keys: []string{"plan"}}
	e.limitProbes.probes = map[probeKey]*limitProbe{probeKeyOf(basis): {basis: basis,
		until: time.Now().Add(6 * 24 * time.Hour), seats: map[uuid.UUID]string{uuid.New(): "lead"}}}
	e.rearmProbesOn(t.Context(), planOf(t, c).SignInID())
	if len(waits) != 1 || waits[0] != 0 {
		t.Errorf("the parked chain's probe was armed for %v, want once, now", waits)
	}
}
