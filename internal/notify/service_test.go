package notify_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// trackerParser is the vendor half of the seam: it declares a source and
// turns a delivery into recipients. Everything the service does around that
// — guards, valve, prompt, wake — is what these tests are about.
type trackerParser struct {
	out []notify.Routed
	err error
	// saw records the registry it was handed, so the "a parser gets the
	// live registry" case can assert it.
	mu  sync.Mutex
	saw *notify.Registry
}

func (*trackerParser) Source() string { return "tracker" }

func (p *trackerParser) Parse(_ context.Context, _ types.RawWebhook, r *notify.Registry) ([]notify.Routed, error) {
	p.mu.Lock()
	p.saw = r
	p.mu.Unlock()
	return p.out, p.err
}

func to(rec notify.Recipient, body string) notify.Routed {
	return notify.Routed{
		Inbound: notify.Inbound{
			Source: "tracker", EventType: "comment", Sender: "ana",
			Subject: "ENG-42", Body: body,
			Metadata: map[string]string{"issue_id": "u-1", "event_type": "comment"},
		},
		To: rec,
	}
}

type countingValve struct {
	mu    sync.Mutex
	calls int
	allow bool
	err   error
}

func (v *countingValve) Allow(context.Context, string, int, time.Time) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return v.allow, v.err
}

func (v *countingValve) seen() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// harness is a service on a live in-memory broker, with a collector on
// every topic it can publish to.
//
// The collectors attach BEFORE anything is handled, because a subscription
// created after a publish starts empty — the broker retains mail per
// subscription, not per topic. That is the broker being right, and it is
// exactly the fleet property the service depends on: a seat's mail waits for
// the node that owns the seat.
type harness struct {
	svc    *notify.Service
	q      *memory.Queue
	reg    *notify.Registry
	parser *trackerParser
	valve  *countingValve
	limit  int
	admits bool

	mu   sync.Mutex
	seen map[string][]*events.Event
}

func (h *harness) collect(t *testing.T, topic string) {
	t.Helper()
	err := h.q.Subscribe(t.Context(), topic, "collect-"+topic,
		func(_ context.Context, ev *events.Event) queue.Result {
			h.mu.Lock()
			h.seen[topic] = append(h.seen[topic], ev)
			h.mu.Unlock()
			return queue.Ack()
		})
	if err != nil {
		t.Fatalf("collect %s: %v", topic, err)
	}
}

// settled waits for the broker to hand over what has been published, then
// returns what landed on a topic.
func (h *harness) settled(t *testing.T, topic string) []*events.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		got := append([]*events.Event(nil), h.seen[topic]...)
		h.mu.Unlock()
		if len(got) > 0 || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) inbox(t *testing.T, handle string) []*events.Event {
	t.Helper()
	return h.settled(t, topics.AgentInbox(handle))
}

// quiet asserts a topic stays empty, which needs a real wait: "nothing
// arrived" and "nothing has arrived YET" are the same observation until
// enough time has passed.
func (h *harness) quiet(t *testing.T, topic string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.seen[topic]; len(got) != 0 {
		t.Fatalf("%s carried %d events, want none", topic, len(got))
	}
}

func (h *harness) skips(t *testing.T) []types.NotificationSkipped {
	t.Helper()
	var out []types.NotificationSkipped
	for _, ev := range h.settled(t, skipTopic) {
		if s, ok := events.DataAs[*types.NotificationSkipped](ev); ok {
			out = append(out, *s)
		}
	}
	return out
}

var skipTopic = topics.Event(types.NotificationSkipped{}.EventType())

// seats every test's collectors cover: the fixture company's three, plus the
// one a swap adds and the skip stream.
var collected = []string{"engineering-lead", "backend-engineer", "dana-founder", "new-seat"}

