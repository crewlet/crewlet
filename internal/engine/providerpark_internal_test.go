package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The provider park, end to end over the in-memory queue: a seat every model
// of whose chain refuses for a usage limit is not redelivered into the refusal,
// and its mail comes back, once and in order, when the limit lifts.

// limitedModel is a fake model behind a spent plan: until its reset it refuses
// every call as a rate limit naming the reset, as Claude Code's backend does
// once it has read "You've hit your 5-hour limit · resets 4:30pm", and after it
// answers. calls counts what reached it, which is the storm this park exists
// to stop.
type limitedModel struct {
	mu    sync.Mutex
	reset time.Time
	// retry is when the backend tries the entry again, where it caps the
	// vendor's reset as cliagent does at an hour; zero is the reset.
	retry time.Time
	now   func() time.Time
	calls int
}

func (m *limitedModel) Model() string { return "opus" }

func (m *limitedModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if now := m.now(); now.Before(m.reset) {
		retry := m.reset
		switch {
		case m.retry.IsZero():
		case now.Before(m.retry):
			retry = m.retry
		default:
			retry = now.Add(time.Hour)
		}
		return nil, &llm.Error{Kind: llm.KindRateLimit, Provider: "cli-agent/claude-code",
			Model: "opus", Key: "default", RetryAt: retry, ResetsAt: m.reset,
			Agent: "claude-code", Subscription: true,
			Err: errors.New("You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)")}
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

func (m *limitedModel) asked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// limitedRig is a park rig whose every turn asks one chain of the members
// given, which are also the company's models (so the park's probe asks the
// same entries), and records the park events the engine publishes.
type limitedRig struct {
	*parkRig
	mu     sync.Mutex
	parked []types.SeatProviderParked
	freed  []types.SeatProviderReleased
}

func newLimitedRig(t *testing.T, members ...chain.Member) *limitedRig {
	t.Helper()
	r := &limitedRig{parkRig: newParkRig(t, "100000000")}
	models, err := chain.New(members, chain.Options{})
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	entries := make([]phase.Entry, 0, len(members))
	for _, m := range members {
		entries = append(entries, phase.Entry{Key: m.Key, Provider: m.Provider})
	}
	reg, err := phase.NewRegistry(entries)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.e.Company().Models = reg
	r.parkRig.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		_, err := models.Complete(context.Background(), llm.Request{})
		return turn.Result{}, err
	}
	r.parkRig.mu.Unlock()
	for _, typ := range []string{"seat_provider_parked", "seat_provider_released"} {
		if err := r.q.Subscribe(t.Context(), topics.Event(typ), "test-"+typ,
			func(_ context.Context, ev *events.Event) queue.Result {
				r.mu.Lock()
				defer r.mu.Unlock()
				if p, ok := events.DataAs[*types.SeatProviderParked](ev); ok && p != nil {
					r.parked = append(r.parked, *p)
				}
				if p, ok := events.DataAs[*types.SeatProviderReleased](ev); ok && p != nil {
					r.freed = append(r.freed, *p)
				}
				return queue.Ack()
			}); err != nil {
			t.Fatalf("Subscribe %s: %v", typ, err)
		}
	}
	return r
}

// send publishes one trigger to the Lead's inbox and answers its id.
func (r *limitedRig) send(t *testing.T) uuid.UUID {
	t.Helper()
	ev := &events.Event{ID: uuid.New(), Type: "task_assigned", Timestamp: r.clock()}
	if err := r.q.Publish(t.Context(), topics.AgentInbox(seatOf("lead")), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return ev.ID
}

func (r *limitedRig) workedIDs() []uuid.UUID {
	r.parkRig.mu.Lock()
	defer r.parkRig.mu.Unlock()
	return slices.Clone(r.worked)
}

// deliveriesLeft is what each turn's delivery had left as the turn ran, in
// the order the turns ran.
func (r *limitedRig) deliveriesLeft() []int {
	r.parkRig.mu.Lock()
	defer r.parkRig.mu.Unlock()
	return slices.Clone(r.left)
}

// at moves the rig's clock.
func (r *limitedRig) at(now time.Time) {
	r.parkRig.mu.Lock()
	r.now = now
	r.parkRig.mu.Unlock()
}

func (r *limitedRig) parks() []types.SeatProviderParked {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.parked)
}

func (r *limitedRig) releases() []types.SeatProviderReleased {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.freed)
}

