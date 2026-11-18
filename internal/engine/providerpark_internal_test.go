package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
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
	// vendor's reset as harness does at an hour; zero is the reset.
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
		return nil, &llm.Error{Kind: llm.KindRateLimit, Provider: "harness/claude-code",
			Model: "opus", Key: "default", RetryAt: retry, ResetsAt: m.reset,
			Harness: "claude-code", Subscription: true,
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
		parks[0].Harness != "claude-code" || !parks[0].Subscription {
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

	next := leadCompany(t, "100000000", "0")
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
// retry, as harness's does once its harness reported it ([chain.Unready]).
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
// is tried again, rather than running a turn to meet it: the provider
// condition an answer's gate and a completion's collected resume ask, beside
// the budget's. A chain whose entries know of no limit is not refused, and the
// resume finds out by running. The refusal carries the chain's limit, keys
// and instant, which a completion's resume parks its seat on.
//
// Mutation: ask the chain nothing (answer not refusing), and the limited seat
// is admitted; drop the limit from the answer, and nothing names what to park
// on.
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
			c := leadCompany(t, "100000000", "0")
			reg, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: tc.model}})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			c.Models = reg
			r.e.epoch.current.Store(c)
			refusal, limit, refused := r.e.providerRefusing(t.Context(), c, "lead", sandbox.ResumeCompletion)
			if refused != tc.refused {
				t.Fatalf("refused = %v (%q), want %v", refused, refusal.Reason, tc.refused)
			}
			if !refused {
				return
			}
			if refusal.Condition != waitProvider || !refusal.Until.Equal(retry) ||
				!strings.Contains(refusal.Reason, "default") {
				t.Errorf("refusal %+v, want one on %q lifting at the entry's retry %v",
					refusal, waitProvider, retry)
			}
			if !slices.Equal(limit.Keys, []string{"default"}) || !limit.RetryAt.Equal(retry) {
				t.Errorf("the chain's limit is %+v, want default's, tried again at %v", limit, retry)
			}
		})
	}
}

// threeSeatDoc is a company of three seats on one model entry.
const threeSeatDoc = `
name: Acme
roles:
  - name: Lead
    handle: lead
  - name: PM
    handle: pm
  - name: QA
    handle: qa
`

// probeRig is an engine over the in-memory queue with threeSeatDoc's company,
// its one entry the model given, its park alarms fired by the case, and its
// clock moved by the case: what a provider park's probe is driven over, with
// no dispatcher in front of it.
type probeRig struct {
	e      *Engine
	q      *memory.Queue
	alarms *fakeAlarms

	mu  sync.Mutex
	now time.Time
}

func newProbeRig(t *testing.T, model llm.Provider) *probeRig {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	r := &probeRig{q: q, alarms: &fakeAlarms{}, now: parkedAt}
	c := companyFor(t, threeSeatDoc)
	reg, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: model}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = reg
	r.e = &Engine{backends: &Backends{Queue: q, Fleet: coordmem.NewFleet()}, clock: r.clock}
	r.e.epoch.current.Store(c)
	r.e.providerParks.after = r.alarms.after
	// EVERY SEAT HELD HERE: each park's hold names its inbox's attachment.
	for _, handle := range []string{"lead", "pm", "qa"} {
		id, err := r.e.seatID(handle)
		if err != nil {
			t.Fatalf("seatID(%s): %v", handle, err)
		}
		takeInbox(t, r.e, q, id)
	}
	return r
}

