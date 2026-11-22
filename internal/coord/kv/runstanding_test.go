package kv

import (
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// A RUN'S STANDING CROSSES A LEAF'S LINK WITHOUT ITS RECORDS. Three records at
// their bound are about 18 MiB, three quarters of a minute at this link's pace
// and minutes at the slowest a link is allowed; the boot's epoch floor reads
// what they state of their seat and lease, which crosses as headers in
// milliseconds. The link's count is the signal: a pass that moved a record
// carries megabytes toward the leaf.
//
// Mutation: pass over the records' values (kv.Watch) in place of their
// headers, and the link carries every record.
func TestARunsStandingCrossesALeafsLinkWithoutItsRecords(t *testing.T) {
	t.Parallel()
	member, leaf, link := overASlowLeaf(t, runLinkRate)
	want := map[string]coord.RunStanding{}
	for i, turn := range []string{"t1", "t2", "t3"} {
		standing := coord.StandingOf(uuid.New(), "swe-"+turn, int64(i+1))
		if created, err := member.CreateSandboxRun(t.Context(), turn, runAtTheBound(t), &standing); err != nil ||
			!created {
			t.Fatalf("create a run's record at its bound = (%v, %v), want created", created, err)
		}
		want[turn] = standing
	}
	before := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
	stood, err := leaf.SandboxRunStandings(t.Context())
	carried := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before
	if err != nil {
		t.Fatalf("list the runs' standing across the link: %v", err)
	}
	if len(stood) != len(want) {
		t.Fatalf("listed %d runs' standing, want %d", len(stood), len(want))
	}
	for _, s := range stood {
		if s.Standing == nil || *s.Standing != want[s.TurnID] {
			t.Errorf("run %s stands as %+v, want what its create stated, %+v", s.TurnID, s.Standing,
				want[s.TurnID])
		}
	}
	if carried >= 64<<10 {
		t.Errorf("listing the standing carried %d bytes to the leaf: a record's value crossed", carried)
	}
}

// A STANDING LANDS IN THE MESSAGE OF ITS RECORD, through a leaf's client in
// the fleet's domain: the leader's message at the revision the change answered
// holds the value and the standing together, so no reader sees one without
// the other. A change stating none leaves none.
//
// Mutation: send the standing in a second publish, and the record's revision
// carries none.
func TestAStandingLandsInTheMessageOfItsRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	leaf := openFleetVia(t, leafClient(t), AcrossLink, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	read, err := newLeaderReader(leaf.js, leaf.runs)
	if err != nil {
		t.Fatal(err)
	}
	inMessage := func(version uint64, value string, want coord.RunStanding) {
		t.Helper()
		at, err := read.at(ctx, encodeKey("t1"), version)
		if err != nil || string(at.Value()) != value {
			t.Fatalf("the leader's message at revision %d = %v, want the value %s", version, err, value)
		}
		if got := standingOf(headersOf(at)); got == nil || *got != want {
			t.Errorf("the message at revision %d states %+v, want %+v", version, got, want)
		}
	}
	launched := coord.StandingOf(uuid.New(), "swe", 3)
	if created, err := leaf.CreateSandboxRun(ctx, "t1", []byte(`{"status":"launching"}`), &launched); err != nil ||
		!created {
		t.Fatalf("create the run through the leaf = (%v, %v), want created", created, err)
	}
	created, _, err := leaf.SandboxRun(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	inMessage(created.Version, `{"status":"launching"}`, launched)

	claimed := coord.StandingOf(launched.Seat, "swe", 7)
	written, changed, err := leaf.MutateSandboxRun(ctx, "t1", func(coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Put: []byte(`{"status":"running"}`), Standing: &claimed}, nil
	})
	if err != nil || !changed {
		t.Fatalf("change the run through the leaf = (%v, %v), want changed", changed, err)
	}
	inMessage(written.Version, `{"status":"running"}`, claimed)

	if _, _, err := leaf.MutateSandboxRun(ctx, "t1", func(coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Put: []byte(`{"status":"resumed"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	stood, err := leaf.SandboxRunStandings(ctx)
	if err != nil || len(stood) != 1 || stood[0].Standing != nil {
		t.Errorf("the standing after a change stating none = %+v, %v; want the run, stating none", stood, err)
	}
}

// A STANDING IS PUBLISHED WHERE THE CLIENT'S OWN UPDATE PUBLISHES: on the
// key's subject behind the client's API prefix, the fleet's domain for a
// leaf's client and none for a member's. A subject of another form can still
// reach the stream through a leaf's link, so the subject itself is compared,
// as each connection sees its own publish.
//
// Mutation: publish without the domain prefix, or with it on every client,
// and the two subjects part.
func TestAStandingIsPublishedWhereTheClientsOwnUpdateIs(t *testing.T) {
	t.Parallel()
	for name, client := range map[string]jetstream.JetStream{
		"a leaf's client, in the fleet's domain": leafClient(t),
		"a member's client":                      jsOf(embeddedNATS(t)),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			b := openFleetVia(t, client, AcrossLink, fmt.Sprintf("p%d", bucketSeq.Add(1))).runs.(settledBucket)
			seen, err := client.Conn().SubscribeSync(">")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = seen.Unsubscribe() }()
			if err := client.Conn().Flush(); err != nil {
				t.Fatal(err)
			}
			theirs, ours := encodeKey("theirs"), encodeKey("ours")
			if _, err := b.KeyValue.Update(ctx, theirs, []byte(theirs), 0); err != nil {
				t.Fatalf("the client's own update: %v", err)
			}
			standing := coord.StandingOf(uuid.New(), "swe", 1)
			if _, err := b.update(ctx, ours, []byte(ours), 0, &standing); err != nil {
				t.Fatalf("an update stating a standing: %v", err)
			}
			subjects := map[string]string{}
			for len(subjects) < 2 {
				m, err := seen.NextMsgWithContext(ctx)
				if err != nil {
					t.Fatalf("the two writes' publishes, %v seen: %v", subjects, err)
				}
				if key := string(m.Data); key == theirs || key == ours {
					subjects[key] = strings.TrimSuffix(m.Subject, key)
				}
			}
			if subjects[ours] != subjects[theirs] {
				t.Errorf("a standing was published under %q, the client's own update under %q",
					subjects[ours], subjects[theirs])
			}
		})
	}
}

// THE WIDEST STANDING IS COUNTED IN kvHeaderBytes: every write's crossing and
// the bound on a value are counted with it, so a standing past it would put a
// record at its bound past what a leaf's read carries back. A seat and a
// digest are one width whatever they hold, an epoch at most twenty bytes, so
// no handle, however long, widens it. 342 bytes, as the docs state.
//
// Mutation: count kvHeaderBytes without the standing, or carry the handle
// itself, and a standing is past it.
func TestTheWidestStandingIsCountedInKvHeaderBytes(t *testing.T) {
	t.Parallel()
	clients := nats.Header{
		"KV-Operation":                      {"PURGE"},
		jetstream.MsgRollup:                 {jetstream.MsgRollupSubject},
		jetstream.ExpectedLastSubjSeqHeader: {strconv.FormatUint(math.MaxUint64, 10)},
		jetstream.MsgTTLHeader:              {time.Duration(math.MaxInt64).String()},
	}
	sized := func(s coord.RunStanding) int {
		h := maps.Clone(clients)
		maps.Copy(h, standingHeader(&s))
		return (&nats.Msg{Header: h}).Size()
	}
	if widest := sized(coord.StandingOf(uuid.Max, strings.Repeat("h", 64<<10), math.MinInt64)); widest != kvHeaderBytes {
		t.Errorf("a write with the widest standing carries %d bytes of header, and kvHeaderBytes counts %d",
			widest, kvHeaderBytes)
	}
	for _, s := range []coord.RunStanding{
		coord.StandingOf(uuid.Nil, "", 0),
		coord.StandingOf(uuid.New(), "swe", math.MaxInt64),
	} {
		if size := sized(s); size > kvHeaderBytes {
			t.Errorf("a write stating %+v carries %d bytes of header, past the %d counted", s, size, kvHeaderBytes)
		}
	}
	if kvHeaderBytes != 342 {
		t.Errorf("kvHeaderBytes is %d, and coord.MaxValueBytes and the docs state 342", kvHeaderBytes)
	}
}

// A STANDING IS READ ONLY WHERE EVERY ONE OF ITS HEADERS PARSES. A reader
// selects on it in place of the record, so a standing half-stated (an older
// build's write, a garbled header) is none, and its record is read whole; a
// header this build does not know is not read.
//
// Mutation: read a missing header as its zero value, and a half-stated
// standing names no seat at epoch zero.
func TestAStandingIsReadOnlyWhereEveryOneOfItsHeadersParses(t *testing.T) {
	t.Parallel()
	stated := coord.StandingOf(uuid.New(), "swe", 9)
	later := standingHeader(&stated)
	later.Set("Crewlet-Run-Something-Later", "x")
	if got := standingOf(later); got == nil || *got != stated {
		t.Errorf("the standing beside a header a later build adds = %+v, want %+v", got, stated)
	}
	for name, garble := range map[string]func(nats.Header){
		"no seat":           func(h nats.Header) { h.Del(runSeatHeader) },
		"no handle":         func(h nats.Header) { h.Del(runHandleHeader) },
		"no epoch":          func(h nats.Header) { h.Del(runEpochHeader) },
		"a short digest":    func(h nats.Header) { h.Set(runHandleHeader, "abcd") },
		"a seat not a UUID": func(h nats.Header) { h.Set(runSeatHeader, "swe") },
		"an epoch not a number": func(h nats.Header) {
			h.Set(runEpochHeader, "seven")
		},
	} {
		h := standingHeader(&stated)
		garble(h)
		if got := standingOf(h); got != nil {
			t.Errorf("a standing with %s = %+v, want none", name, got)
		}
	}
	if got := standingOf(nil); got != nil {
		t.Errorf("no header at all = %+v, want no standing", got)
	}
}
