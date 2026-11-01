package kv

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// A VALVE UNDER CONTENTION ALWAYS ANSWERS.
//
// Every attempt a write to the valve loses is another node's increment, so it
// either wins room or reads the window full — the two answers the valve
// exists to give. Sixteen attempts answered "could not say" instead once more
// than sixteen increments overlapped a caller's attempts, and the
// notification path fails open on that: the valve opened under exactly the
// pressure it is for. Twenty losses here, each a real increment by another
// node, against a limit with room for them and one without.
func TestAValveUnderContentionAlwaysAnswers(t *testing.T) {
	t.Parallel()
	at := time.Now()
	for _, tc := range []struct {
		name    string
		limit   int
		allowed bool
		lost    int64
		count   int
	}{
		// Seeded with one, so twenty peers' increments leave room: the
		// caller's own lands twenty-second.
		{name: "with room it is let through", limit: 40, allowed: true, lost: 20, count: 22},
		// The ninth peer increment fills the window; the caller's next
		// read finds it full and stops racing.
		{name: "without room it is refused", limit: 10, allowed: false, lost: 9, count: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := openFleet(t, embeddedNATS(t))
			if ok, err := store.Allow(t.Context(), "seat:hot", tc.limit, time.Second, at); err != nil || !ok {
				t.Fatalf("seeding the window = (%v, %v)", ok, err)
			}
			key := counterKey("seat:hot", time.Second, at)
			peers := &peerIncrements{KeyValue: store.rate, key: key}
			peers.remaining.Store(20)
			store.rate = peers

			ok, err := store.Allow(t.Context(), "seat:hot", tc.limit, time.Second, at)
			if err != nil {
				t.Fatalf("Allow = %v after %d lost races: contention answered as a valve that "+
					"could not say", err, peers.lost.Load())
			}
			if ok != tc.allowed {
				t.Fatalf("Allow = %v, want %v", ok, tc.allowed)
			}
			if got := peers.lost.Load(); got != tc.lost {
				t.Fatalf("the caller lost %d races, want %d", got, tc.lost)
			}
			if got := rateCount(t, peers.KeyValue, key); got != tc.count {
				t.Fatalf("the window counts %d, want %d", got, tc.count)
			}
		})
	}
}

