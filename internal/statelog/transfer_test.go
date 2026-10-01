package statelog_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// transferHarness is a broker, a donor holding an artefact, and a joiner.
type transferHarness struct {
	t        *testing.T
	nc       *nats.Conn
	dir      string
	artefact string
	manifest statelog.Manifest
	donor    *statelog.Donor
}

func newTransferHarness(t *testing.T, bytes int) *transferHarness {
	return newTransferHarnessWith(t, bytes, 0)
}

// newTransferHarnessWith is newTransferHarness with a stated admission bound,
// for the case about a donor that is already full.
func newTransferHarnessWith(t *testing.T, bytes, maxTransfers int) *transferHarness {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	nc := q.Conn()

	dir := t.TempDir()
	artefact := filepath.Join(dir, "snapshot-4200.db")
	body := make([]byte, bytes)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("build an artefact: %v", err)
	}
	if err := os.WriteFile(artefact, body, 0o600); err != nil {
		t.Fatalf("write an artefact: %v", err)
	}
	digest, err := store.FileDigest(artefact)
	if err != nil {
		t.Fatalf("digest the artefact: %v", err)
	}

	h := &transferHarness{
		t: t, nc: nc, dir: dir, artefact: artefact,
		manifest: statelog.Manifest{
			V:       statelog.ManifestVersion,
			TakenAt: time.Unix(1_700_000_000, 0).UTC(),
			NodeID:  "donor",
			Bytes:   int64(len(body)),
			SHA256:  digest,
			Domains: map[string]statelog.DomainPosition{
				"probe": {
					Stream:        probeStream,
					Generation:    1,
					Seq:           4_200,
					RecordVersion: 1,
					Replay:        statelog.ReplayStrict,
				},
			},
		},
	}
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID:       "donor",
		Dial:         func(context.Context) (*nats.Conn, error) { return q.Conn(), nil },
		Newest:       func() (statelog.Manifest, bool) { return h.manifest, true },
		Path:         func(statelog.Manifest) string { return h.artefact },
		MaxTransfers: maxTransfers,
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	h.donor = donor

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	// The donor's subscriptions have to exist before the first request, or
	// the request lands on nothing and the case measures the race rather
	// than the protocol.
	waitForSubject(t, nc, statelog.SubjectOffer)
	return h
}

// waitForSubject waits until somebody is listening, which is what makes a
// request-response case about the protocol rather than about startup order.
func waitForSubject(t *testing.T, nc *nats.Conn, subject string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := nc.Request(subject, []byte(`{"node_id":"probe"}`), 200*time.Millisecond); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nobody is listening on %s", subject)
}

// AN ARTEFACT ARRIVES BYTE FOR BYTE, ACROSS A CREDIT WINDOW.
//
// The transfer is tuned for a network rather than reused verbatim from this
// tree's in-process one: that one is strict stop-and-wait at 128 KiB because
// it runs over an in-memory connection, and across two broker hops that is one
// chunk per round trip. What is asserted here is what the window is FOR — that
// many chunks in flight still arrive in order and whole.
func TestAnArtefactArrivesByteForByte(t *testing.T) {
	t.Parallel()
	// Several times the window, so the donor genuinely blocks on credits
	// rather than sending everything at once.
	h := newTransferHarness(t, statelog.SnapshotChunkBytes*statelog.SnapshotTransferWindow*2+7)

	offers, err := statelog.CollectOffers(t.Context(), h.nc, statelog.OfferRequest{
		NodeID: "joiner",
	}, statelog.OfferWindow)
	if err != nil {
		t.Fatalf("CollectOffers: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("%d offer(s), want 1", len(offers))
	}

	dest := filepath.Join(t.TempDir(), "adopt.part")
	n, err := statelog.FetchArtefact(t.Context(), h.nc, offers[0], dest)
	if err != nil {
		t.Fatalf("FetchArtefact: %v", err)
	}
	if n != h.manifest.Bytes {
		t.Fatalf("received %d bytes, want %d", n, h.manifest.Bytes)
	}
	got, err := store.FileDigest(dest)
	if err != nil {
		t.Fatalf("digest what arrived: %v", err)
	}
	if got != h.manifest.SHA256 {
		t.Fatalf("the artefact's checksum is %s and the manifest says %s — a "+
			"transfer that reorders or drops a chunk arrives looking exactly "+
			"like one that did not", got, h.manifest.SHA256)
	}
}

// A JOINER TOLD TO STOP STOPS ASKING.
//
// Collecting offers waits out a window for donors to answer, and it waited out
// the whole of it whatever its caller said: a node shutting down while it
// looked for a snapshot sat in the collection until the window closed, and
// then reported the offers as though nothing had happened. A cancelled caller
// is answered at once, with the cancellation.
func TestCollectingOffersStopsWhenItsCallerDoes(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	const window = 10 * time.Second
	started := time.Now()
	offers, err := statelog.CollectOffers(ctx, h.nc, statelog.OfferRequest{
		NodeID: "joiner",
	}, window)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled collection answered %d offer(s) and %v, want the "+
			"cancellation", len(offers), err)
	}
	if waited := time.Since(started); waited >= window/2 {
		t.Errorf("a cancelled collection took %v of its %v window", waited, window)
	}
}

