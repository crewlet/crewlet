package statelog_test

import (
	"context"
	"crypto/rand"
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
