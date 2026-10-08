package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// Gate G7's inbound half: a verified delivery becomes a woken seat, through
// the whole spine, on a real node.
//
// The vendor is a STUB PARSER rather than a stub server, and deliberately:
// what these tests are about is the path from a raw webhook to a seat's
// inbox (the guards, the valve, the prompt, the wake), which every vendor
// shares and none of them owns. Each vendor's own parsing is tested against
// its own payloads.

// stubVendor is a whole integration in ten lines: a source, a parser and a
// prompt.
type stubVendor struct {
	mu  sync.Mutex
	out []notify.Routed
}

func (*stubVendor) Source() string { return "stub" }

func (v *stubVendor) Parse(context.Context, types.RawWebhook, *notify.Registry) ([]notify.Routed, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.out, nil
}

func (v *stubVendor) says(routed ...notify.Routed) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.out = routed
}

func stubPrompt() notify.ChatPrompt {
	return notify.ChatPrompt{Backend: "stub", Label: "Stub"}
}

func routed(handle, body string, meta map[string]string) notify.Routed {
	m := map[string]string{"channel": "C1", "ts": "p1", "transport": "stub"}
	for k, v := range meta {
		m[k] = v
	}
	return notify.Routed{
		Inbound: notify.Inbound{
			Source: "stub", EventType: "message", Sender: "ana",
			Subject: "a message", Body: body, Metadata: m,
		},
		To: notify.Recipient{Handle: handle},
	}
}

// inbox collects what lands on a seat's inbox topic.
//
// THE ENVELOPES TOO, not only the decoded payloads. The broker partitions a
// seat's inbox on the envelope's own bag (see notify.Stamp / notify.KeyOf),
// and the metadata copy inside the payload is what a PROMPT renders from — so
// a partition-key assertion made against the payload copy cannot fail for
// the bug it describes, which is the key never reaching the envelope at all.
type inbox struct {
	mu        sync.Mutex
	seen      []*types.ExternalNotification
	envelopes []*events.Event
}

func watchInbox(t *testing.T, n *node, handle string) *inbox {
	t.Helper()
	box := &inbox{}
	err := n.engine.Backends().Queue.Subscribe(t.Context(),
		topics.AgentInbox(handle), "e2e-inbox-"+handle,
		func(_ context.Context, ev *events.Event) queue.Result {
			if got, ok := events.DataAs[*types.ExternalNotification](ev); ok {
				box.mu.Lock()
				box.seen = append(box.seen, got)
				box.envelopes = append(box.envelopes, ev)
				box.mu.Unlock()
			}
			return queue.Ack()
		})
	if err != nil {
		t.Fatalf("watch %s inbox: %v", handle, err)
	}
	return box
}

// settled waits for want wakes to reach the inbox, and returns every one that
// has. ON THE SUITE'S ONE BUDGET ([waitBudget]): it had five seconds of its
// own, which is the wait that fails first on a loaded runner, and a suite with
// two disagreeing timeouts fails in whichever place holds the smaller one.
//
// AT LEAST want, read the moment the last of them arrives, so it cannot see a
// wake beyond them: a case that means EXACTLY says so with [onlyWakes].
func (b *inbox) settled(t *testing.T, want int) []*types.ExternalNotification {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d wake(s) on the seat's inbox", want), func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.seen) >= want
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*types.ExternalNotification(nil), b.seen...)
}

// What the notification service records when a delivery wakes nobody
// (notification_skipped), in its own words (internal/notify): the reason is
// how a case tells WHICH gate dropped a delivery, and a gate that stopped
// dropping it, or another that started to, is a different engine. A reworded
// reason fails the case as soon as the delivery is decided, naming every reason
// that was recorded ([skip.heldAs]).
const (
	reasonHumanSeat   = "human seat"
	reasonSelfAction  = "self-action: the recipient caused this event"
	reasonUnparsed    = "no parser for this source"
	reasonRateLimited = "rate limit exceeded"
)