// A SEAT WHOSE MODEL IS SPENT IS PARKED UNTIL THE LIMIT LIFTS, AND ITS MAIL IS
// DELIVERED THEN, ONCE AND IN ORDER.
//
// The storm this replaces: a turn refused by the limit was NAKed, its
// redelivery a second later was refused the same way, and with every partition
// of every seat doing it the model was asked about once a second per seat until
// the reset, each turn flipping the seat between working, failed and stopped.
// A notification that arrives while the seat waits joins the queue behind the
// parked delivery rather than starting a turn of its own.
//
// Mutation: drop the provider stage from the dispatcher, and the refused
// delivery is NAKed: the hold is never taken and the second notification runs
// a turn into the limit.
func TestASpentModelParksTheSeatUntilItsResetAndThenDeliversOnce(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(14 * time.Minute)
	model := &limitedModel{reset: reset}
	r := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = r.clock

	first := r.send(t)
	if got := r.runs(); got != 1 {
		t.Fatalf("%d turns ran, want the one the limit refused", got)
	}
	if !slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("holds = %v, want the provider hold on the Lead's inbox", r.holds())
	}
	if n := r.deferralsNoted(); n != 1 {
		t.Errorf("the deferral was noted %d times, want once", n)
	}
	alarms := r.alarms.live()
	if len(alarms) != 1 || alarms[0].wait != reset.Sub(parkedAt) {
		t.Fatalf("alarms = %+v, want one set for the reset, 14 minutes away", alarms)
	}
	parks := r.parks()
	if len(parks) != 1 || parks[0].ResetsAt == nil || !parks[0].ResetsAt.Equal(reset) ||
		!slices.Equal(parks[0].ProviderKeys, []string{"default"}) || parks[0].AgentHandle != "lead" ||
		parks[0].CLI != "claude-code" || !parks[0].Subscription {
		t.Fatalf("parks published = %+v, want one naming the reset and the chain", parks)
	}

	// A notification that arrives during the park, and the renew that
	// resumes the attachment: neither reaches the model.
	second := r.send(t)
	r.renew(t)
	if got := model.asked(); got != 1 {
		t.Fatalf("the model was asked %d times during the park, want once", got)
	}

	// The reset: the alarm's probe is answered, the hold is lifted, and both
	// deliveries run, once each, the parked one first.
	r.at(reset.Add(time.Second))
	alarms[0].fire()
	if h := r.holds(); slices.Contains(h, pauseReasonProvider) {
		t.Errorf("the provider hold outlived the reset: %v", h)
	}
	want := []uuid.UUID{first, first, second}
	if got := r.workedIDs(); !slices.Equal(got, want) {
		t.Fatalf("turns ran on %v, want the refused delivery, then it again and the "+
			"notification after it, once each: %v", got, want)
	}
	if got := model.asked(); got != 4 {
		t.Errorf("the model was asked %d times, want the refusal, the probe and one call per "+
			"delivery", got)
	}
	if got := r.releases(); len(got) != 1 || got[0].Reason != "its models answered" {
		t.Errorf("releases published = %+v, want one saying the models answered", got)
	}
}

