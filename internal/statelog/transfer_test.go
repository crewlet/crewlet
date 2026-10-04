package statelog_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
			V:         statelog.ManifestVersion,
			Partition: statelog.EstatePartition.String(),
			Artifact:  filepath.Base(artefact),
			TakenAt:   time.Unix(1_700_000_000, 0).UTC(),
			NodeID:    "donor",
			Bytes:     int64(len(body)),
			SHA256:    digest,
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
		NodeID: "donor", Layout: statelog.EstateLayout(probeDomain{}.Name()),
		Keeps:  statelog.KeepsOnly(statelog.EstatePartition).Keeps,
		Dial:   func(context.Context) (*nats.Conn, error) { return q.Conn(), nil },
		Newest: func(statelog.PartitionID) (statelog.Manifest, bool) { return h.manifest, true },
		Path:   func(statelog.Manifest) string { return h.artefact },
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

// waitForSubject waits until somebody answers an offer request for layout 0's
// one partition, which is what makes a request-response case about the
// protocol rather than about startup order.
func waitForSubject(t *testing.T, nc *nats.Conn, subject string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	ask := []byte(`{"node_id":"probe","layout":0,"partition":"estate.000"}`)
	for time.Now().Before(deadline) {
		if _, err := nc.Request(subject, ask, 200*time.Millisecond); err == nil {
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
		NodeID: "joiner", Partition: statelog.EstatePartition.String(),
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
		NodeID: "joiner", Partition: statelog.EstatePartition.String(),
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
	build := map[string]statelog.Registered{"probe": {Domain: probeDomain{}, Log: logOf(probeDomain{}), Spec: specOf(probeDomain{})}}
	base := func() statelog.Offer {
		return statelog.Offer{Manifest: statelog.Manifest{
			V: statelog.ManifestVersion, Partition: statelog.EstatePartition.String(),
			Domains: map[string]statelog.DomainPosition{
				"probe": {
					Stream: probeStream, Generation: 1, Seq: 4_200,
					RecordVersion: 1, Replay: statelog.ReplayStrict,
				},
			},
		}}
	}
	req := statelog.OfferRequest{
		Partition:   statelog.EstatePartition.String(),
		Need:        map[string]uint64{"probe": 4_000},
		Generations: map[string]uint32{"probe": 1},
	}

	if err := base().Usable(req, build, nil); err != nil {
		t.Fatalf("a usable offer was refused: %v", err)
	}
	// A SCHEMA THIS BINARY HAS is no reason to refuse, so the migration case
	// below is a refusal of the one it lacks rather than of the list.
	known := []string{"0001_the_state_log_lands.sql"}
	withSchema := base()
	withSchema.Manifest.Migrations = known
	if err := withSchema.Usable(req, build, known); err != nil {
		t.Fatalf("an offer on a schema this binary has was refused: %v", err)
	}

	for name, tc := range map[string]struct {
		breaks func(*statelog.Offer)
		names  string
	}{
		"a domain this build registers and the artefact never named": {
			breaks: func(o *statelog.Offer) { delete(o.Manifest.Domains, "probe") },
			names:  "adopted wholesale",
		},
		"rows applied from a record version above this build's": {
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
		// THE SCHEMA, from the list the manifest carries: fetched whole, the
		// file was refused for the same migration once it had arrived.
		"a migration this binary does not have": {
			breaks: func(o *statelog.Offer) {
				o.Manifest.Migrations = append(o.Manifest.Migrations,
					"9999_a_newer_build_reshaped_this.sql")
			},
			names: "9999_a_newer_build_reshaped_this.sql",
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := base()
			tc.breaks(&o)
			err := o.Usable(req, build, known)
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

// AN OFFER REQUEST NAMES A PARTITION OF THE DONOR'S LAYOUT, OR IT IS REFUSED —
// UNLESS THE DONOR RUNS LAYOUT 0.
//
// A snapshot is a copy of ONE partition's file, so a donor answers for the
// partition named. A request naming none is what a build from before
// partitions asks, whose join installs whatever it is handed as its whole
// estate: a donor running layout 0 answers it for `estate.000`, the whole
// estate, and one running a divided layout refuses it. A request for another
// layout's partition is not answered either: that layout's files are not the
// copy the joiner needs, whatever their name.
func TestAnOfferRequestMustNameAPartitionOfTheDonorsLayout(t *testing.T) {
	t.Parallel()
	one := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"probe"}},
	}}
	for name, tc := range map[string]struct {
		layout  statelog.Layout
		req     statelog.OfferRequest
		refused bool
	}{
		"layout 0's one partition": {layout: statelog.EstateLayout("probe"),
			req: statelog.OfferRequest{Partition: "estate.000"}},
		"no partition, at a donor running layout 0": {layout: statelog.EstateLayout("probe"),
			req: statelog.OfferRequest{}},
		"a partition of layout 1": {layout: one,
			req: statelog.OfferRequest{Layout: 1, Partition: "tracker.001"}},
		"no partition, as a build before partitions asks": {refused: true,
			layout: one, req: statelog.OfferRequest{}},
		"no partition, of layout 1": {refused: true,
			layout: one, req: statelog.OfferRequest{Layout: 1}},
		"another layout's partition": {refused: true, layout: one,
			req: statelog.OfferRequest{Layout: 2, Partition: "tracker.001"}},
		"a partition the layout does not have": {refused: true, layout: one,
			req: statelog.OfferRequest{Layout: 1, Partition: "tracker.002"}},
		"a name that is no partition's": {refused: true, layout: one,
			req: statelog.OfferRequest{Layout: 1, Partition: "tracker.1"}},
	} {
		err := tc.req.Validate(tc.layout)
		if (err != nil) != tc.refused {
			t.Errorf("%s: refused %v (%v), want %v", name, err != nil, err, tc.refused)
		}
		if err != nil && !errors.Is(err, statelog.ErrOfferRequest) {
			t.Errorf("%s: the refusal %v is not ErrOfferRequest", name, err)
		}
	}
	// AND A REQUEST NAMING NONE IS SAID TO BE WHAT IT IS, which is what an
	// operator reading a donor's log needs to know about the asker.
	err := statelog.OfferRequest{}.Validate(one)
	if err == nil || !strings.Contains(err.Error(), "predates partitions") {
		t.Errorf("a request naming no partition is refused as %v", err)
	}
	// AND AT LAYOUT 0 IT IS FOR THE WHOLE ESTATE.
	if p, err := (statelog.OfferRequest{}).Target(statelog.EstateLayout("probe")); err != nil ||
		p != statelog.EstatePartition {
		t.Errorf("a request naming no partition at layout 0 is for %v (%v), want %s", p, err,
			statelog.EstatePartition)
	}
}

// A DONOR ANSWERS ONLY FOR A PARTITION IT SERVES, AND NEVER A REQUEST NAMING
// NONE.
//
// Its artefact of one partition is no copy of another's file, a copy of a
// partition it is still joining or has begun to leave is not the partition's,
// and a request naming no partition is a build that would install one
// partition's file as its whole estate — so each is answered with silence,
// the donor's "no", while the partition it serves is offered as ever.
func TestADonorOffersOnlyThePartitionsItServes(t *testing.T) {
	t.Parallel()
	layout := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"probe"}},
	}}
	served := statelog.PartitionID{Space: statelog.SpaceTracker}
	other := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor", Layout: layout, Keeps: statelog.KeepsOnly(served).Keeps,
		Dial: func(context.Context) (*nats.Conn, error) { return q.DialOwned() },
		Newest: func(p statelog.PartitionID) (statelog.Manifest, bool) {
			// AN ARTEFACT OF EVERY PARTITION, so what decides is whether
			// the donor serves it.
			return statelog.Manifest{V: statelog.ManifestVersion, Layout: 1,
				Partition: p.String(), Artifact: "snapshot-1-1.db"}, true
		},
		Path: func(statelog.Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	ask := func(req statelog.OfferRequest) []statelog.Offer {
		offers, err := statelog.CollectOffers(t.Context(), q.Conn(), req, 300*time.Millisecond)
		if err != nil {
			t.Fatalf("CollectOffers: %v", err)
		}
		return offers
	}
	deadline := time.Now().Add(10 * time.Second)
	var offers []statelog.Offer
	for len(offers) == 0 && time.Now().Before(deadline) {
		offers = ask(statelog.OfferRequest{Layout: 1, Partition: served.String()})
	}
	if len(offers) != 1 || offers[0].Manifest.Partition != served.String() {
		t.Fatalf("the served partition was offered %+v, want its one artefact", offers)
	}
	for name, req := range map[string]statelog.OfferRequest{
		"a partition it keeps no copy of": {Layout: 1, Partition: other.String()},
		"a request naming no partition":   {Layout: 1},
	} {
		if got := ask(req); len(got) != 0 {
			t.Errorf("%s was answered with %+v", name, got)
		}
	}
}

