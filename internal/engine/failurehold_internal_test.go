package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var (
	imageRefused = sandbox.Because(sandbox.WhyConfiguration, errors.New("the runtime refused the image"))
	vendorDown   = sandbox.Because(sandbox.WhyProviderUnavailable, errors.New("the sandbox provider is down"))
)

// EVERY FAILURE KIND IS CLASSIFIED: each sandbox reason either names a failure
// that pauses its seat or is left to the retry, and every failure that pauses
// a seat says why in the table, so a new reason is a decision, not a default.
func TestEveryFailureKindIsClassified(t *testing.T) {
	t.Parallel()
	for _, why := range sandbox.Whys {
		kind, classified := whyFailures[why]
		if !classified {
			t.Errorf("sandbox reason %q is neither a failure that pauses its seat nor one left to the retry", why)
			continue
		}
		if kind != "" && failureRules[kind].says == "" {
			t.Errorf("sandbox reason %q pauses its seat as %q, which the table does not word", why, kind)
		}
	}
	reached := map[failureKind]bool{failureModelsRefused: true}
	for _, kind := range whyFailures {
		reached[kind] = true
	}
	for kind := range failureRules {
		if !reached[kind] {
			t.Errorf("failure %q is in the table and nothing classifies an error as it", kind)
		}
	}
}

// A CHAIN EVERY MEMBER OF WHICH REFUSED ITS CREDENTIAL WILL REFUSE AGAIN; one
// member limited or failing otherwise is a chain a retry may get through.
func TestAChainThatRefusedEveryCredentialIsAFailureThatWillRepeat(t *testing.T) {
	t.Parallel()
	auth := &llm.Error{Kind: llm.KindAuth}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"every member refused", &chain.Error{Failures: []error{auth, auth}}, true},
		{"one member limited", &chain.Error{Failures: []error{auth, &llm.Error{Kind: llm.KindRateLimit}}}, false},
		{"no member tried", &chain.Error{}, false},
		{"not a chain", auth, false},
	} {
		kind, repeats := repeatingFailure(c.err)
		if repeats != c.want || (repeats && kind != failureModelsRefused) {
			t.Errorf("%s: classified as (%q, %v), want a repeating failure %v", c.name, kind, repeats, c.want)
		}
	}
}

// THE SECOND IDENTICAL FAILURE IN A ROW PAUSES THE SEAT, through the real
// dispatcher: two turns run, the second pause the seat as the engine, the
// delivery waits on its inbox, and no third turn runs.
func TestTheSecondIdenticalFailurePausesTheSeat(t *testing.T) {
	t.Parallel()
	e, q, fleet := pausingEngine(t)
	var turns atomic.Int32
	d := &Dispatcher{
		Conditions: func(h string) inbox.Conditions {
			paused, known := e.pauseOfHandle(h)
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				Paused: paused, PauseUnknown: !known}
		},
		Ledgered: inbox.Ledgered,
		Park:     e.park,
		Pause:    e.holdInbox,
		Failure:  e.failureHold,
		Turn: func(context.Context, Request) (turn.Result, error) {
			turns.Add(1)
			return turn.Result{}, imageRefused
		},
	}
	if err := q.SubscribeBatch(t.Context(), topics.AgentInbox(ceoSeat), topics.AgentInboxGroup(ceoSeat),
		func(ctx context.Context, evs []*events.Event) queue.Result { return d.Dispatch(ctx, "ceo", evs) },
		func(*events.Event) string { return "one-conversation" }, nil); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	sendTask(t, q, "ceo", "t-1")
	waitFor(t, "the seat to be paused for its failure", func() bool {
		p, paused, _ := e.pauseOf(ceoSeat)
		return paused && p.Failure != nil
	})
	p, _, err := fleet.SeatPause(t.Context(), ceoSeat)
	if err != nil || p.Failure == nil || p.Failure.Kind != string(failureSandboxSettings) ||
		p.ByKind != iam.ActorSystem || p.By != engineActor {
		t.Fatalf("the pause is %+v (%v), want the engine's, for the sandbox's configuration", p, err)
	}
	waitFor(t, "the inbox to be held", func() bool { return pauseHeld(q, ceoSeat) })
	if got := turns.Load(); got != 2 {
		t.Errorf("%d turns ran, want 2: the failure and its one repeat", got)
	}
}

// ONLY A RUN OF ONE FAILURE PAUSES: a turn that worked, a failure a retry may
// cure or another failure in between starts the count again.
func TestOnlyARunOfOneFailurePausesTheSeat(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	models := &chain.Error{Failures: []error{&llm.Error{Kind: llm.KindAuth}}}
	for _, between := range []error{nil, vendorDown, errors.New("a plain failure"), models} {
		if _, parked, err := e.failureHold(ctx, "ceo", imageRefused); parked || err != nil {
			t.Fatalf("the first failure paused (%v, %v)", parked, err)
		}
		if between == nil {
			e.failureCleared("ceo")
		} else if _, parked, err := e.failureHold(ctx, "ceo", between); parked || err != nil {
			t.Fatalf("%v between paused (%v, %v)", between, parked, err)
		}
		if _, parked, err := e.failureHold(ctx, "ceo", imageRefused); parked || err != nil {
			t.Fatalf("the failure after %v paused (%v, %v), want a new run", between, parked, err)
		}
		e.failures.forget(ceoSeat)
	}
	if all, err := fleet.ListSeatPauses(ctx); err != nil || len(all) != 0 {
		t.Errorf("broken runs left pauses %+v (%v)", all, err)
	}
	for range 2 {
		if _, _, err := e.failureHold(ctx, "ceo", vendorDown); err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := fleet.ListSeatPauses(ctx); len(all) != 0 {
		t.Errorf("a failure a retry may cure paused the seat: %+v", all)
	}
}

// AN APPLY THAT CHANGES WHAT THE FAILURE DEPENDED ON LIFTS ITS PAUSE, as the
// engine; one that changes something else, or a person's pause, stays.
func TestAChangedSettingLiftsTheFailurePause(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	for range 2 {
		if _, _, err := e.failureHold(ctx, "ceo", imageRefused); err != nil {
			t.Fatal(err)
		}
	}
	pauseSeat(t, fleet, seatOf("cto"), false)
	waitFor(t, "the copy to see both pauses", func() bool {
		_, a, _ := e.pauseOf(ceoSeat)
		_, b, _ := e.pauseOf(seatOf("cto"))
		return a && b
	})
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	activated, err := fleet.Activate(ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte("{}"),
		At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	same := companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
  - name: CTO
    handle: cto
    sandbox:
      max_turns: 5
`)
	same.ActivatedAt = activated.At
	e.liftChangedFailurePauses(ctx, same)
	if all, _ := fleet.ListSeatPauses(ctx); len(all) != 2 {
		t.Fatalf("an apply that left the failing seat's sandbox alone lifted a pause: %+v", all)
	}
	changed := companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
    sandbox:
      max_turns: 5
  - name: CTO
    handle: cto
`)
	changed.ActivatedAt = activated.At
	e.liftChangedFailurePauses(ctx, changed)
	all, err := fleet.ListSeatPauses(ctx)
	if err != nil || len(all) != 1 || all[0].Seat != seatOf("cto") {
		t.Errorf("after the failing seat's sandbox changed the pauses are %+v (%v), want the person's alone", all, err)
	}
}