func (r *probeRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *probeRig) at(now time.Time) {
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// park parks handle on the entry's limit, tried again at retry.
func (r *probeRig) park(t *testing.T, handle string, retry time.Time) {
	t.Helper()
	r.parkOn(t, handle, "default", retry)
}

// parkOn parks handle on the limit of a chain of the one entry key, tried
// again at retry.
func (r *probeRig) parkOn(t *testing.T, handle, key string, retry time.Time) {
	t.Helper()
	refusal := &llm.Error{Kind: llm.KindRateLimit, Key: key, RetryAt: retry,
		Err: errors.New("You've hit your 5-hour limit")}
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{key},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	if _, parked, err := r.e.providerPark(t.Context(), handle, limit); err != nil || !parked {
		t.Fatalf("providerPark(%s) = %v, %v", handle, parked, err)
	}
}

// ONE PROBE PER LIMITED CHAIN, HOWEVER MANY SEATS IT HOLDS. Three seats parked
// on one spent entry for 40 hours are probed once an hour, not three times,
// and the answer that ends the limit releases all three: the limit is the
// entry's, and each probe is a full, charged run of its harness.
//
// Mutation: give each seat's park an alarm and a probe of its own, and every
// tick asks the model three times.
func TestSeatsOnOneLimitedEntryShareOneProbe(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(40 * time.Hour)
	model := &limitedModel{reset: reset, retry: parkedAt.Add(time.Hour)}
	r := newProbeRig(t, model)
	model.now = r.clock
	e, q, alarms, at := r.e, r.q, r.alarms, r.at
	var released atomic.Int32
	if err := q.Subscribe(t.Context(), topics.Event("seat_provider_released"), "test-released",
		func(context.Context, *events.Event) queue.Result { released.Add(1); return queue.Ack() }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_, refusal := model.Complete(t.Context(), llm.Request{})
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	handles := []string{"lead", "pm", "qa"}
	for _, h := range handles {
		if _, parked, err := e.providerPark(t.Context(), h, limit); err != nil || !parked {
			t.Fatalf("providerPark(%s) = %v, %v", h, parked, err)
		}
	}
	held := func() int {
		n := 0
		for _, h := range handles {
			id, _ := org.DeriveAgentID("Acme", h)
			if slices.Contains(q.PauseHolds(topics.AgentInbox(id), topics.AgentInboxGroup(id)),
				pauseReasonProvider) {
				n++
			}
		}
		return n
	}

	asked := model.asked()
	for tick := parkedAt.Add(time.Hour); tick.Before(reset); tick = tick.Add(time.Hour) {
		live := alarms.live()
		if len(live) != 1 {
			t.Fatalf("at %v, %d alarms are armed for three seats on one entry, want one", tick, len(live))
		}
		at(tick)
		live[0].fire()
		if got := model.asked() - asked; got != 1 {
			t.Fatalf("at %v the model was asked %d times, want one probe for all three seats", tick, got)
		}
		asked = model.asked()
		if n := held(); n != 3 {
			t.Fatalf("at %v, %d of the three seats are held, want all three", tick, n)
		}
	}
	at(reset.Add(time.Second))
	alarms.live()[0].fire()
	if got := model.asked() - asked; got != 1 {
		t.Errorf("the probe that answered asked the model %d times, want once", got)
	}
	if n := held(); n != 0 {
		t.Errorf("%d seats still held after the probe answered, want none", n)
	}
	if got := released.Load(); got != 3 {
		t.Errorf("%d releases published, want one per seat", got)
	}
	if live := alarms.live(); len(live) != 0 {
		t.Errorf("%d alarms left armed after the limit lifted, want none", len(live))
	}
}

// heldCall is a model whose calls the case scripts: the first `limited` calls
// refuse for a limit at once, naming a next try later than the one a case
// parks on (so the renewal is news, which is published), and every later one
// is held until its context ends (or the case does), lingering a moment after,
// so a case can see who waited for it. started is told of each held call, and
// ended counts them.
type heldCall struct {
	limited int
	linger  time.Duration
	done    <-chan struct{}
	started chan struct{}
	calls   atomic.Int32
	ended   atomic.Int32
}

func newHeldCall(t *testing.T, limited int) *heldCall {
	return &heldCall{limited: limited, done: t.Context().Done(), started: make(chan struct{}, 4)}
}

func (*heldCall) Model() string { return "opus" }

func (m *heldCall) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	if int(m.calls.Add(1)) <= m.limited {
		return nil, &llm.Error{Kind: llm.KindRateLimit, Key: "default",
			RetryAt: parkedAt.Add(2 * time.Hour), Err: errors.New("You've hit your 5-hour limit")}
	}
	m.started <- struct{}{}
	select {
	case <-ctx.Done():
	case <-m.done:
	}
	time.Sleep(m.linger)
	m.ended.Add(1)
	return nil, ctx.Err()
}

// awaitSignal fails the case unless ch is told within a few seconds.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// onItsOwn runs fire on a goroutine of its own, as a timer does, and closes the
// channel it returns once fire has returned.
func onItsOwn(fire func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fire()
	}()
	return done
}