func newService(t *testing.T, mutate func(*notify.Options, *harness)) *harness {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start queue: %v", err)
	}
	h := &harness{
		q:      q,
		reg:    registry(t),
		parser: &trackerParser{},
		valve:  &countingValve{allow: true},
		admits: true,
		seen:   map[string][]*events.Event{},
	}
	for _, handle := range collected {
		h.collect(t, topics.AgentInbox(handle))
	}
	h.collect(t, skipTopic)

	opts := notify.Options{
		Queue:     h.q,
		Registry:  func() *notify.Registry { return h.reg },
		Prompts:   prompts(),
		Parsers:   []notify.Parser{h.parser},
		Valve:     h.valve,
		RateLimit: func() int { return h.limit },
		Admits:    func() bool { return h.admits },
		Now:       func() time.Time { return t0 },
	}
	if mutate != nil {
		mutate(&opts, h)
	}
	svc, err := notify.New(opts)
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	h.svc = svc
	return h
}

func delivery(source string) *events.Event {
	ev := events.New(types.RawWebhook{
		Body: map[string]any{"issue": "ENG-42"}, Headers: map[string]string{},
	}, events.NewTrace())
	ev.Source = source
	return ev
}

func TestADeliveryWakesTheSeatItNames(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "please look")}

	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}

	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 1 {
		t.Fatalf("the seat was woken %d times", len(woken))
	}
	n, ok := events.DataAs[*types.ExternalNotification](woken[0])
	if !ok {
		t.Fatalf("the wake carries %T", woken[0].Data)
	}
	lead, _ := h.reg.ByHandle("engineering-lead")
	if n.Agent != lead.AgentID.String() {
		t.Fatalf("the wake names agent %q, want %q", n.Agent, lead.AgentID)
	}
	if n.NotificationSource != "tracker" || n.Sender != "ana" || n.Subject != "ENG-42" {
		t.Fatalf("the wake reads %+v", n)
	}
	// The SALIENT body is the raw message; Body is the vendor's rendered
	// trigger. A worker filtering on the salient text must not be handed
	// scaffolding.
	if n.SalientBody == nil || *n.SalientBody != "please look" {
		t.Fatalf("salient body = %v", n.SalientBody)
	}
	if !strings.Contains(n.Body, "How to triage") || !strings.Contains(n.Body, "please look") {
		t.Fatalf("the rendered trigger is %q", n.Body)
	}
	// A webhook naming a thing-that-changed is a POINTER: the Plan-phase
	// relevance filters skip their auxiliary model call, because filtering
	// against a bare pointer is near-guaranteed to be worth nothing.
	if !n.ContextRequiresRecon {
		t.Fatal("a pointer trigger did not ask for recon")
	}
	// The vendor's conversation key rides along so the inbox coalescer
	// partitions without re-deriving the vendor's rule.
	if got := n.Metadata[notify.KeyField]; got != "tracker:u-1" {
		t.Fatalf("conversation key = %q", got)
	}
	// And the trace is the webhook's, so a delivery and the turn it wakes
	// are one story.
	if woken[0].TraceID != "" && woken[0].TraceID == events.NewTrace().TraceID {
		t.Fatal("the wake minted a fresh trace")
	}
}

// THE ENVELOPE CARRIES THE CONVERSATION KEY, not only the payload's metadata.
//
// The regression this exists for, and why the assertion above was not enough:
// the key was written ONLY into the notification's Metadata map, which travels
// inside the typed payload. The inbox partitions on the ENVELOPE's own bag —
// node.conversationKey and notify.KeyOf both read ev.Payload — so every wake
// fell back to a key of its own event id, every partition was a singleton, and
// ten comments on one thread woke a seat ten times and ran ten turns instead
// of the one digest turn the design describes. Both copies are load-bearing:
// the metadata one is what a prompt renders from, this one is what the broker
// groups on.
func TestTheWakeEnvelopeCarriesTheConversationKey(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "please look")}

	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}

	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 1 {
		t.Fatalf("the seat was woken %d times", len(woken))
	}
	if got := notify.KeyOf(woken[0]); got != "tracker:u-1" {
		t.Errorf("the partition function reads %q, want the vendor's key", got)
	}
}

// TWO DELIVERIES IN ONE CONVERSATION PARTITION TOGETHER, which is the whole
// point of carrying the key: the inbox groups by it, so a busy thread becomes
// one digest turn rather than one turn per message.
func TestTwoDeliveriesInOneConversationShareAPartition(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "first")}
	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "second")}
	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}

	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 2 {
		t.Fatalf("the seat was woken %d times, want twice", len(woken))
	}
	first, second := notify.KeyOf(woken[0]), notify.KeyOf(woken[1])
	if first != second {
		t.Errorf("two events in one conversation keyed %q and %q", first, second)
	}
	if !notify.Derived(first) {
		t.Errorf("key %q is the per-event fallback, so nothing can coalesce", first)
	}
}