// storedLimit is the most rows one of these listings reads: far above the few
// dozen wakes and skips any case here produces, so a listing is the whole set
// it asks for — a short one could miss the very wake a case asserts never
// happened.
const storedLimit = 500

// storedAs is every event this node's store holds that q selects, decoded as
// P — the whole envelope read back by id, since a listing carries no payload.
func storedAs[P events.Payload](t *testing.T, n *node, q store.ListQuery) []P {
	t.Helper()
	log := n.engine.Backends().Store.Events()
	rows, err := log.List(t.Context(), q)
	if err != nil {
		t.Fatalf("list stored %s events: %v", q.Type, err)
	}
	var out []P
	for _, row := range rows {
		full, err := log.ByID(t.Context(), row.ID, time.Now())
		if err != nil {
			t.Fatalf("read stored %s %s: %v", row.Type, row.ID, err)
		}
		var ev events.Event
		if err := json.Unmarshal(full.Payload, &ev); err != nil {
			t.Fatalf("decode stored %s %s: %v", row.Type, row.ID, err)
		}
		if data, ok := events.DataAs[P](&ev); ok {
			out = append(out, data)
		}
	}
	return out
}

// wakesFor is every wake the notification service published to handle's
// inbox on this node.
//
// FROM THE EVENT STORE, which the publishing goroutine writes before its
// publish returns, rather than from an inbox subscription, which receives it
// some time later: once a delivery's own record says it was handled, every
// wake it caused is already here, so a read made then is a whole answer and
// not a race with a consumer.
func wakesFor(t *testing.T, n *node, handle string) []*types.ExternalNotification {
	t.Helper()
	var out []*types.ExternalNotification
	for _, wake := range storedAs[*types.ExternalNotification](t, n, store.ListQuery{
		Type: types.ExternalNotification{}.EventType(), Limit: storedLimit,
	}) {
		if wake.Metadata[notify.RecipientField] == handle {
			out = append(out, wake)
		}
	}
	return out
}

// skipsFrom is every notification_skipped this node recorded for a delivery
// from source.
func skipsFrom(t *testing.T, n *node, source string) []*types.NotificationSkipped {
	t.Helper()
	return storedAs[*types.NotificationSkipped](t, n, store.ListQuery{
		Type: types.NotificationSkipped{}.EventType(), Source: "notify." + source,
		Limit: storedLimit,
	})
}

// skip is the notification_skipped a case expects a delivery to end in: from
// source, naming handle ("" for a delivery dropped before anybody was
// resolved), for reason.
type skip struct{ source, handle, reason string }

// reasons is every reason n recorded for skipping a delivery from want's source
// that names want's handle — WHATEVER THE REASON, so a wait on it ends the
// moment the delivery is decided, and a reason other than want's fails the case
// at once by name. Matched on the reason as well, a reason reworded in
// internal/notify, or a different gate dropping the delivery, waited out the
// whole budget and then reported only a timeout.
func (want skip) reasons(t *testing.T, n *node) []string {
	t.Helper()
	var out []string
	for _, got := range skipsFrom(t, n, want.source) {
		if got.Handle == want.handle {
			out = append(out, got.Reason)
		}
	}
	return out
}

func (want skip) String() string {
	return fmt.Sprintf("a %s delivery skipped for %q (%q)", want.source, want.handle, want.reason)
}

// skipsRecorded renders every skip n recorded from source, for a failure.
func skipsRecorded(t *testing.T, n *node, source string) string {
	t.Helper()
	var recorded []string
	for _, got := range skipsFrom(t, n, source) {
		recorded = append(recorded, fmt.Sprintf("%q (%q)", got.Handle, got.Reason))
	}
	return fmt.Sprintf("skips recorded from %s: %v", source, recorded)
}

