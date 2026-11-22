package engine

import (
	"crypto/rand"
	"errors"
	"strings"
	"sync"
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
	"github.com/crewlet/crewlet/internal/seat"
)

// scaledRate is the case's link: twice the object store's floor (a mebibyte
// in objstore.MiBPace). The leaf's acknowledged bytes move in the RELAY's
// steps, a loopback segment of some 64 KiB at a time (linktest's package
// doc), where a real far end acknowledges every segment or two of 1,460
// bytes. At the floor the meter's line over the probe's bound is about
// 120 KiB, two such steps, so one step more or less in a span decides the
// verdict; at twice the floor a span holds four or more.
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
	resource := coord.SeatResource(uuid.New())
	lease, _, err := f.leafStore.TryAcquire(t.Context(), resource, coord.AcquireOptions{Owner: "node:leaf",
		TTL: f.leafStore.TTL(), Ungated: true, Protocol: coord.ProtocolVersion})
	if err != nil || lease == nil {
		t.Fatalf("claim %s = (%v, %v)", resource, lease, err)
	}
	return f, resource, lease
}

// bulkOnTheDataLink sends outlastingBytes from the leaf's queue connection to
// the member across the data link, and answers on the channel it returns once
// the member has them, after waiting until the link carries more of them than
// a connection's window; before is what the link had carried toward the
// member when the upload began.
func (f leafAcross) bulkOnTheDataLink(t *testing.T) (done <-chan error, before int64) {
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
	before = f.carried()
	answered := make(chan error, 1)
	go func() {
		_, err := near.Request(subject, body, 2*time.Minute)
		answered <- err
	}()
	pollUntil(t, time.Minute, "the bulk on the data link", func() bool {
		return f.carried() > before+scaledInflight
	})
	return answered, before
}

// carried is what the link has carried of the leaf's data connections toward
// the member.
func (f leafAcross) carried() int64 {
	return f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried
}

// A LARGE WRITE ON THE DATA LINK KEEPS THE NODE'S SEATS (#1035): behind a
// first-in, first-out link, a leaf's upload whose crossing outlasts
// the TTL holds the data path's probe unanswered for the whole crossing, past
// its bound. The link moves throughout, so the data path stays proven, and the
// renewals, on the lease link, keep landing: the seat's epoch never moves.
// Decided by the verdict, read every few milliseconds, and every renewal read
// back to back while the upload crosses, never by a clock.
//
// Red with ruling 2's letter (a probe out past its bound refuses, the link's
// movement aside): the verdict refuses some five seconds in. Red on main, whose
// renewals rode the data link: each waits the upload out, past its term.
func TestALargeWriteOnTheDataLinkKeepsTheNodesSeats(t *testing.T) {
	t.Parallel()
	f, resource, lease := overAScaledLink(t)
	q := f.leaf.Queue.(*jetstream.Queue)
	done, _ := f.bulkOnTheDataLink(t)
	stop := watchedForRefusal(t, q)
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
		renewed, err := f.leafStore.Renew(t.Context(), resource, "node:leaf", lease.Epoch, f.leafStore.TTL())
		if err != nil || !renewed {
			t.Fatalf("renewal %d behind the upload = (%v, %v), want it landed", renewals+1, renewed, err)
		}
		renewals++
	}
	if err := stop(); err != nil {
		t.Fatalf("while the upload crossed the data path read %v: the link moved throughout", err)
	}
	got, err := f.memberStore.Get(t.Context(), resource)
	if err != nil || got == nil || got.Epoch != lease.Epoch || got.Owner != "node:leaf" {
		t.Errorf("after the upload the seat reads (%v, %v) on the member, want the leaf's at epoch %d",
			got, err, lease.Epoch)
	}
}

// watchedForRefusal reads q's data path every few milliseconds, from its own
// goroutine, until stop (or the case's end), which ends it and answers the
// first refusal read. The log line data_path_unproven would show a shorter
// flap, but the queue logs to the process's logger, which a parallel case
// cannot capture.
func watchedForRefusal(t *testing.T, q *jetstream.Queue) (stop func() error) {
	first := make(chan error, 1)
	quit, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		tick := time.NewTicker(5 * time.Millisecond) //fixedwait:liveness
		defer tick.Stop()
		for {
			if err := q.DataPath(); err != nil {
				first <- err
				return
			}
			select {
			case <-quit:
				return
			case <-tick.C:
			}
		}
	}()
	stop = sync.OnceValue(func() error {
		close(quit)
		<-ended
		select {
		case err := <-first:
			return err
		default:
			return nil
		}
	})
	t.Cleanup(func() { _ = stop() })
	return stop
}

// A DATA LINK THAT DIES MID-WRITE IS REFUSED WITHIN THE PROBE'S BOUND: the
// link had been moving under an upload, so its silence counts from its last
// byte, and once it has carried nothing for the probe's bound the hold is
// refused for that silence, not left to the moving cap minutes later. The
// seat is still the leaf's when it is: refused before its TTL ran out.
//
// The upload moves for a probe period and a window before the link dies, so
// a probe went out while it moved; the minute is a liveness bound, far inside
// the cap (some four minutes), so the refusal seen is the silence clause's.
//
// Mutant: silence counted from the probe's send, not the link's last byte —
// the link moved since, so only the cap refuses, past the minute.
func TestADataLinkThatDiesMidWriteIsRefusedWithinTheProbesBound(t *testing.T) {
	t.Parallel()
	f, resource, lease := overAScaledLink(t)
	_, before := f.bulkOnTheDataLink(t)
	// The data path's probe period (jetstream's dataPathProbeEvery), in bytes
	// of the link: no faster than scaledRate, so a tick fell inside it.
	probePeriod := config.MinLeaseTTL / seat.HeartbeatRatio
	moved := before + scaledRate*int64(probePeriod/time.Millisecond)/1000 + scaledInflight
	pollUntil(t, time.Minute, "a probe period of the upload on the data link", func() bool {
		return f.carried() > moved
	})
	if at := f.carried() - before; at >= outlastingBytes-scaledInflight {
		t.Fatalf("the link carried %d bytes of the %d-byte upload before it died: the case needs it "+
			"to die mid-write", at, outlastingBytes)
	}
	f.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
	t.Cleanup(func() { f.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })

	refused := renewUntilRefused(t, f.leafStore, resource, "node:leaf", lease.Epoch)
	if !errors.Is(refused, coord.ErrUnavailable) || !errors.Is(refused, jetstream.ErrDataPathUnproven) {
		t.Fatalf("a renewal across a data link dead mid-write = %v, want unknown for the unproven "+
			"data path", refused)
	}
	if !strings.Contains(refused.Error(), "carried nothing") {
		t.Errorf("the refusal %q does not name the link's silence", refused)
	}
	got, err := f.memberStore.Get(t.Context(), resource)
	if err != nil || got == nil || got.Epoch != lease.Epoch || got.Owner != "node:leaf" {
		t.Errorf("at the refusal the seat reads (%v, %v) on the member, want still the leaf's at "+
			"epoch %d", got, err, lease.Epoch)
	}
}