// THE LAST SEAT OUT ENDS THE PROBE'S CALL, AND THE CALL RELEASES NOTHING. A
// seat given up while its probe's call is out leaves the probe; as the last
// seat on it, it retires the probe, and the call is ended rather than left to
// run to its own timeout, its answer settling a park nobody holds.
//
// Mutation: leave the call running when its last seat leaves, and it is never
// ended.
func TestTheLastSeatOutEndsTheProbesCall(t *testing.T) {
	t.Parallel()
	model := newHeldCall(t, 0)
	r := newProbeRig(t, model)
	var probeReleases atomic.Int32
	r.q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if p, ok := events.DataAs[*types.SeatProviderReleased](ev); ok && p != nil &&
			strings.Contains(p.Reason, "models") {
			probeReleases.Add(1)
		}
	})
	r.park(t, "lead", parkedAt.Add(time.Hour))
	call := onItsOwn(r.alarms.live()[0].fire)
	awaitSignal(t, model.started, "the probe's call to start")

	r.e.forgetProviderPark(t.Context(), seatOf("lead"))
	awaitSignal(t, call, "the probe's call to be ended by the last seat leaving")
	if got := model.ended.Load(); got != 1 {
		t.Errorf("%d calls ended, want the probe's", got)
	}
	if got := probeReleases.Load(); got != 0 {
		t.Errorf("the ended call released %d parks, want none: its seat had left", got)
	}
}

// A STOPPING NODE ENDS A PROBE'S CALL AND WAITS FOR IT, so no call this node
// made outlives its teardown.
//
// Mutation: return from the stop without waiting, and the call is still
// running when it does.
func TestAStopWaitsForAProbesCall(t *testing.T) {
	t.Parallel()
	model := newHeldCall(t, 0)
	model.linger = 200 * time.Millisecond
	r := newProbeRig(t, model)
	r.park(t, "lead", parkedAt.Add(time.Hour))
	onItsOwn(r.alarms.live()[0].fire)
	awaitSignal(t, model.started, "the probe's call to start")

	r.e.stopProviderParks()
	if got := model.ended.Load(); got != 1 {
		t.Fatalf("the stop returned with %d of the probe's call ended, want it waited for", got)
	}
}

// A NEWER CALL OF A PROBE IS NOT FORGOTTEN BY AN OLDER ONE ENDING. A limit
// that holds re-arms the probe before its call has returned, and an alarm for
// an instant already past fires at once, so the next call can be out before the
// one that armed it has cleaned up. That cleanup clears its own call, never
// the newer one, so the last seat leaving still ends the newer call.
//
// Mutation: clear the probe's call unconditionally when a call ends, and the
// newer one is never ended.
func TestAnOlderProbeCallLeavesTheNewerOnesCancel(t *testing.T) {
	t.Parallel()
	model := newHeldCall(t, 1)
	r := newProbeRig(t, model)
	var renewing atomic.Bool
	var newer <-chan struct{}
	r.q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		// THE OLDER CALL'S RENEWAL is published after it re-armed the probe
		// and before its cleanup: the newer call starts here, in between.
		if _, ok := events.DataAs[*types.SeatProviderParked](ev); !ok || !renewing.CompareAndSwap(true, false) {
			return
		}
		live := r.alarms.live()
		if len(live) != 1 {
			t.Errorf("%d alarms armed by the renewal, want one", len(live))
			return
		}
		newer = onItsOwn(live[0].fire)
		awaitSignal(t, model.started, "the newer call to start")
	})
	r.park(t, "lead", parkedAt.Add(time.Hour))
	renewing.Store(true)
	r.alarms.live()[0].fire()
	if newer == nil {
		t.Fatal("the older call's renewal never started a newer call")
	}

	r.e.forgetProviderPark(t.Context(), seatOf("lead"))
	awaitSignal(t, newer, "the newer call to be ended by the last seat leaving")
}

