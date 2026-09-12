package statelog_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/nats-io/nats.go"
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
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.Conn(), nil },
		Newest: func() (statelog.Manifest, bool) { return h.manifest, true },
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
		break_ func(*statelog.Offer)
		names  string
	}{
		"a domain this build registers and the artefact never named": {
			break_: func(o *statelog.Offer) { delete(o.Manifest.Domains, "probe") },
			names:  "adopted wholesale",
		},
		"a donor that read more record versions than this build": {
			break_: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.RecordVersion = 9
				o.Manifest.Domains["probe"] = p
			},
			names: "never can",
		},
		"a different replay protocol": {
			break_: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Replay = statelog.ReplayCompacted
				o.Manifest.Domains["probe"] = p
			},
			names: "permanent stall",
		},
		"another generation's sequences": {
			break_: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Generation = 2
				o.Manifest.Domains["probe"] = p
			},
			names: "different history",
		},
		"a position below the floor": {
			break_: func(o *statelog.Offer) {
				p := o.Manifest.Domains["probe"]
				p.Seq = 3_000
				o.Manifest.Domains["probe"] = p
			},
			names: "already gone",
		},
		"a manifest version this build does not read": {
			break_: func(o *statelog.Offer) { o.Manifest.V = 99 },
			names:  "version 99",
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := base()
			tc.break_(&o)
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