// Most webhooks concern nobody here. Recording a skip for each would bury
// the ones that matter.
// And a self-contained trigger does NOT ask for recon: the vendor decides
// per event type, and a service that answered for it would send every seat
// looking behind a message that is already the whole context.
func TestASelfContainedTriggerDoesNotAskForRecon(t *testing.T) {
	h := newService(t, nil)
	r := to(notify.Recipient{Handle: "engineering-lead"}, "the whole thing")
	r.EventType = "message"
	h.parser.out = []notify.Routed{r}

	h.svc.Handle(t.Context(), delivery("tracker"))
	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 1 {
		t.Fatalf("the seat was woken %d times", len(woken))
	}
	n, _ := events.DataAs[*types.ExternalNotification](woken[0])
	if n.ContextRequiresRecon {
		t.Fatal("a self-contained trigger asked for recon")
	}
}

func TestADeliveryThatConcernsNobodyIsQuiet(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = nil

	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v", got)
	}
	h.quiet(t, skipTopic)
}

// "Nobody parses this source" and "nothing happened" are opposite facts, and
// only the first tells an operator their integration is wired at the edge
// and nowhere else.
func TestAnUnparsedSourceIsRecorded(t *testing.T) {
	h := newService(t, nil)

	got := h.svc.Handle(t.Context(), delivery("mystery"))
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack — a redelivery finds no parser either", got)
	}
	skips := h.skips(t)
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "parser") {
		t.Fatalf("skips = %+v", skips)
	}
	if skips[0].NotificationSource != "mystery" {
		t.Fatalf("the skip names source %q", skips[0].NotificationSource)
	}
}

func TestARecipientNobodyMatchesIsRecorded(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{
		to(notify.Recipient{Handle: "nobody-here", Email: "who@example.com"}, "hello"),
	}

	h.svc.Handle(t.Context(), delivery("tracker"))
	skips := h.skips(t)
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "no seat") {
		t.Fatalf("skips = %+v", skips)
	}
}

// Handle, role, email, then the vendor's own ids — most specific first,
// because each later form is a guess the earlier one did not need to make.
func TestTheRecipientCascadeTriesEveryForm(t *testing.T) {
	h := newService(t, nil)
	if err := h.reg.Register("tracker", "acct-cto", "backend-engineer"); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, c := range []struct {
		name   string
		to     notify.Recipient
		handle string
	}{
		{"handle", notify.Recipient{Handle: "engineering-lead"}, "engineering-lead"},
		{"role", notify.Recipient{Role: "Backend Engineer"}, "backend-engineer"},
		{"email", notify.Recipient{Email: "lead@example.com"}, "engineering-lead"},
		{"plus-address", notify.Recipient{Email: "notif+backend-engineer@x.com"}, "backend-engineer"},
		{"external id", notify.Recipient{ExternalIDs: []string{"acct-cto"}}, "backend-engineer"},
		// Several candidates, only one of which names a colleague.
		{"several ids", notify.Recipient{ExternalIDs: []string{"nobody", "acct-cto"}}, "backend-engineer"},
		// A handle wins over a contradicting external id.
		{"handle beats id", notify.Recipient{
			Handle: "engineering-lead", ExternalIDs: []string{"acct-cto"},
		}, "engineering-lead"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newService(t, nil)
			if err := h.reg.Register("tracker", "acct-cto", "backend-engineer"); err != nil {
				t.Fatalf("register: %v", err)
			}
			h.parser.out = []notify.Routed{to(c.to, "hello")}
			h.svc.Handle(t.Context(), delivery("tracker"))
			if woken := h.inbox(t, c.handle); len(woken) != 1 {
				t.Fatalf("%s resolved to %d wakes for %s", c.name, len(woken), c.handle)
			}
		})
	}
}