// A RELEASED SEAT'S PROVIDER PARK TAKES ITS HOLD WITH IT. The release detaches
// the seat's inbox, which drops every hold on it and ends the attachment they
// were taken for, and forgets the seat's parks after; a park a turn asks for in
// between (a resume, or a delivery the release did not wait for) names that
// ended attachment, and the queue refuses it. Taken anyway, as it was when
// holds were keyed by the subscription alone, its hold kept the seat deaf on
// this node once it came back, with no park left to lift it.
//
// Mutation: take a park's hold for the inbox's current attachment rather than
// the one the seat held, and the park asked between the detach and the forget
// holds the seat's inbox.
func TestAReleasedSeatsProviderParkTakesItsHoldWithIt(t *testing.T) {
	t.Parallel()
	r := newProbeRig(t, answers{})
	inbox, group := topics.AgentInbox(seatOf("lead")), topics.AgentInboxGroup(seatOf("lead"))
	r.park(t, "lead", parkedAt.Add(time.Hour))
	if holds := r.q.PauseHolds(inbox, group); !slices.Contains(holds, pauseReasonProvider) {
		t.Fatalf("the premise: the park holds the inbox under %q, holds %v", pauseReasonProvider, holds)
	}

	// THE RELEASE'S DETACH, and a park asked before the release tells the
	// engine.
	if _, err := r.q.Detach(t.Context(), inbox, group); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	refusal := &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: parkedAt.Add(time.Hour),
		Err: errors.New("You've hit your 5-hour limit")}
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	if _, parked, err := r.e.providerPark(t.Context(), "lead", limit); !errors.Is(err, queue.ErrAttachmentGone) || parked {
		t.Fatalf("a park asked once the release detached the inbox = %v, %v; want it refused", parked, err)
	}
	r.e.detachInbox(seatOf("lead"))
	r.e.forgetProviderPark(t.Context(), seatOf("lead"))
	if holds := r.q.PauseHolds(inbox, group); slices.Contains(holds, pauseReasonProvider) {
		t.Fatalf("the released seat's inbox is still held %v by a park asked after its detach", holds)
	}
}

// A PARK ITS SEAT'S RELEASE FORGOT BEFORE THE PARK WAS WHOLE IS UNDONE ALL THE
// SAME. The release's forget can land between a park's take and the park's own
// look at the seat: it drops the record (the release's detach having dropped
// the hold), and the park joins
// its chain's probe and says the seat parked after it. With no record left to
// release, the park that finds its seat gone still leaves the probe and says
// the seat released: kept on the probe, the seat's alarm went on calling the
// models as the seat, and the seat read parked while a peer ran it.
//
// Mutation: undo only a park whose record is left (unparkReleased), and the
// probe's alarm stays armed for the seat, with nothing said.
func TestAParkItsSeatsReleaseForgotFirstIsUndoneWhole(t *testing.T) {
	t.Parallel()
	r := newProbeRig(t, answers{})
	var released atomic.Int32
	r.q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if p, ok := events.DataAs[*types.SeatProviderReleased](ev); ok && p != nil &&
			p.Reason == seatReleasedHere {
			released.Add(1)
		}
	})
	// WHAT THE PARK LEFT AFTER THE RELEASE'S FORGET: its place on the probe,
	// and no record.
	basis := providerBasis{models: r.e.Company().Models, keys: []string{"default"}}
	r.e.joinLimitProbe(t.Context(), basis, seatOf("lead"), "lead", parkedAt.Add(time.Hour))
	if live := r.alarms.live(); len(live) != 1 {
		t.Fatalf("the premise: %d probe alarms armed, want the seat's", len(live))
	}

	r.e.unparkReleased(t.Context(), seatOf("lead"), "lead", basis)
	if live := r.alarms.live(); len(live) != 0 {
		t.Fatalf("%d probe alarms armed for a seat this node no longer holds, want none", len(live))
	}
	if got := released.Load(); got != 1 {
		t.Fatalf("%d releases published, want the one saying the seat was released", got)
	}
}

// A PARK ITS SEAT'S RELEASE FORGOT FIRST LEAVES NO WAIT BEHIND.
//
// The release's forget can land between a park's take and its join of the
// chain's probe ([Engine.unparkReleased]): it forgets the seat's wait and drops
// the park's record, and the join after it begins a wait again. The park's
// undoing left that wait, which nothing then ended: it outlived the seat on
// this node, and a park once the node held the seat again carried its start,
// so a seat that had just met the limit read as waiting since before it let
// go, past the fifteen minutes `llm_limit_waiting` is judged at.
//
// Mutation: undo the park without forgetting the seat's wait, and this fails.
func TestAParkItsSeatsReleaseForgotFirstLeavesNoWaitBehind(t *testing.T) {
	t.Parallel()
	r := newProbeRig(t, answers{})
	lead := seatOf("lead")
	// WHAT THE PARK LEFT AFTER THE RELEASE'S FORGET: the wait its join began,
	// its place on the probe, and no record.
	basis := providerBasis{models: r.e.Company().Models, keys: []string{"default"}}
	r.e.joinLimitProbe(t.Context(), basis, lead, "lead", parkedAt.Add(time.Hour))
	r.e.unparkReleased(t.Context(), lead, "lead", basis)

	// THE SEAT HELD HERE AGAIN, a day later, meeting the limit.
	again := parkedAt.Add(24 * time.Hour)
	r.at(again)
	r.park(t, "lead", again.Add(time.Hour))
	rows := 0
	var since time.Time
	for _, row := range r.e.parkedByEntry()["default"] {
		if row.Seat == lead {
			rows++
			since = row.Since
		}
	}
	if rows != 1 || !since.Equal(again) {
		t.Errorf("the seat parked once this node held it again waits from %v (%d rows), want "+
			"one new wait from %v: the park its release forgot first left its wait behind",
			since, rows, again)
	}
}

