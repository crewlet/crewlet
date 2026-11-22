package kv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// runLinkRate holds a leaf's link to 420 KiB a second each way: a run's
// record at its bound crosses raw in 14.6 s, past passIdle and the client's
// ten-second reset of its watch, far inside the runs bucket's quiet across a
// link.
const runLinkRate = 420 << 10

// watchLinkRate holds a leaf's link to the pace at which a run's record at its
// bound, behind its protocol line, crosses raw in 25 s (251,668 bytes a
// second): past two of the client's five-second heartbeats.
var watchLinkRate = int64(jsapi.RequestBytes(coord.MaxValueBytes+kvHeaderBytes)) / 25

// runAtTheBound is a run's record of exactly what one key holds: random text
// in a JSON string, so the bytes on the link are its own.
func runAtTheBound(t *testing.T) []byte {
	t.Helper()
	const frame = len(`""`)
	random := make([]byte, base64.StdEncoding.DecodedLen(coord.MaxValueBytes-frame)+3)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return []byte(`"` + base64.StdEncoding.EncodeToString(random)[:coord.MaxValueBytes-frame] + `"`)
}

// A RUNS LISTING ACROSS A LEAF'S LINK DELIVERS ITS RECORDS IN ITS PASS. Two
// records at the bound arrive 14.6 s apart, so a pass quiet after passIdle
// ends between them and hands the rest to its certifying reads; here those go
// to a reader nobody answers, so only a pass given its records' crossing
// lists them.
//
// Mutation: begin the pass with passIdle, or watch through the client's
// watcher, and the listing fails on the blind read.
func TestARunsListingAcrossALeafsLinkDeliversItsRecordsInItsPass(t *testing.T) {
	t.Parallel()
	pace := time.Second * (1 << 20) / runLinkRate
	if raw := jsapi.Sending(coord.MaxValueBytes+kvHeaderBytes, pace); raw <= 2*passIdle {
		t.Fatalf("a record at its bound crosses the link raw in %v, inside two idle intervals", raw)
	}
	member, leaf, _ := overASlowLeaf(t, runLinkRate)
	want := map[string][]byte{}
	for _, turn := range []string{"t1", "t2"} {
		value := runAtTheBound(t)
		if _, err := member.runs.Put(t.Context(), encodeKey(turn), value); err != nil {
			t.Fatalf("put a run's record at its bound: %v", err)
		}
		want[encodeKey(turn)] = value
	}
	blind, err := jsapi.Account().Client(leaf.js.Conn())
	if err != nil {
		t.Fatal(err)
	}
	read, err := newLeaderReader(blind, leaf.runs)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string][]byte{}
	err = listUnder(t.Context(), leaf.js, read, leaf.runs, runRecordKeys, "the sandbox runs", false,
		func(kve jetstream.KeyValueEntry) error {
			listed[kve.Key()] = bytes.Clone(kve.Value())
			return nil
		})
	if err != nil {
		t.Fatalf("the runs listing across the link = %v: its pass ended before a record crossed, "+
			"and certifying it asked a reader nobody answers", err)
	}
	if len(listed) != len(want) {
		t.Fatalf("listed %d records, want %d", len(listed), len(want))
	}
	for key, value := range want {
		if !bytes.Equal(listed[key], value) {
			t.Errorf("the record %s listed is not the one written", key)
		}
	}
}

