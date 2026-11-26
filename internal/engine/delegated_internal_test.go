package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
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
	// err is every record's failure, none for nil.
	err error
}

func (a *sweepAudit) RecordVerdict(_ context.Context, v builtin.DelegatedVerdict) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return false, a.err
	}
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
		return seal.OpenRawArguments(run.TurnID, *run.HeldCall)
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

// A request no delegate answered goes to people once the company's deadline
// passes, not before, whether the deadline is the engine's default or the
// document's; the escalation is recorded as the engine's, naming the server and
// command (the access log refuses a record without), its sentence the deadline.
//
// Mutation: compare the deadline the wrong way round, or skip the sweep's
// write, and a request waits on a silent delegate for ever; sweep by the
// default rather than the document's deadline, and a company that gave its
// delegates longer sees their requests early; record no subject, and the
// escalation never lands in the access log.
func TestADelegatedRequestReachesPeopleAtItsDeadline(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		deadline time.Duration
		words    string
	}{
		{delegated.DefaultDeadline, "within 15 minutes"},
		{25 * time.Minute, "within 25 minutes"},
		{450 * time.Second, "within 450 seconds"},
	} {
		heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
		store := sandbox.NewCoordStore(memory.NewFleet())
		sandboxtest.DelegatedParked(t, store, "t-late", []string{"security-a"}, heldAt)
		rules, audit := sweepRules{delegates: []string{"security-a"}, live: true}, &sweepAudit{}

		sweepDelegated(t.Context(), store, rules, c.deadline, audit, openHeld(t), nil, heldAt.Add(c.deadline-time.Second))
		if e := escalation(t, store, "t-late"); e != nil || len(audit.verdicts) != 0 {
			t.Fatalf("%v: a request a second before its deadline went to people: %+v, %+v", c.deadline, e,
				audit.verdicts)
		}
		sweepDelegated(t.Context(), store, rules, c.deadline, audit, openHeld(t), nil, heldAt.Add(c.deadline))
		e := escalation(t, store, "t-late")
		if e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledDeadline}) {
			t.Fatalf("%v: a request at its deadline: %+v, want it put to people for the deadline", c.deadline, e)
		}
		if len(e.Engine) != 1 || !strings.Contains(e.Engine[0], c.words) {
			t.Fatalf("%v: people are told %q, want it to say %q", c.deadline, e.Engine, c.words)
		}
		if len(audit.verdicts) != 1 || audit.verdicts[0].ByKind != builtin.VerdictByEngine ||
			audit.verdicts[0].Verdict != string(delegated.Escalate) || !audit.verdicts[0].Final ||
			!audit.verdicts[0].At.Equal(heldAt) {
			t.Fatalf("the sweep recorded %+v, want one final escalation by the engine, minted at the request",
				audit.verdicts)
		}
		if v := audit.verdicts[0]; v.Host != "api-1" || v.Subject != "sudo systemctl restart api" {
			t.Fatalf("the engine's escalation names %q on %q, want the command it decided on api-1", v.Subject, v.Host)
		}
		sweepDelegated(t.Context(), store, rules, c.deadline, audit, openHeld(t), nil, heldAt.Add(2*c.deadline))
		if len(audit.verdicts) != 1 {
			t.Fatalf("a second sweep recorded again: %+v", audit.verdicts)
		}
	}
}