// pastLimit is a model that refuses every call for a limit naming a next try
// already past: what an entry kept on the monotonic clock said after its host
// slept inside its window. calls counts what reached it.
type pastLimit struct{ calls atomic.Int32 }

func (*pastLimit) Model() string { return "opus" }

func (m *pastLimit) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.calls.Add(1)
	return nil, &llm.Error{Kind: llm.KindRateLimit, Key: "default",
		RetryAt: parkedAt.Add(-time.Minute), Err: errors.New("You've hit your 5-hour limit")}
}

// A PROBE REFUSED WITH A NEXT TRY ALREADY PAST WAITS BEFORE IT ASKS AGAIN.
// Armed for the instant its models named, a probe whose models named one
// already past was due at once: it asked, was refused and asked again as fast
// as the call returned, renewing every parked seat's park on each pass. After
// a host slept inside a limit's window that was thousands of probes a second,
// with a WARN per seat per probe while the broker was down. So the renewal
// waits the shortest window an entry sits out, and says when it is tried
// again by that.
//
// Mutation: arm the renewal for the instant the models named, and its alarm
// is due at once.
func TestAProbeRefusedWithANextTryAlreadyPastWaitsBeforeItAsksAgain(t *testing.T) {
	t.Parallel()
	model := &pastLimit{}
	r := newProbeRig(t, model)
	var parks []types.SeatProviderParked
	var mu sync.Mutex
	r.q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if p, ok := events.DataAs[*types.SeatProviderParked](ev); ok && p != nil {
			mu.Lock()
			parks = append(parks, *p)
			mu.Unlock()
		}
	})
	r.park(t, "lead", parkedAt.Add(-time.Minute))
	live := r.alarms.live()
	if len(live) != 1 || live[0].wait != 0 {
		t.Fatalf("alarms = %+v, want the park's one probe due now", live)
	}
	live[0].fire()
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("the probe asked the models %d times, want once", got)
	}
	next := r.alarms.live()
	if len(next) != 1 || next[0].wait != harness.ShortestSitOut {
		t.Fatalf("alarms = %+v, want the renewal's one armed %s on, not at once",
			next, harness.ShortestSitOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(parks) != 2 || !parks[1].RetryAt.Equal(parkedAt.Add(harness.ShortestSitOut)) {
		t.Errorf("parks = %+v, want the renewal saying the seat is tried again %s on",
			parks, harness.ShortestSitOut)
	}
}

// A PARK TAKEN WHILE A RELEASE OF THE SEAT IS LIFTING KEEPS THE SEAT HELD.
//
// A release drops its record under the parks' lock and resumes outside it; a
// park taken in between re-holds the same attachment and records itself, and
// the late resume would lift its hold, leaving the seat recorded parked with
// its inbox not held. So the release reads the record again after its resume
// (the package doc's "A lift takes back a hold taken in its window"). Shared by
// both kinds of park; the provider's is the one taken with no alarm.
//
// Mutation: drop the read after the resume, and the seat is left recorded
// parked with its inbox unheld.
func TestAParkTakenAsItsSeatsReleaseLiftsKeepsTheSeatHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	seat := seatOf("ceo")
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.providerParks.take(ctx, e, providerParkSpec, seat, "ceo", providerBasis{},
		time.Time{}, "every model is limited"); err != nil {
		t.Fatalf("take: %v", err)
	}

	staged := &foldAtResume{EventQueue: q, subject: subject, hold: providerParkSpec.hold,
		fold: func() {
			if err := e.providerParks.take(ctx, e, providerParkSpec, seat, "ceo",
				providerBasis{}, time.Time{}, "every model is limited again"); err != nil {
				t.Errorf("the park in the release's window: %v", err)
			}
		}}
	e.backends.Queue = staged
	if !e.providerParks.release(ctx, e, providerParkSpec, seat, "a probe was answered") {
		t.Fatal("the release lifted nothing")
	}

	if !staged.fired.Load() {
		t.Fatal("the release lifted no park hold, so nothing was staged in its window")
	}
	parking := e.providerParks.parking(seat)
	if parking == nil {
		t.Fatal("the park taken in the release's window is not recorded")
	}
	if !slices.Contains(q.(*memory.Queue).PauseHolds(subject, group), providerParkSpec.hold) {
		t.Errorf("the seat is recorded parked with its inbox unheld %v: the release's late "+
			"resume took the hold the park in its window owns",
			q.(*memory.Queue).PauseHolds(subject, group))
	}
	if parking.inbox != e.inboxOf(seat) {
		t.Errorf("the park is recorded for attachment %d, want %d, which its release lifts",
			parking.inbox, e.inboxOf(seat))
	}
}