// A WATCH ACROSS A SLOW LINK DELIVERS A RECORD LONGER THAN TWO HEARTBEATS TO
// CROSS. The client's watch resets its consumer when nothing arrived in two
// five-second heartbeats, and no heartbeat passes a delivery still crossing,
// so a record taking 25 s is cut and asked for again every ten seconds and
// never lands, each copy on the link. The bucket's own watcher beats at its
// quiet, so the record lands once.
//
// Mutation: let Watch delegate to the client's handle, and nothing arrives.
func TestAWatchAcrossASlowLinkDeliversARecordLongerThanTwoHeartbeatsToCross(t *testing.T) {
	t.Parallel()
	member, leaf, link := overASlowLeaf(t, watchLinkRate)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	key := encodeKey("t1")
	updates, err := leaf.WatchSandboxRun(ctx, "t1")
	if err != nil {
		t.Fatalf("watch the run across the link: %v", err)
	}
	if first := <-updates; first.Found {
		t.Fatalf("the watch's first state found a record nobody wrote")
	}
	heartbeat := leaf.runs.(settledBucket).quiet(key)
	stream, err := member.js.Stream(ctx, bucketStream(member.runs))
	if err != nil {
		t.Fatal(err)
	}
	consumers := stream.ListConsumers(ctx)
	watching := 0
	for info := range consumers.Info() {
		watching++
		if info.Config.IdleHeartbeat != heartbeat {
			t.Errorf("the watch's consumer beats every %v, want the bucket's quiet %v",
				info.Config.IdleHeartbeat, heartbeat)
		}
	}
	if err := consumers.Err(); err != nil || watching == 0 {
		t.Fatalf("the runs stream's consumers = %d, %v: want the watch's", watching, err)
	}
	before := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
	value := runAtTheBound(t)
	if _, err := member.runs.Put(ctx, key, value); err != nil {
		t.Fatalf("put a run's record at its bound: %v", err)
	}
	select {
	case u, ok := <-updates:
		if !ok || !u.Found || !bytes.Equal(u.Record.Value, value) {
			t.Fatalf("the watch handed over (found %v, open %v) and not the record written", u.Found, ok)
		}
	case <-ctx.Done():
		t.Fatalf("the watch delivered nothing of a record taking 25 s to cross: %v", ctx.Err())
	}
	if carried := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before; carried >= 2*int64(len(value)) {
		t.Errorf("the link carried %d bytes to the leaf for one record of %d: it was sent again", carried, len(value))
	}
}

// headersSpy is a settled bucket that keeps the consumer of each headers-only
// watch it opens, as the broker holds it.
type headersSpy struct {
	settledBucket
	info *nats.ConsumerInfo
	err  error
}

func (s *headersSpy) watchHeaders(ctx context.Context, keys string) (jetstream.KeyWatcher, error) {
	w, err := s.settledBucket.watchHeaders(ctx, keys)
	if err != nil {
		return nil, err
	}
	s.info, s.err = w.(*bucketWatcher).sub.ConsumerInfo()
	return w, nil
}

// THE MARKER SWEEP ACROSS A LINK CARRIES HEADERS ONLY: it decides on a key's
// operation, revision and instant, so a run's record at its bound never
// crosses for it. A guard for the sweep's move onto the bucket's own watcher.
//
// Mutation: drop HeadersOnly from watchHeaders, or let watchWalk call Watch,
// and the record crosses.
func TestTheMarkerSweepAcrossALinkCarriesHeadersOnly(t *testing.T) {
	t.Parallel()
	member, leaf, link := overASlowLeaf(t, runLinkRate)
	if _, err := member.runs.Put(t.Context(), encodeKey("t1"), runAtTheBound(t)); err != nil {
		t.Fatalf("put a run's record at its bound: %v", err)
	}
	spy := &headersSpy{settledBucket: leaf.runs.(settledBucket)}
	before := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
	pass, err := walkForSweep(t.Context(), leaf.js, spy)
	if err != nil {
		t.Fatalf("the sweep's walk across the link: %v", err)
	}
	if _, ok := pass.keys[encodeKey("t1")]; !ok {
		t.Errorf("the sweep's walk did not see the run's key")
	}
	if spy.info == nil || !spy.info.Config.HeadersOnly {
		t.Errorf("the sweep's consumer = %+v, %v: want one delivering headers only", spy.info, spy.err)
	}
	if carried := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before; carried >= 64<<10 {
		t.Errorf("the sweep carried %d bytes to the leaf: a record's value crossed", carried)
	}
}
