package kv

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A FLOOR READ IS ONE REQUEST TO THE STREAM'S LEADER AND CREATES NO CONSUMER.
//
// That is why [FleetStore.Floor] exists: the state-log write fence reads a
// floor on every write at an expectation of zero, and a listing's ordered
// consumer is, on a clustered broker, two metadata-group proposals a loaded
// fleet answers late enough to refuse a page create. A tap on `$JS.API.>` sees every API request the read
// sends. The listing is the control: the tap must see a listing's consumer,
// or a blind tap would pass a read that lists.
func TestAFloorReadCreatesNoConsumer(t *testing.T) {
	t.Parallel()
	ns, nc := embeddedServer(t)
	fleet := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()
	if err := fleet.PutFloor(ctx, coord.TrimFloor{
		Domain: "tracker", Generation: 1, TrimTo: 40, Floor: 40,
		At: time.Now().UTC(), By: "node-a",
	}); err != nil {
		t.Fatalf("PutFloor: %v", err)
	}
	if err := fleet.PutHold(ctx, coord.TrimHold{
		Owner: "backup", At: time.Now().UTC(),
		Streams: map[string]coord.Position{"LOG": {Stream: "LOG", Generation: 1, Seq: 5}},
	}); err != nil {
		t.Fatalf("PutHold: %v", err)
	}

	tap, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect the tap: %v", err)
	}
	t.Cleanup(tap.Close)
	sub, err := tap.SubscribeSync("$JS.API.>")
	if err != nil {
		t.Fatalf("subscribe the tap: %v", err)
	}
	if err := tap.Flush(); err != nil {
		t.Fatalf("flush the tap's subscription: %v", err)
	}
	// requests drains what the tap has seen since the last drain: after a
	// flush, every request routed before it is already in the tap's queue.
	requests := func() []string {
		t.Helper()
		if err := tap.Flush(); err != nil {
			t.Fatalf("flush the tap: %v", err)
		}
		var seen []string
		for {
			msg, err := sub.NextMsg(50 * time.Millisecond)
			if errors.Is(err, nats.ErrTimeout) {
				return seen
			}
			if err != nil {
				t.Fatalf("read the tap: %v", err)
			}
			seen = append(seen, msg.Subject)
		}
	}

	for _, domain := range []string{"tracker", "pages"} {
		if _, _, err := fleet.Floor(ctx, domain); err != nil {
			t.Fatalf("Floor(%s): %v", domain, err)
		}
	}
	read := requests()
	if len(read) != 2 {
		t.Fatalf("two floor reads sent %d API request(s), want one each: %v",
			len(read), read)
	}
	for _, subject := range read {
		if !strings.HasPrefix(subject, "$JS.API.STREAM.MSG.GET.") {
			t.Fatalf("a floor read sent %s, want only the leader's message get — "+
				"anything else is a consumer, an index or an info read the write "+
				"fence would wait on: %v", subject, read)
		}
	}

	// THE CONTROL: a listing of the register, seen by the same tap.
	if _, err := fleet.Holds(ctx); err != nil {
		t.Fatalf("Holds: %v", err)
	}
	listed := requests()
	var created bool
	for _, subject := range listed {
		created = created || strings.HasPrefix(subject, "$JS.API.CONSUMER.CREATE.")
	}
	if !created {
		t.Fatalf("the tap saw no consumer created by a listing of the register "+
			"(%v), so it cannot tell a floor read that lists from one that does "+
			"not", listed)
	}
}

// A ROW THE KEY READ CANNOT VOUCH FOR IS RAISED, never read as no floor.
//
// PutFloor validates and keys by domain, so the test writes the bucket
// directly: undecodable bytes, and a valid row under another domain's key.
// Read as absent, either is a domain whose trim never ran, which clears every
// write fence against a log the trim may have moved past.
func TestAFloorRowTheKeyCannotVouchForIsRaised(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	fleet := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()

	if _, err := fleet.positions.Put(ctx, coord.FloorKey("tracker"), []byte("{")); err != nil {
		t.Fatalf("plant an undecodable floor: %v", err)
	}
	row, found, err := fleet.Floor(ctx, "tracker")
	if err == nil {
		t.Fatalf("an undecodable floor read as (%+v, %v) with no error", row, found)
	}
	rowFault(t, "an undecodable row", err)

	other, err := json.Marshal(coord.TrimFloor{Domain: "tracker", Floor: 90, TrimTo: 90,
		At: time.Now().UTC(), By: "node-a"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := fleet.positions.Put(ctx, coord.FloorKey("pages"), other); err != nil {
		t.Fatalf("plant a row under another domain's key: %v", err)
	}
	row, found, err = fleet.Floor(ctx, "pages")
	if err == nil {
		t.Fatalf("pages' key holding the tracker's row read as (%+v, %v) with no "+
			"error — a cursor on the pages log compared against the tracker's "+
			"sequences", row, found)
	}
	rowFault(t, "another domain's row under the key", err)
	if !strings.Contains(err.Error(), coord.FloorKey("pages")) {
		t.Fatalf("the refusal %q does not name the key to remove", err)
	}
	// AND IT NAMES THE REMEDY THAT WORKS: nothing rewrites a floor it cannot
	// read (the trim carries each floor forward from its row), so the refusal
	// names purging the key in its bucket and promises no rewrite.
	remove := fmt.Sprintf("nats kv purge %s %s", fleet.positions.Bucket(), coord.FloorKey("pages"))
	if !strings.Contains(err.Error(), remove) ||
		strings.Contains(err.Error(), "rewrites it at its next tick") {
		t.Fatalf("the refusal %q does not name the remedy %q, or promises a rewrite "+
			"nothing makes", err, remove)
	}

	// AND A STORE THAT DOES NOT ANSWER IS THE OTHER CLASS: nothing is wrong
	// with any row, and the refusal clears by itself when it answers again.
	nc.Close()
	if _, _, err := fleet.Floor(ctx, "tracker"); !errors.Is(err, coord.ErrUnavailable) ||
		errors.Is(err, coord.ErrFloorUnreadable) {
		t.Fatalf("a floor read over a closed connection = %v, want coord.ErrUnavailable "+
			"and not coord.ErrFloorUnreadable — a store that did not answer is no "+
			"row to remove", err)
	}
}

// rowFault holds a refusal of a floor row to its class: the row's fault
// ([coord.ErrFloorUnreadable]), never the store's ([coord.ErrUnavailable]),
// since the two clear in opposite ways and a refusal built on one names the
// remedy by it.
func rowFault(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, coord.ErrFloorUnreadable) || errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("%s is refused %v, want coord.ErrFloorUnreadable and not "+
			"coord.ErrUnavailable — the store answered, and the row is what has to go",
			what, err)
	}
}
