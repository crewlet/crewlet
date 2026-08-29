package inbox_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

func ev(t *testing.T, kind string) *events.Event {
	t.Helper()
	return &events.Event{ID: uuid.New(), Type: kind}
}

func copyOf(e *events.Event) *events.Event {
	dup := *e
	return &dup
}

// healthy is a node that can do the work: every guard passes.
func healthy() inbox.Conditions {
	return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
}

func always(string) bool { return true }

func ids(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID.String())
	}
	return out
}

func TestAHealthyNodeProceeds(t *testing.T) {
	t.Parallel()
	// The control. Without it every guard assertion below passes for a
	// handler that refuses everything.
	got := inbox.Screen(healthy(), []*events.Event{ev(t, "notification")})
	if got.Action != inbox.ActionProceed {
		t.Errorf("action = %s (%s), want proceed", got.Action, got.Reason)
	}
	if len(got.Events) != 1 {
		t.Errorf("events = %d, want the one delivered", len(got.Events))
	}
}

func TestDedupeRunsBeforeEveryParkingBranch(t *testing.T) {
	t.Parallel()
	// The parking branches REPUBLISH. Deduping after them meant every park
	// pushed the duplicates back onto the topic, so copies multiplied
	// across shed and sandbox cycles instead of holding steady.
	one := ev(t, "notification")
	dup := copyOf(one)
	partition := []*events.Event{one, dup, ev(t, "notification")}

	c := healthy()
	c.AwaitingSandbox = true
	got := inbox.Screen(c, partition)
	if got.Action != inbox.ActionPark {
		t.Fatalf("action = %s, want park", got.Action)
	}
	if len(got.Events) != 2 {
		t.Errorf("parked %d events, want the 2 distinct ones — a park that "+
			"republishes duplicates multiplies them every cycle", len(got.Events))
	}
}

func TestDedupeKeepsConversationOrder(t *testing.T) {
	t.Parallel()
	// The surviving list is a CONVERSATION: one key-partition, read in
	// sequence. Set-ifying or sorting here reorders a thread.
	a, b, c := ev(t, "notification"), ev(t, "notification"), ev(t, "notification")
	got := inbox.Screen(healthy(), []*events.Event{a, b, copyOf(a), c})
	want := []string{a.ID.String(), b.ID.String(), c.ID.String()}
	for i, id := range ids(got.Events) {
		if i >= len(want) || id != want[i] {
			t.Fatalf("order = %v, want first-seen %v", ids(got.Events), want)
		}
	}
	if len(got.Events) != 3 {
		t.Errorf("events = %d, want 3", len(got.Events))
	}
}

func TestAnEmptyPartitionDrops(t *testing.T) {
	t.Parallel()
	for name, evs := range map[string][]*events.Event{
		"nothing":   nil,
		"only nils": {nil, nil},
	} {
		if got := inbox.Screen(healthy(), evs); got.Action != inbox.ActionDrop {
			t.Errorf("%s: action = %s, want drop", name, got.Action)
		}
	}
}

func TestTheGuardsFireInTheDocumentedOrder(t *testing.T) {
	t.Parallel()
	// Each case turns ON one more failure than the last, so what each
	// asserts is that its guard OUTRANKS everything below it. Reordering
	// any pair breaks exactly one of these — which is what makes the
	// sequence, and not just the individual branches, the thing under test.
	for _, tc := range []struct {
		name string
		c    inbox.Conditions
		want inbox.Action
	}{
		{"ownership outranks everything", inbox.Conditions{}, inbox.ActionDefer},
		{"no engine outranks sandbox and posture", inbox.Conditions{
			Owned: true, AwaitingSandbox: true,
		}, inbox.ActionPauseAndPark},
		{"sandbox outranks posture", inbox.Conditions{
			Owned: true, TurnEngineReady: true, AwaitingSandbox: true,
		}, inbox.ActionPark},
		{"posture is last", inbox.Conditions{
			Owned: true, TurnEngineReady: true,
		}, inbox.ActionDefer},
		{"every guard passing proceeds", inbox.Conditions{
			Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
		}, inbox.ActionProceed},
	} {
		got := inbox.Screen(tc.c, []*events.Event{ev(t, "notification")})
		if got.Action != tc.want {
			t.Errorf("%s: action = %s (%s), want %s", tc.name, got.Action, got.Reason, tc.want)
		}
	}
}

func TestSandboxOutranksPostureSoAClarificationBehavesTheSameEverywhere(t *testing.T) {
	t.Parallel()
	// Stated separately from the table because it is the one ordering with
	// a user-visible consequence rather than an internal one: a seat
	// mid-sandbox is already parked, so an answer to its clarification
	// reaching a SHEDDING node must behave exactly as it does on a healthy
	// one. Deferring it instead strands a box whose pending row is already
	// flipped.
	c := inbox.Conditions{Owned: true, TurnEngineReady: true, AwaitingSandbox: true, AdmitsTriggers: false}
	if got := inbox.Screen(c, []*events.Event{ev(t, "notification")}); got.Action != inbox.ActionPark {
		t.Errorf("action = %s, want park", got.Action)
	}
}

