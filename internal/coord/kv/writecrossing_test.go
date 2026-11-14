package kv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// slowLinkRate is the pace the case below holds a leaf's link to, towards its
// member: 96 KiB a second, about three times the slowest a link is allowed to
// be ([objstore.MiBPace]), so a listing near its bound takes about nine
// seconds to cross — past the five seconds a write whose caller set no
// deadline was given whatever it carried, and far inside the time its bytes
// are given at the slowest pace allowed.
const slowLinkRate = 96 << 10

// A LISTING ACROSS A LEAF'S LINK IS WAITED FOR AS LONG AS ITS BYTES TAKE TO
// CROSS IT.
//
// A node without the `data` role reaches the coordination buckets across its
// leaf link, and every node with seats writes its MCP listings there: a record
// of up to coord.MaxMCPListingBytes, which a link at the slowest pace the
// engine allows carries in about 26 seconds. Written under the fifteen seconds
// of the beat that wrote it, a listing at its bound failed on any link below
// about 60 KiB a second as the store being unavailable while its bytes were
// still on the link, and the next beat sent it whole again behind them — the
// queue's defect of crewlet/crewlet#1005 in the coordination store. A listing
// is now written with no deadline of its caller's, and a write with none was
// given the client's five seconds alone, whatever it carried. Here the leaf's
// link carries 96 KiB a second towards its member, so a listing near the
// bound takes about nine seconds, and its caller sets no deadline: it is
// written, and read back whole.
//
// Mutation: drop the crossing from a settled bucket's budget, or open the
// store as though its connection crossed no link, and the listing fails at
// five seconds.
func TestAListingAcrossALeafsLinkIsWaitedForAsLongAsItsBytesTakeToCross(t *testing.T) {
	t.Parallel()
	link := linktest.Slow(t, fmt.Sprintf("127.0.0.1:%d", leafMember(t)), slowLinkRate)
	fleet := openFleetVia(t, leafClientAt(t, link.Addr(), "leaf"), AcrossLink,
		fmt.Sprintf("x%d", bucketSeq.Add(1)))
	listing := listingNearTheBound(t)
	start := time.Now()
	stored, err := fleet.PutMCPListing(context.Background(), listing)
	took := time.Since(start) //fixedwait:measure the write's duration, reported beside its answer and deciding nothing
	if err != nil {
		t.Fatalf("a listing of %d bytes across a link carrying %d KiB a second, with no deadline "+
			"of its caller's, was not written (after %v): %v", listing.EncodedSize(),
			slowLinkRate>>10, took, err)
	}
	held, err := fleet.ListMCPListings(context.Background())
	if err != nil {
		t.Fatalf("read the listings back: %v", err)
	}
	if len(held) != 1 || held[0].Version != stored.Version || len(held[0].Tools) != len(listing.Tools) {
		t.Errorf("the store holds %d listings after the write answered version %d, want the "+
			"one of %d tools", len(held), stored.Version, len(listing.Tools))
	}
}

// listingNearTheBound is a listing that encodes within a few kibibytes under
// coord.MaxMCPListingBytes: eight hundred tools, their words random text, so
// the bytes on the link are the listing's own and not what a compressing link
// would leave of them.
func listingNearTheBound(t *testing.T) coord.MCPListing {
	t.Helper()
	const tools = 800
	unknown := coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
		Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown}
	words := make([]byte, tools*coord.MaxMCPToolDescriptionBytes)
	if _, err := rand.Read(words); err != nil {
		t.Fatalf("random words: %v", err)
	}
	text := base64.StdEncoding.EncodeToString(words)
	build := func(each int) coord.MCPListing {
		l := coord.MCPListing{Server: "big", Node: "leaf", Epoch: 1, At: time.Now().UTC(), Instances: 1}
		for i := range tools {
			name := fmt.Sprintf("tool_%04d", i)
			sum := sha256.Sum256([]byte(name))
			digest := hex.EncodeToString(sum[:])
			part := func(which string) string {
				sum := sha256.Sum256([]byte(name + "#" + which))
				return hex.EncodeToString(sum[:])[:coord.MCPToolPartLength]
			}
			l.Tools = append(l.Tools, coord.MCPListedTool{Name: name, Raw: name,
				Description: text[i*each : (i+1)*each], Hints: unknown, Declared: unknown,
				Digest: digest, Instances: 1, Parts: coord.MCPToolParts{Title: part("title"),
					Description: part("description"), Schema: part("schema"), Hints: part("hints")}})
		}
		return l
	}
	// THE WORDS SIZED TO FILL WHAT THE IDENTITIES LEAVE, once: a
	// description's bytes are its own, since no base64 character is escaped.
	room := coord.MaxMCPListingBytes - build(0).EncodedSize()
	l := build(min(room/tools, coord.MaxMCPToolDescriptionBytes))
	if size := l.EncodedSize(); size > coord.MaxMCPListingBytes || size < coord.MaxMCPListingBytes-8<<10 {
		t.Fatalf("the listing encodes to %d bytes, want within 8 KiB under %d", size, coord.MaxMCPListingBytes)
	}
	if strings.Contains(l.Tools[0].Description, "\\") {
		t.Fatalf("the listing's words are escaped as stored, so its size is not its words'")
	}
	return l
}

