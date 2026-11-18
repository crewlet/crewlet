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

// A VALVE UNDER CONTENTION ALWAYS ANSWERS. Every lost write is another node's
// increment, so the caller either wins room or reads the window full, never
// "could not say": the notification path fails open on that, opening the valve
// under exactly the pressure it is for. Twenty real peer increments, against a
// limit with room for them and one without.
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
		// Seeded with one; the caller's own lands twenty-second.
		{name: "with room it is let through", limit: 40, allowed: true, lost: 20, count: 22},
		// The ninth peer increment fills the window; the caller stops racing.
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

// THE FLEET STORE'S OTHER RACES OUTLAST MORE LOSSES THAN SIXTEEN. A lost race
// is a store that answered, so none is an outage within [fleetCASAttempts]: a
// channel's count ([writeLines]), a follow, and a chat read flush and forget
// ([casRetry]) each lose twenty in a row. The custody claim and the object
// backend's record take the same [casRetry] but cannot be staged here: they
// lose only to a key removed between a refused create and the leader's read.
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
		// Already recorded, so a re-assert writes at the revision read.
		name:   "a follow",
		bucket: func(f *FleetStore) *jetstream.KeyValue { return &f.follows },
		key:    followKey("slack", "ceo", "C1", "T2"),
		setup: func(t *testing.T, f *FleetStore) {
			if _, err := f.Follow(t.Context(), "slack", "ceo", "C1", "T2", time.Now(), overwrite("collective")); err != nil {
				t.Fatalf("Follow: %v", err)
			}
		},
		run: func(t *testing.T, f *FleetStore) {
			got, err := f.Follow(t.Context(), "slack", "ceo", "C1", "T2", time.Now(), overwrite("mention"))
			if err != nil || got != "mention" {
				t.Fatalf("Follow = (%q, %v), want the mention written", got, err)
			}
		},
	}, {
		// The record exists, so the flush is an update over it.
		name:   "a chat read flush",
		bucket: func(f *FleetStore) *jetstream.KeyValue { return &f.chatReads },
		key:    chatReadKey("designer"),
		setup: func(t *testing.T, f *FleetStore) {
			if _, err := f.AdvanceChatRead(t.Context(), "designer", coord.ChatReadDelta{
				Cursors: map[string]int64{"room-a": 1}, At: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("AdvanceChatRead: %v", err)
			}
		},
		run: func(t *testing.T, f *FleetStore) {
			state, err := f.AdvanceChatRead(t.Context(), "designer", coord.ChatReadDelta{
				Cursors: map[string]int64{"room-b": 7}, At: time.Now().UTC(),
			})
			if err != nil || state.Cursors["room-a"] != 1 || state.Cursors["room-b"] != 7 {
				t.Fatalf("AdvanceChatRead = (%v, %v), want both rooms' cursors", state.Cursors, err)
			}
		},
	}, {
		// Stamped absent long ago, so the forget is a purge at the
		// revision it judged.
		name:   "a chat read forget",
		bucket: func(f *FleetStore) *jetstream.KeyValue { return &f.chatReads },
		key:    chatReadKey("leaver"),
		setup: func(t *testing.T, f *FleetStore) {
			if _, err := f.AdvanceChatRead(t.Context(), "leaver", coord.ChatReadDelta{
				Cursors: map[string]int64{"room-a": 1}, At: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("AdvanceChatRead: %v", err)
			}
			if _, err := f.SetChatReadAbsence(t.Context(), "leaver", time.Now().Add(-48*time.Hour)); err != nil {
				t.Fatalf("SetChatReadAbsence: %v", err)
			}
		},
		run: func(t *testing.T, f *FleetStore) {
			removed, err := f.ForgetChatRead(t.Context(), "leaver", time.Now().Add(-24*time.Hour))
			if err != nil || !removed {
				t.Fatalf("ForgetChatRead = (%v, %v), want the record removed", removed, err)
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

// A RACE WHOSE CALLER HANGS UP IS TOLD SO, AND WRITES NOTHING: between lost
// attempts nothing has landed, so the caller gets its context's error and the
// record is unchanged. A write losing every attempt fails closed after
// [fleetCASAttempts] (TestAWriteThatNeverSettlesFailsClosedAsAnOutage).
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
// key each lose to another node's increment, landed just before.
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

// loseWrites is a bucket whose updates and purges of one key lose their race
// and write nothing: the next `remaining`, or every one when forever is set.
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

func (k *loseWrites) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.loses(key) {
		return jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Purge(ctx, key, opts...)
}
