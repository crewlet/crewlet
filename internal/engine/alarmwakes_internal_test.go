package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN ALARM WAKES EXACTLY THE SEATS THE HEALTH TOOLS ADMIT. The parser every
// node's inbound edge runs for the source `alarms` is built from the epoch's
// chart, and the seats it routes a transition to are the seats the authority
// table admits to the health tools — the granted one woken, the ungranted one
// neither woken nor offered them — on a node running no native half at all,
// since a transition is routed by the chart alone.
//
// Mutation: build the parser from every agent seat, or from none, or only
// where a native half runs, and the case is red.
func TestAnAlarmWakesExactlyTheSeatsTheHealthToolsAdmit(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, healthToolsCompany))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	parsers, prompts := (&Engine{}).nativeParsers(c)
	var parser notify.Parser
	for i, p := range parsers {
		if p.Source() == alarmwake.Source {
			parser = p
			if prompts[i].Source() != alarmwake.Source {
				t.Errorf("the alarms' parser is paired with the %s prompt", prompts[i].Source())
			}
		}
	}
	if parser == nil {
		t.Fatalf("a node running no native half routes %d sources and not %s", len(parsers),
			alarmwake.Source)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	w, err := alarmwake.Encode(statelog.AlarmTransition{
		ID:   statelog.TransitionID("node-a", statelog.KindApplyLag, "", statelog.TransitionFired, at),
		Node: "node-a", Kind: statelog.KindApplyLag, Change: statelog.TransitionFired, Since: at, At: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := parser.Parse(t.Context(), w, notify.NewRegistry(c.Org, nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var woken []string
	for _, r := range routed {
		woken = append(woken, r.To.Handle)
	}

	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize: builtin.Decide(orgchart.Of(c.Org)),
		Health:    builtin.HealthDeps{Leases: coordmem.New()},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var admitted []string
	for _, handle := range []string{"sre", "dev"} {
		if !slices.Contains(c.withheldFrom(t.Context(), handle, reg), builtin.FleetHealthTool) {
			admitted = append(admitted, handle)
		}
	}
	if !slices.Equal(woken, admitted) || !slices.Equal(woken, []string{"sre"}) {
		t.Errorf("an alarm wakes %v and the health tools admit %v, want both exactly sre",
			woken, admitted)
	}
}

// refusingBroker refuses its first refuse publishes, then takes every one.
type refusingBroker struct {
	mu       sync.Mutex
	refuse   int
	attempts int
	got      []*events.Event
	topics   []string
}

func (b *refusingBroker) Publish(_ context.Context, topic string, ev *events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if b.refuse > 0 {
		b.refuse--
		return errors.New("the broker is electing a leader")
	}
	b.got = append(b.got, ev)
	b.topics = append(b.topics, topic)
	return nil
}

func (b *refusingBroker) taken() ([]*events.Event, []string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.got), slices.Clone(b.topics), b.attempts
}

// byChange is every series of name in rec, summed by its change attribute.
func byChange(rec *metrics.Recorder, name string) map[string]uint64 {
	out := map[string]uint64{}
	for _, s := range rec.Read() {
		if s.Name == name {
			out[s.Attrs["change"]] += s.Total
		}
	}
	return out
}

// AN ALARM TRANSITION IS PUBLISHED UNTIL THE BROKER TAKES IT.
//
// What a node's tracker hands on — here a fire and its clear, from a real
// tracker — is published as the inbound delivery the alarm wakes are derived
// from: on the inbound topic, under source `alarms`, read back by alarmwake as
// the very transition the tracker made. A broker that refuses is retried until
// it takes the delivery, and each transition is taken ONCE and in the order it
// was made, so the clear never reaches a seat before its fire. The publishes
// the broker took are counted by change.
//
// Mutation: drop a transition on its first refused publish, or move on past
// it, and a case is red.
func TestAnAlarmTransitionIsPublishedUntilTheBrokerTakesIt(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	broker := &refusingBroker{refuse: 3}
	o := newAlarmOutbox(broker, rec)
	o.retry = func(int) time.Duration { return time.Millisecond }

	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	tracker.OnTransition("node-a", o.Add)
	lag := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 4m12s behind the log"}
	for _, alarms := range [][]statelog.Alarm{nil, {lag}, nil} {
		tracker.Observe(t.Context(), alarms)
		clock = clock.Add(statelog.AlarmInterval)
	}
	if got := o.pendingCount(); got != 2 {
		t.Fatalf("the premise: the tracker queued %d transitions, want a fire and its clear", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		o.run(ctx)
	}()
	waitUntil(t, 10*time.Second, "both transitions to be published", func() bool {
		got, _, _ := broker.taken()
		return len(got) == 2
	})
	cancel()
	<-stopped

	got, published, attempts := broker.taken()
	if attempts != 5 {
		t.Errorf("the broker was asked %d times, want 5: three refusals, then each transition once",
			attempts)
	}
	var changes []statelog.TransitionChange
	for i, ev := range got {
		if published[i] != topics.NotificationsInbound || ev.Source != alarmwake.Source {
			t.Errorf("transition %d went to %s as %q, want %s as %q", i, published[i], ev.Source,
				topics.NotificationsInbound, alarmwake.Source)
		}
		w, ok := events.DataAs[*types.RawWebhook](ev)
		if !ok || w == nil {
			t.Fatalf("transition %d is not an inbound delivery: %+v", i, ev)
		}
		tr, err := alarmwake.Decode(*w)
		if err != nil {
			t.Fatalf("transition %d does not decode: %v", i, err)
		}
		if tr.Node != "node-a" || tr.Kind != statelog.KindApplyLag || tr.ID == "" {
			t.Errorf("transition %d reads %+v, want node-a's apply_lag with its id", i, tr)
		}
		changes = append(changes, tr.Change)
	}
	if want := []statelog.TransitionChange{statelog.TransitionFired, statelog.TransitionCleared}; !slices.Equal(changes, want) {
		t.Errorf("the broker took %v, want %v: each once, in the order the tracker made them",
			changes, want)
	}
	if n := byChange(rec, metrics.AlarmTransitions); n["fired"] != 1 || n["cleared"] != 1 {
		t.Errorf("the published transitions are counted %v, want one fired and one cleared", n)
	}
	if o.pendingCount() != 0 {
		t.Errorf("%d transitions are still queued after the broker took them all", o.pendingCount())
	}
}

// A FULL ALARM OUTBOX DROPS ITS OLDEST, counted, and never the transition in
// flight: the broker may already hold that one, and counting it lost would
// say a seat was not told when it was.
//
// Mutation: drop the newest instead, or the head while it is in flight, and
// the case is red.
func TestAFullAlarmOutboxDropsItsOldestFirst(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	o := newAlarmOutbox(&refusingBroker{}, rec)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	transition := func(i int) statelog.AlarmTransition {
		since := at.Add(time.Duration(i) * time.Second)
		return statelog.AlarmTransition{
			ID:   statelog.TransitionID("node-a", statelog.KindApplyLag, "", statelog.TransitionFired, since),
			Node: "node-a", Kind: statelog.KindApplyLag, Change: statelog.TransitionFired,
			Since: since, At: since,
		}
	}
	o.Add(transition(0))
	if head, ok := o.head(); !ok || head.ID != transition(0).ID {
		t.Fatalf("the premise: the head in flight is %+v", head)
	}
	for i := 1; i <= alarmOutboxCap+1; i++ {
		o.Add(transition(i))
	}
	o.mu.Lock()
	pending := slices.Clone(o.pending)
	o.mu.Unlock()
	if len(pending) != alarmOutboxCap {
		t.Fatalf("the outbox holds %d transitions, want its bound, %d", len(pending), alarmOutboxCap)
	}
	if pending[0].ID != transition(0).ID {
		t.Errorf("the transition in flight was dropped")
	}
	if pending[1].ID != transition(3).ID || pending[len(pending)-1].ID != transition(alarmOutboxCap+1).ID {
		t.Errorf("the outbox kept %s .. %s after the head, want the oldest two behind it dropped "+
			"and the newest kept", pending[1].ID, pending[len(pending)-1].ID)
	}
	var dropped uint64
	for _, s := range rec.Read() {
		if s.Name == metrics.AlarmTransitionsDropped {
			dropped += s.Total
		}
	}
	if dropped != 2 {
		t.Errorf("the outbox counted %d dropped transitions, want 2", dropped)
	}
}
