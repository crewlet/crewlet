package changefeed_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ---- the fixture ------------------------------------------------------ //

type capture struct {
	mu   sync.Mutex
	sent []*events.Event
	fail error
}

func (c *capture) Publish(_ context.Context, _ string, ev *events.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, ev)
	return nil
}

// first is the earliest published event, under the lock.
func (c *capture) first(t *testing.T) *events.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		t.Fatal("nothing was published")
	}
	return c.sent[0]
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *capture) breakWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

type claims struct {
	mu    sync.Mutex
	held  map[string]bool
	fail  error
	calls int
}

func newClaims() *claims { return &claims{held: map[string]bool{}} }

func (c *claims) Claim(_ context.Context, key string, _ time.Duration, _ time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.fail != nil {
		return false, c.fail
	}
	if c.held[key] {
		return false, nil
	}
	c.held[key] = true
	return true, nil
}

func (c *claims) Release(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, key)
	return nil
}

func (c *claims) isHeld(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held[key]
}

func (c *claims) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *claims) breakWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

// probe is a translator over a trivial record shape, so these tests exercise
// the feed's own rules rather than a domain's decoding.
type probe struct {
	mu   sync.Mutex
	wake bool
	err  error
	seen int
}

func newProbe() *probe { return &probe{wake: true} }

func (p *probe) Source() changefeed.Source {
	return changefeed.Source{Name: "work", Group: testGroup}
}

// testGroup is this fixture's own consumer name. Declared as a constant beside
// the translator that reads it, which is the shape every real estate has now
// that no helper derives one from a document family.
const testGroup = "crewlet-test-feed"

func (p *probe) Translate(_ context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen++
	if p.err != nil {
		return changefeed.Delivery{}, false, p.err
	}
	// THE ID INSIDE THE RECORD, not the delivery's own: the translator is
	// the only thing that can read it, which is why the claim seed is its
	// output rather than the framework's input.
	id := rec.Key
	if _, rest, ok := strings.Cut(rec.Key, "/"); ok {
		id = rest
	}
	return changefeed.Delivery{
		Body:  map[string]any{"key": rec.Key},
		ID:    id,
		Actor: "eng",
	}, p.wake, nil
}

func (p *probe) set(wake bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wake, p.err = wake, err
}

func (p *probe) translations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

func run(t *testing.T, docs *estate, pub *capture, cl *claims, tr changefeed.Translator) {
	t.Helper()
	feed, err := changefeed.New(changefeed.Options{
		Opener: docs, Publisher: pub, Claims: cl, Translator: tr,
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the feed did not stop")
		}
	})
	// The feed's consumer must exist before a change is written, or
	// DeliverNew correctly drops it.
	settle(t, func() bool { return true }, "")
	time.Sleep(20 * time.Millisecond)
}

func settle(t *testing.T, want func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if why != "" {
		t.Fatal(why)
	}
}

func writeChange(t *testing.T, docs *estate, id string) {
	t.Helper()
	docs.deliver(changefeed.Record{ID: id, Key: "item/" + id, Payload: []byte(`{}`)})
}

// ---- the cases -------------------------------------------------------- //

// A COMMITTED CHANGE BECOMES A WAKE, published by something that outlives the
// writer. That is the whole contract: a node that dies between the write and
// the publish costs a redelivery, not a lost notification.
func TestACommittedChangeBecomesAWake(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	run(t, docs, pub, cl, newProbe())

	writeChange(t, docs, "u1")
	settle(t, func() bool { return pub.count() == 1 }, "the change never became a wake")

	got := pub.first(t)
	if got.Source != "work" {
		t.Errorf("source = %q", got.Source)
	}
	w, ok := events.DataAs[*types.RawWebhook](got)
	if !ok || w == nil {
		t.Fatalf("the published event is not a raw webhook: %+v", got)
	}
	// THE RECORD TRAVELS IN THE BODY, so the node that wins the message
	// routes without reading anything — a projection that had not caught up
	// would otherwise route from a stale head or block the feed.
	if w.Body["key"] != "item/u1" {
		t.Errorf("the body does not carry the record: %+v", w.Body)
	}
	if w.Handle != "eng" {
		t.Errorf("the actor did not travel: %q", w.Handle)
	}
}