// A PARK TAKEN IN A RELEASE'S WINDOW KEEPS ITS WAIT.
//
// A release marks the seat's wait released, or a probe's answer ends it, once
// its resume has returned, and a turn that meets the limit again in between
// parks the seat on that record first. Landed on the standing park's record,
// the late end or mark made a later re-park begin a new wait, so a seat that
// had waited throughout read as newly waiting and `llm_limit_waiting`, judged
// at fifteen minutes, went out (the package doc's "A provider park's release
// and its wait").
//
// Mutation: let either the release's mark or the probe's end land on the
// record of a park that stands, and its subtest fails.
func TestAParkTakenInAReleasesWindowKeepsItsWait(t *testing.T) {
	t.Parallel()
	for _, answered := range []bool{true, false} {
		name := "a probe's answer ends the wait under a park in its window"
		if !answered {
			name = "a release marks the wait released under a park in its window"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			reset := parkedAt.Add(time.Hour)
			model := &limitedModel{reset: reset}
			r := newProbeRig(t, model)
			model.now = r.clock
			e, q := r.e, r.q
			lead, err := e.seatID("lead")
			if err != nil {
				t.Fatalf("seatID: %v", err)
			}
			r.park(t, "lead", reset)

			since := func(key string) time.Time {
				t.Helper()
				for _, row := range e.parkedByEntry()[key] {
					if row.Seat == lead {
						return row.Since
					}
				}
				t.Fatalf("the seat is not waiting on %s: %+v", key, e.parkedByEntry())
				return time.Time{}
			}
			staged := &foldAtResume{EventQueue: q, subject: topics.AgentInbox(lead),
				hold: pauseReasonProvider, fold: func() { r.park(t, "lead", reset.Add(time.Hour)) }}
			e.backends.Queue = staged
			released := reset.Add(time.Second)
			r.at(released)
			if answered {
				live := r.alarms.live()
				if len(live) != 1 {
					t.Fatalf("%d alarms armed for one seat's park, want its probe's one", len(live))
				}
				live[0].fire()
			} else if !e.releaseProviderPark(ctx, lead, "the seat's models changed") {
				t.Fatal("the release lifted nothing")
			}
			if !staged.fired.Load() {
				t.Fatal("the release lifted no park hold, so nothing was staged in its window")
			}
			if e.providerParks.parking(lead) == nil {
				t.Fatal("the park taken in the release's window is not recorded")
			}
			if got := since("default"); !got.Equal(parkedAt) {
				t.Fatalf("the park in the release's window waits from %v, want %v, the wait it "+
					"carried", got, parkedAt)
			}

			if answered {
				// THE WINDOW PARK'S OWN RELEASE, by an apply, and its
				// re-park within the carry window.
				r.at(released.Add(time.Minute))
				if !e.releaseProviderPark(ctx, lead, "the seat's models changed") {
					t.Fatal("the window park's release lifted nothing")
				}
				r.at(released.Add(time.Minute + parkCarryWindow/2))
				// AND BETWEEN THE TWO, while a park would continue the
				// wait, the heartbeat reports it.
				if got := since("default"); !got.Equal(parkedAt) {
					t.Errorf("between the window park's release and its re-park the seat "+
						"waits from %v, want %v", got, parkedAt)
				}
				r.park(t, "lead", reset.Add(2*time.Hour))
				if got := since("default"); !got.Equal(parkedAt) {
					t.Errorf("the re-park within %v of the window park's release waits from "+
						"%v, want %v: the probe's answer ended the wait that park stood on",
						parkCarryWindow, got, parkedAt)
				}
				return
			}
			// THE STANDING SEAT PARKED ON ANOTHER CHAIN, longer after the
			// release than a park would carry a released wait.
			r.at(released.Add(2 * parkCarryWindow))
			r.parkOn(t, "lead", "spare", reset.Add(2*time.Hour))
			if got := since("spare"); !got.Equal(parkedAt) {
				t.Errorf("the standing seat's park on another chain waits from %v, want %v: "+
					"the release marked released the wait the park in its window stood on",
					got, parkedAt)
			}
		})
	}
}

