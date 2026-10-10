package engine

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
)

// servingProfile is a node serving the API whose key table is value, under the
// one keyring key every node of these cases holds.
func servingProfile(t *testing.T, id, value string) placement.NodeProfile {
	t.Helper()
	ring := secrets.Keyring{Keys: map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}}
	f, err := ring.Fingerprint("test", []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return placement.NodeProfile{ID: id, Auth: placement.AuthDigest{Serves: true, Keys: f}}
}

// present takes a node's presence lease for ttl, carrying its profile as every
// node's heartbeat does.
func present(t *testing.T, store *coordmemory.Backend, p placement.NodeProfile, ttl time.Duration) {
	t.Helper()
	lease, _, err := store.TryAcquire(t.Context(), coord.NodeResource(p.ID), coord.AcquireOptions{
		Owner: p.ID + ":test", TTL: ttl, Ungated: true, Meta: p.Meta(),
	})
	if err != nil || lease == nil {
		t.Fatalf("presence for %s: %v", p.ID, err)
	}
}

// awaitDivergence waits until the view's answer gives want, failing with the
// last answer if it never does — a poll on the answer itself, because a
// listing is recorded after the call that made it returns.
func awaitDivergence(t *testing.T, view *coord.LeaseView, self placement.NodeProfile, want [][]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := authDivergence(view, self)
		if slices.EqualFunc(got, want, slices.Equal[[]string]) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("divergence = %v, want %v", got, want)
		}
		view.Invalidate()
		time.Sleep(5 * time.Millisecond)
	}
}

// THE ALARM'S POPULATION IS THE LIVE PRESENCE, AND ITS THRESHOLD THE LEASE TTL
// (ADR-0015): two live nodes holding different tables raise it, and once the
// odd one's presence lapses it stops counting — a node that has gone never
// holds the alarm up, and nothing in the comparison keeps a clock of its own.
//
// AND THIS NODE AS IT IS: a row under this node's own id is the previous
// process's for a lease TTL after a restart, and comparing the fleet against
// it would raise — or clear — the alarm over a table this process does not
// hold.
func TestADeadNodeStopsCountingWhenItsPresenceLapses(t *testing.T) {
	t.Parallel()
	store := coordmemory.New()
	self := servingProfile(t, "core-1", "v1")
	present(t, store, servingProfile(t, "core-1", "the table before this restart"), time.Hour)
	present(t, store, servingProfile(t, "core-2", "v2"), time.Minute)
	present(t, store, servingProfile(t, "core-3", "v1"), time.Hour)

	view, err := coord.NewLeaseView(store, coord.ClassNode, coord.ViewOptions{
		Every: 10 * time.Millisecond, Trust: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = view.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	awaitDivergence(t, view, self, [][]string{{"core-1", "core-3"}, {"core-2"}})

	store.Advance(2 * time.Minute)
	awaitDivergence(t, view, self, nil)
}

// A VIEW THAT CANNOT SAY, AND NO VIEW AT ALL, REPORT NOTHING: a presence
// listing nobody can read shows no table differing, and a node with no
// coordination has no fleet to differ from.
func TestNoPresenceToReadRaisesNothing(t *testing.T) {
	t.Parallel()
	self := servingProfile(t, "core-1", "v1")
	if got := authDivergence(nil, self); got != nil {
		t.Errorf("no view = %v", got)
	}
	view, err := coord.NewLeaseView(coordmemory.New(), coord.ClassNode, coord.ViewOptions{
		Every: time.Second, Trust: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := authDivergence(view, self); got != nil {
		t.Errorf("a view that never listed = %v", got)
	}
}