func TestEveryDeferRecordsThatTheConsumerStopped(t *testing.T) {
	t.Parallel()
	// Ownership freshness refuses inside an ordinary heartbeat window on a
	// perfectly healthy node — nothing detaches, nothing changes hands, so
	// nothing else would ever un-quiesce the consumer. Without the note the
	// seat goes deaf for the life of the process the first time a batch
	// lands in that window.
	for name, c := range map[string]inbox.Conditions{
		"not owned": {},
		"shedding":  {Owned: true, TurnEngineReady: true},
	} {
		got := inbox.Screen(c, []*events.Event{ev(t, "notification")})
		if got.Action != inbox.ActionDefer {
			t.Fatalf("%s: action = %s, want defer", name, got.Action)
		}
		if !got.NoteDeferred {
			t.Errorf("%s: a defer did not record that the consumer stopped", name)
		}
		if got.Reason == "" {
			t.Errorf("%s: a defer carried no reason", name)
		}
	}
	// A park is not a defer and must NOT quiesce: the topic pause and the
	// coordinator's resume already own that consumer's lifecycle.
	c := healthy()
	c.AwaitingSandbox = true
	if got := inbox.Screen(c, []*events.Event{ev(t, "notification")}); got.NoteDeferred {
		t.Error("a park asked the host to quiesce the consumer")
	}
}

func TestOnlyADeferReachesTheQueueAsADefer(t *testing.T) {
	t.Parallel()
	// A park acks only AFTER its requeue lands, which the caller sequences.
	// Mapping it to a Defer here would stop the consumer on a seat that is
	// merely busy.
	deferred := inbox.Screen(inbox.Conditions{}, []*events.Event{ev(t, "notification")})
	if deferred.Result().Outcome != queue.OutcomeDefer {
		t.Errorf("a defer mapped to %v", deferred.Result().Outcome)
	}
	c := healthy()
	c.AwaitingSandbox = true
	parked := inbox.Screen(c, []*events.Event{ev(t, "notification")})
	if parked.Result().Outcome != queue.OutcomeAck {
		t.Errorf("a park mapped to %v, want an ack", parked.Result().Outcome)
	}
}

func TestTheWorkKeyComesFromConstituentIDs(t *testing.T) {
	t.Parallel()
	// A coalesced digest is minted fresh on every merge, so a key taken
	// from it would differ on every redelivery and match nothing. Keying on
	// the constituents is what makes a redelivery recognisable.
	a, b := ev(t, "notification"), ev(t, "notification")
	first := inbox.Route([]*events.Event{a, b}, always)
	second := inbox.Route([]*events.Event{a, b}, always)
	if first.WorkKey == "" {
		t.Fatal("no work key was derived")
	}
	if first.WorkKey != second.WorkKey {
		t.Errorf("the same constituents keyed differently: %q vs %q", first.WorkKey, second.WorkKey)
	}
	if third := inbox.Route([]*events.Event{a}, always); third.WorkKey == first.WorkKey {
		t.Error("a different constituent set produced the same key")
	}
}

func TestOnlyLedgeredTypesContributeToTheKey(t *testing.T) {
	t.Parallel()
	// A type the ledger never writes cannot be looked up by it, so
	// including it produces a key matching nothing — the turn then reruns
	// on every redelivery.
	ledgered := ev(t, "notification")
	other := ev(t, "internal")
	only := func(kind string) bool { return kind == "notification" }

	with := inbox.Route([]*events.Event{ledgered, other}, only)
	without := inbox.Route([]*events.Event{ledgered}, only)
	if with.WorkKey != without.WorkKey {
		t.Errorf("a non-ledgered event changed the key: %q vs %q", with.WorkKey, without.WorkKey)
	}
	// But it still gets DISPATCHED — it is not ledgered, not irrelevant.
	if len(with.Events) != 2 {
		t.Errorf("dispatch dropped the non-ledgered event: %d events", len(with.Events))
	}
}

func TestAPartitionWithNoLedgeredEventsKeysToNothing(t *testing.T) {
	t.Parallel()
	// An empty key is the documented "a turn with no ledgerable trigger",
	// which skips the guard because there is nothing to collapse. It must
	// not be a hash of nothing that then collides with every other such
	// turn.
	never := func(string) bool { return false }
	if got := inbox.Route([]*events.Event{ev(t, "internal")}, never); got.WorkKey != "" {
		t.Errorf("work key = %q, want empty", got.WorkKey)
	}
}

func TestOnlyAMultiEventPartitionCoalesces(t *testing.T) {
	t.Parallel()
	// A single event dispatches exactly as it did before batching existed.
	if got := inbox.Route([]*events.Event{ev(t, "notification")}, always); got.Coalesce {
		t.Error("a single event was routed for coalescing")
	}
	if got := inbox.Route([]*events.Event{ev(t, "notification"), ev(t, "notification")}, always); !got.Coalesce {
		t.Error("a multi-event partition was not routed for coalescing")
	}
}

func TestTheDegradedPathRunsTheHeadAndRequeuesTheTail(t *testing.T) {
	t.Parallel()
	a, b, c := ev(t, "notification"), ev(t, "notification"), ev(t, "notification")
	head, tail, key := inbox.Degraded([]*events.Event{a, b, c}, always)

	if len(head) != 1 || head[0] != a {
		t.Errorf("head = %v, want the first event", ids(head))
	}
	if len(tail) != 2 || tail[0] != b || tail[1] != c {
		t.Errorf("tail = %v, want the rest in order", ids(tail))
	}
	// The head's key is the HEAD'S ALONE. Recording the partition's key
	// would mark the tail worked while its copies are still on the queue
	// waiting to be.
	if want := inbox.WorkKeyFor([]*events.Event{a}, always); key != want {
		t.Errorf("head key = %q, want the head's own %q", key, want)
	}
	if partition := inbox.WorkKeyFor([]*events.Event{a, b, c}, always); key == partition {
		t.Error("the head was recorded under the whole partition's key")
	}
}

func TestDegradingAnEmptyPartitionIsNotAPanic(t *testing.T) {
	t.Parallel()
	head, tail, key := inbox.Degraded(nil, always)
	if head != nil || tail != nil || key != "" {
		t.Errorf("got %v / %v / %q, want nothing", head, tail, key)
	}
}
