package kv

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// streamMessages is how many messages kv's stream holds — the one count a
// marker the sweep removed, or a value it must not have, shows up in.
func streamMessages(ctx context.Context, t *testing.T, f *FleetStore, kv jetstream.KeyValue) uint64 {
	t.Helper()
	stream, err := f.js.Stream(ctx, bucketStream(kv))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	return info.State.Msgs
}

// THE SWEEP REMOVES EXACTLY THE MARKERS OLDER THAN ITS CUTOFF.
//
// Older, because the horizon is the whole of what the job decides: a marker
// written after the cutoff stays, and a live value is never touched at any
// cutoff. Counted from the broker's own answer — messages the purge REMOVED —
// and checked against the stream's message count, so a sweep that counted
// what it asked for rather than what went cannot pass.
func TestTheSweepRemovesExactlyTheMarkersOlderThanItsCutoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	for _, scope := range []string{"live", "old", "young"} {
		putBudget(ctx, t, f.budgets, scope, 1)
	}
	if err := f.budgets.Delete(ctx, encodeKey("old")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The broker stamps a marker with its own clock, which on an embedded
	// broker is this process's: a gap either side of the cutoff keeps the
	// two markers on their own sides of it.
	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(20 * time.Millisecond)
	if err := f.budgets.Purge(ctx, encodeKey("young")); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := streamMessages(ctx, t, f, f.budgets); n != 3 {
		t.Fatalf("the bucket holds %d messages before the sweep, want 3 — one "+
			"value and two markers", n)
	}

	if swept, err := sweepMarkers(ctx, f.js, f.budgets, cutoff.Add(-time.Hour)); err != nil || swept != 0 {
		t.Fatalf("a sweep with a cutoff before every marker = (%d, %v), want (0, nil)", swept, err)
	}
	swept, err := sweepMarkers(ctx, f.js, f.budgets, cutoff)
	if err != nil || swept != 1 {
		t.Fatalf("a sweep between the two markers = (%d, %v), want (1, nil): "+
			"only the marker written before the cutoff goes", swept, err)
	}
	if n := streamMessages(ctx, t, f, f.budgets); n != 2 {
		t.Fatalf("the bucket holds %d messages after sweeping the older marker, want 2", n)
	}
	swept, err = sweepMarkers(ctx, f.js, f.budgets, time.Now().Add(time.Hour))
	if err != nil || swept != 1 {
		t.Fatalf("a sweep after both markers = (%d, %v), want (1, nil)", swept, err)
	}
	names, err := keysUnder(ctx, f.js, f.budgets, jetstream.AllKeys)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if want := []string{encodeKey("live")}; !slices.Equal(names, want) {
		t.Fatalf("the index names %v after the sweep, want %v: the live value "+
			"and nothing else", names, want)
	}
	if kve, err := f.budgets.Get(ctx, encodeKey("live")); err != nil || kve.Revision() == 0 {
		t.Fatalf("the live value after the sweep = (%v, %v)", kve, err)
	}
}

// THE SWEEP NEVER REMOVES A VALUE WRITTEN AFTER ITS PASS SAW THE MARKER.
//
// The client's own PurgeDeletes collects markers with a watcher and then
// purges each one's subject outright, so a key created again between the
// watcher delivering its marker and the purge lost its new value — a sweep of
// records nobody needs deleting a record somebody had just written. The purge
// here is bounded at the marker's own revision and the broker applies the
// bound, so the later value survives with no read to race. The pass is
// replayed from before the re-creation, which is that interleaving every time
// rather than once in a thousand.
func TestTheSweepNeverRemovesAValueWrittenAfterItsPassSawTheMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	putBudget(ctx, t, f.budgets, "seat-a", 1)
	if err := f.budgets.Purge(ctx, encodeKey("seat-a")); err != nil {
		t.Fatalf("purge: %v", err)
	}
	before := recordPass(ctx, t, f.budgets)
	if !markerIn(before, encodeKey("seat-a")) {
		t.Fatal("the recorded pass holds no marker for seat-a, so the sweep has nothing to race")
	}
	putBudget(ctx, t, f.budgets, "seat-a", 7)

	swept, err := sweepMarkers(ctx, f.js, replayKV{KeyValue: f.budgets, pass: before}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Errorf("the sweep reports removing %d messages, want 0: the marker it "+
			"saw was already replaced by the value written after it", swept)
	}
	got, err := listScopes(ctx, f, f.budgets)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if want := map[string]int{"seat-a": 7}; !maps.Equal(got, want) {
		t.Fatalf("listed %v after the sweep, want %v: the value written after "+
			"the sweep's pass saw the marker was removed with it", got, want)
	}
}