// deadlined is a bucket's client handle that records the deadline of the
// context each write it is handed waits in, beside the instant it was handed
// it, and answers every write as written.
//
// EMBEDS THE INTERFACE: the four writes are four of dozens of methods.
type deadlined struct {
	jetstream.KeyValue
	writes []waited
}

// waited is one write as deadlined saw it.
type waited struct {
	op           string
	at, deadline time.Time
	set          bool
}

func (d *deadlined) note(ctx context.Context, op string) {
	deadline, set := ctx.Deadline()
	d.writes = append(d.writes, waited{op: op, at: time.Now(), deadline: deadline, set: set})
}

func (d *deadlined) Put(ctx context.Context, _ string, _ []byte) (uint64, error) {
	d.note(ctx, "put")
	return 1, nil
}

func (d *deadlined) Update(ctx context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
	d.note(ctx, "update")
	return 2, nil
}

func (d *deadlined) Delete(ctx context.Context, _ string, _ ...jetstream.KVDeleteOpt) error {
	d.note(ctx, "delete")
	return nil
}

func (d *deadlined) Purge(ctx context.Context, _ string, _ ...jetstream.KVDeleteOpt) error {
	d.note(ctx, "purge")
	return nil
}

// A COORDINATION WRITE IS GIVEN ITS BYTES' CROSSING ONLY ACROSS A LINK.
//
// Every write of a settled bucket whose caller set no deadline waits the
// request term, and where the store reaches its buckets across a link the
// crossing of what the write carries — its key, its value, the most header a
// write of the client's carries — for every send the client may make of it
// inside the one context it is handed, at the object store's floor. A store
// in the node's own process crosses nothing, and its writes keep the request
// term alone. A caller's own deadline is every write's as it is. Exact, in a
// bubble.
//
// Mutation: drop the crossing from a settled bucket's budget, and every write
// across a link is held to the request term alone; give an in-process store a
// pace, and its writes wait for crossings they never make.
func TestACoordinationWriteIsGivenItsBytesCrossingOnlyAcrossALink(t *testing.T) {
	t.Parallel()
	const key = "mcp.listing.big.leaf"
	value := make([]byte, 64<<10)
	for _, c := range []struct {
		reach Reach
		pace  time.Duration
	}{
		{InProcess, 0},
		{AcrossLink, objstore.MiBPace},
	} {
		t.Run(string(c.reach), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				writes := map[string]time.Duration{}
				for op, carries := range map[string]int{
					"put": len(key) + len(value), "update": len(key) + len(value),
					"delete": len(key), "purge": len(key),
				} {
					writes[op] = jsapi.RequestTerm + jsapi.WriteSends*
						jsapi.Sending(carries+kvHeaderBytes, c.pace)
				}

				d := &deadlined{}
				b := settled(termed{term: jsapi.RequestTerm}, d, c.reach)
				write := func(ctx context.Context) {
					t.Helper()
					if _, err := b.Put(ctx, key, value); err != nil {
						t.Fatalf("put: %v", err)
					}
					if _, err := b.Update(ctx, key, value, 1); err != nil {
						t.Fatalf("update: %v", err)
					}
					if err := b.Delete(ctx, key, jetstream.LastRevision(2)); err != nil {
						t.Fatalf("delete: %v", err)
					}
					if err := b.Purge(ctx, key); err != nil {
						t.Fatalf("purge: %v", err)
					}
				}
				write(context.Background())
				for _, w := range d.writes {
					if want := writes[w.op]; !w.set || w.deadline.Sub(w.at) != want {
						t.Errorf("a %s with no deadline of its caller's waits %v (set %t), want %v",
							w.op, w.deadline.Sub(w.at), w.set, want)
					}
				}
				if len(d.writes) != len(writes) {
					t.Fatalf("%d writes reached the client, want %d", len(d.writes), len(writes))
				}

				d.writes = nil
				caller, cancel := context.WithTimeout(context.Background(), time.Hour)
				defer cancel()
				write(caller)
				kept, _ := caller.Deadline()
				for _, w := range d.writes {
					if !w.deadline.Equal(kept) {
						t.Errorf("a %s with its caller's hour waits until %v, want the caller's %v",
							w.op, w.deadline, kept)
					}
				}
			})
		})
	}
}