// A comment naming three colleagues is three notifications.
func TestOneDeliveryCanWakeSeveralSeats(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{
		to(notify.Recipient{Handle: "engineering-lead"}, "you too"),
		to(notify.Recipient{Handle: "backend-engineer"}, "and you"),
	}

	h.svc.Handle(t.Context(), delivery("tracker"))
	for _, handle := range []string{"engineering-lead", "backend-engineer"} {
		if woken := h.inbox(t, handle); len(woken) != 1 {
			t.Fatalf("%s was woken %d times", handle, len(woken))
		}
	}
}

// The self-action guard, reached through the service.
func TestTheServiceRefusesToWakeASeatForItsOwnAction(t *testing.T) {
	h := newService(t, nil)
	if err := h.reg.Register("tracker", "acct-lead", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r := to(notify.Recipient{Handle: "engineering-lead"}, "my own comment")
	r.Metadata[notify.ActorField] = "acct-lead"
	h.parser.out = []notify.Routed{r}

	h.svc.Handle(t.Context(), delivery("tracker"))
	h.quiet(t, topics.AgentInbox("engineering-lead"))
	skips := h.skips(t)
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "self-action") {
		t.Fatalf("skips = %+v", skips)
	}
}

// A human seat is addressable and never woken: a person reads the surface
// the event arrived on.
func TestAHumanRecipientIsNotWoken(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "dana-founder"}, "for you")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	h.quiet(t, topics.AgentInbox("dana-founder"))
	if skips := h.skips(t); len(skips) != 1 || !strings.Contains(skips[0].Reason, "human") {
		t.Fatalf("skips = %+v", skips)
	}
}

// Off by default: a company that never asked for a rate limit never reads
// the counter.
func TestTheValveIsNotConsultedWhenItIsOff(t *testing.T) {
	h := newService(t, nil)
	h.limit = 0
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "hi")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	if h.valve.seen() != 0 {
		t.Fatalf("a disabled valve was consulted %d times", h.valve.seen())
	}
	if woken := h.inbox(t, "engineering-lead"); len(woken) != 1 {
		t.Fatal("the seat was not woken")
	}
}

func TestARateLimitedSeatIsNotWoken(t *testing.T) {
	h := newService(t, nil)
	h.limit = 5
	h.valve.allow = false
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "hi")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	if h.valve.seen() != 1 {
		t.Fatalf("the valve was consulted %d times", h.valve.seen())
	}
	h.quiet(t, topics.AgentInbox("engineering-lead"))
	if skips := h.skips(t); len(skips) != 1 || !strings.Contains(skips[0].Reason, "rate limit") {
		t.Fatalf("skips = %+v", skips)
	}
}

// The valve FAILS OPEN. A limiter that cannot be reached must not stop real
// notifications — it is a valve, not a gate.
//
// FALSE BESIDE THE ERROR, which is the shape the only production valve
// actually returns: a counter that cannot read its bucket cannot report a
// count, so every one of its error paths comes back paired with false. This
// case used to pass true, which no implementation produces — so it exercised a
// branch nothing reaches, and the service dropped every inbound notification
// for every seat for as long as the coordination store was unreachable while
// this stayed green.
func TestAnUnreachableValveStillDelivers(t *testing.T) {
	h := newService(t, nil)
	h.limit = 5
	h.valve.allow, h.valve.err = false, errors.New("counter unreachable")
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "hi")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	if woken := h.inbox(t, "engineering-lead"); len(woken) != 1 {
		t.Fatal("an unreachable valve stopped a real notification")
	}
}

// A shedding node DEFERS: leaves the delivery unacked and stops consuming.
// A NAK dead-letters a healthy delivery after a few one-second redeliveries
// while a shed can last minutes.
func TestAsheddingNodeDefersRatherThanNaks(t *testing.T) {
	h := newService(t, nil)
	h.admits = false
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "hi")}

	got := h.svc.Handle(t.Context(), delivery("tracker"))
	if got.Outcome != queue.OutcomeDefer {
		t.Fatalf("Handle = %+v, want a deferral", got)
	}
	if got.Reason == "" {
		t.Fatal("the deferral carries no reason, which is its only record")
	}
	h.quiet(t, topics.AgentInbox("engineering-lead"))
}