// A PROBE'S ANSWER ENDS THE WAIT OF EVERY SEAT IT RELEASES.
//
// The answer releases each seat's park and then ends its wait, which a park of
// the seat that stands keeps ([Engine.unlessParked]). Ended before the
// release, the wait would meet the very park being released and be kept, and
// the release would only mark it released, so a seat that met a new limit
// within [parkCarryWindow] of the answer read as waiting since before its
// models answered.
//
// Mutation: end the wait before the release, and this fails.
func TestAProbesAnswerEndsTheWaitOfEverySeatItReleases(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(time.Hour)
	model := &limitedModel{reset: reset}
	r := newProbeRig(t, model)
	model.now = r.clock
	lead, err := r.e.seatID("lead")
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	r.park(t, "lead", reset)
	answered := reset.Add(time.Second)
	r.at(answered)
	live := r.alarms.live()
	if len(live) != 1 {
		t.Fatalf("%d alarms armed for one seat's park, want its probe's one", len(live))
	}
	live[0].fire()
	if r.e.providerParks.parking(lead) != nil {
		t.Fatal("the probe's answer left the seat parked")
	}

	again := answered.Add(parkCarryWindow / 2)
	r.at(again)
	r.park(t, "lead", again.Add(time.Hour))
	rows := 0
	var since time.Time
	for _, row := range r.e.parkedByEntry()["default"] {
		if row.Seat == lead {
			rows++
			since = row.Since
		}
	}
	if rows != 1 || !since.Equal(again) {
		t.Errorf("the seat that met a new limit %v after its models answered waits from %v "+
			"(%d rows), want one new wait from %v", parkCarryWindow/2, since, rows, again)
	}
}

// A SEAT'S RELEASE FORGETS ITS WAIT, PARKED OR NOT.
//
// A release that finds a park standing leaves the seat's wait record to it
// ([Engine.unlessParked]), but the seat's release by this node forgets the
// record before it forgets the park, and a wait is this node's only while it
// holds the seat. Left to the park it was about to forget, the record outlived
// the seat, and a park after the node took the seat again carried a start from
// before it let go.
//
// Mutation: forget the wait on the seat's release only where no park stands,
// and this fails.
func TestASeatsReleaseForgetsItsWaitUnderItsPark(t *testing.T) {
	t.Parallel()
	reset := parkedAt.Add(time.Hour)
	model := &limitedModel{reset: reset}
	r := newProbeRig(t, model)
	model.now = r.clock
	lead, err := r.e.seatID("lead")
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	r.park(t, "lead", reset)
	r.e.forgetProviderPark(t.Context(), lead)

	again := parkedAt.Add(10 * time.Second)
	r.at(again)
	r.park(t, "lead", reset)
	for _, row := range r.e.parkedByEntry()["default"] {
		if row.Seat == lead && !row.Since.Equal(again) {
			t.Errorf("the seat parked after its release waits from %v, want a new wait at %v: "+
				"the release left its wait to the park it forgot", row.Since, again)
		}
	}
}

