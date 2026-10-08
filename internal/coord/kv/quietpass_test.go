package kv

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// quietKV is a bucket whose pass hands over the entries it was given and then
// GOES QUIET: no further entry, no end-of-initial-values marker and no close.
//
// That is the watcher the client hands back when every message its consumer
// had pending is REMOVED before the pass reaches it — an age, a purge, a
// marker swept by another node. The client sends its end marker on a delivery,
// so with nothing left to deliver it never sends one. The index read, the
// certifying reads and the stream are the broker's own.
type quietKV struct {
	jetstream.KeyValue
	pass []jetstream.KeyValueEntry
}

func (k quietKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	ch := make(chan jetstream.KeyValueEntry, len(k.pass))
	for _, kve := range k.pass {
		ch <- kve
	}
	return stubWatcher{ch: ch}, nil
}

// A LISTING WHOSE PASS GOES QUIET ENDS, AND THE INDEX SUPPLIES WHAT THE PASS
// DID NOT DELIVER.
//
// The pass used to end on the client's marker and nothing else, so a pass
// that would never receive one held its listing until the caller's context
// ended: measured as an idle budget bucket's listing blocked for nine minutes
// and forty-two seconds, which only the test binary's timeout ended. Ended
// after [passIdle] instead, the listing is still complete, because the keys
// the pass did not deliver are exactly what the certification reads from the
// leader — one read each, and no more.
//
// THE CALLER'S CONTEXT IS BOUNDED, at four quiet intervals, so a regression
// fails here as this case rather than as the package's timeout.
func TestAListingWhosePassGoesQuietEndsWithEveryLiveKey(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	for i, scope := range []string{"org", "seat-a", "seat-b"} {
		putBudget(t.Context(), t, f.budgets, scope, 10*(i+1))
	}
	pass := recordPass(t.Context(), t, f.budgets)
	if len(pass) != 3 {
		t.Fatalf("setup: the recorded pass holds %d entries, want 3", len(pass))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*passIdle)
	defer cancel()
	wire := countWireReads(t, nc, f.budgets)
	started := time.Now()
	got, err := listScopes(ctx, f, quietKV{KeyValue: f.budgets, pass: pass[:1]})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("a listing whose pass went quiet after one entry held its "+
			"caller for %v and failed: %v", elapsed, err)
	}
	want := map[string]int{"org": 10, "seat-a": 20, "seat-b": 30}
	if !maps.Equal(got, want) {
		t.Fatalf("listed %v, want %v: the keys the quiet pass never delivered "+
			"are in the index, and a listing without them reports live records "+
			"as absent", got, want)
	}
	if n := wire.leaderReads(t); n != 2 {
		t.Errorf("the listing sent the leader %d reads, want 2 — one for each "+
			"key the pass did not deliver, and none for the one it did", n)
	}
}

// AND A SWEEP WHOSE PASS GOES QUIET ENDS, removing the markers it was handed.
//
// Nothing certifies a sweep — a marker it misses is swept on the next tick —
// so its quiet counts from the start rather than from an index it never
// reads. Waiting on the client's marker instead, it held the maintenance duty
// on the one bucket for as long as the duty's context lived.
func TestASweepWhosePassGoesQuietEndsWithWhatItWasHanded(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	putBudget(t.Context(), t, f.budgets, "gone", 10)
	if err := f.budgets.Delete(t.Context(), encodeKey("gone")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	pass := recordPass(t.Context(), t, f.budgets)
	if len(pass) != 1 || pass[0].Operation() == jetstream.KeyValuePut {
		t.Fatalf("setup: the recorded pass is %v, want the one delete marker", pass)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*passIdle)
	defer cancel()
	swept, err := sweepMarkers(ctx, f.js, quietKV{KeyValue: f.budgets, pass: pass},
		time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("a sweep whose pass went quiet failed rather than ending: %v", err)
	}
	if swept != 1 {
		t.Errorf("the sweep removed %d messages, want the 1 marker its pass "+
			"handed over before going quiet", swept)
	}
}