// heldAs holds that the delivery want describes was skipped for want's reason,
// once its decision is recorded.
func (want skip) heldAs(t *testing.T, n *node) {
	t.Helper()
	if reasons := want.reasons(t, n); !slices.Contains(reasons, want.reason) {
		t.Fatalf("a %s delivery was skipped for %q as %q, want %q — a different gate "+
			"dropped it, or internal/notify words that gate's reason differently now",
			want.source, want.handle, reasons, want.reason)
	}
}

// dropped waits for the delivery under test to end in want — its TERMINAL
// RECORD, published only once the service has decided to wake nobody — and
// holds that it woke nobody at handle.
//
// A WAKE FOR handle ENDS THE WAIT AS SURELY AS THE RECORD DOES, since it is
// the other way that delivery can end: a gate that stopped dropping it fails
// the case at once, naming the wake, rather than at the end of the budget.
func dropped(t *testing.T, n *node, handle string, want skip) {
	t.Helper()
	waitFor(t, want.String(), func() bool {
		return len(want.reasons(t, n)) > 0 || len(wakesFor(t, n, handle)) > 0
	}, func() string { return skipsRecorded(t, n, want.source) })
	nobodyWoken(t, n, handle)
	want.heldAs(t, n)
}

// settleInbound returns once every delivery published to n's inbound edge
// before it was called has been handled.
//
// FOR A DELIVERY THAT LEAVES NO RECORD. A parser that routes a delivery to
// nobody — a green pipeline, a username no seat holds — is acked with nothing
// published at all (internal/notify: "most webhooks concern nobody here"), so
// there is no record of its own to wait for. So this publishes one more
// delivery behind it, from a source nothing parses, and waits for THAT one's
// record: the service handles one node's deliveries one at a time in the order
// they were published, which [TestTheInboundEdgeHandlesOneNodesDeliveriesInOrder]
// holds, so once the last is recorded every one before it was handled.
//
// ONE NODE ONLY. The inbound group is fleet-wide, and on a fleet the delivery
// under test and this one may be handled by different members, in either
// order.
func settleInbound(t *testing.T, n *node) {
	t.Helper()
	source := "e2e-settle-" + uuid.NewString()
	ev := events.New(types.RawWebhook{Body: map[string]any{}, Headers: map[string]string{}},
		events.NewTrace())
	ev.Source = source
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.NotificationsInbound, ev); err != nil {
		t.Fatalf("publish the settling delivery: %v", err)
	}
	settling := skip{source: source, reason: reasonUnparsed}
	waitFor(t, "the settling delivery's record: "+settling.String(), func() bool {
		return len(settling.reasons(t, n)) > 0
	}, func() string { return skipsRecorded(t, n, source) })
	settling.heldAs(t, n)
}

// nobodyWoken holds that no wake for handle was published on n — called once
// the delivery under test has reached its terminal record ([dropped],
// [settleInbound]), so a wake that came late is counted rather than missed.
// It replaced a fixed 200 ms sleep, which a seat woken 201 ms after delivery
// passed, and which grew weaker the slower the runner got.
func nobodyWoken(t *testing.T, n *node, handle string) {
	t.Helper()
	if wakes := wakesFor(t, n, handle); len(wakes) != 0 {
		t.Fatalf("%s was woken %d time(s), want none: %s", handle, len(wakes),
			describeWakes(wakes))
	}
}

// onlyWakes holds that EXACTLY want wakes reached handle on n, once every
// delivery published before the call has been handled ([settleInbound]). A
// count read the moment the first wake arrived could never see a second.
func onlyWakes(t *testing.T, n *node, handle string, want int) {
	t.Helper()
	settleInbound(t, n)
	if wakes := wakesFor(t, n, handle); len(wakes) != want {
		t.Fatalf("%s was woken %d time(s), want %d: %s", handle, len(wakes), want,
			describeWakes(wakes))
	}
}

// describeWakes names each wake by where it came from and why, for a failure
// a reader has to act on.
func describeWakes(wakes []*types.ExternalNotification) string {
	parts := make([]string, 0, len(wakes))
	for _, wake := range wakes {
		parts = append(parts, fmt.Sprintf("%s %s %q (%s)", wake.NotificationSource,
			wake.SourceEventType, wake.Subject, wake.Metadata["event_type"]))
	}
	return strings.Join(parts, "; ")
}

