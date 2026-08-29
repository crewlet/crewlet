package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Gate G7's inbound half: a verified delivery becomes a woken seat, through
// the whole spine, on a real node.
//
// The vendor is a STUB PARSER rather than a stub server, and deliberately:
// what these tests are about is the path from a raw webhook to a seat's
// inbox — the guards, the valve, the prompt, the wake — which every vendor
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
type inbox struct {
	mu   sync.Mutex
	seen []*types.ExternalNotification
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
				box.mu.Unlock()
			}
			return queue.Ack()
		})
	if err != nil {
		t.Fatalf("watch %s inbox: %v", handle, err)
	}
	return box
}

func (b *inbox) settled(t *testing.T, want int) []*types.ExternalNotification {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.seen)
		b.mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*types.ExternalNotification(nil), b.seen...)
}

func (b *inbox) quiet(t *testing.T) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) != 0 {
		t.Fatalf("the seat was woken %d times, want none", len(b.seen))
	}
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
	// The resolved recipient and the conversation key both ride along —
	// the first is what a parser cannot know, the second is what lets the
	// inbox coalesce without re-deriving a vendor's rule.
	if got := woken.Metadata[notify.RecipientField]; got != "ceo" {
		t.Fatalf("the recipient stamp reads %q", got)
	}
	if got := woken.Metadata[notify.KeyField]; got != "stub:C1:p1" {
		t.Fatalf("the conversation key reads %q", got)
	}
}

// A human seat is addressable and never woken: a person reads the surface
// the event arrived on.
func TestAHumanRecipientIsNeverWokenEndToEnd(t *testing.T) {
	n, vendor := startInbound(t, nil)
	box := watchInbox(t, n, "founder")
	vendor.says(routed("founder", "for you", nil))

	deliver(t, n)
	box.quiet(t)
}

// The self-action guard, through the whole path: without it a seat assigned
// to its own issue receives a webhook for every comment it posts.
func TestASeatIsNotWokenByItsOwnActionEndToEnd(t *testing.T) {
	n, vendor := startInbound(t, nil)
	box := watchInbox(t, n, "ceo")
	if err := n.engine.Registry().Register("stub", "acct-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}
	vendor.says(routed("ceo", "my own comment",
		map[string]string{notify.ActorField: "acct-ceo"}))

	deliver(t, n)
	box.quiet(t)
}

// THE VALVE IS READ LIVE off the epoch: an apply that changes the cap takes
// effect on the next notification, not on the next restart.
func TestTheRateValveFollowsTheAppliedConfig(t *testing.T) {
	n, vendor := startInbound(t, func(doc string) string {
		return strings.Replace(doc, "name: Nimbus",
			"name: Nimbus\nnotification_rate_limit: 2", 1)
	})
	box := watchInbox(t, n, "ceo")
	vendor.says(routed("ceo", "one", nil))

	// TEN AGAINST A CAP OF TWO. Not three: the window is a wall clock the
	// test cannot control, so a batch that straddles a boundary gets a
	// fresh allowance and an exact count is unassertable — measured, it
	// fails about one full-suite run in four. What IS assertable is that
	// the valve BIT, which is the claim.
	const burst = 10
	for range burst {
		deliver(t, n)
	}
	time.Sleep(400 * time.Millisecond)
	underCap := len(box.settled(t, 2))
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
	if _, _, err := n.engine.Apply(t.Context(), raised); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// A fresh window, so the earlier refusals do not colour this.
	time.Sleep(1100 * time.Millisecond)
	for range burst {
		deliver(t, n)
	}
	// EVERY ONE lands now, whichever window they fall in: the raised cap
	// is far above the burst, so a boundary crossing cannot change the
	// answer the way it could above.
	if got := len(box.settled(t, underCap+burst)); got != underCap+burst {
		t.Fatalf("%d of %d landed after the cap was raised (%d had landed before)",
			got-underCap, burst, underCap)
	}
}
