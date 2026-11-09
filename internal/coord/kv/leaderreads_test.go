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
// The state-log zero fence reads its domain's trim floor on every write at an
// expectation of zero, from the positions bucket's stream LEADER, under the
// write's own context — which carries no deadline, so the client's five
// seconds were the read's whole bound. Sent as one request, a read a member
// dropped across an election waited them out for a reply nobody would send,
// and the write was refused floor_unknown with the floor perfectly readable.
// It is the same leader read every single-key read here makes ([getLatest]),
// asked again as a metadata read is ([jsprovision.ReadBoundedWith]) after a
// dropped request.
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
// Each was one request held for its caller's whole context, and a member that
// still believes in a leader its group has just lost answers a stream info no
// more than it answers a message read: every listing on the node, every gated
// claim and every lease renewal's clock read went unavailable for the seconds
// it took to notice the leader was gone. They are one read ([leaderInfo]),
// asked as every leader read is.
func TestAStreamInfoAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	t.Run("the_key_index", func(t *testing.T) {
		t.Parallel()
		ns, nc := embeddedServer(t)
		f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
		putBudget(t.Context(), t, f.channels, "org", 1)
		held := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(bucketStream(f.channels)), 1)
		// THE TAP counts what reached the leader once the drop was
		// released: the index is ONE request a page, never a stream
		// handle's own info and then the index on it.
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
	// EACH READ ON AN INTERCEPTION OF ITS OWN, dropping exactly its first
	// request: two reads behind one interception that drops two requests
	// prove only the first read, since its own re-ask is the second request
	// dropped and the read after it is never dropped at all.
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
// A certified listing reads every key its pass delivered as a delete marker
// again from the stream LEADER ([certify]), because a replica behind a
// re-creation serves one for a key live throughout. That read is a leader
// read like any other: dropped across an election and held for the listing's
// whole context, it made the listing unavailable — every lease and every
// presence read on the node with it — for a key the leader would have
// answered a second later.
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
// The lease gate's barrier and the store clock were each one request the
// client held for its five seconds where the caller set none, and a lease
// decision's whole cost was sized against that: hedged, a read asks again
// inside the bound, and a broker that never answers costs a renewal no more
// than it did. The client here is given a default of 3.9 seconds — just
// short of a third request, at four — so the read is asked at none and two,
// twice, where a read held to the five seconds of a default it did not take
// from its client is asked three times, and one held to the thirty-second
// ceiling fifteen.
//
// THE READ RUNS IN A SYNCTEST BUBBLE, and the broker outside it, so its timers
// fire at exactly their instants: on the machine's clock the bound and the
// third request were a tenth of a second apart, and the verdict turned on how
// late a loaded runner woke — natsobj's twin of this case failed on CI with
// the third sent past the bound, which the hedge no longer does
// ([jsprovision.ReadBoundedWith]). The requests are counted behind a fence on
// the read's own connection ([jsapitest.Held.Fence]).
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