// A PROBE THAT MEETS THE LIMIT AGAIN ARMS THE PARK AGAIN, until the instant
// the refusal names, and touches nothing on the inbox: no turn runs, and the
// park says what the vendors now say.
//
// Mutation: lift the hold at the alarm, as the park used to, and the held
// delivery runs a turn into the limit.
func TestAProbeThatMeetsTheLimitArmsTheParkAgain(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(3 * time.Hour)
	probe := parkedAt.Add(time.Hour)
	model := &limitedModel{reset: reset, retry: probe}
	r := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = r.clock

	r.send(t)
	alarms := r.alarms.live()
	if len(alarms) != 1 || alarms[0].wait != time.Hour {
		t.Fatalf("alarms = %+v, want one for the probe an hour away", alarms)
	}
	r.renew(t)
	r.at(probe)
	alarms[0].fire()
	if got := r.runs(); got != 1 {
		t.Fatalf("%d turns ran, want only the one the limit refused: the probe is not a turn", got)
	}
	if got := model.asked(); got != 2 {
		t.Errorf("the model was asked %d times, want the refusal and the probe", got)
	}
	if !slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("holds = %v, want the seat still parked after the probe", r.holds())
	}
	if next := r.alarms.live(); len(next) != 1 || next[0].wait != time.Hour {
		t.Fatalf("alarms = %+v, want one armed for the next try, an hour on", next)
	}
	if got := r.parks(); len(got) != 2 || got[1].ResetsAt == nil || !got[1].ResetsAt.Equal(reset) {
		t.Errorf("parks = %+v, want the probe's renewal naming the same reset", got)
	}
	if got := r.releases(); len(got) != 0 {
		t.Errorf("releases = %+v, want none while the limit holds", got)
	}
}

// A LIMIT THAT OUTLASTS EVERY PROBE COSTS THE HELD MAIL NOTHING: a 40-hour
// limit the backend tries again hourly is probed forty times, and the held
// delivery runs once, when it lifts, with every delivery its park left it.
//
// The defect this closes: each probe used to be the held delivery's own turn,
// parked again by a deferral that spent one of its deliveries, so the message
// was dead-lettered about a day into the limit, after 24 probes.
//
// Mutation: lift the hold at each alarm (the old probe), and the held delivery
// runs a turn per probe, each spending a delivery.
func TestAFortyHourLimitDeliversTheHeldMailOnceWithItsDeliveriesIntact(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(40 * time.Hour)
	model := &limitedModel{reset: reset, retry: parkedAt.Add(time.Hour)}
	r := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = r.clock

	held := r.send(t)
	r.renew(t)
	probes := 0
	for now := parkedAt.Add(time.Hour); now.Before(reset); now = now.Add(time.Hour) {
		alarms := r.alarms.live()
		if len(alarms) != 1 {
			t.Fatalf("at %v, alarms = %d, want the one park's", now, len(alarms))
		}
		r.at(now)
		alarms[0].fire()
		probes++
		if got := r.runs(); got != 1 {
			t.Fatalf("after %d probes, %d turns ran, want only the one the limit refused", probes, got)
		}
	}
	r.at(reset.Add(time.Second))
	alarms := r.alarms.live()
	if len(alarms) != 1 {
		t.Fatalf("at the reset, alarms = %d, want the park's", len(alarms))
	}
	alarms[0].fire()

	if got := r.workedIDs(); !slices.Equal(got, []uuid.UUID{held, held}) {
		t.Fatalf("turns ran on %v, want the refused delivery and then it once more", got)
	}
	if got := model.asked(); got != 1+probes+1+1 {
		t.Errorf("the model was asked %d times, want the refusal, %d probes, the probe that "+
			"answered and the delivery's turn", got, probes)
	}
	left := r.deliveriesLeft()
	if len(left) != 2 || left[1] != left[0]-1 {
		t.Fatalf("deliveries left per turn = %v, want the redelivery one short of the first: "+
			"the park's deferral is the only one spent", left)
	}
}

// A CHAIN THAT IS NOT LIMITED THROUGHOUT IS NOT PARKED. A spent head whose
// fallback failed for a reason nothing says will last keeps the ordinary
// retry: the next delivery may well find the fallback answering.
//
// Mutation: park on the chain's first member alone, and the seat is held.
func TestASpentHeadWithAFailingFallbackIsNotParked(t *testing.T) {
	t.Parallel()
	model := &limitedModel{reset: parkedAt.Add(time.Hour)}
	r := newLimitedRig(t,
		chain.Member{Key: "default", Provider: model},
		chain.Member{Key: "key", Provider: failingModel{}},
	)
	model.now = r.clock
	r.send(t)
	if h := r.holds(); slices.Contains(h, pauseReasonProvider) {
		t.Fatalf("holds = %v, want no provider park for a chain whose fallback did not refuse for a limit", h)
	}
	if got := r.parks(); len(got) != 0 {
		t.Errorf("parks published = %+v, want none", got)
	}
}