// A payload this node cannot read will not become readable on a redelivery,
// so it is acked rather than naked — retrying burns the redelivery budget
// and dead-letters it anyway, just later and noisier.
func TestAnUnreadablePayloadIsNotRetried(t *testing.T) {
	h := newService(t, nil)
	ev := events.New(types.MessageSent{Channel: "C1"}, events.NewTrace())
	ev.Source = "tracker"

	if got := h.svc.Handle(t.Context(), ev); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}
	if skips := h.skips(t); len(skips) != 1 || !strings.Contains(skips[0].Reason, "unreadable") {
		t.Fatalf("skips = %+v", skips)
	}
}

// A malformed payload is the parser's problem and will be malformed again on
// a redelivery.
func TestAParseFailureIsRecordedAndNotRetried(t *testing.T) {
	h := newService(t, nil)
	h.parser.err = errors.New("no issue in the payload")

	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}
	if skips := h.skips(t); len(skips) != 1 || !strings.Contains(skips[0].Reason, "parse failed") {
		t.Fatalf("skips = %+v", skips)
	}
}

// The registry is read through a function, so an epoch swap is picked up: a
// service holding the old one would resolve against a company that is no
// longer running.
func TestTheServiceResolvesAgainstTheLiveRegistry(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "new-seat"}, "hi")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	h.quiet(t, topics.AgentInbox("new-seat"))

	// The apply lands: a new org, a new registry.
	o := company()
	o.Roles = append(o.Roles, &org.Role{Name: "New Seat"})
	o.Normalize()
	h.reg = notify.NewRegistry(o)

	h.svc.Handle(t.Context(), delivery("tracker"))
	if woken := h.inbox(t, "new-seat"); len(woken) != 1 {
		t.Fatalf("the swapped-in seat was woken %d times", len(woken))
	}
}

// failingPublisher makes the WAKE fail while everything else works, which
// is the only failure in this path worth a redelivery.
type failingPublisher struct {
	*memory.Queue
	boom error
}

func (p failingPublisher) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if strings.HasPrefix(topic, "crewlet.agent.") {
		return p.boom
	}
	return p.Queue.Publish(ctx, topic, ev)
}

// A publish failure means the wake never happened. The API's delivery ledger
// already holds the claim, so the provider's own retry is refused — this
// redelivery is the only second chance the wake gets.
func TestAFailedWakeIsRetried(t *testing.T) {
	boom := errors.New("broker unreachable")
	var h *harness
	h = newService(t, func(o *notify.Options, hh *harness) {
		h = hh
		o.Queue = failingPublisher{Queue: hh.q, boom: boom}
	})
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "hi")}

	got := h.svc.Handle(t.Context(), delivery("tracker"))
	if got.Outcome != queue.OutcomeNak {
		t.Fatalf("Handle = %+v, want a NAK so the wake is retried", got)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("the NAK carries %v, want the publish failure", got.Err)
	}
	h.quiet(t, topics.AgentInbox("engineering-lead"))
}

// One recipient failing must not silently swallow the others: the retry
// re-runs the whole delivery, and the skip stream is what says which of them
// had already been decided.
func TestOneFailedWakeAmongSeveralStillRetries(t *testing.T) {
	boom := errors.New("broker unreachable")
	var h *harness
	h = newService(t, func(o *notify.Options, hh *harness) {
		h = hh
		o.Queue = failingPublisher{Queue: hh.q, boom: boom}
	})
	h.parser.out = []notify.Routed{
		to(notify.Recipient{Handle: "engineering-lead"}, "you"),
		to(notify.Recipient{Handle: "dana-founder"}, "and you"),
	}

	got := h.svc.Handle(t.Context(), delivery("tracker"))
	if got.Outcome != queue.OutcomeNak {
		t.Fatalf("Handle = %+v", got)
	}
	// The human seat was decided, not attempted, so its skip still lands.
	if skips := h.skips(t); len(skips) != 1 || !strings.Contains(skips[0].Reason, "human") {
		t.Fatalf("skips = %+v", skips)
	}
}