// deliver publishes a raw webhook the way the API's inbound edge does.
func deliver(t *testing.T, n *node) {
	t.Helper()
	ev := events.New(types.RawWebhook{
		Body: map[string]any{"message": "hello"}, Headers: map[string]string{},
	}, events.NewTrace())
	ev.Source = "stub"
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.NotificationsInbound, ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// startInbound stands a node up with the stub vendor routed.
func startInbound(t *testing.T, amend func(string) string) (*node, *stubVendor) {
	t.Helper()
	vendor := &stubVendor{}
	n := startWith(t, amend)
	if err := n.engine.RouteInbound(t.Context(),
		[]notify.Parser{vendor}, []notify.Prompt{stubPrompt()}); err != nil {
		t.Fatalf("RouteInbound: %v", err)
	}
	return n, vendor
}

func TestAVerifiedDeliveryWakesTheSeat(t *testing.T) {
	t.Parallel()
	n, vendor := startInbound(t, nil)
	box := watchInbox(t, n, "ceo")
	vendor.says(routed("ceo", "can you look at this", nil))

	deliver(t, n)

	got := box.settled(t, 1)
	if len(got) != 1 {
		t.Fatalf("the seat was woken %d times", len(got))
	}
	woken := got[0]
	// The wake names the seat by its DERIVED id, which is what lets any
	// node address a seat another node is running.
	lead, ok := n.engine.Registry().ByHandle("ceo")
	if !ok || woken.Agent != lead.AgentID.String() {
		t.Fatalf("the wake names agent %q, want %q", woken.Agent, lead.AgentID)
	}
	// The salient body is the raw message and Body is the rendered
	// trigger: a worker filtering on the salient text must not be handed
	// scaffolding.
	if woken.SalientBody == nil || *woken.SalientBody != "can you look at this" {
		t.Fatalf("salient body = %v", woken.SalientBody)
	}
	if !strings.Contains(woken.Body, "## Triage") {
		t.Fatalf("the trigger was not rendered:\n%s", woken.Body)
	}
	// The resolved recipient and both keys ride along — the first is what a
	// parser cannot know, the second is what lets the inbox coalesce
	// without re-deriving a third-party app's rule, and the third is where
	// the turn's history is filed. A top-level message in an open channel
	// is the shape where the two keys coincide: the thread it starts IS the
	// conversation.
	if got := woken.Metadata[notify.RecipientField]; got != "ceo" {
		t.Fatalf("the recipient stamp reads %q", got)
	}
	if got := woken.Metadata[notify.PartitionField]; got != "stub:C1:p1" {
		t.Fatalf("the partition key reads %q", got)
	}
	if got := woken.Metadata[notify.ConversationField]; got != "stub:C1:p1" {
		t.Fatalf("the conversation identity reads %q", got)
	}
	// AND ON THE ENVELOPE, which is the copy that actually partitions the
	// inbox. The metadata one above is what a prompt renders from; while
	// the key was written to it alone every partition fell back to the
	// event's own id, so ten comments on one thread woke a seat ten times.
	box.mu.Lock()
	envelope := box.envelopes[0]
	box.mu.Unlock()
	if got := notify.KeyOf(envelope); got != "stub:C1:p1" {
		t.Fatalf("the broker would partition this on %q", got)
	}
	if got := notify.ConversationIdentityOf(envelope); got != "stub:C1:p1" {
		t.Fatalf("the ledger would key this turn on %q", got)
	}
	// ONCE, which the read above cannot say: it returned as the first wake
	// arrived.
	onlyWakes(t, n, "ceo", 1)
}

// A human seat is addressable and never woken: a person reads the surface
// the event arrived on.
func TestAHumanRecipientIsNeverWokenEndToEnd(t *testing.T) {
	t.Parallel()
	n, vendor := startInbound(t, nil)
	vendor.says(routed("founder", "for you", nil))

	deliver(t, n)
	// DROPPED BY THE HUMAN-SEAT GATE BY NAME, which is what the old sleep
	// could not say: an absence it read was the same whichever gate made it.
	dropped(t, n, "founder", skip{"stub", "founder", reasonHumanSeat})
}

// The self-action guard, through the whole path: without it a seat assigned
// to its own issue receives a webhook for every comment it posts.
func TestASeatIsNotWokenByItsOwnActionEndToEnd(t *testing.T) {
	t.Parallel()
	n, vendor := startInbound(t, nil)
	if err := n.engine.Registry().Register("stub", "acct-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}
	vendor.says(routed("ceo", "my own comment",
		map[string]string{notify.ActorField: "acct-ceo"}))

	deliver(t, n)
	dropped(t, n, "ceo", skip{"stub", "ceo", reasonSelfAction})
}

// orderedVendor is a parser that records when each delivery's handling starts
// and ends, by the number the delivery carries, and holds the first one until
// the second starts or holdFirst passes.
type orderedVendor struct {
	second chan struct{}
	once   sync.Once

	mu  sync.Mutex
	log []string
}

// holdFirst is how long the first delivery is held, which is how long a
// consumer that handles deliveries concurrently is given to start the second
// one beside it. Such a consumer hands a fetched delivery to its handler at
// once, so it would start the second within milliseconds; a second is ample,
// and a sequential consumer pays it exactly once.
const holdFirst = time.Second

func (*orderedVendor) Source() string { return "ordered" }

func (v *orderedVendor) Parse(_ context.Context, w types.RawWebhook, _ *notify.Registry) (
	[]notify.Routed, error) {
	n := fmt.Sprint(w.Body["n"])
	v.record("start " + n)
	switch n {
	case "1":
		select {
		case <-v.second:
		case <-time.After(holdFirst):
		}
	case "2":
		v.once.Do(func() { close(v.second) })
	}
	v.record("end " + n)
	// TO NOBODY, the path that leaves no record: what the order buys is
	// exactly the settling those deliveries need ([settleInbound]).
	return nil, nil
}

func (v *orderedVendor) record(entry string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.log = append(v.log, entry)
}

func (v *orderedVendor) recorded() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.log)
}