// A DONOR THAT GIVES UP SAYS SO IN THE TERMINATOR, and a truncated file is
// never reported as a snapshot.
//
// Every failure arrives looking exactly like a normal end of stream, so the
// verdict in the terminator's header is the only thing between that and an
// adopted artefact with a hole in it.
func TestAnAbandonedTransferIsNeverReportedAsASnapshot(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	// The artefact goes away between the offer and the fetch, which is
	// what a rotation mid-transfer looks like from here.
	offers, err := statelog.CollectOffers(t.Context(), h.nc, statelog.OfferRequest{
		NodeID: "joiner",
	}, statelog.OfferWindow)
	if err != nil || len(offers) != 1 {
		t.Fatalf("CollectOffers = (%d, %v)", len(offers), err)
	}
	if err := os.Remove(h.artefact); err != nil {
		t.Fatalf("remove the artefact: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "adopt.part")
	if _, err := statelog.FetchArtefact(t.Context(), h.nc, offers[0], dest); err == nil {
		t.Fatal("a transfer with no artefact behind it reported success")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a failed transfer left a file behind — a partial artefact that " +
			"survives is one a later attempt refuses to overwrite")
	}
}

// AN OFFER IS REFUSED FROM ITS MANIFEST, BEFORE ANY BYTES MOVE.
//
// A gigabyte-scale transfer that ends in a refusal is a gigabyte-scale
// transfer nobody needed, and every one of these is answerable from the
// manifest alone.
func TestAnUnusableOfferIsRefusedBeforeTheTransfer(t *testing.T) {
	t.Parallel()
	build := map[string]statelog.Registered{"probe": {Domain: probeDomain{}}}
	base := func() statelog.Offer {
		return statelog.Offer{Manifest: statelog.Manifest{
			V: statelog.ManifestVersion,
			Domains: map[string]statelog.DomainPosition{
				"probe": {
					Stream: probeStream, Generation: 1, Seq: 4_200,
					RecordVersion: 1, Replay: statelog.ReplayStrict,
				},
			},
		}}
	}
	req := statelog.OfferRequest{
		Need:        map[string]uint64{"probe": 4_000},
		Generations: map[string]uint32{"probe": 1},
	}

	if err := base().Usable(req, build); err != nil {
		t.Fatalf("a usable offer was refused: %v", err)
	}

	for name, tc := range map[string]struct {
		breaks func(*statelog.Offer)
		names  string
	}{
		"a domain this build registers and the artefact never named": {
			breaks: func(o *statelog.Offer) { delete(o.Manifest.Domains, "probe") },
			names:  "adopted wholesale",
		},
		"a donor that read more record versions than this build": {
			breaks: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.RecordVersion = 9
				o.Manifest.Domains["probe"] = p
			},
			names: "never can",
		},
		"a different replay protocol": {
			breaks: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Replay = statelog.ReplayCompacted
				o.Manifest.Domains["probe"] = p
			},
			names: "permanent stall",
		},
		"another generation's sequences": {
			breaks: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Generation = 2
				o.Manifest.Domains["probe"] = p
			},
			names: "different history",
		},
		"a position below the floor": {
			breaks: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Seq = 3_000
				o.Manifest.Domains["probe"] = p
			},
			names: "already gone",
		},
		"a manifest version this build does not read": {
			breaks: func(o *statelog.Offer) { o.Manifest.V = 99 },
			names:  "version 99",
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := base()
			tc.breaks(&o)
			err := o.Usable(req, build)
			if err == nil {
				t.Fatalf("an offer with %s was accepted", name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
}

// NEWER IS STRICTLY BETTER, and an older offer is never preferable on age.
//
// The only thing an older artefact buys is a longer replay, so a joiner that
// took the first reply would be choosing the fastest peer rather than the best
// artefact.
func TestOffersAreRankedNewestFirst(t *testing.T) {
	t.Parallel()
	at := func(seq uint64) statelog.Offer {
		return statelog.Offer{Manifest: statelog.Manifest{
			V:       statelog.ManifestVersion,
			Domains: map[string]statelog.DomainPosition{"probe": {Seq: seq}},
		}}
	}
	if got := at(9_000).Newest(); got != 9_000 {
		t.Fatalf("Newest() = %d, want 9000", got)
	}
	if at(9_000).Newest() <= at(4_200).Newest() {
		t.Fatal("a newer artefact does not rank above an older one")
	}
}

// TestNothingBelowTheFloorOutranksAnythingAboveIt, and among equals the choice
// is this joiner's own.
//
// Ordering by the artefact alone makes every joiner in a fleet rank the same
// offers the same way, so a fleet that restarts together sends every one of
// them at whichever node happens to hold the newest artefact. That node then
// refuses all but a few, and the rest spend a collection window asking again.
// Shuffling the EQUALS before the ordering keeps "best artefact first" exact
// and makes the choice among the best differ per joiner.
func TestNothingBelowTheFloorOutranksAnythingAboveIt(t *testing.T) {
	t.Parallel()
	offer := func(node string, seq uint64) statelog.Offer {
		return statelog.Offer{Fetch: node, Manifest: statelog.Manifest{
			V:       statelog.ManifestVersion,
			Domains: map[string]statelog.DomainPosition{"probe": {Seq: seq}},
		}}
	}
	// Four donors at one position and one that is strictly better.
	equal := []statelog.Offer{
		offer("a", 9_000), offer("b", 9_000), offer("c", 9_000), offer("d", 9_000),
	}
	best := offer("e", 9_500)

	firsts := map[string]int{}
	for seed := range uint64(64) {
		ranked := statelog.RankOffersForTest(append([]statelog.Offer{best}, equal...), seed)
		if ranked[0].Fetch != "e" {
			t.Fatalf("seed %d ranked %q first; a strictly newer artefact must always "+
				"come first, or the shuffle is choosing the replay length",
				seed, ranked[0].Fetch)
		}
		firsts[ranked[1].Fetch]++
	}
	if len(firsts) < 2 {
		t.Errorf("across 64 seeds the same donor was always chosen among the equals "+
			"(%v) — every joiner in a fleet would then ask one node", firsts)
	}
}

// TestADonorAdmitsAtMostNTransfers, and tells the rest so at once.
//
// Every fetch is a goroutine holding an open file and a credit window of chunks
// in flight, on a subject every joiner in the fleet can reach. Unbounded, a
// fleet that restarts together spawns one per joiner on whichever node answered
// first — and since the offers were ranked identically everywhere, that is one
// node.
//
// WHAT IS OVER THE BOUND IS TERMINATED WITH A REASON rather than queued. A
// joiner told no immediately asks the next donor; one left waiting on a queue
// it cannot see spends its whole collection window on a node that was never
// going to answer it.
func TestADonorAdmitsAtMostNTransfers(t *testing.T) {
	t.Parallel()
	// One admission, against an artefact big enough that a transfer is
	// several credit windows long rather than one round trip.
	h := newTransferHarnessWith(t, statelog.SnapshotChunkBytes*statelog.SnapshotTransferWindow*8, 1)

	offers, err := statelog.CollectOffers(t.Context(), h.nc, statelog.OfferRequest{
		NodeID: "joiner",
	}, statelog.OfferWindow)
	if err != nil {
		t.Fatalf("CollectOffers: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("collected %d offer(s), want 1", len(offers))
	}

	// EIGHT JOINERS AT ONCE, which is the shape this bounds: a fleet
	// restarting together, every node ranking the same offer first.
	const joiners = 8
	results := make(chan error, joiners)
	var start sync.WaitGroup
	start.Add(1)
	for i := range joiners {
		go func() {
			start.Wait()
			_, err := statelog.FetchArtefact(t.Context(), h.nc, offers[0],
				filepath.Join(t.TempDir(), fmt.Sprintf("joiner-%d.db", i)))
			results <- err
		}()
	}
	start.Done()

	var admitted, refused int
	var refusal string
	for range joiners {
		switch err := <-results; {
		case err == nil:
			admitted++
		case strings.Contains(err.Error(), "ask another node"):
			refused++
			refusal = err.Error()
		default:
			t.Errorf("a transfer failed for an unexpected reason: %v", err)
		}
	}
	if refused == 0 {
		t.Fatalf("all %d concurrent joiners were admitted against a bound of 1, so a "+
			"fleet restarting together would spawn one goroutine per joiner here",
			joiners)
	}
	if admitted == 0 {
		t.Error("no joiner was admitted at all, so the bound refuses rather than bounds")
	}
	if !strings.Contains(refusal, "which is its limit") {
		t.Errorf("the refusal does not say why: %q", refusal)
	}
}

// openSilentFleet is a broker with ONE listener on the offer subject that
// never answers — a fleet whose donors hold nothing — and a wait that returns
// once the joiner is COLLECTING: the listener has heard its ask, and the flush
// that follows the ask has had time to complete.
//
// SILENT RATHER THAN ABSENT, because an absent fleet is answered by the broker:
// with nothing subscribed it reports "no responders" and a collection ends at
// once, which is exactly how a case about a collection that ENDS EARLY passes
// for the wrong reason.
//
// AND SETTLED PAST THE FLUSH, for the same reason one step later. The listener
// hears the ask before the broker's answer to the joiner's flush has arrived,
// and an interruption landing inside that flush ends it there — correctly, so
// every assertion here holds on that path too, but a case whose interruption
// lands in the flush says nothing about the collection loop, and a mutation
// that broke only the loop went green on it. A broker in this process answers
// a flush in microseconds, so the settle is two orders of magnitude of room.
func openSilentFleet(t *testing.T) (*js.Queue, func()) {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	listener, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	asked := make(chan struct{}, 1)
	if _, err := listener.Subscribe(statelog.SubjectOffer, func(*nats.Msg) {
		select {
		case asked <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("listen for asks: %v", err)
	}
	// The subscription has to reach the broker before the first ask, or
	// the ask meets "no responders" and the case measures startup order.
	if err := listener.Flush(); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}
	return q, func() {
		t.Helper()
		select {
		case <-asked:
		case <-time.After(time.Minute):
			t.Fatal("the joiner never asked the fleet for offers")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A JOINER WHOSE CALLER GIVES UP STOPS COLLECTING, and says why.
//
// The window is the joiner's patience, not a promise to wait it out: a boot
// interrupted by a signal and a running node being stopped mid-rejoin both
// cancel the context they asked under. A collection that ignored it held each
// for the rest of the window — the rejoin's for the whole of the state log's
// Stop, which waits for it — and then answered "nobody offered anything",
// which a boot reads as "come up on what you have" and logs as a fleet with
// nothing to donate.
func TestACancelledJoinerStopsCollecting(t *testing.T) {
	t.Parallel()
	q, asked := openSilentFleet(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		offers []statelog.Offer
		err    error
	}
	done := make(chan result, 1)
	go func() {
		// AN HOUR, so a collection that ignores the cancellation cannot
		// end inside the watchdog below by any route but the window.
		offers, err := statelog.CollectOffers(ctx, q.Conn(),
			statelog.OfferRequest{NodeID: "joiner"}, time.Hour)
		done <- result{offers, err}
	}()
	asked()
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("a cancelled collection returned (%d offer(s), %v), want the "+
				"cancellation — an empty window reads as 'nobody could donate'",
				len(got.offers), got.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a cancelled collection was still waiting a minute into an " +
			"hour-long window — the caller's context is not being read")
	}
}

// A JOINER WHOSE CALLER HAS ALREADY GIVEN UP ASKS NOBODY.
//
// An ask is not free to the fleet: every donor answers it, with a manifest
// and a fetch subject, for a joiner that will never read either.
//
// "Nothing was published" is observed with a SENTINEL rather than a wait: the
// sentinel goes out on the joiner's own connection afterwards, and a broker
// delivers one connection's messages in order, so the listener's first
// message is the sentinel exactly when nothing preceded it.
func TestAJoinerWhoseCallerHasGivenUpAsksNobody(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	listener, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	heard, err := listener.SubscribeSync(statelog.SubjectOffer)
	if err != nil {
		t.Fatalf("listen for asks: %v", err)
	}
	if err := listener.Flush(); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A SHORT WINDOW, so a joiner that asks anyway and then waits the
	// window out comes back inside this case rather than an hour later.
	if _, err := statelog.CollectOffers(ctx, q.Conn(),
		statelog.OfferRequest{NodeID: "joiner"}, 100*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectOffers under a cancelled context = %v, want the cancellation", err)
	}

	if err := q.Conn().Publish(statelog.SubjectOffer, []byte("sentinel")); err != nil {
		t.Fatalf("publish the sentinel: %v", err)
	}
	first, err := heard.NextMsg(time.Minute)
	if err != nil {
		t.Fatalf("the listener heard nothing, not even the sentinel: %v", err)
	}
	if string(first.Data) != "sentinel" {
		t.Fatalf("the fleet was asked (%q) by a joiner whose caller had already "+
			"given up", first.Data)
	}
}

// A JOINER THAT CAN NO LONGER HEAR IS NOT TOLD THAT NOBODY ANSWERED.
//
// "Nobody offered anything" is a statement about the FLEET, and the callers
// act on it — a boot comes up on the history it has, a rejoin backs off. A
// connection that closed mid-window has heard nothing from a fleet that may
// be offering, so reporting its silence as the fleet's is the same lie a
// cancelled caller must not be told, from the other side.
func TestAJoinerThatCannotHearIsNotToldNobodyAnswered(t *testing.T) {
	t.Parallel()
	q, asked := openSilentFleet(t)
	// ITS OWN CONNECTION, because this case closes it.
	joiner, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the joiner: %v", err)
	}
	t.Cleanup(joiner.Close)

	done := make(chan error, 1)
	go func() {
		_, err := statelog.CollectOffers(t.Context(), joiner,
			statelog.OfferRequest{NodeID: "joiner"}, time.Hour)
		done <- err
	}()
	asked()
	joiner.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a joiner whose connection closed mid-window reported an " +
				"empty fleet — a boot comes up on that answer as if nobody " +
				"could donate")
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("a closed connection was reported as a cancelled caller: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a joiner whose connection closed was still collecting a " +
			"minute into an hour-long window")
	}
}

// A CALLER'S OWN DEADLINE IS NOT BLAMED ON THE DONOR.
//
// The wait for each chunk is a child of the caller's context, so a caller
// whose deadline passes sees the same DeadlineExceeded a stalled donor
// produces. Reading only that, the transfer reported a donor that "stopped
// sending (no chunk for 30s)" a fraction of a second in, and wrapped nothing a
// caller could test — so the caller's own end was indistinguishable from a
// peer's fault.
func TestACallersDeadlineIsNotBlamedOnTheDonor(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	// A donor that accepted the fetch and went quiet.
	fetch := statelog.SubjectFetchPrefix + "quiet"
	quiet, err := q.Conn().SubscribeSync(fetch)
	if err != nil {
		t.Fatalf("listen for the fetch: %v", err)
	}
	t.Cleanup(func() { _ = quiet.Unsubscribe() })

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "adopt.part")
	_, err = statelog.FetchArtefact(ctx, q.Conn(), statelog.Offer{Fetch: fetch}, dest)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fetch whose caller's deadline passed returned %v, want that "+
			"deadline — anything else blames the donor for the caller's own end", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("an abandoned fetch left its partial file behind")
	}
}

// A FETCH WHOSE CALLER HAS ALREADY GIVEN UP ASKS NO DONOR TO SEND.
//
// The fetch is the expensive half of a join: the request sets a donor sending
// up to a full credit window of chunks into an inbox nobody will read, and ties
// up a goroutine, an open artefact and a credit subscription on it until the
// chunk wait runs out. A joiner being stopped between choosing an offer and
// fetching it must not start that, so the request is never published — which
// is only observable from the donor's side, hence the sentinel: the listener
// hears whatever reached the subject, in order, and the first thing it hears
// must be the probe this case sent after the fetch returned.
func TestAFetchWhoseCallerHasGivenUpAsksNobody(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	fetch := statelog.SubjectFetchPrefix + "donor"
	listener, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	heard, err := listener.SubscribeSync(fetch)
	if err != nil {
		t.Fatalf("listen for the fetch: %v", err)
	}
	if err := listener.Flush(); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dest := filepath.Join(t.TempDir(), "adopt.part")
	if _, err := statelog.FetchArtefact(ctx, q.Conn(), statelog.Offer{Fetch: fetch}, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchArtefact under a cancelled context = %v, want the cancellation", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a fetch that never started left a file behind")
	}

	if err := q.Conn().Publish(fetch, []byte("sentinel")); err != nil {
		t.Fatalf("publish the sentinel: %v", err)
	}
	first, err := heard.NextMsg(time.Minute)
	if err != nil {
		t.Fatalf("the listener heard nothing, not even the sentinel: %v", err)
	}
	if string(first.Data) != "sentinel" {
		t.Fatalf("a donor was asked to send (%q) by a joiner whose caller had "+
			"already given up", first.Data)
	}
}