// A RELEASE THAT TOOK A PARK'S HOLD BACK SAYS THE INBOX STAYS HELD.
//
// A park taken while a release was in flight holds the seat again once the
// release's resume returns ([seatParks.reholdParked]), and that park logged its
// own line before the release's. The release's line (`seat_budget_park_released`,
// `seat_provider_park_released`: one release serves both) then read as the
// seat's last word, a seat whose mail flows again, while its inbox was held.
// So the line says what the release's re-read found.
//
// NOT PARALLEL: it reads the process's log, which [logging.Configure] owns.
//
// Mutation: log the release without what its re-read found, and this fails.
func TestAReleaseThatTookAParksHoldBackSaysTheInboxStaysHeld(t *testing.T) {
	ctx := t.Context()
	lines := processLog(t)
	e, q := engineOn(t)
	seat := seatOf("ceo")
	park := func(why string) {
		t.Helper()
		if err := e.providerParks.take(ctx, e, providerParkSpec, seat, "ceo", providerBasis{},
			time.Time{}, why); err != nil {
			t.Errorf("take: %v", err)
		}
	}
	park("every model is limited")
	if !e.providerParks.release(ctx, e, providerParkSpec, seat, "a probe was answered") {
		t.Fatal("the plain release lifted nothing")
	}
	park("every model is limited")

	staged := &foldAtResume{EventQueue: q, subject: topics.AgentInbox(seat),
		hold: providerParkSpec.hold, fold: func() { park("every model is limited again") }}
	e.backends.Queue = staged
	if !e.providerParks.release(ctx, e, providerParkSpec, seat, "a probe was answered") {
		t.Fatal("the release lifted nothing")
	}
	if !staged.fired.Load() {
		t.Fatal("the release lifted no park hold, so nothing was staged in its window")
	}

	saysHeldAgain(t, lines, providerParkSpec.released, "agent_id", seat.String())
}

// A PARK ON ITS CHAIN TAKEN AS THE PROBE'S RELEASE LIFTS IS LEFT WHOLE: HELD,
// PROBED AND ANNOUNCED.
//
// The park is taken in the release's window, before its resume reaches the
// queue (a turn on another partition) or once it has (the resumed mail, which
// the in-memory twin hands over inside the resume). So the release's own
// effects on the seat are decided with its record's drop, before any later
// park can be taken (the package doc's "A provider park's release and its
// wait").
//
// Mutation: take the seat off its probe after the resume, and the park is left
// on no probe; announce the release after it, and the seat is drawn released.
func TestAParkOnItsChainTakenAsItsProbesReleaseLiftsIsLeftWhole(t *testing.T) {
	t.Parallel()
	for _, late := range []bool{false, true} {
		name := "before the resume reaches the queue"
		if late {
			name = "once the resume has reached the queue"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reset := parkedAt.Add(time.Hour)
			model := &limitedModel{reset: reset}
			r := newProbeRig(t, model)
			model.now = r.clock
			e, q := r.e, r.q
			lead, err := e.seatID("lead")
			if err != nil {
				t.Fatalf("seatID: %v", err)
			}
			subject, group := topics.AgentInbox(lead), topics.AgentInboxGroup(lead)
			r.park(t, "lead", reset)

			staged := &foldAtResume{EventQueue: q, subject: subject, hold: pauseReasonProvider,
				late: late, fold: func() { r.park(t, "lead", reset.Add(time.Hour)) }}
			e.backends.Queue = staged
			r.at(reset.Add(time.Second))
			live := r.alarms.live()
			if len(live) != 1 {
				t.Fatalf("%d alarms armed for one seat's park, want its probe's one", len(live))
			}
			live[0].fire()

			if !staged.fired.Load() {
				t.Fatal("the probe's answer lifted no park hold, so nothing was staged in its window")
			}
			parking := e.providerParks.parking(lead)
			if parking == nil {
				t.Fatal("the park taken in the release's window is not recorded")
			}
			if !slices.Contains(q.PauseHolds(subject, group), pauseReasonProvider) {
				t.Errorf("the seat is recorded parked with its inbox unheld %v",
					q.PauseHolds(subject, group))
			}
			e.limitProbes.mu.Lock()
			probe := e.limitProbes.probes[probeKeyOf(parking.basis)]
			probed := probe != nil && probe.seats[lead] != ""
			e.limitProbes.mu.Unlock()
			if !probed || len(r.alarms.live()) != 1 {
				t.Errorf("the seat is parked on no probe (on one: %v, alarms armed: %d): the "+
					"release took it off the probe its new park joined, and nothing releases it",
					probed, len(r.alarms.live()))
			}
			e.parkNotices.mu.Lock()
			last := string(e.parkNotices.sent["lead"])
			e.parkNotices.mu.Unlock()
			if !strings.HasPrefix(last, `{"type":"seat_provider_parked"`) {
				t.Errorf("the last notice published for the seat is %s, want its new park: the "+
					"release was announced after the park that followed it", last)
			}
		})
	}
}