// refusingInFlight is deadlined whose every update its leader answers in
// flight, writing nothing — and answers with errPast an attempt whose deadline
// is past past, so a write that outran its bound ends rather than running on.
type refusingInFlight struct {
	deadlined
	past time.Time
}

// errPast is what refusingInFlight answers an attempt waiting past its bound.
var errPast = errors.New("an attempt waited past the write's bound")

func (r *refusingInFlight) Update(ctx context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
	r.note(ctx, "update")
	if deadline, _ := ctx.Deadline(); deadline.After(r.past) { //fixedwait:stated the deadline an attempt was handed against the bound its write began with, both on the case's bubble clock
		return 0, errPast
	}
	return 0, refusedAs(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
}

func (r *refusingInFlight) Bucket() string { return "refusing" }

// A SETTLED WRITE MET BY IN-FLIGHT ANSWERS FOR EVER ENDS UNDECIDED AT ITS
// BOUND, every call of it waiting that one bound.
//
// The client sends a write nobody heard again inside the one context it is
// handed, so each call of it is given the budget's whole bound: the request
// term and, across a link, a crossing for each of the sends a write may make.
// An attempt after an in-flight answer is another call within that same
// bound, never a bound of its own: counted per attempt with no end, a leader
// answering in flight at once pushed a write's end out faster than time
// passed, and it never ended undecided. Here every call is answered in flight
// at once, and the write ends Undecided at its bound. Exact, in a bubble.
//
// Mutation: give each attempt a bound of its own, or let the bound grow with
// the attempts, and an attempt waits past it.
func TestASettledWriteMetByInFlightAnswersForeverEndsUndecidedAtItsBound(t *testing.T) {
	t.Parallel()
	const key = "mcp.listing.big.leaf"
	value := make([]byte, 64<<10)
	bound := jsapi.RequestTerm + jsapi.WriteSends*jsapi.Sending(len(key)+len(value)+kvHeaderBytes,
		objstore.MiBPace)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		r := &refusingInFlight{past: start.Add(bound)}
		b := settled(termed{term: jsapi.RequestTerm}, r, AcrossLink)
		_, err := b.Update(context.Background(), key, value, 1)
		var undecided *jsinflight.Undecided
		if !errors.As(err, &undecided) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Update = %v after %v and %d attempts, want Undecided at the write's "+
				"bound, %v", err, time.Since(start), len(r.writes), bound)
		}
		if ended := time.Since(start); ended != bound {
			t.Errorf("a write met by in-flight answers for ever ended %v after it started, "+
				"want its bound, %v", ended, bound)
		}
		if len(r.writes) < 2 {
			t.Fatalf("%d attempts, want the in-flight answers waited out", len(r.writes))
		}
		for i, w := range r.writes {
			if !w.set || !w.deadline.Equal(start.Add(bound)) {
				t.Errorf("attempt %d waited until %v after the write started (set %t), want "+
					"its bound, %v", i+1, w.deadline.Sub(start), w.set, bound)
				break
			}
		}
	})
}