// A CLAIM COLLAPSES A REDELIVERY. A node that died after publishing but
// before acking hands the change to a peer, and without the claim that peer
// wakes everybody a second time.
func TestARedeliveredChangeIsPublishedOnce(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	p := newProbe()
	run(t, docs, pub, cl, p)

	writeChange(t, docs, "u1")
	settle(t, func() bool { return pub.count() == 1 }, "the change never published")

	// A second consumer in the same group, as a peer taking a redelivery.
	second := &capture{}
	run(t, docs, second, cl, newProbe())
	writeChange(t, docs, "u1-again")
	settle(t, func() bool { return pub.count()+second.count() == 2 },
		"the second change never published")

	// Replaying the FIRST change's id through the claim is what a
	// redelivery does.
	if won, err := cl.Claim(t.Context(), changefeed.ClaimKey("work", "u1"), time.Minute, time.Now()); err != nil || won {
		t.Errorf("the first change's claim was not held: won=%v err=%v", won, err)
	}
}

// THE CLAIM FAILS OPEN. A coordination store that cannot be reached must not
// silently stop the company's notifications — the duplicate is collapsed
// downstream by the deterministic wake id, where a swallowed change is a wake
// nobody is ever told about.
func TestAnUnreachableClaimStorePublishesAnyway(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	cl.breakWith(errors.New("the coordination store is unreachable"))
	run(t, docs, pub, cl, newProbe())

	writeChange(t, docs, "u1")
	settle(t, func() bool { return pub.count() == 1 },
		"a claim failure swallowed the wake")
}

// A DECISION IS HANDLING. A quiet import or a record this build has no rule
// for is ACKED — naking it would circle it to the dead-letter path for having
// been handled correctly.
func TestAChangeThatWakesNobodyIsAckedRatherThanRetried(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	p := newProbe()
	p.set(false, nil)
	run(t, docs, pub, cl, p)

	writeChange(t, docs, "u1")
	settle(t, func() bool { return p.translations() == 1 }, "the change was never translated")

	// Nothing published, and — the point — nothing redelivered: a second
	// change is translated exactly once too, which it would not be if the
	// first were circling.
	writeChange(t, docs, "u2")
	settle(t, func() bool { return p.translations() == 2 }, "the second change never arrived")
	time.Sleep(100 * time.Millisecond)
	if got := p.translations(); got != 2 {
		t.Errorf("%d translations for two changes — a handled change is being retried", got)
	}
	if pub.count() != 0 {
		t.Errorf("a change that wakes nobody published %d events", pub.count())
	}
}

// A FAILED PUBLISH RELEASES THE CLAIM BEFORE NAKING. A claim held over a
// delivery that never published would make the redelivery skip it — a wake
// lost to a broker hiccup, which is the exact failure the durable record
// exists to prevent.
func TestAFailedPublishReleasesItsClaim(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	pub.breakWith(errors.New("the broker is down"))
	run(t, docs, pub, cl, newProbe())

	writeChange(t, docs, "u1")
	settle(t, func() bool { return cl.attempts() > 0 }, "the change was never claimed")
	settle(t, func() bool { return !cl.isHeld(changefeed.ClaimKey("work", "u1")) },
		"the claim was held over a wake that never published")

	// AND THE REDELIVERY LANDS IT. The nak is what makes the broker hand
	// it back, and the released claim is what lets the retry through.
	pub.breakWith(nil)
	settle(t, func() bool { return pub.count() == 1 },
		"the naked change never came back after the broker recovered")
}

// A WAKE ID IS DERIVED, which is what makes a duplicate recognisable at all:
// with random ids the inbox dedupe and the completion ledger cannot see the
// pair, so any producer retry wakes a seat twice.
func TestWakeIDsAreDerivedPerRecipient(t *testing.T) {
	t.Parallel()
	a := changefeed.WakeID("change-1", "eng")
	if a != changefeed.WakeID("change-1", "eng") {
		t.Error("the same change and recipient produced two ids")
	}
	// PER RECIPIENT: one change legitimately wakes several seats, and one
	// id for all of them would have the first delivered and the rest
	// deduplicated away.
	if a == changefeed.WakeID("change-1", "pm") {
		t.Error("two recipients of one change share a wake id")
	}
	if a == changefeed.WakeID("change-2", "eng") {
		t.Error("two changes to one recipient share a wake id")
	}
	if a.String() == "00000000-0000-0000-0000-000000000000" {
		t.Error("the derived id is the zero uuid")
	}
}