// failingModel fails as a server error.
type failingModel struct{}

func (failingModel) Model() string { return "sonnet" }

func (failingModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{Kind: llm.KindServer, Key: "key", Err: errors.New("overloaded")}
}

// NEW MODELS RELEASE THE PARK AT ONCE, and say so: every epoch builds its
// providers afresh, and a fresh provider has sat nothing out.
func TestNewModelsReleaseTheParkAtOnce(t *testing.T) {
	t.Parallel()
	model := &limitedModel{reset: parkedAt.Add(time.Hour)}
	r := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = r.clock
	r.send(t)
	if !slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("precondition: the seat is not parked (holds %v)", r.holds())
	}

	// The same company: nothing is released.
	r.e.reconcileProviderParks(t.Context(), r.e.Company())
	if !slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("an apply with the same models released the park: holds %v", r.holds())
	}

	next := companyFor(t, strings.Replace(leadDoc, "%d", "100000000", 1))
	next.Models = newEmptyRegistry(t)
	r.e.epoch.current.Store(next)
	r.e.reconcileProviderParks(t.Context(), next)
	if slices.Contains(r.holds(), pauseReasonProvider) {
		t.Errorf("new models left the park standing: holds %v", r.holds())
	}
	if got := r.releases(); len(got) != 1 || !strings.Contains(got[0].Reason, "models changed") {
		t.Errorf("releases published = %+v, want one saying the models changed", got)
	}
	if got := r.alarms.live(); len(got) != 0 {
		t.Errorf("%d alarms still armed after the release, want none", len(got))
	}
}

// newEmptyRegistry is a registry of one model that answers, standing for the
// models a new epoch builds.
func newEmptyRegistry(t *testing.T) *phase.Registry {
	t.Helper()
	reg, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: &limitedModel{now: time.Now}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// knownLimit is a model whose entry already knows it is at its limit until
// retry, as cliagent's does once its CLI reported it ([chain.Unready]).
type knownLimit struct{ retry time.Time }

func (knownLimit) Model() string { return "opus" }

func (knownLimit) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, errors.New("a retried resume was run against a limit the entry already knew")
}

func (m knownLimit) Unready(context.Context) error {
	if m.retry.IsZero() {
		return nil
	}
	return &llm.Error{Kind: llm.KindRateLimit, RetryAt: m.retry, Err: errors.New("limited")}
}

// A RETRIED RESUME WAITS FOR A LIMIT THE MODELS ALREADY KNOW, until the first
// is tried again, rather than running a turn to meet it: the turn gate's
// provider condition, beside the budget's. A chain whose entries know of no
// limit is not refused, and the resume finds out by running.
//
// Mutation: ask the chain nothing (answer not refusing), and the limited seat
// is admitted.
func TestARetriedResumeWaitsForALimitItsModelsKnow(t *testing.T) {
	t.Parallel()
	retry := parkedAt.Add(14 * time.Minute)
	for name, tc := range map[string]struct {
		model   knownLimit
		refused bool
	}{
		"its models know a limit": {model: knownLimit{retry: retry}, refused: true},
		"its models know none":    {model: knownLimit{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100000000")
			c := companyFor(t, strings.Replace(leadDoc, "%d", "100000000", 1))
			reg, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: tc.model}})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			c.Models = reg
			r.e.epoch.current.Store(c)
			reason, until, refused := r.e.providerRefusing(t.Context(), "lead")
			if refused != tc.refused {
				t.Fatalf("refused = %v (%q), want %v", refused, reason, tc.refused)
			}
			if refused && (!until.Equal(retry) || !strings.Contains(reason, "default")) {
				t.Errorf("refusal (%q) lifts at %v, want the entry's retry %v", reason, until, retry)
			}
		})
	}
}