// THE INBOUND EDGE HANDLES ONE NODE'S DELIVERIES ONE AT A TIME, IN THE ORDER
// THEY WERE PUBLISHED.
//
// Held here because the negative cases rest on it rather than because the
// queue contract promises it: a delivery a parser routes to nobody leaves no
// record, so those cases publish one more delivery behind it and wait for THAT
// one's ([settleInbound]). That proves the first was handled only if nothing is
// handled before what was published ahead of it — on a consumer that handled
// deliveries side by side, the settling one could be recorded while the one
// under test was still being decided, and every such case would be back to
// asserting an absence at an arbitrary moment.
//
// The first delivery's handling is held until the second's starts, or for
// [holdFirst]: a consumer that handled the two side by side starts the second
// inside that hold, and the log shows it before the first ended.
func TestTheInboundEdgeHandlesOneNodesDeliveriesInOrder(t *testing.T) {
	t.Parallel()
	n := start(t)
	vendor := &orderedVendor{second: make(chan struct{})}
	if err := n.engine.RouteInbound(t.Context(), []notify.Parser{vendor}, nil); err != nil {
		t.Fatalf("RouteInbound: %v", err)
	}
	for _, number := range []string{"1", "2"} {
		ev := events.New(types.RawWebhook{
			Body: map[string]any{"n": number}, Headers: map[string]string{},
		}, events.NewTrace())
		ev.Source = vendor.Source()
		if err := n.engine.Backends().Queue.Publish(t.Context(),
			topics.NotificationsInbound, ev); err != nil {
			t.Fatalf("publish delivery %s: %v", number, err)
		}
	}
	waitFor(t, "both deliveries to be handled", func() bool {
		return len(vendor.recorded()) == 4
	}, func() string { return fmt.Sprint(vendor.recorded()) })
	want := []string{"start 1", "end 1", "start 2", "end 2"}
	if got := vendor.recorded(); !slices.Equal(got, want) {
		t.Fatalf("the inbound edge handled two deliveries as %v, want %v — one at "+
			"a time, in the order they were published, which every case settling "+
			"on a later delivery's record depends on", got, want)
	}
}