// AN ARTEFACT IS ADOPTED ONLY AS THE PARTITION IT IS A COPY OF, AND ONLY IF IT
// NAMES EXACTLY THAT PARTITION'S LOGS.
//
// A recipient installs the file whole, so an artefact of another partition —
// or of the same name in another layout — is a copy of some other file's rows,
// and one naming a log the recipient's partition does not carry is too: a
// joiner that took it would believe it holds rows for a log that is not its
// own. Each is refused from the manifest alone, before any byte moves.
func TestAnArtefactIsAdoptedOnlyAsThePartitionItCopies(t *testing.T) {
	t.Parallel()
	build := map[string]statelog.Registered{"probe": {Domain: probeDomain{}, Log: logOf(probeDomain{}), Spec: specOf(probeDomain{})}}
	req := statelog.OfferRequest{Partition: statelog.EstatePartition.String(),
		Need: map[string]uint64{"probe": 1}}
	offer := func(change func(*statelog.Manifest)) statelog.Offer {
		m := statelog.Manifest{V: statelog.ManifestVersion, Partition: statelog.EstatePartition.String(),
			Domains: map[string]statelog.DomainPosition{"probe": {
				Stream: probeStream, Generation: 1, Seq: 4_200,
				RecordVersion: 1, Replay: statelog.ReplayStrict,
			}}}
		change(&m)
		return statelog.Offer{Manifest: m}
	}
	if err := offer(func(*statelog.Manifest) {}).Usable(req, build, nil); err != nil {
		t.Fatalf("the control artefact was refused: %v", err)
	}
	for name, change := range map[string]func(*statelog.Manifest){
		"another partition": func(m *statelog.Manifest) { m.Partition = "tracker.007" },
		"another layout":    func(m *statelog.Manifest) { m.Layout = 1 },
		"no partition":      func(m *statelog.Manifest) { m.Partition = "" },
		"a log the partition does not carry": func(m *statelog.Manifest) {
			m.Domains["tracker@tracker.007"] = statelog.DomainPosition{Seq: 9}
		},
	} {
		if err := offer(change).Usable(req, build, nil); err == nil {
			t.Errorf("an artefact of %s was accepted", name)
		}
	}
}

// A FETCH STREAMS THE ARTEFACT THE JOINER CHOSE, OR NONE.
//
// A joiner accepts an offer from its manifest, so the donor streams exactly
// that artefact: one replaced by a newer take since the offer is refused at
// once, naming what replaced it, rather than streamed and refused only after
// the transfer by a checksum it could never match.
func TestAFetchOfAReplacedArtefactIsRefusedBeforeItStreams(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	offers, err := statelog.CollectOffers(t.Context(), h.nc, statelog.OfferRequest{
		NodeID: "joiner", Partition: statelog.EstatePartition.String(),
	}, statelog.OfferWindow)
	if err != nil || len(offers) != 1 {
		t.Fatalf("CollectOffers = (%d, %v)", len(offers), err)
	}
	chosen := offers[0]
	chosen.Manifest.Artifact = "snapshot-1-1.db"
	_, err = statelog.FetchArtefact(t.Context(), h.nc, chosen, filepath.Join(t.TempDir(), "adopt.part"))
	if err == nil || !strings.Contains(err.Error(), "410") {
		t.Fatalf("a fetch of an artefact the donor no longer holds answered %v, "+
			"want its refusal", err)
	}
}
