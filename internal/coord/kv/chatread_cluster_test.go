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
// Two tabs flushing one person's record at once is what draws a replicated
// stream's in-flight answer — the leader declining to compare a write while
// another to the same subject is still in flight — and the conformance suite
// cannot see it: it runs against one server and through a leaf link, both
// solo streams, and a solo stream never gives that answer. So the burst is
// run here, on three members: sixty-four flushes at once, four times the
// sixteen rounds a lost race was once allowed, each naming a room of its own
// so a flush that was dropped is a cursor missing from the record.
//
// Every round one of them loses is a write another of them won, the loop races
// within the bound every fleet key shares ([casRetry], [fleetCASAttempts]) —
// four times the flushes here, each lost round followed by a window of round
// trips — and the bucket handle waits out the in-flight answer inside the
// write (settle.go) — so all of them land, and every refusal a flush's own
// code met was one the leader DECIDED. race_test.go holds the bound
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
		BucketPrefix: "chatreadrace", Clustered: true, Replicas: 3,
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

// refusalsSeen is a bucket handle that counts the refusals its conditional
// writes are answered with, as the code above the handle receives them: every
// lost race, and how many refusals were the leader's in-flight answer.
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
