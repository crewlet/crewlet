package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/secrets"
)

// sweepRules is the live rules as a sweep reads them: whether ssh_operate is
// delegated, and to whom.
type sweepRules struct {
	delegates []string
	live      bool
}

func (r sweepRules) Rule(delegated.Kind) ([]string, int, int, bool) {
	return r.delegates, 1, delegated.DefaultSampleOneIn, r.live
}

// sweepAudit keeps what the sweep recorded.
type sweepAudit struct {
	mu       sync.Mutex
	verdicts []builtin.DelegatedVerdict
}

func (a *sweepAudit) RecordVerdict(_ context.Context, v builtin.DelegatedVerdict) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verdicts = append(a.verdicts, v)
	return true, nil
}

// openHeld opens a held call's arguments as sandboxtest sealed them.
func openHeld(t *testing.T) func(sandbox.PendingRun) (map[string]any, error) {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := sandbox.NewCallSeal(cipher)
	return func(run sandbox.PendingRun) (map[string]any, error) {
		return seal.OpenArguments(run.TurnID, *run.HeldCall)
	}
}

// escalation is what the sweep wrote on turnID's held call, nil for none.
func escalation(t *testing.T, store sandbox.PendingStore, turnID string) *sandbox.HeldEscalation {
	t.Helper()
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("reading %s: %v, %v", turnID, found, err)
	}
	return run.HeldCall.Delegation.Escalated
}

// A request no delegate answered goes to people once its deadline passes, and
// not a moment before; the escalation is recorded as the engine's, naming the
// server and the command it decided, which the access log refuses a record
// without.
//
// Mutation: compare the deadline the wrong way round, or skip the sweep's
// write, and a request waits on a silent delegate for ever; record no subject,
// and the escalation never lands in the access log.
func TestADelegatedRequestReachesPeopleAtItsDeadline(t *testing.T) {
	t.Parallel()
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	store := sandbox.NewCoordStore(memory.NewFleet())
	sandboxtest.DelegatedParked(t, store, "t-late", []string{"security-a"}, heldAt)
	rules, audit := sweepRules{delegates: []string{"security-a"}, live: true}, &sweepAudit{}

	sweepDelegated(t.Context(), store, rules, audit, openHeld(t), heldAt.Add(delegated.Deadline-time.Second))
	if e := escalation(t, store, "t-late"); e != nil || len(audit.verdicts) != 0 {
		t.Fatalf("a request a second before its deadline went to people: %+v, %+v", e, audit.verdicts)
	}
	sweepDelegated(t.Context(), store, rules, audit, openHeld(t), heldAt.Add(delegated.Deadline))
	e := escalation(t, store, "t-late")
	if e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledDeadline}) {
		t.Fatalf("a request at its deadline: %+v, want it put to people for the deadline", e)
	}
	if len(audit.verdicts) != 1 || audit.verdicts[0].ByKind != builtin.VerdictByEngine ||
		audit.verdicts[0].Verdict != string(delegated.Escalate) || !audit.verdicts[0].Final ||
		!audit.verdicts[0].At.Equal(heldAt) {
		t.Fatalf("the sweep recorded %+v, want one final escalation by the engine, minted at the request", audit.verdicts)
	}
	if v := audit.verdicts[0]; v.Host != "api-1" || v.Subject != "sudo systemctl restart api" {
		t.Fatalf("the engine's escalation names %q on %q, want the command it decided on api-1", v.Subject, v.Host)
	}
	sweepDelegated(t.Context(), store, rules, audit, openHeld(t), heldAt.Add(2*delegated.Deadline))
	if len(audit.verdicts) != 1 {
		t.Fatalf("a second sweep recorded again: %+v", audit.verdicts)
	}
}

// Turning delegated decisions off, or moving a kind to other delegates, puts
// every request still waiting on the old ones to people on the next sweep,
// whatever its deadline.
//
// Mutation: ignore the live rule in the sweep, and a request switched off
// waits out its deadline on a delegate the company no longer trusts.
func TestTurningDelegationOffReturnsEveryRequestToPeople(t *testing.T) {
	t.Parallel()
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		rules sweepRules
	}{
		{"turned off", sweepRules{live: false}},
		{"another delegate", sweepRules{delegates: []string{"security-b"}, live: true}},
	} {
		store := sandbox.NewCoordStore(memory.NewFleet())
		sandboxtest.DelegatedParked(t, store, "t-off", []string{"security-a"}, heldAt)
		audit := &sweepAudit{}
		sweepDelegated(t.Context(), store, c.rules, audit, openHeld(t), heldAt.Add(time.Minute))
		e := escalation(t, store, "t-off")
		if e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledOff}) || len(audit.verdicts) != 1 {
			t.Errorf("%s: the request reads %+v with %d records, want it put to people for delegation_off",
				c.name, e, len(audit.verdicts))
		}
	}
	store := sandbox.NewCoordStore(memory.NewFleet())
	sandboxtest.DelegatedParked(t, store, "t-on", []string{"security-a"}, heldAt)
	sweepDelegated(t.Context(), store, sweepRules{delegates: []string{"security-a"}, live: true}, &sweepAudit{},
		openHeld(t), heldAt.Add(time.Minute))
	if e := escalation(t, store, "t-on"); e != nil {
		t.Errorf("a request its live rule still delegates went to people early: %+v", e)
	}
}