// THE VALVE IS READ LIVE off the epoch: an apply that changes the cap takes
// effect on the next notification, not on the next restart.
func TestTheRateValveFollowsTheAppliedConfig(t *testing.T) {
	t.Parallel()
	n, vendor := startInbound(t, func(doc string) string {
		return strings.Replace(doc, "name: Nimbus",
			"name: Nimbus\nnotification_rate_limit: 2", 1)
	})
	vendor.says(routed("ceo", "one", nil))

	// EVERY DELIVERY IS DECIDED ONE WAY OR THE OTHER — a wake, or a skip
	// naming the valve — so the count of both is the burst's own terminal
	// record. Read at an arbitrary moment instead (it was 400 ms), a
	// delivery decided after the read made the exact count asserted below
	// off by one on a loaded runner.
	decided := func() (wakes, refused int) {
		for _, recorded := range skipsFrom(t, n, "stub") {
			if recorded.Handle == "ceo" && recorded.Reason == reasonRateLimited {
				refused++
			}
		}
		return len(wakesFor(t, n, "ceo")), refused
	}
	awaitDecided := func(want int) (wakes, refused int) {
		t.Helper()
		waitFor(t, fmt.Sprintf("%d deliveries to be woken or refused", want), func() bool {
			wakes, refused = decided()
			return wakes+refused == want
		}, func() string { return fmt.Sprintf("%d woken, %d refused", wakes, refused) })
		return wakes, refused
	}

	// TEN AGAINST A CAP OF TWO. Not three: the window is a wall clock the
	// test cannot control, so a batch that straddles a boundary gets a
	// fresh allowance and an exact count is unassertable — measured, it
	// fails about one full-suite run in four. What IS assertable is that
	// the valve BIT, which is the claim.
	const burst = 10
	for range burst {
		deliver(t, n)
	}
	underCap, _ := awaitDecided(burst)
	if underCap < 2 {
		t.Fatalf("only %d notifications passed a cap of 2", underCap)
	}
	if underCap >= burst {
		t.Fatalf("all %d notifications passed a cap of 2", underCap)
	}

	// THE APPLY: raising the cap must take effect without a restart.
	//
	// A FRESH DOCUMENT, never the live epoch's config mutated in place —
	// an epoch is published rather than mutated, and editing the pointer
	// would change what the running company reads with no apply at all,
	// which would also make this test pass against a captured cap.
	raised, err := config.ParseCompany([]byte(strings.Replace(
		fmt.Sprintf(companyDoc, n.model.url),
		"name: Nimbus", "name: Nimbus\nnotification_rate_limit: 500", 1)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	if _, _, err := n.engine.Apply(t.Context(), raised, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// NO FRESH WINDOW IS WAITED FOR. The valve reads the cap live off the
	// epoch, and its counter moves only on a delivery it lets through —
	// so the window the first burst left holds at most its own few, far
	// below the raised cap, and the second burst fits beside them.
	for range burst {
		deliver(t, n)
	}
	// EVERY ONE lands now, whichever window they fall in: the raised cap
	// is far above the burst, so a boundary crossing cannot change the
	// answer the way it could above.
	if got, refused := awaitDecided(2 * burst); got != underCap+burst {
		t.Fatalf("%d of %d landed after the cap was raised (%d had landed before; "+
			"%d refused in all)", got-underCap, burst, underCap, refused)
	}
}
