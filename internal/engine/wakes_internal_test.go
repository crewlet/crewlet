package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A WAKE NEVER LANDS ON A SUBJECT WITH NO CONSUMER.
//
// A seat's inbox and the inbound edge are both interest-retention: what is
// published before the consumer exists is discarded and reported as sent. The
// cases here hold the two engine-side orders that keep a wake out of that
// window; the node's wake queue, which holds the rest, is covered in
// internal/node.

// wakesCompany is a company of two seats; wakesCompanyPlusCFO adds a third.
const wakesCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Dev
    handle: dev
    llm: zulu
`

var wakesCompanyPlusCFO = wakesCompany + `  - name: CFO
    handle: cfo
    llm: zulu
`

// ensureWatch is the engine's queue, with a hook on every mailbox it makes.
//
// The concrete backend embedded, not the contract: a boot asks the broker for
// what the contract does not carry (a state log's conditional append), and a
// wrapper over the interface would fail the boot on that instead.
type ensureWatch struct {
	*jetstream.Queue
	mu       sync.Mutex
	onEnsure func(topic string)
	refuse   string
}

func (q *ensureWatch) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	q.mu.Lock()
	hook, refuse := q.onEnsure, q.refuse
	q.mu.Unlock()
	if hook != nil {
		hook(topic)
	}
	if topic == refuse {
		return false, errors.New("the broker did not answer")
	}
	return q.Queue.EnsureSubscription(ctx, topic, group)
}

// watchedBackends opens a node's backends on a fresh broker with the queue
// behind an ensureWatch.
func watchedBackends(t *testing.T, cfg *config.Company) (*config.Bootstrap, *Backends, *ensureWatch) {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	real, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend this case needs", back.Queue)
	}
	watch := &ensureWatch{Queue: real}
	borrowed := *back
	borrowed.Queue = watch
	return &b, &borrowed, watch
}

func parseWakesCompany(t *testing.T, doc string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return cfg
}

// A SEAT A REVISION ADDS HAS A MAILBOX BEFORE ANYTHING CAN WAKE IT.
//
// An apply indexes the revision's parties before it publishes the revision,
// so a delivery naming the new seat resolves from that moment and its wake is
// published to the seat's inbox. The mailbox used to be made only after the
// publish, by the list every published company follows, so every wake routed
// in between was discarded by the stream. Observed at the moment the new
// seat's mailbox is made: the registry must not be answering for the seat yet.
//
// Mutation: drop the revision's ensure from the apply and the seat's mailbox
// is first made after the registry already resolves it, which fails here.
func TestASeatARevisionAddsHasAMailboxBeforeItCanBeWoken(t *testing.T) {
	t.Parallel()
	cfg := parseWakesCompany(t, wakesCompany)
	b, back, watch := watchedBackends(t, cfg)
	e, err := New(t.Context(), Options{Bootstrap: b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	var (
		firstEnsure = map[uuid.UUID]bool{}
		mu          sync.Mutex
		engine      atomic.Pointer[Engine]
	)
	engine.Store(e)
	watch.mu.Lock()
	watch.onEnsure = func(topic string) {
		seat, ok := topics.SeatFromInbox(topic)
		if !ok {
			return
		}
		_, routable := engine.Load().Registry().ByHandle("cfo")
		mu.Lock()
		defer mu.Unlock()
		if _, seen := firstEnsure[seat]; !seen {
			firstEnsure[seat] = routable
		}
	}
	watch.mu.Unlock()

	if _, _, err := e.Apply(t.Context(), parseWakesCompany(t, wakesCompanyPlusCFO), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var cfo uuid.UUID
	for _, s := range e.Company().Seats() {
		if s.Handle == "cfo" {
			cfo = s.ID
		}
	}
	if cfo == uuid.Nil {
		t.Fatal("the applied revision's seat is not in the running company")
	}
	mu.Lock()
	routable, ensured := firstEnsure[cfo]
	mu.Unlock()
	if !ensured {
		t.Fatal("the apply made no mailbox for the seat it added")
	}
	if routable {
		t.Fatal("the added seat's mailbox was first made after the registry already resolved " +
			"the seat, so a wake routed to it in between was published onto a subject the " +
			"stream discards")
	}
}

// A NODE THAT CANNOT MAKE THE INBOUND EDGE'S MAILBOX DOES NOT BOOT.
//
// The change feeds and the Mattermost transport publish onto the edge before
// the router attaches to it, so the boot makes the edge's mailbox itself,
// ahead of them, rather than leaving it to the router's own subscribe. A
// refusal stops the boot exactly as a refused router subscribe always has: a
// node that cannot hold its inbound mail routes nothing while looking healthy.
//
// Mutation: drop the edge's own ensure from the boot and from
// [Engine.startNotifications], and the first to ask is the router's subscribe,
// after the feeds have started: the boot still fails, but on the router's
// error, which fails here.
func TestANodeThatCannotHoldItsInboundMailDoesNotBoot(t *testing.T) {
	t.Parallel()
	cfg := parseWakesCompany(t, wakesCompany)
	b, back, watch := watchedBackends(t, cfg)
	watch.refuse = topics.NotificationsInbound

	_, err := New(t.Context(), Options{Bootstrap: b, Company: cfg, Backends: back})
	if err == nil {
		t.Fatal("a node whose broker refused the inbound edge's mailbox booted anyway")
	}
	if !strings.Contains(err.Error(), "inbound edge's mailbox") {
		t.Fatalf("the boot failed for another reason (%v); this case needs the refusal of "+
			"the inbound edge's mailbox to be what stops it", err)
	}
}
