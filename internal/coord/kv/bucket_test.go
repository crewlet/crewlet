package kv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// statusOf reads a bucket's status through the client's handle, which is a
// metadata read the stream leader answers. A test asks it; nothing in the
// package does, which is why the interface the package holds has no such
// method.
func statusOf(ctx context.Context, b *leaderBucket) (jetstream.KeyValueStatus, error) {
	kv, ok := b.client.(jetstream.KeyValue)
	if !ok {
		return nil, fmt.Errorf("the bucket's client is a %T, not the client's own handle", b.client)
	}
	return kv.Status(ctx)
}

// A LEADER READ SEES A REMOVAL EXACTLY AS THE CLIENT WROTE IT.
//
// The leader's answer is a stored message, and whether that message is a value
// or a removal is spelled in headers the CLIENT writes and does not export —
// so this package restates the spelling, and this case holds the restatement
// to the client by writing every kind of removal through the client and
// reading it back through the leader. A spelling that drifted would read a
// delete marker as a live value with an empty body: a released claim still
// held, a purged counter at zero rather than absent, a removed key listed.
//
// Mutation: spell [kvOperationDelete] or [kvOperationPurge] differently, or
// make [operationOf] answer a put for a marker, and the removed keys read back
// as present.
func TestLeaderReadsSeeTheClientsOwnRemovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("r%d", bucketSeq.Add(1)))
	b := store.runs

	for _, key := range []string{"kept", "deleted", "purged"} {
		if _, err := b.Put(ctx, key, []byte(`{"v":1}`)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := b.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := b.Purge(ctx, "purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if e, err := b.Get(ctx, "kept"); err != nil || string(e.Value()) != `{"v":1}` {
		t.Fatalf("Get(kept) = (%v, %v), want the value written", e, err)
	}
	for key, want := range map[string]jetstream.KeyValueOp{
		"deleted": jetstream.KeyValueDelete,
		"purged":  jetstream.KeyValuePurge,
	} {
		if e, err := b.Get(ctx, key); !errors.Is(err, jetstream.ErrKeyNotFound) {
			t.Errorf("Get(%s) = (%v, %v), want ErrKeyNotFound for a removed key", key, e, err)
		}
		e, found, err := b.latest(ctx, key)
		if err != nil || !found || e.Operation() != want {
			t.Errorf("the leader's newest message on %s reads as (%v, found=%t, %v), want "+
				"the client's %v marker", key, e, found, err, want)
		}
	}

	var listed []string
	if err := eachEntry(ctx, b, func(e jetstream.KeyValueEntry) error {
		listed = append(listed, e.Key())
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !slices.Equal(listed, []string{"kept"}) {
		t.Errorf("the walk listed %v, want only the key nobody removed", listed)
	}

	// AND A CREATE STEPS OVER EITHER MARKER, which is the read the client's
	// own Create made through a replica.
	for _, key := range []string{"deleted", "purged"} {
		if _, err := b.Create(ctx, key, []byte(`{"v":2}`)); err != nil {
			t.Errorf("Create over the %s marker: %v, want it written", key, err)
		}
	}
	if _, err := b.Create(ctx, "kept", []byte(`{"v":2}`)); !errors.Is(err, jetstream.ErrKeyExists) {
		t.Errorf("Create over a live value = %v, want ErrKeyExists", err)
	}
}

// lagging is a bucket's client handle whose ordered pass is served by a copy
// that STOPPED at an earlier moment: it delivers the snapshot it was given and
// the end-of-initial-values marker, and nothing the bucket learned since.
//
// That is exactly what a pass hosted on a follower hands over when the
// follower has not applied the newest writes — the server places the pass's
// consumer on a random member of the stream — and it is staged rather than
// raced because a race reproduces it a few times in a hundred: a case that
// rests on that can pass with the fix removed.
type lagging struct {
	clientBucket
	snapshot []jetstream.KeyValueEntry
}

func (l lagging) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	ch := make(chan jetstream.KeyValueEntry, len(l.snapshot)+1)
	for _, e := range l.snapshot {
		ch <- e
	}
	ch <- nil
	return stubWatcher{ch: ch}, nil
}

// passOf records what the client's own ordered pass delivers right now — the
// bucket as one copy holds it at this moment, removals included.
func passOf(ctx context.Context, t *testing.T, b *leaderBucket) []jetstream.KeyValueEntry {
	t.Helper()
	var out []jetstream.KeyValueEntry
	if err := orderedPass(ctx, b.client, jetstream.AllKeys, "the snapshot",
		func(e jetstream.KeyValueEntry) error {
			out = append(out, e)
			return nil
		}); err != nil {
		t.Fatalf("take the snapshot: %v", err)
	}
	return out
}

// A WALK WHOSE PASS IS SERVED BY A COPY THAT IS BEHIND COMES BACK CURRENT.
//
// The pass is an ordered consumer, and on a replicated bucket the server
// places it on any member of the stream, so it can be served by a follower
// that has not applied the newest acknowledged writes. Read alone, that pass
// was the listing: a budget reset missed the counter just charged, so the next
// charges counted from where it should have been cleared, and the trim floor —
// a minimum over the position rows — rises over a row it cannot see.
//
// Every way the copy can be behind is staged against one snapshot: a key that
// was rewritten since (the old value must not be listed), one removed by a
// delete and one by a purge (neither may be listed), and one created since (it
// must be). And the copy that had nothing at all, which is a pass that is
// behind by the whole bucket.
//
// Mutation: make [leaderBucket.closeOnLeader] return without asking, and every
// one of those comes back as the copy had it.
func TestAWalkServedByACopyThatIsBehindIsClosedByTheLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("l%d", bucketSeq.Add(1)))
	b := store.budgets

	put := func(key, value string) {
		t.Helper()
		if _, err := b.Put(ctx, key, []byte(value)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	for _, key := range []string{"rewritten", "deleted", "purged", "unchanged"} {
		put(key, "before")
	}
	behind := passOf(ctx, t, b)

	put("rewritten", "after")
	if err := b.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := b.Purge(ctx, "purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	put("created", "after")

	want := map[string]string{"rewritten": "after", "unchanged": "before", "created": "after"}
	for name, snapshot := range map[string][]jetstream.KeyValueEntry{
		"a copy that stopped part way": behind,
		"a copy that had nothing":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			stale := *b
			stale.client = lagging{clientBucket: b.client, snapshot: snapshot}
			got := map[string]string{}
			if err := eachEntry(ctx, &stale, func(e jetstream.KeyValueEntry) error {
				got[e.Key()] = string(e.Value())
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("the walk listed %v, want %v: a pass served by a copy that "+
					"is behind was handed over as the bucket", got, want)
			}
		})
	}
}
