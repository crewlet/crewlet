package kv

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// SIXTY-FOUR FLUSHES FOR ONE PERSON ON A REPLICATED BUCKET ALL LAND.
//
// Concurrent flushes of one record draw a replicated stream's in-flight answer
// (the leader declining to compare a write while another to the subject is in
// flight), which no solo stream gives, so the conformance suite cannot see it.
// Sixty-four flushes run here on three members, each naming its own room, so a
// dropped flush is a missing cursor. All land, within the shared bound
// ([casRetry], [fleetCASAttempts]), and every refusal a flush's own code met
// was one the leader DECIDED (settle.go). race_test.go holds the bound
// deterministically, since a cluster stages its race only sometimes.
func TestSixtyFourFlushesForOnePersonOnAReplicatedBucketAllLand(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartCluster(t, 3, js.Config{})
	ctx := t.Context()
	nc, err := c.Servers[0].Conn()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	client, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	f, err := OpenFleet(ctx, client, FleetConfig{
		BucketPrefix: "chatreadrace", Clustered: true, Replicas: 3, Reach: InProcess,
		MaxRateWindow: time.Minute, MaxClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, BudgetRetention: time.Minute,
		RebaseRetention:  10 * time.Minute,
		StatusFreshness:  10 * time.Minute,
		CustodyRetention: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("OpenFleet on a three-replica cluster: %v", err)
	}
	seen := &refusalsSeen{KeyValue: f.chatReads}
	f.chatReads = seen

	const flushes = 64
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, flushes)
	for i := range flushes {
		wg.Go(func() {
			_, err := f.AdvanceChatRead(ctx, "designer", coord.ChatReadDelta{
				Cursors: map[string]int64{fmt.Sprintf("room-%d", i): int64(i + 1)}, At: at,
			})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent flush on a replicated bucket failed: %v", err)
		}
	}
	state, err := f.ChatRead(ctx, "designer")
	if err != nil {
		t.Fatalf("ChatRead: %v", err)
	}
	if len(state.Cursors) != flushes {
		t.Fatalf("%d concurrent flushes left %d cursors (%v): a flush was lost",
			flushes, len(state.Cursors), state.Cursors)
	}
	lost, inFlight := seen.counts()
	if lost == 0 {
		t.Fatalf("premise: %d flushes at once lost no round to each other, so this "+
			"case raced nothing", flushes)
	}
	if inFlight != 0 {
		t.Errorf("%d refusal(s) reaching the record's own code were the leader's "+
			"in-flight answer, which decides nothing: the bucket handle is not "+
			"waiting it out", inFlight)
	}
}

// refusalsSeen counts the refusals its conditional writes are answered with,
// as the code above the handle receives them: lost races, and in-flight answers.
type refusalsSeen struct {
	jetstream.KeyValue

	mu       sync.Mutex
	lost     int
	inFlight int
}

func (k *refusalsSeen) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	landed, err := k.KeyValue.Update(ctx, key, value, revision)
	if err != nil {
		k.mu.Lock()
		if lostUpdateRace(err) {
			k.lost++
		}
		if jsinflight.InFlight(err) {
			k.inFlight++
		}
		k.mu.Unlock()
	}
	return landed, err
}

func (k *refusalsSeen) counts() (lost, inFlight int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lost, k.inFlight
}