// EVERY BUCKET THE BROKER NEVER AGES IS SWEPT, and no other.
//
// The set is DERIVED from the retention each bucket is opened with, and this
// holds it against the broker's own account of every stream the store made —
// so a bucket added without an age is swept without anybody remembering to
// list it, and a bucket with one is left to its age. A hand-kept list is how
// the lifetime token counters, which purged on every reset, were missing from
// the first account of which buckets keep markers — and a hand-kept list is
// what would have kept sweeping their windowed successor once it took an age
// of its own.
func TestEveryBucketTheBrokerNeverAgesIsSwept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	f := openFleetForTest(t, nc, prefix)

	var ageless, aged []string
	streams := f.js.ListStreams(ctx)
	for info := range streams.Info() {
		bucket, ok := strings.CutPrefix(info.Config.Name, "KV_"+prefix+"_")
		if !ok {
			continue
		}
		if info.Config.MaxAge == 0 {
			ageless = append(ageless, bucket)
		} else {
			aged = append(aged, bucket)
		}
	}
	if err := streams.Err(); err != nil {
		t.Fatalf("list streams: %v", err)
	}
	var swept []string
	for _, kv := range f.ageless {
		swept = append(swept, strings.TrimPrefix(kv.Bucket(), prefix+"_"))
	}
	slices.Sort(ageless)
	slices.Sort(swept)
	if !slices.Equal(swept, ageless) {
		t.Fatalf("the sweep covers %v, and the buckets the broker never ages "+
			"are %v", swept, ageless)
	}
	for _, must := range []string{"sandbox_runs", "secrets", "channels", "mailboxes", "statelog_positions"} {
		if !slices.Contains(swept, must) {
			t.Errorf("the %s bucket removes records and has no age, and the sweep does not cover it", must)
		}
	}
	// AND A BUCKET WITH AN AGE IS LEFT TO IT: the token counters' windows
	// are aged by the broker, so sweeping them too would be a second
	// retention over the same markers.
	if slices.Contains(swept, "budgets") {
		t.Error("the sweep covers the budgets bucket, which the broker ages")
	}
	if len(aged) == 0 {
		t.Fatal("no bucket with an age was found, so this case cannot tell a derived set from every bucket")
	}
}

