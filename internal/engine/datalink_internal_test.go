package engine

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// scaledRate is the case's link: twice the object store's floor, a mebibyte
// in objstore.MiBPace, so bulk crossing it reads moving to the data path's
// meter, which asks 0.7 of the floor, whatever the steps its acknowledged
// bytes move in.
const scaledRate = 2 * (1 << 20) * int64(time.Second) / int64(objstore.MiBPace)

// scaledInflight is what the case's first-in, first-out link reads of a
// connection ahead of carrying it, its congestion window: what a renewal on
// the lease link queues behind of the data link's bulk, under half a second
// at scaledRate.
const scaledInflight = 32 << 10

// outlastingBytes is a bulk whose crossing outlasts the floor TTL: 1.5 MiB,
// 22.5 s at scaledRate against config.MinLeaseTTL's 15 s. The link paces it,
// so the crossing takes that long whatever the runner's speed.
const outlastingBytes = 1536 << 10

// overAScaledLink is a member and a leaf of it across one first-in, first-out
// link at scaledRate, holding leases at the floor TTL, and a seat the leaf
// holds.
func overAScaledLink(t *testing.T) (leafAcross, string, *coord.Lease) {
	t.Helper()
	f := openLeafAcross(t, config.MinLeaseTTL, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedFIFO(t, target, scaledRate, scaledInflight)
	})
	if got := f.leafStore.TTL(); got != config.MinLeaseTTL {
		t.Fatalf("the leaf holds leases at %v, want the floor TTL %v", got, config.MinLeaseTTL)
	}
	seat := coord.SeatResource(uuid.New())
	lease, _, err := f.leafStore.TryAcquire(t.Context(), seat, coord.AcquireOptions{Owner: "node:leaf",
		TTL: f.leafStore.TTL(), Ungated: true, Protocol: coord.ProtocolVersion})
	if err != nil || lease == nil {
		t.Fatalf("claim %s = (%v, %v)", seat, lease, err)
	}
	return f, seat, lease
}

// bulkOnTheDataLink sends outlastingBytes from the leaf's queue connection to
// the member across the data link, and answers on the channel it returns once
// the member has them, after waiting until the link carries more of them than
// a connection's window.
func (f leafAcross) bulkOnTheDataLink(t *testing.T) <-chan error {
	t.Helper()
	far := f.member.Queue.(*jetstream.Queue).Conn()
	near := f.leaf.Queue.(*jetstream.Queue).Conn()
	const subject = "bulk.across.the.data.link"
	sub, err := far.Subscribe(subject, func(m *nats.Msg) { _ = m.Respond(nil) })
	if err != nil {
		t.Fatalf("answer the bulk on the member: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := far.Flush(); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, time.Minute, "the member's interest across the data link", func() bool {
		_, err := near.Request(subject, nil, time.Second)
		return err == nil
	})
	body := make([]byte, outlastingBytes)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	carried := func() int64 {
		return f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried
	}
	before := carried()
	done := make(chan error, 1)
	go func() {
		_, err := near.Request(subject, body, 2*time.Minute)
		done <- err
	}()
	pollUntil(t, time.Minute, "the bulk on the data link", func() bool {
		return carried() > before+scaledInflight
	})
	return done
}

// A LARGE WRITE ON THE DATA LINK KEEPS THE NODE'S SEATS (#1035): behind a
// first-in, first-out link, a leaf's upload whose crossing outlasts
// the TTL holds the data path's probe unanswered for the whole crossing, past
// its bound. The link moves throughout, so the data path stays proven, and the
// renewals, on the lease link, keep landing: the seat's epoch never moves.
// Decided by the verdict and every renewal read back to back while the upload
// crosses, never by a clock.
//
// Red with ruling 2's letter (a probe out past its bound refuses, the link's
// movement aside): the verdict refuses some five seconds in. Red on main, whose
// renewals rode the data link: each waits the upload out, past its term.
func TestALargeWriteOnTheDataLinkKeepsTheNodesSeats(t *testing.T) {
	t.Parallel()
	f, seat, lease := overAScaledLink(t)
	q := f.leaf.Queue.(*jetstream.Queue)
	done := f.bulkOnTheDataLink(t)
	renewals := 0
	for crossing := true; crossing; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the upload: %v", err)
			}
			crossing = false
		default:
		}
		if err := q.DataPath(); err != nil {
			t.Fatalf("after %d renewals behind the upload, the data path reads %v while its link "+
				"moves", renewals, err)
		}
		renewed, err := f.leafStore.Renew(t.Context(), seat, "node:leaf", lease.Epoch, f.leafStore.TTL())
		if err != nil || !renewed {
			t.Fatalf("renewal %d behind the upload = (%v, %v), want it landed", renewals+1, renewed, err)
		}
		renewals++
	}
	got, err := f.memberStore.Get(t.Context(), seat)
	if err != nil || got == nil || got.Epoch != lease.Epoch || got.Owner != "node:leaf" {
		t.Errorf("after the upload the seat reads (%v, %v) on the member, want the leaf's at epoch %d",
			got, err, lease.Epoch)
	}
}

// A DATA LINK THAT DIES MID-WRITE IS REFUSED WITHIN THE PROBE'S BOUND: the
// link had been moving under an upload, so its silence counts from its last
// byte, and once it has carried nothing for the probe's bound the hold is
// refused for that silence, not left to the moving cap minutes later. The
// seat is still the leaf's when it is: refused before its TTL ran out.
//
// The case's minute is a liveness bound, far inside the cap (some four
// minutes), so the refusal it sees is the silence clause's, as it names.
//
// Mutant: silence counted from the probe's send, not the link's last byte —
// the link moved since, so only the cap refuses, past the minute.
func TestADataLinkThatDiesMidWriteIsRefusedWithinTheProbesBound(t *testing.T) {
	t.Parallel()
	f, seat, lease := overAScaledLink(t)
	_ = f.bulkOnTheDataLink(t)
	f.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
	t.Cleanup(func() { f.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })

	refused := renewUntilRefused(t, f.leafStore, seat, "node:leaf", lease.Epoch)
	if !errors.Is(refused, coord.ErrUnavailable) || !errors.Is(refused, jetstream.ErrDataPathUnproven) {
		t.Fatalf("a renewal across a data link dead mid-write = %v, want unknown for the unproven "+
			"data path", refused)
	}
	if !strings.Contains(refused.Error(), "carried nothing") {
		t.Errorf("the refusal %q does not name the link's silence", refused)
	}
	got, err := f.memberStore.Get(t.Context(), seat)
	if err != nil || got == nil || got.Epoch != lease.Epoch || got.Owner != "node:leaf" {
		t.Errorf("at the refusal the seat reads (%v, %v) on the member, want still the leaf's at "+
			"epoch %d", got, err, lease.Epoch)
	}
}
