package kv

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A WRITE FENCE'S FLOOR READ THAT A MEMBER DROPS IS ASKED AGAIN.
//
// The state-log zero fence reads its domain's trim floor from the positions
// bucket's stream LEADER under a write's context, which carries no deadline.
// Asked once, a read a member dropped across an election waited out the
// client's default, and the write was refused floor_unknown with the floor
// readable. It is [getLatest]'s leader read, asked again as
// [jsprovision.ReadBoundedWith] asks.
func TestAFloorReadAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, nc := embeddedServer(t)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	fleet := openFleetForTest(t, nc, prefix)
	want := coord.TrimFloor{Domain: "tracker", Generation: 1, TrimTo: 40, Floor: 40,
		At: time.Now().UTC().Truncate(time.Millisecond), By: "node-a"}
	if err := fleet.PutFloor(t.Context(), want); err != nil {
		t.Fatalf("PutFloor: %v", err)
	}
	held := jsapitest.DropFirst(t, ns, jsapitest.MsgGet("KV_"+prefix+positionsSuffix), 1)

	got, ok, err := fleet.Floor(context.Background(), "tracker")
	if err != nil || !ok {
		t.Fatalf("Floor behind one dropped read = (%v, %v): a read the broker "+
			"dropped once was not asked again", ok, err)
	}
	if got.Floor != want.Floor || got.Generation != want.Generation || got.By != want.By {
		t.Errorf("Floor = %+v, want %+v", got, want)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d floor reads, want the 1 it dropped", n)
	}
}

// A STREAM INFO ONLY THE LEADER ANSWERS IS ASKED AGAIN WHEN A MEMBER DROPS IT
// — the certified listing's key index, the lease gate's barrier and every
// lease decision's store clock alike.
//
// A member that still believes in a lost leader drops a stream info as it
// drops a message read; asked once and held for the caller's context, every
// listing, gated claim and lease clock read on the node stalled until the
// loss was noticed. They are one read, [leaderInfo].
func TestAStreamInfoAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	t.Run("the_key_index", func(t *testing.T) {
		t.Parallel()
		ns, nc := embeddedServer(t)
		f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
		putBudget(t.Context(), t, f.channels, "org", 1)
		held := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(bucketStream(f.channels)), 1)
		// THE TAP counts requests after the drop: the index is ONE request
		// a page, never a stream handle's info and then the index.
		tap, err := nc.SubscribeSync(jsapitest.StreamInfo(bucketStream(f.channels)))
		if err != nil {
			t.Fatal(err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}

		names, err := keysUnder(context.Background(), f.js, f.channels, jetstream.AllKeys)
		if err != nil {
			t.Fatalf("a key index the broker dropped once was not asked again: %v", err)
		}
		if len(names) != 1 || names[0] != encodeKey("org") {
			t.Errorf("the index named %q, want [%s]", names, encodeKey("org"))
		}
		if n := held.Count(); n != 1 {
			t.Errorf("the broker held %d stream infos, want the 1 it dropped", n)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		if n, _, err := tap.Pending(); err != nil || n != 1 {
			t.Errorf("the index asked the leader %d more time(s) once the drop was "+
				"released (%v), want the 1 request of its one page", n, err)
		}
	})
	// EACH READ ON AN INTERCEPTION OF ITS OWN, dropping just its first
	// request: one dropping two proves only the first read, whose re-ask is
	// the second one dropped.
	t.Run("the_gate_barrier", func(t *testing.T) {
		t.Parallel()
		ns, nc := embeddedServer(t)
		s := openStore(t, nc, time.Minute)
		t.Cleanup(s.Close)
		held := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(s.leases.stream), 1)

		if _, at, err := s.streamPoint(context.Background(), s.leases); err != nil || at.IsZero() {
			t.Fatalf("the gate's barrier behind a dropped read = (%v, %v): it was "+
				"not asked again", at, err)
		}
		if n := held.Count(); n != 1 {
			t.Errorf("the broker held %d stream infos, want the 1 the barrier's read "+
				"had dropped", n)
		}
	})
	t.Run("the_store_clock", func(t *testing.T) {
		t.Parallel()
		ns, nc := embeddedServer(t)
		s := openStore(t, nc, time.Minute)
		t.Cleanup(s.Close)
		held := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(s.leases.stream), 1)

		if at, err := s.storeNow(context.Background(), s.leases); err != nil || at.IsZero() {
			t.Fatalf("the store clock behind a dropped read = (%v, %v): it was "+
				"not asked again", at, err)
		}
		if n := held.Count(); n != 1 {
			t.Errorf("the broker held %d stream infos, want the 1 the clock's read "+
				"had dropped", n)
		}
	})
}

// A LISTING'S CERTIFYING READ THAT A MEMBER DROPS IS ASKED AGAIN.
//
// [certify] rereads each delete marker a pass delivered from the stream
// LEADER. Asked once and dropped across an election, it held the listing, and
// every lease and presence read on the node, for the listing's whole context.
func TestAListingsCertifyingReadAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, nc := embeddedServer(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()
	for _, scope := range []string{"org", "gone"} {
		putBudget(ctx, t, f.channels, scope, 1)
	}
	if err := f.channels.Delete(ctx, encodeKey("gone")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	held := jsapitest.DropFirst(t, ns, jsapitest.MsgGet(bucketStream(f.channels)), 1)

	got, err := listScopes(context.Background(), f, f.channels)
	if err != nil {
		t.Fatalf("a listing whose certifying read the broker dropped once "+
			"failed rather than asking again: %v", err)
	}
	if want := map[string]int{"org": 1}; !maps.Equal(got, want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d leader reads, want the 1 marker's it dropped", n)
	}
}

// A STREAM INFO NOBODY ANSWERS, FOR A CALLER WITH NO DEADLINE, ENDS AT ITS
// CLIENT'S OWN DEFAULT — asked again inside it, and never stretched to the
// lookup ceiling ([leaderInfo]).
//
// A lease decision's cost is sized against the client's default. Here it is
// 3.9 seconds, just short of a third request at four: asked at zero and two,
// where a 5-second default not its client's asks three times, and the
// 30-second ceiling fifteen.
//
// THE READ RUNS IN A SYNCTEST BUBBLE, the broker outside it, so timers fire at
// their instants rather than at a loaded runner's whim. Requests are counted
// behind a fence on the read's own connection ([jsapitest.Held.Fence]).
func TestAStreamInfoNobodyAnswersEndsAtTheClientsDefault(t *testing.T) {
	t.Parallel()
	const bound = 3900 * time.Millisecond
	ns, nc := embeddedServer(t)
	s := openStore(t, nc, time.Minute)
	t.Cleanup(s.Close)
	js, err := jetstream.New(nc, jetstream.WithDefaultTimeout(bound))
	if err != nil {
		t.Fatal(err)
	}
	held := jsapitest.Hold(t, ns, jsapitest.StreamInfo(s.leases.stream))
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, err := leaderInfo(t.Context(), js, s.leases.stream)
		if !jsprovision.Unanswered(t.Context(), err) {
			t.Errorf("a stream info nobody answered ended with %v, which its caller "+
				"cannot read as silence", err)
		}
		if took := time.Since(start); took != bound {
			t.Errorf("the read ended after %v, want its client's %v", took, bound)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	asked, err := held.Fence(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("the read was asked %d times inside its client's %v, want 2: at none "+
			"and two seconds", asked, bound)
	}
}