// A DONOR WHOSE CONNECTION IS CLOSED UNDER IT SAYS SO.
//
// The NATS client closes a connection for good on an error it does not retry,
// and the donor's is the connection carrying the largest messages a node
// sends. Serve used to wait on its context and nothing else, so a donor whose
// connection was gone sat subscribed to nothing for the rest of the node's
// life, advertising an artefact it could no longer stream. It returns now,
// naming the close, which is what lets its owner dial again — and only its
// owner can, because a donor's loss is not the node's.
//
// The case closes the connection the donor dialled, from outside the donor,
// which is what the client does when it gives up on one.
//
// Mutation: wait on the context alone in Serve and the donor never returns.
func TestADonorWhoseConnectionIsClosedUnderItReturns(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	dialled := make(chan *nats.Conn, 1)
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "closed-under",
		Dial: func(context.Context) (*nats.Conn, error) {
			nc, err := q.DialOwned()
			if err == nil {
				dialled <- nc
			}
			return nc, err
		},
		Newest: func() (statelog.Manifest, bool) { return statelog.Manifest{}, true },
		Path:   func(statelog.Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- donor.Serve(t.Context()) }()
	nc := <-dialled
	waitForSubject(t, q.Conn(), statelog.SubjectOffer)

	nc.Close()

	select {
	case err := <-served:
		if !errors.Is(err, statelog.ErrDonorConnectionClosed) {
			t.Errorf("Serve returned %v, want %v", err, statelog.ErrDonorConnectionClosed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the donor's connection was closed and Serve is still waiting " +
			"on its context: a donor subscribed to nothing, for good")
	}
}

// A DONOR HANDED A CONNECTION ALREADY CLOSED HAD NOTHING TO SERVE OVER, AND
// SAYS SO AS A FAILED DIAL.
//
// [statelog.ErrDonorConnectionClosed] tells the caller the donor HAD been
// serving, and the engine's loop answers it as a new outage: it redials from
// its base, a second later, and logs the stop every time. A connection that was
// closed before Serve could subscribe on it is the opposite — the dial failed,
// as a refused one does — and called a close, one that arrived closed on every
// redial would be redialled once a second, with an ERROR line each time, for as
// long as it went on. The close is staged where it cannot be raced: the dial
// itself hands back a connection it has closed.
//
// Mutation: check the connection for a close before subscribing and answer
// ErrDonorConnectionClosed, as Serve did, and both checks go red; ignore the
// subscription's refusal and wait as though serving, and the case times out.
func TestADonorHandedAClosedConnectionReportsAFailedDial(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "closed-on-arrival",
		Dial: func(context.Context) (*nats.Conn, error) {
			nc, err := q.DialOwned()
			if err == nil {
				nc.Close()
			}
			return nc, err
		},
		Newest: func() (statelog.Manifest, bool) { return statelog.Manifest{}, true },
		Path:   func(statelog.Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- donor.Serve(t.Context()) }()

	select {
	case err := <-served:
		if errors.Is(err, statelog.ErrDonorConnectionClosed) {
			t.Errorf("Serve answered a connection that arrived closed with %v, "+
				"which says the donor had been serving: its loop would redial at "+
				"once and log it as a new outage on every attempt", err)
		}
		if err == nil || !errors.Is(err, nats.ErrConnectionClosed) {
			t.Errorf("Serve returned %v, want a failed dial naming the closed "+
				"connection (%v)", err, nats.ErrConnectionClosed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve is waiting on a connection that arrived closed")
	}
}