// THE SWEEP TIMES A CALL ANOTHER NODE HELD ON ITS OWN CLOCK, from the pass that
// first saw it waiting (sandbox.HeldCall.WaitedFor): a duty holder whose clock
// runs half an hour ahead of the holding node's does not put a fresh request to
// people at once, and puts it to them the deadline after it first saw it.
//
// Mutation: forget what the sweep saw between passes, or judge the call's own
// instant on the duty's clock, and a check below goes red.
func TestTheSweepTimesACallAnotherNodeHeldOnItsOwnClock(t *testing.T) {
	t.Parallel()
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	store := sandbox.NewCoordStore(memory.NewFleet())
	sandboxtest.DelegatedParked(t, store, "t-ahead", []string{"security-a"}, heldAt)
	rules, audit := sweepRules{delegates: []string{"security-a"}, live: true}, &sweepAudit{}
	seen := &heldSightings{clock: "node-b:2"}
	ahead := heldAt.Add(30*time.Minute + time.Second)
	sweepDelegated(t.Context(), store, rules, delegated.DefaultDeadline, audit, openHeld(t), seen, ahead)
	if e := escalation(t, store, "t-ahead"); e != nil {
		t.Fatalf("a request a second old went to people on a duty half an hour ahead: %+v", e)
	}
	sweepDelegated(t.Context(), store, rules, delegated.DefaultDeadline, audit, openHeld(t), seen,
		ahead.Add(delegated.DefaultDeadline-time.Second))
	if e := escalation(t, store, "t-ahead"); e != nil {
		t.Fatalf("a request went to people before its deadline on the duty's own clock: %+v", e)
	}
	sweepDelegated(t.Context(), store, rules, delegated.DefaultDeadline, audit, openHeld(t), seen,
		ahead.Add(delegated.DefaultDeadline))
	if e := escalation(t, store, "t-ahead"); e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledDeadline}) {
		t.Fatalf("a request the deadline past the duty first saw it reads %+v, want it put to people", e)
	}
	sweepDelegated(t.Context(), store, rules, delegated.DefaultDeadline, audit, openHeld(t), seen,
		ahead.Add(2*delegated.DefaultDeadline))
	if len(seen.first) != 0 {
		t.Errorf("the sweep still keeps %d calls no longer waiting on their delegates", len(seen.first))
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
		sweepDelegated(t.Context(), store, c.rules, delegated.DefaultDeadline, audit, openHeld(t),
			nil, heldAt.Add(time.Minute))
		e := escalation(t, store, "t-off")
		if e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledOff}) || len(audit.verdicts) != 1 {
			t.Errorf("%s: the request reads %+v with %d records, want it put to people for delegation_off",
				c.name, e, len(audit.verdicts))
		}
	}
	store := sandbox.NewCoordStore(memory.NewFleet())
	sandboxtest.DelegatedParked(t, store, "t-on", []string{"security-a"}, heldAt)
	sweepDelegated(t.Context(), store, sweepRules{delegates: []string{"security-a"}, live: true},
		delegated.DefaultDeadline, &sweepAudit{}, openHeld(t), nil, heldAt.Add(time.Minute))
	if e := escalation(t, store, "t-on"); e != nil {
		t.Errorf("a request its live rule still delegates went to people early: %+v", e)
	}
}

// A request one of its own delegates made goes to people at once, whatever
// its deadline: no delegate decides its own request, and a panel's other half
// never allows alone. The wake escalates it as it is held; the sweep catches
// one the wake could not.
//
// Mutation: leave a delegate's own request to its deadline, and its delegate
// sits on its own merge for fifteen minutes, or its panel partner allows it.
func TestADelegatesOwnRequestReachesPeopleAtOnce(t *testing.T) {
	t.Parallel()
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	store := sandbox.NewCoordStore(memory.NewFleet())
	// sandboxtest's runs are swe's.
	sandboxtest.DelegatedParked(t, store, "t-own", []string{"swe", "security-b"}, heldAt)
	audit := &sweepAudit{}
	sweepDelegated(t.Context(), store, sweepRules{delegates: []string{"swe", "security-b"}, live: true},
		delegated.DefaultDeadline, audit, openHeld(t), nil, heldAt.Add(time.Second))
	e := escalation(t, store, "t-own")
	if e == nil || !slices.Equal(e.Overruled, []string{delegated.OverruledSelf}) || len(audit.verdicts) != 1 ||
		audit.verdicts[0].Subject == "" {
		t.Fatalf("a delegate's own request reads %+v with records %+v, want it put to people for self at once",
			e, audit.verdicts)
	}
}