// A CREATE OVER A MARKER THE SWEEP REMOVES MID-WAY STILL CREATES.
//
// The interleaving [createKey] exists for. It reads a marker and writes over
// it at the marker's revision; the sweep removes the marker in between; that
// write is refused against a subject that now holds nothing. The client's own
// Create reported that refusal as the key existing — a removed mailbox record a
// node then believed was registered, a released claim a replay could never
// take. Read again from the leader, the key holds nothing, and the create
// writes at zero and wins.
func TestACreateOverAMarkerTheSweepRemovesMidWayStillCreates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	key := encodeKey("turn-1")
	if _, err := f.runs.Put(ctx, key, []byte(`{"status":"done"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := f.runs.Purge(ctx, key); err != nil {
		t.Fatalf("purge: %v", err)
	}
	purge, err := newLeaderPurge(f.js, f.runs)
	if err != nil {
		t.Fatalf("purger: %v", err)
	}
	racing := &racedCreate{KeyValue: f.runs, sweep: func(ctx context.Context, key string, revision uint64) error {
		_, err := purge.through(ctx, key, revision)
		return err
	}}

	revision, err := createKey(ctx, f.js, racing, key, []byte(`{"status":"running"}`))
	if err != nil {
		t.Fatalf("a create over a marker swept between its read and its write = %v, "+
			"want it created: the key holds nothing at all", err)
	}
	if racing.sweeps != 1 {
		t.Fatalf("the sweep ran %d times inside the create, want 1 — this case "+
			"proves nothing unless the create reached the write over the marker", racing.sweeps)
	}
	kve, err := f.runs.Get(ctx, key)
	if err != nil || kve.Revision() != revision || string(kve.Value()) != `{"status":"running"}` {
		t.Fatalf("the created run reads back as (%v, %v), want revision %d", kve, err, revision)
	}
}

// racedCreate is a bucket whose write over a marker is preceded by the sweep
// removing that marker: the first conditional write at a NON-ZERO revision —
// which is only ever a create stepping over the marker it read — first purges
// the subject through that revision, exactly as [sweepMarkers] would.
type racedCreate struct {
	jetstream.KeyValue
	sweep  func(ctx context.Context, key string, revision uint64) error
	sweeps int
}

func (k *racedCreate) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if revision != 0 && k.sweeps == 0 {
		k.sweeps++
		if err := k.sweep(ctx, key, revision); err != nil {
			return 0, err
		}
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// A CREATE OVER A LIVE VALUE IS REFUSED AS THE KEY EXISTING, read from the
// leader; A CREATE OVER A MARKER CREATES.
//
// The two ordinary answers, with the one property the client's Create did not
// have: the question "is this a marker or a value" is asked of the stream
// LEADER, never by a direct get a replica that is behind answers — that
// replica still holds a removed value, and a create over a key the leader had
// removed reported it as held.
func TestACreateAsksTheLeaderWhatTheKeyHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	if _, err := f.runs.Put(ctx, encodeKey("live"), []byte(`{}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := f.runs.Put(ctx, encodeKey("removed"), []byte(`{}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := f.runs.Delete(ctx, encodeKey("removed")); err != nil {
		t.Fatalf("delete: %v", err)
	}

	wire := countWireReads(t, nc, f.runs)
	_, err := createKey(ctx, f.js, f.runs, encodeKey("live"), []byte(`{"again":true}`))
	if !errors.Is(err, jetstream.ErrKeyExists) {
		t.Fatalf("a create over a live value = %v, want ErrKeyExists", err)
	}
	if _, err := createKey(ctx, f.js, f.runs, encodeKey("removed"), []byte(`{"again":true}`)); err != nil {
		t.Fatalf("a create over a marker = %v, want it created", err)
	}
	if n := wire.leaderReads(t); n != 2 {
		t.Errorf("the two creates asked the leader %d times, want 2 — once each, "+
			"after the write at zero was refused", n)
	}
	if n := wire.directReads(t); n != 0 {
		t.Errorf("the creates sent %d direct gets, want 0: a replica that is "+
			"behind answers one with a value the leader has removed", n)
	}
}

// NOTHING IN THIS PACKAGE CREATES THROUGH THE BUCKET HANDLE, OR SWEEPS THROUGH
// THE CLIENT.
//
// Both are one call away and both look right: the bucket's Create is what a
// create-only write reaches for, and the client's PurgeDeletes is a sweep of
// exactly these markers. The first conditions on a marker the sweep may remove
// and asks a replica whether the key is one; the second purges a subject
// outright and takes a value written after it looked (markers.go says both).
// Neither fails a test by itself on one server, so the source is read: no call
// to a method named Create or PurgeDeletes anywhere in this package's own code.
func TestNothingCreatesThroughTheBucketHandleOrSweepsThroughTheClient(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	read := 0
	var found []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Create", "PurgeDeletes":
					found = append(found, fmt.Sprintf("%s: .%s(", fset.Position(call.Pos()), sel.Sel.Name))
				}
			}
			return true
		})
	}
	if read < 5 {
		t.Fatalf("read %d source files, which is not this package — the check certified nothing", read)
	}
	if len(found) > 0 {
		t.Errorf("these calls reach past createKey or sweepMarkers:\n  %s\n"+
			"a create goes through createKey and a marker sweep through "+
			"sweepMarkers — see markers.go for what each of these costs",
			strings.Join(found, "\n  "))
	}
}