// A STORE IS TOLD HOW ITS CONNECTION REACHES ITS BUCKETS, and refuses to open
// without being told.
//
// Nothing a store is handed says whether its connection crosses a link, and a
// store that guessed "in process" for a leaf's connection would hold a large
// write to the request term alone. So each store's configuration states it,
// and one that does not — or states a reach nothing knows — is refused,
// naming the field of the configuration its caller built, exactly as the
// error opens: the lease store's "Config.Reach" is also the tail of
// "EpochsConfig.Reach", the configuration it derives and checks after its own.
//
// Mutation: open a store whose configuration states no reach, and its row
// goes red; drop the lease store's own check, and the refusal names the
// epochs' configuration, which its caller never built.
func TestAStoreRefusesToOpenWithoutItsReach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := jsOf(embeddedNATS(t))
	for _, c := range []struct {
		name  string
		field string
		open  func(Reach) error
	}{
		{"the lease store", "Config.Reach", func(r Reach) error {
			_, err := Open(ctx, client, Config{TTL: time.Minute, Reach: r})
			return err
		}},
		{"the fleet store", "FleetConfig.Reach", func(r Reach) error {
			_, err := OpenFleet(ctx, client, FleetConfig{Reach: r, MaxRateWindow: time.Minute,
				MaxClaimTTL: time.Minute, LedgerRetention: time.Minute, FireRetention: time.Minute,
				FollowRetention: time.Minute, RebaseRetention: time.Minute, BudgetRetention: time.Minute,
				StatusFreshness: time.Minute, CustodyRetention: time.Minute})
			return err
		}},
		{"the epochs", "EpochsConfig.Reach", func(r Reach) error {
			_, err := OpenEpochs(ctx, client, EpochsConfig{Reach: r})
			return err
		}},
	} {
		for _, r := range []Reach{"", "nearby"} {
			if err := c.open(r); err == nil || !strings.HasPrefix(err.Error(), "coord/kv: "+c.field+" ") {
				t.Errorf("%s opened with the reach %q = %v, want it refused naming %s",
					c.name, r, err, c.field)
			}
		}
	}
}

// A RUN'S VALUE IS GIVEN ITS CROSSING BY WHOEVER BOUNDS ITS CHANGE, AS THE
// STORE GIVES IT, AND ONLY ACROSS A LINK.
//
// A caller that bounds a change of a run's row for a reason of its own adds
// the time a value at what one key holds takes to cross its node's connection
// (coord.SandboxRuns.ValueCrossing), since the store keeps a caller's deadline
// as it is — and every change is a read and a write. So that figure is what
// the store gives such a change itself: the read's reply crossing, read off
// the term a read of the runs bucket is given beyond the read term less its
// reply's handling in process, and the write's crossings, read off a settled
// bucket's own budget for a put of a value at what one key holds under the
// longest key a run's write is sent under beyond the request term — both at
// the slowest pace a link is allowed, the contract's figure
// (coord.MaxValueCrossing) — and none on a member's own connection, where
// nothing crosses. Exact, in a bubble.
//
// Mutation: give an in-process store a crossing, and every budget a data node
// holds a row's change to waits thirteen minutes past its reason; drop the
// read, the key or a send from the figure, count a send more in the budget,
// or move the contract's figure, and the figure and the store's own read and
// budget part.
func TestARunsValueIsGivenItsCrossingOnlyAcrossALink(t *testing.T) {
	t.Parallel()
	turnID := uuid.Must(uuid.NewV7()).String()
	for _, key := range append([]string{encodeKey(turnID)}, partKeys(turnID)...) {
		if len(key) > longestRunKey {
			t.Fatalf("a run's key %q is %d bytes, past the %d a run's value is counted at", key, len(key),
				longestRunKey)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		d := &deadlined{}
		b := settled(termed{term: jsapi.RequestTerm}, d, AcrossLink)
		key := strings.Repeat("k", longestRunKey)
		if _, err := b.Put(context.Background(), key, make([]byte, coord.MaxValueBytes)); err != nil {
			t.Fatalf("put a value at what one key holds: %v", err)
		}
		if len(d.writes) != 1 || !d.writes[0].set {
			t.Fatalf("the put reached the client %d times, want once, with a deadline", len(d.writes))
		}
		written := d.writes[0].deadline.Sub(d.writes[0].at) - jsapi.RequestTerm
		read := runsReadTerm(t, AcrossLink) - jsprovision.ReadTerm - jsapi.Handling(coord.MaxValueBytes+kvHeaderBytes)
		if given := read + written; given != coord.MaxValueCrossing {
			t.Errorf("a settled bucket across a link gives a change of a run's value at what one key "+
				"holds %v beyond the read term and the request term — %v for its read's reply and %v "+
				"for its write — and coord.MaxValueCrossing says %v", given, read, written,
				coord.MaxValueCrossing)
		}
	})
	nc := embeddedNATS(t)
	for reach, crossing := range map[Reach]time.Duration{
		InProcess:  0,
		AcrossLink: coord.MaxValueCrossing,
	} {
		store := openFleetVia(t, jsOf(nc), reach, fmt.Sprintf("x%d", bucketSeq.Add(1)))
		if got := store.ValueCrossing(); got != crossing {
			t.Errorf("a store reaching its buckets %s gives a run's value a crossing of %v, want %v",
				reach, got, crossing)
		}
	}
}