// A MERGE WHOSE PULL REQUEST MERGED BEFORE ANYBODY DECIDED IT is answered by
// the engine, never put to its delegate: recorded first as the engine's final
// denial for already_merged, naming the change, then answered not carried
// out with the reason. A second request of a merge, held while the first was
// carried out, reached its delegate after the merge and cost a delegate turn
// and a flagged denial. A pull request still open, a read or a record that
// fails, or another kind, leave the request to its delegates.
//
// Mutation: skip the record or answer before it, and the engine carries out a
// verdict the access log never held; answer on a failed read, and a merge
// nobody read is denied.
func TestAMergeAlreadyMadeIsAnsweredWithoutWakingItsDelegate(t *testing.T) {
	t.Parallel()
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	request := func(act delegated.Kind) sandbox.PendingRun {
		return sandbox.PendingRun{TurnID: "t-merge", LaunchID: "launch-1", AgentHandle: "pm",
			Status: sandbox.StatusAwaiting, HeldCall: &sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000e1",
				LaunchID: "launch-1", Act: string(act), HeldAt: heldAt, Fixed: true,
				Delegation: &sandbox.HeldDelegation{Rule: 2, Delegates: []string{"release"},
					Digest: strings.Repeat("ab", 32)}}}
	}
	args := map[string]any{"change": "https://github.com/acme/web/pull/42", "head": strings.Repeat("a1", 20),
		"method": "merge", "reason": "QA approved it"}
	open := func(sandbox.PendingRun) (map[string]any, error) { return args, nil }
	const said = "acme/web#42 has already merged, so there is nothing left to merge; nobody was asked."
	for name, c := range map[string]struct {
		act      delegated.Kind
		merged   func(context.Context, string, map[string]any) (string, error)
		auditErr error
		answered bool
	}{
		"merged": {act: delegated.KindPRMerge, answered: true,
			merged: func(context.Context, string, map[string]any) (string, error) { return said, nil }},
		"still open": {act: delegated.KindPRMerge,
			merged: func(context.Context, string, map[string]any) (string, error) { return "", nil }},
		"unread": {act: delegated.KindPRMerge,
			merged: func(context.Context, string, map[string]any) (string, error) {
				return "", errors.New("GitHub did not answer")
			}},
		"unrecorded": {act: delegated.KindPRMerge, auditErr: errors.New("the log is down"),
			merged: func(context.Context, string, map[string]any) (string, error) { return said, nil }},
		"a command": {act: delegated.KindSSHOperate,
			merged: func(context.Context, string, map[string]any) (string, error) { return said, nil }},
	} {
		audit := &sweepAudit{err: c.auditErr}
		var answers []string
		answer := func(_ context.Context, run sandbox.PendingRun, words string) error {
			if len(audit.verdicts) == 0 {
				t.Errorf("%s: answered before the access log held the verdict", name)
			}
			answers = append(answers, words)
			return nil
		}
		got := mergedAlready(t.Context(), request(c.act), open, c.merged, audit, answer)
		if got != c.answered || (len(answers) == 1) != c.answered {
			t.Fatalf("%s: answered %v with %q, want answered %v", name, got, answers, c.answered)
		}
		if !c.answered {
			continue
		}
		if answers[0] != said {
			t.Errorf("%s: the run is told %q, want the reason", name, answers[0])
		}
		v := audit.verdicts[0]
		if v.ByKind != builtin.VerdictByEngine || v.Verdict != string(delegated.Deny) || !v.Final ||
			!slices.Equal(v.Overruled, []string{delegated.OverruledAlreadyMerged}) || !v.At.Equal(heldAt) ||
			v.Rule != 2 || v.Handle != "pm" || v.Subject == "" {
			t.Errorf("%s: recorded %+v, want the engine's final denial for already_merged, naming the change", name, v)
		}
	}
}

// A growth act nobody answered lapses a day after it was held, not before:
// the engine answers it Deny, once, with the reason, as the actor a growth
// product records as a lapse, and an act already being answered is left alone.
//
// Mutation: compare the lapse the wrong way round, sweep acts that wait on no
// lapse, or answer as another actor, and a raise runs days after the agent
// reasoned about it, an SSH command a person has not read yet is denied, or a
// lapse is logged as an administrator's denial.
func TestAGrowthActLapsesADayAfterItWasHeld(t *testing.T) {
	t.Parallel()
	if step, _ := (growth.Call{Approver: engineActor}).Denial(); step != growth.StepLapsed {
		t.Errorf("the engine answers as %q, which a growth product records as %s", engineActor, step)
	}
	heldAt := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	store := sandbox.NewCoordStore(memory.NewFleet())
	sandboxtest.GrowthParked(t, store, "t-spend", heldAt)
	sandboxtest.DelegatedParked(t, store, "t-ssh", []string{"security-a"}, heldAt)
	var answered []string
	answer := func(_ context.Context, run sandbox.PendingRun, said string) error {
		if !strings.Contains(said, "lapsed") {
			t.Errorf("the lapse said %q", said)
		}
		answered = append(answered, run.TurnID)
		return nil
	}
	sweepLapsed(t.Context(), store, answer, heldAt.Add(sandbox.HeldLapse-time.Second))
	if len(answered) != 0 {
		t.Fatalf("answered %v before the lapse", answered)
	}
	sweepLapsed(t.Context(), store, answer, heldAt.Add(sandbox.HeldLapse))
	if len(answered) != 1 || answered[0] != "t-spend" {
		t.Fatalf("answered %v at the lapse, want the growth act alone", answered)
	}
}