func TestTwoParsersCannotClaimOneSource(t *testing.T) {
	_, err := notify.New(notify.Options{
		Queue:    memory.New(),
		Registry: func() *notify.Registry { return nil },
		Parsers:  []notify.Parser{&trackerParser{}, &trackerParser{}},
	})
	if err == nil {
		t.Fatal("two parsers claimed one source")
	}
	if !strings.Contains(err.Error(), "tracker") {
		t.Fatalf("the error does not name the source: %v", err)
	}
}

func TestTheServiceNeedsItsHalves(t *testing.T) {
	if _, err := notify.New(notify.Options{Registry: func() *notify.Registry { return nil }}); err == nil {
		t.Fatal("a service was built with no queue")
	}
	if _, err := notify.New(notify.Options{Queue: memory.New()}); err == nil {
		t.Fatal("a service was built with no registry")
	}
}

func TestDelegationMetadataIsSafeParsed(t *testing.T) {
	depth, parent, chain := notify.DelegationOf(map[string]string{
		"delegation_depth": "2", "parent_turn_id": "t-1",
		"delegation_chain": `["a","b"]`,
	})
	if depth != 2 || parent != "t-1" || strings.Join(chain, ",") != "a,b" {
		t.Fatalf("DelegationOf = %d, %q, %v", depth, parent, chain)
	}

	// Webhook metadata is a string of arbitrary shape. A malformed field
	// falls back rather than aborting a notification that is otherwise
	// perfectly routable.
	depth, _, chain = notify.DelegationOf(map[string]string{
		"delegation_depth": "not a number", "delegation_chain": "{not json}",
	})
	if depth != 0 || chain != nil {
		t.Fatalf("malformed metadata produced %d, %v", depth, chain)
	}

	// Falsy-but-valid entries SURVIVE: a producer encoding numeric ids
	// must not lose a 0 to a truthiness filter.
	_, _, chain = notify.DelegationOf(map[string]string{"delegation_chain": `[0,"",null,"z"]`})
	if strings.Join(chain, ",") != "0,z" {
		t.Fatalf("chain = %v, want the 0 kept and the empties dropped", chain)
	}
}

// The resolved handle is the one fact about a notification a parser cannot
// know — which seat an account id or an email belongs to is answered by the
// org, not by the payload — so the service stamps it after the cascade.
func TestTheResolvedRecipientIsStamped(t *testing.T) {
	h := newService(t, nil)
	h.parser.out = []notify.Routed{to(notify.Recipient{Email: "lead@example.com"}, "hi")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 1 {
		t.Fatalf("the seat was woken %d times", len(woken))
	}
	n, _ := events.DataAs[*types.ExternalNotification](woken[0])
	if got := n.Metadata[notify.RecipientField]; got != "engineering-lead" {
		t.Fatalf("the recipient stamp reads %q", got)
	}
}

// A parser producing several recipients from one payload may hand back ONE
// shared metadata map. Stamping into it would make every copy claim the last
// recipient — and the digest a seat then reads would name somebody else.
func TestASharedMetadataMapIsNotStampedInPlace(t *testing.T) {
	h := newService(t, nil)
	shared := map[string]string{"issue_id": "u-1", "event_type": "comment"}
	mk := func(handle string) notify.Routed {
		r := to(notify.Recipient{Handle: handle}, "hi")
		r.Metadata = shared // deliberately the same map
		return r
	}
	h.parser.out = []notify.Routed{mk("engineering-lead"), mk("backend-engineer")}

	h.svc.Handle(t.Context(), delivery("tracker"))
	for _, handle := range []string{"engineering-lead", "backend-engineer"} {
		woken := h.inbox(t, handle)
		if len(woken) != 1 {
			t.Fatalf("%s was woken %d times", handle, len(woken))
		}
		n, _ := events.DataAs[*types.ExternalNotification](woken[0])
		if got := n.Metadata[notify.RecipientField]; got != handle {
			t.Fatalf("%s received a notification stamped for %q", handle, got)
		}
	}
	// And the parser's own map is untouched, so a parser reusing one
	// across deliveries is not accumulating other seats' stamps.
	if _, stamped := shared[notify.RecipientField]; stamped {
		t.Fatal("the parser's own metadata map was written through")
	}
}