// A READ OF A RUN'S VALUE IS GIVEN THE ARRIVAL OF A REPLY AT ITS BOUND, AT
// ITS NODE'S REACH — AND NO OTHER BUCKET'S IS.
//
// Every change of a run's record or of a part beside it begins with the
// stream leader's read of the value, whose reply carries it base64-encoded in
// JSON: eight mebibytes at its bound. Held to the read term and the lookup
// ceiling, that read was asked again at two seconds and at four while the
// first reply was still being built or crossing — each another whole reply —
// and across a link at the slowest pace allowed, which carries such a reply in
// four minutes, it was never answered inside the thirty-second ceiling, so no
// value past about 768 KiB could be read, nor changed (crewlet/crewlet#1149).
// So the runs bucket's reads are given the arrival of a reply at its bound:
// its handling in process, on a member's own connection, and its crossing at
// the floor besides, across a link. A bucket of small values keeps the read
// term, so a request a member dropped is asked again at two seconds. Read off
// a store opened as the engine opens one.
//
// Mutation: read the runs bucket as any other, open it without declaring its
// values large, or drop the handling or the crossing from a read's arrival,
// and its row goes red; declare every bucket large, and the leases' does.
func TestARunsValueIsReadGivenTheArrivalOfAReplyAtItsBound(t *testing.T) {
	t.Parallel()
	// THE FIGURES STATED, not derived again: a value at its bound with the most
	// header a write leaves on it, base64-encoded in its reply beside the
	// reply's envelope, is a reply of exactly eight mebibytes — eight seconds
	// handled at a second a mebibyte, and 240 crossing at thirty.
	if reply := jsapi.ReplyBytes(coord.MaxValueBytes + kvHeaderBytes); reply != 8<<20 {
		t.Fatalf("the reply to a read of a run's value at its bound is %d bytes, want eight mebibytes", reply)
	}
	for reach, want := range map[Reach]time.Duration{
		InProcess:  jsprovision.ReadTerm + 8*time.Second,
		AcrossLink: jsprovision.ReadTerm + 8*time.Second + 240*time.Second,
	} {
		if got := runsReadTerm(t, reach); got != want {
			t.Errorf("a read of a run's value %s is given a term of %v, want %v", reach, got, want)
		}
	}
	nc := embeddedNATS(t)
	for _, reach := range []Reach{InProcess, AcrossLink} {
		store := openFleetVia(t, jsOf(nc), reach, fmt.Sprintf("x%d", bucketSeq.Add(1)))
		runs, err := newLeaderReader(store.js, store.runs)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := runs.leader.Term(), runsReadTerm(t, reach); got != want {
			t.Errorf("the fleet store's runs %s are read at a term of %v, want %v", reach, got, want)
		}
		claims, err := newLeaderReader(store.js, store.claims)
		if err != nil {
			t.Fatal(err)
		}
		if got := claims.leader.Term(); got != jsprovision.ReadTerm {
			t.Errorf("the fleet store's claims %s are read at a term of %v, want the read term %v",
				reach, got, jsprovision.ReadTerm)
		}
	}
}

// runsReadTerm is the term a read of a bucket declared to hold values at what
// one key holds ([holdingLarge]) is given, opened at reach: the store's own
// reader of it ([newLeaderReader]), on a client that answers nothing.
func runsReadTerm(t *testing.T, reach Reach) time.Duration {
	t.Helper()
	read, err := newLeaderReader(connless{termed{term: jsapi.RequestTerm}},
		holdingLarge(settled(termed{term: jsapi.RequestTerm}, named{bucket: "runs"}, reach)))
	if err != nil {
		t.Fatal(err)
	}
	return read.leader.Term()
}

// connless is a client of the account's API that reaches no broker: enough to
// address a leader's reads and read their term, which asks nothing.
type connless struct{ termed }

func (connless) Conn() *nats.Conn { return nil }

// named is a bucket's handle that says its name and does nothing else.
type named struct {
	jetstream.KeyValue
	bucket string
}

func (n named) Bucket() string { return n.bucket }

// partKeys is the key of every part a run whose turn id is turnID keeps beside
// its record.
func partKeys(turnID string) []string {
	var keys []string
	for _, part := range coord.RunParts() {
		keys = append(keys, runPartKey(turnID, part))
	}
	return keys
}
