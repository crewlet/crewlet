package kv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// A CHAT READ RACE LOST ON A REPLICATED BUCKET IS A LOST RACE, NOT AN OUTAGE.
//
// A three-replica stream refuses a stale write with one of TWO codes: 10071
// when the write it lost to has been applied, and 10164 when the subject
// still has that write in flight — which is what two tabs flushing one
// person's record at once produce. The read state's retry matched 10071
// alone, so on every fleet a race caught in flight fell through to
// "unavailable" and the flush failed. The conformance suite could not see it:
// it runs against one server and through a leaf link, both solo streams, and
// a solo stream never answers 10164.
//
// The broker decides which code a loser gets, so the case does not try to
// stage one. It COLLECTS the refusals a real three-replica bucket hands out —
// the applied loss from a sequential stale write, the in-flight loss from
// concurrent writes at one revision, retried until the broker produces one
// (it does on most bursts), and the create a peer beat — and holds the
// classifier to every one of them. Then it drives the whole loop on the same
// bucket with a burst of flushes for one person at once, where a round can
// lose to a winner still in flight more than once: bounded by a count of
// rounds, sixteen flushes exhausted sixteen about one burst in a hundred and
// fifty. The loop races for the store's patience instead
// ([FleetStore.retryRaces]), which race_test.go holds deterministically,
// since a cluster stages its race only sometimes.
func TestAChatReadRaceLostOnAReplicatedBucketIsRetried(t *testing.T) {
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
		RateWindow: time.Minute, MaxClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, BudgetRetention: time.Minute, CooldownMax: time.Hour,
		RebaseRetention:  10 * time.Minute,
		StatusFreshness:  10 * time.Minute,
		CustodyRetention: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("OpenFleet on a three-replica cluster: %v", err)
	}
	key := chatReadKey("founder")
	put := func() uint64 {
		t.Helper()
		revision, err := f.chatReads.Put(ctx, key, []byte(`{}`))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		return revision
	}

	refusals := map[string]error{}

	// THE APPLIED LOSS: a write at a revision a peer has already moved past.
	stale := put()
	put()
	_, refusals["a write at a revision already moved past"] = f.chatReads.Update(ctx, key, []byte(`{}`), stale)

	// THE IN-FLIGHT LOSS, which only a replicated stream answers.
	refusals["a write racing one still in flight"] = refusalWithCode(t, f, key,
		jetstream.JSErrCodeStreamWrongLastSequenceConstant)

	// THE CREATE A PEER BEAT: the caller read no record, and one is there.
	_, refusals["a create over a record a peer wrote"] = f.create(ctx, f.chatReads, key, []byte(`{}`))

	for name, refusal := range refusals {
		if refusal == nil {
			t.Fatalf("%s was accepted, so there is no refusal to classify", name)
		}
		if !chatReadRaceLost(refusal) {
			t.Errorf("%s answered %v on a three-replica bucket, and the retry reads "+
				"it as an outage: the flush fails instead of being merged again", name, refusal)
		}
	}
	for name, other := range map[string]error{
		"a cancelled request": context.Canceled,
		"another API error":   &jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound},
	} {
		if chatReadRaceLost(other) {
			t.Errorf("%s (%v) reads as a lost race, so a store that cannot be "+
				"reached is retried as if somebody had written first", name, other)
		}
	}

	// AND THE WHOLE LOOP: sixty-four flushes for one person at once, four
	// times the sixteen a count of rounds was measured to hold, each naming a
	// room of its own, so a flush that was dropped is a cursor missing from
	// the record. Every round one of them loses is a write another of them
	// won, and the loop races for a time rather than a count, so all of them
	// land.
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
}

// refusalWithCode fires bursts of concurrent writes at one revision of key
// until one of them is refused with code, and returns that refusal as the
// client handed it back.
//
// Bounded, because the broker decides which code each loser gets: a burst of
// sixteen drew the in-flight code on most attempts when measured, so a hundred
// bursts without one says the PREMISE no longer holds — and the case fails
// saying so, rather than holding the classifier to nothing.
func refusalWithCode(t *testing.T, f *FleetStore, key string, code jetstream.ErrorCode) error {
	t.Helper()
	const bursts, writers = 100, 16
	for range bursts {
		revision, err := f.chatReads.Put(t.Context(), key, []byte(`{}`))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			found error
		)
		for range writers {
			wg.Go(func() {
				_, err := f.chatReads.Update(t.Context(), key, []byte(`{}`), revision)
				var api *jetstream.APIError
				if errors.As(err, &api) && api.ErrorCode == code {
					mu.Lock()
					found = err
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if found != nil {
			return found
		}
	}
	t.Fatalf("premise: %d bursts of %d concurrent writes at one revision on a "+
		"three-replica bucket never drew code %d, so this case would hold the "+
		"classifier to a refusal the broker no longer gives", bursts, writers, code)
	return nil
}