// The claim key is scoped by source, because two families mint ids
// independently and a bare id would let a page change suppress a work change
// that happened to collide.
func TestClaimKeysAreScopedBySource(t *testing.T) {
	t.Parallel()
	if changefeed.ClaimKey("work", "u1") == changefeed.ClaimKey("page", "u1") {
		t.Error("two families share a claim key")
	}
}

// THE DURABLE GROUP'S NAME IS THE FLEET'S POSITION, so it must be stable and
// it must be each estate's own.
//
// Nothing derives one any more. The helper that built a name from a document
// family went with the families, and each estate now declares a constant
// beside the translator that reads it — so what can still be checked, and what
// matters, is that the shipped names are DISTINCT: two estates sharing a group
// share a position, and each would ack the other's records into a void.
func TestEveryEstateDeclaresItsOwnGroupAndTheyAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for estate, group := range map[string]string{
		"work": tracker.FeedGroup,
		"page": pages.FeedGroup,
	} {
		if group == "" {
			t.Errorf("%s declares no group, so its feed has no durable "+
				"position at all", estate)
			continue
		}
		if other, clash := seen[group]; clash {
			t.Errorf("%s and %s share the group %q — one estate's consumer "+
				"would ack the other's records", estate, other, group)
		}
		seen[group] = estate
	}
}

// A feed refuses an incomplete wiring rather than starting and failing later,
// which on this path means a company whose writes silently wake nobody.
func TestAFeedRefusesAnIncompleteWiring(t *testing.T) {
	t.Parallel()
	opener := newEstate()
	for _, opts := range []changefeed.Options{
		{Publisher: &capture{}, Translator: newProbe()},
		{Opener: opener, Translator: newProbe()},
		{Opener: opener, Publisher: &capture{}},
		// A translator whose source names no estate and no group: the
		// first would share a claim key space with every other estate,
		// the second is the fleet's own position.
		{Opener: opener, Publisher: &capture{}, Translator: &nameless{}},
		{Opener: opener, Publisher: &capture{}, Translator: &groupless{}},
	} {
		if _, err := changefeed.New(opts); err == nil {
			t.Errorf("an incomplete wiring was accepted: %+v", opts)
		}
	}
}

// nameless and groupless are translators whose source is incomplete in each
// of the two ways that matter.
type nameless struct{ probe }

func (n *nameless) Source() changefeed.Source {
	return changefeed.Source{Group: "crewlet-pages-feed"}
}

type groupless struct{ probe }

func (g *groupless) Source() changefeed.Source { return changefeed.Source{Name: "page"} }

// A REMOVED RECORD IS ACKED AND NEVER TRANSLATED.
//
// The decision moved out of the two domain translators and into the framework
// when the seam did, and it can only live in one place: every estate answers
// it the same way — the wake this record once produced was delivered when it
// was written — so a removal must not reach a translator and must not circle
// back through the dead-letter path.
func TestARemovedRecordIsAckedWithoutTranslation(t *testing.T) {
	t.Parallel()
	docs, pub, cl := newEstate(), &capture{}, newClaims()
	p := newProbe()
	run(t, docs, pub, cl, p)

	writeChange(t, docs, "u1")
	settle(t, func() bool { return p.translations() == 1 }, "the change was never translated")
	docs.deliver(changefeed.Record{ID: "u1", Key: "item/u1", Removed: true})

	// The removal must not reach the translator, and it must not circle:
	// a later change is translated exactly once, which it would not be if
	// the removal were being redelivered.
	writeChange(t, docs, "u2")
	settle(t, func() bool { return p.translations() == 2 }, "the second change never arrived")
	time.Sleep(100 * time.Millisecond)
	if got := p.translations(); got != 2 {
		t.Errorf("%d translations for two changes and a removal — the removal "+
			"is being translated or retried", got)
	}
	if got := docs.acks("u1"); got < 2 {
		t.Errorf("the removal was acked %d time(s) beyond its change — a "+
			"removal that is not acked comes back for ever", got-1)
	}
}