// THE FLEET STORE'S OTHER RACES OUTLAST MORE LOSSES THAN SIXTEEN.
//
// Each lost race is a store that answered, so none of these is an outage
// until the shared bound says it is ([fleetCASAttempts]), sized for the nodes
// writing a key rather than for the calls: a channel's count, coalesced on its
// line ([writeLines]), and an unfollow ([casRetry]), each made to lose twenty
// in a row. The custody claim and the object backend's record lose a race only
// to a key removed between a refused create and the leader's read of it, which
// no bucket in front of the leader can stage; they take the same [casRetry].
func TestTheFleetStoresOtherRacesOutlastMoreLossesThanSixteen(t *testing.T) {
	t.Parallel()
	const losses = 20
	for _, tc := range []struct {
		name   string
		bucket func(*FleetStore) *jetstream.KeyValue
		key    string
		setup  func(t *testing.T, f *FleetStore)
		run    func(t *testing.T, f *FleetStore)
	}{{
		name:   "a channel's count",
		bucket: func(f *FleetStore) *jetstream.KeyValue { return &f.channels },
		key:    encodeKey("ch-1"),
		setup: func(t *testing.T, f *FleetStore) {
			if err := f.OpenChannel(t.Context(), coord.Channel{
				ID: "ch-1", Requester: "a", Target: "b", OpenedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("OpenChannel: %v", err)
			}
		},
		run: func(t *testing.T, f *FleetStore) {
			ch, found, err := f.CountChannelMessage(t.Context(), "ch-1", time.Now())
			if err != nil || !found || ch.Messages != 1 {
				t.Fatalf("CountChannelMessage = (%+v, %v, %v), want one message counted", ch, found, err)
			}
		},
	}, {
		name:   "an unfollow",
		bucket: func(f *FleetStore) *jetstream.KeyValue { return &f.follows },
		key:    followKey("slack", "ceo", "C1", "T1"),
		setup: func(t *testing.T, f *FleetStore) {
			if err := f.Follow(t.Context(), "slack", "ceo", "C1", "T1", "mention", time.Now()); err != nil {
				t.Fatalf("Follow: %v", err)
			}
		},
		run: func(t *testing.T, f *FleetStore) {
			removed, err := f.Unfollow(t.Context(), "slack", "ceo", "C1", "T1")
			if err != nil || !removed {
				t.Fatalf("Unfollow = (%v, %v), want the follow removed", removed, err)
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := openFleet(t, embeddedNATS(t))
			if tc.setup != nil {
				tc.setup(t, store)
			}
			slot := tc.bucket(store)
			losing := &loseWrites{KeyValue: *slot, key: tc.key}
			losing.remaining.Store(losses)
			*slot = losing
			tc.run(t, store)
			if got := losing.lost.Load(); got != losses {
				t.Fatalf("%d writes lost, want the %d injected: the fault did not reach the write",
					got, losses)
			}
		})
	}
}

// A RACE WHOSE CALLER HANGS UP IS TOLD SO, AND WRITES NOTHING.
//
// Between a lost attempt and the next nothing of a write has landed, so a
// caller that stops listening there is withdrawn rather than raced on for: told
// its own context's error, and the record left as it was. A write that loses
// every attempt is the other end of the same rule, and fails closed after
// exactly [fleetCASAttempts] of them
// (TestAWriteThatNeverSettlesFailsClosedAsAnOutage).
func TestARaceEndsAtTheCallersHangUp(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	openCountedChannel(t, store, "k")
	losing := &loseWrites{KeyValue: store.channels, key: encodeKey("k"), forever: true}
	store.channels = losing

	ctx, cancel := context.WithCancel(t.Context())
	answered := make(chan error, 1)
	go func() {
		_, _, err := store.CountChannelMessage(ctx, "k", time.Now())
		answered <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for losing.lost.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the race did not lose twice within 30s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-answered:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("CountChannelMessage = %v, want the store unavailable for the cancelled context", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the race went on for a caller no longer listening")
	}
	store.channels = losing.KeyValue
	if ch, found, err := store.Channel(t.Context(), "k"); err != nil || !found || ch.Messages != 1 {
		t.Fatalf("Channel = (%+v, %v, %v), want only the message counted before", ch, found, err)
	}
}

// openCountedChannel opens channel id on store with one message already
// counted, so a write that loses every race leaves a count to compare with.
func openCountedChannel(t *testing.T, store *FleetStore, id string) {
	t.Helper()
	if err := store.OpenChannel(t.Context(), coord.Channel{
		ID: id, Requester: "a", Target: "b", OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	if _, _, err := store.CountChannelMessage(t.Context(), id, time.Now()); err != nil {
		t.Fatalf("CountChannelMessage: %v", err)
	}
}

// rateCount reads one window's count.
func rateCount(t *testing.T, kv jetstream.KeyValue, key string) int {
	t.Helper()
	entry, err := kv.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("read the window: %v", err)
	}
	var record rateRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		t.Fatalf("decode the window: %v", err)
	}
	return record.Count
}

// peerIncrements is a rate bucket on which the next `remaining` writes to one
// key lose their race — each to another node's increment, landed just before
// it, which is what every lost race on a valve is.
type peerIncrements struct {
	jetstream.KeyValue
	key       string
	remaining atomic.Int64
	lost      atomic.Int64
}

func (k *peerIncrements) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key != k.key || k.remaining.Add(-1) < 0 {
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	entry, err := k.KeyValue.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	var record rateRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, err
	}
	raw, err := encodeRate(rateRecord{Count: record.Count + 1})
	if err != nil {
		return 0, err
	}
	if _, err := k.KeyValue.Update(ctx, key, raw, entry.Revision()); err != nil {
		return 0, err
	}
	k.lost.Add(1)
	return 0, jetstream.ErrKeyRevisionMismatch
}

// loseWrites is a bucket on which writes to one key lose their race and write
// nothing — an update or a delete refused at the revision it was made at. It
// loses the next `remaining`, or every one when forever is set.
type loseWrites struct {
	jetstream.KeyValue
	key       string
	forever   bool
	remaining atomic.Int64
	lost      atomic.Int64
}

func (k *loseWrites) loses(key string) bool {
	if key != k.key || (!k.forever && k.remaining.Add(-1) < 0) {
		return false
	}
	k.lost.Add(1)
	return true
}

func (k *loseWrites) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if !k.loses(key) {
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	return 0, jetstream.ErrKeyRevisionMismatch
}

func (k *loseWrites) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.loses(key) {
		return jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Delete(ctx, key, opts...)
}
