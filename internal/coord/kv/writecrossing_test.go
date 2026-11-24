package kv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// slowLinkRate is the pace the case below holds a leaf's link to, towards its
// member: 96 KiB a second, about three times the slowest allowed
// ([objstore.MiBPace]), so a listing near its bound takes about nine seconds
// to cross: past the client's five-second default, far inside what its bytes
// are given at the slowest pace.
const slowLinkRate = 96 << 10

// A LISTING ACROSS A LEAF'S LINK IS WAITED FOR AS LONG AS ITS BYTES TAKE TO
// CROSS IT (package doc, "A write across a link waits as long as its bytes
// take to cross"). A listing is written with no deadline of its caller's, and
// one of up to coord.MaxMCPListingBytes takes about 26 seconds at the slowest
// pace allowed. Here one near the bound crosses the leaf's link in about nine
// seconds, and is written and read back whole.
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
// it, and answers every write as written; a conditional write reaches it as
// its client's publish ([deadlined.publish]).
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

func (d *deadlined) publish(ctx context.Context, _ *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	d.note(ctx, "update")
	return &jetstream.PubAck{Sequence: 2}, nil
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
// A settled bucket's write whose caller set no deadline waits the request
// term plus, across a link, the crossing of its key, its value and the most
// header a client write carries, for every send the client may make inside
// its one context, at the object store's floor. In process it keeps the
// request term alone; a caller's own deadline is kept as it is. Exact, in a
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
				b := settled(termed{term: jsapi.RequestTerm, publish: d.publish},
					named{KeyValue: d, bucket: "crossing"}, c.reach, undeclaredHolding)
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

func (r *refusingInFlight) publish(ctx context.Context, _ *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	r.note(ctx, "update")
	if deadline, _ := ctx.Deadline(); deadline.After(r.past) { //fixedwait:stated the deadline an attempt was handed against the bound its write began with, both on the case's bubble clock
		return nil, errPast
	}
	return nil, leaderRefusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
}

func (r *refusingInFlight) Bucket() string { return "refusing" }

// A SETTLED WRITE MET BY IN-FLIGHT ANSWERS FOR EVER ENDS UNDECIDED AT ITS
// BOUND, every call of it waiting that one bound.
//
// The client resends a write inside the one context it is handed, so each
// call gets the budget's whole bound: the request term and, across a link, a
// crossing per send. An attempt after an in-flight answer is another call
// within that bound, never a bound of its own: counted per attempt, a leader
// answering in flight at once pushes the end out faster than time passes, and
// the write never ends. Exact, in a bubble.
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
		b := settled(termed{term: jsapi.RequestTerm, publish: r.publish}, r, AcrossLink, undeclaredHolding)
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
// Nothing a store is handed says whether its connection crosses a link, and
// guessing "in process" for a leaf would hold a large write to the request
// term alone. A configuration stating no reach, or one nothing knows, is
// refused naming the field its caller built, as the error opens: the lease
// store's "Config.Reach" is also the tail of "EpochsConfig.Reach", which it
// derives and checks after its own.
//
// Mutation: open a store stating no reach, and its row goes red; drop the
// lease store's own check, and the refusal names EpochsConfig.Reach instead.
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
			_, err := Open(ctx, client, client, Config{TTL: time.Minute, Reach: r})
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
			_, err := OpenEpochs(ctx, client, client, EpochsConfig{Reach: r})
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

// A RUN'S VALUE IS GIVEN ITS CROSSING BY WHOEVER BOUNDS ITS CHANGE OR ITS READ,
// AS THE STORE GIVES IT, AND ONLY ACROSS A LINK.
//
// The store keeps a caller's deadline as it is, so a caller bounding a run's
// change adds coord.SandboxRuns.ValueCrossing: the runs read term past the read
// term plus a settled put of a full value under the longest run key past the
// request term, at the slowest pace allowed (coord.MaxValueCrossing); one
// bounding a read, its reply's alone (coord.MaxValueReadCrossing). In process
// both are zero, or a data node's every change waits past its reason. Bubbled.
//
// Mutation: cross in process, drop the read, key or a send from the figure,
// count a send more, give a read a change's crossing, or move either constant.
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
		b := settled(termed{term: jsapi.RequestTerm}, d, AcrossLink, runsHolding)
		key := strings.Repeat("k", longestRunKey)
		if _, err := b.Put(context.Background(), key, make([]byte, coord.MaxValueBytes)); err != nil {
			t.Fatalf("put a value at what one key holds: %v", err)
		}
		if len(d.writes) != 1 || !d.writes[0].set {
			t.Fatalf("the put reached the client %d times, want once, with a deadline", len(d.writes))
		}
		written := d.writes[0].deadline.Sub(d.writes[0].at) - jsapi.RequestTerm
		read := AcrossLink.readCrossing()
		if given := read + written; given != coord.MaxValueCrossing {
			t.Errorf("a settled bucket across a link gives a change of a run's value at what one key "+
				"holds %v beyond the read term and the request term — %v for its read's reply and %v "+
				"for its write — and coord.MaxValueCrossing says %v", given, read, written,
				coord.MaxValueCrossing)
		}
		if read != coord.MaxValueReadCrossing {
			t.Errorf("a read of a run's value at what one key holds is given %v across a link for its "+
				"reply, and coord.MaxValueReadCrossing says %v", read, coord.MaxValueReadCrossing)
		}
	})
	nc := embeddedNATS(t)
	for reach, crossing := range map[Reach]struct{ change, read time.Duration }{
		InProcess:  {},
		AcrossLink: {coord.MaxValueCrossing, coord.MaxValueReadCrossing},
	} {
		store := openFleetVia(t, jsOf(nc), reach, fmt.Sprintf("x%d", bucketSeq.Add(1)))
		if got := store.ValueCrossing(); got != crossing.change {
			t.Errorf("a store reaching its buckets %s gives a change of a run's value a crossing of %v, "+
				"want %v", reach, got, crossing.change)
		}
		if got := store.ValueReadCrossing(); got != crossing.read {
			t.Errorf("a store reaching its buckets %s gives a read of a run's value a crossing of %v, "+
				"want %v", reach, got, crossing.read)
		}
	}
}

// EVERY READ IS GIVEN THE CROSSING OF ITS CLASS'S BOUND, AND EVERY PASS OR
// WATCH THE RAW CROSSING OF THE LARGEST VALUE UNDER ITS FILTER.
//
// A run's change reads a reply of 8 MiB at the bound: four minutes at the
// slowest link pace, none of its handling. A body's read gets its arrival, the
// pointer and claims the term, each positions class its own. A pass over the
// runs, values or headers, is quiet and beats 185 s across a link, 5 s in
// process. A class is one key at most, at the term only if it crosses in it.
//
// Mutation: let leaderFor or most ignore a class, undeclare any class above,
// read bodies or an integration's findings at the term, declare a class past
// one key, or give a headers pass passIdle.
func TestEveryReadIsGivenTheCrossingOfItsClassBound(t *testing.T) {
	t.Parallel()
	// THE FIGURES STATED, not derived again: a full value with the most header,
	// base64-encoded in its reply's envelope, is exactly eight mebibytes: 240
	// seconds at thirty a mebibyte. Its raw delivery behind a protocol line
	// takes 180.007 s.
	if reply := jsapi.ReplyBytes(coord.MaxValueBytes + kvHeaderBytes); reply != 8<<20 {
		t.Fatalf("the reply to a read of a run's value at its bound is %d bytes, want eight mebibytes", reply)
	}
	runQuiet := passIdle + jsapi.Sending(coord.MaxValueBytes+kvHeaderBytes, objstore.MiBPace)
	if runQuiet < 185*time.Second || runQuiet > 185*time.Second+10*time.Millisecond {
		t.Fatalf("a run's value at its bound crosses raw in %v beside the pass's idle, want 185.0 s", runQuiet)
	}
	turnID := uuid.Must(uuid.NewV7()).String()
	body := bodyKey(coord.BodyRef{SHA256: strings.Repeat("0", 64)})
	nc := embeddedNATS(t)
	for _, reach := range []Reach{InProcess, AcrossLink} {
		store := openFleetVia(t, jsOf(nc), reach, fmt.Sprintf("x%d", bucketSeq.Add(1)))
		runTerm, quiet := jsprovision.ReadTerm, passIdle
		if reach == AcrossLink {
			runTerm, quiet = jsprovision.ReadTerm+240*time.Second, runQuiet
		}
		for _, c := range []struct {
			bucket     jetstream.KeyValue
			key        string // "" for a filter no read names
			keys       string
			term, idle time.Duration
		}{
			{store.runs, encodeKey(turnID), encodeKey(turnID), runTerm, quiet},
			{store.runs, partKeys(turnID)[0], partKeys(turnID)[0], runTerm, quiet},
			{store.runs, "", runRecordKeys, 0, quiet},
			{store.runs, "", jetstream.AllKeys, 0, quiet},
			{store.config, activationKey, activationKey, jsprovision.ReadTerm, passIdle},
			{store.config, body, body, jsprovision.ReadTerm +
				jsapi.Arrival(coord.MaxValueBytes+kvHeaderBytes, reach.pace()), passIdle +
				jsapi.Sending(coord.MaxValueBytes+kvHeaderBytes, reach.pace())},
			{store.claims, "delivery", jetstream.AllKeys, jsprovision.ReadTerm, passIdle},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, c.bucket)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("%s's %q %s is read at a term of %v, want %v", c.bucket.Bucket(), c.key,
						reach, got, c.term)
				}
			}
			if got := c.bucket.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over %s's %q %s is quiet after %v, want %v", c.bucket.Bucket(), c.keys,
					reach, got, c.idle)
			}
		}
		// THE POSITIONS REGISTER'S CLASSES, stated from their bounds: a
		// deciding class its reply's crossing, a carrying one its arrival, a
		// small one and the rest the term; a pass the raw crossing of its
		// filter's largest, the whole register's the listing's.
		pace := reach.pace()
		deciding := func(bound int) time.Duration {
			return jsprovision.ReadTerm + jsapi.Crossing(bound+kvHeaderBytes, pace)
		}
		carrying := func(bound int) time.Duration {
			return jsprovision.ReadTerm + jsapi.Arrival(bound+kvHeaderBytes, pace)
		}
		raw := func(bound int) time.Duration { return passIdle + jsapi.Sending(bound+kvHeaderBytes, pace) }
		digest := strings.Repeat("a", 64)
		for _, c := range []struct {
			key, keys  string // key "" for a filter no read names
			term, idle time.Duration
		}{
			{coord.MCPListingKey("search", "node-a"), coord.DocumentFilter(coord.MCPListingClass),
				carrying(coord.MaxMCPListingBytes), raw(coord.MaxMCPListingBytes)},
			{coord.MCPDefinitionKey(digest), coord.DocumentFilter(coord.MCPDefinitionClass),
				carrying(coord.MaxMCPDefinitionBytes), raw(coord.MaxMCPDefinitionBytes)},
			{coord.MCPCheckKey("search"), coord.DocumentFilter(coord.MCPCheckClass),
				deciding(coord.MaxMCPCheckBytes), raw(coord.MaxMCPCheckBytes)},
			{coord.AccessReviewKey("sre"), coord.AccessReviewFilter(),
				deciding(coord.MaxAccessReviewBytes), raw(coord.MaxAccessReviewBytes)},
			{coord.SeatAccessKey("gitlab", "sre"), coord.SeatAccessFilter("gitlab"),
				deciding(coord.MaxSeatAccessBytes), raw(coord.MaxSeatAccessBytes)},
			{coord.HomeBoardKey("founder"), coord.HomeBoardFilter(),
				deciding(coord.MaxHomeBoardRecordBytes), raw(coord.MaxHomeBoardRecordBytes)},
			{coord.AtlasWakeKey("sre", "entry"), coord.AtlasWakeFilter(), jsprovision.ReadTerm, passIdle},
			{coord.DeviceSignInKey("codex"), coord.DocumentFilter(coord.DeviceSignInClass),
				jsprovision.ReadTerm, passIdle},
			{coord.ModelCheckKey("anthropic"), coord.DocumentFilter(coord.ModelCheckClass),
				deciding(coord.MaxModelCheckBytes), raw(coord.MaxModelCheckBytes)},
			{coord.MCPGrantFamily.GrantKey("search"), coord.DocumentFilter(coord.MCPGrantClass),
				deciding(coord.MaxOAuthGrantBytes), raw(coord.MaxOAuthGrantBytes)},
			{coord.IntegrationGrantFamily.SignInKey("gitlab"), coord.DocumentFilter(coord.IntegrationSignInClass),
				deciding(coord.MaxOAuthSignInBytes), raw(coord.MaxOAuthSignInBytes)},
			{coord.GrantRequestKey("abcdefghij"), coord.GrantRequestFilter(), jsprovision.ReadTerm, passIdle},
			// A BOOT'S READS AND LISTINGS keep the term and the pass's idle.
			{coord.PositionKey("node-a"), coord.DocumentFilter(coord.PositionClass), jsprovision.ReadTerm, passIdle},
			{coord.MaintenanceKey("CREWLET_TRACKER_LOG"), coord.DocumentFilter(coord.MaintenanceClass),
				jsprovision.ReadTerm, passIdle},
			{coord.FloorKey("tracker"), coord.DocumentFilter(coord.FloorClass), jsprovision.ReadTerm, passIdle},
			{coord.AdmissionKey("node-a"), coord.DocumentFilter(coord.AdmissionClass), jsprovision.ReadTerm, passIdle},
			{"", jetstream.AllKeys, 0, raw(coord.MaxMCPListingBytes)},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, store.positions)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("the positions register's %q %s is read at a term of %v, want %v", c.key,
						reach, got, c.term)
				}
			}
			if got := store.positions.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over the positions register's %q %s is quiet after %v, want %v", c.keys,
					reach, got, c.idle)
			}
		}
		// THE UPLOADS BUCKET'S CLASSES, every read of each deciding a write a
		// caller waits on; a pass over the whole bucket an upload record's.
		for _, c := range []struct {
			key, keys  string // key "" for a filter no read names
			term, idle time.Duration
		}{
			{coord.UploadLedgerKey, coord.UploadLedgerKey,
				deciding(coord.MaxUploadLedgerBytes), raw(coord.MaxUploadLedgerBytes)},
			{coord.UploadRecordKey(turnID), coord.DocumentFilter(coord.UploadRecordClass),
				deciding(coord.MaxUploadRecordBytes), raw(coord.MaxUploadRecordBytes)},
			{coord.SaveLedgerKey("sre"), coord.DocumentFilter(coord.SaveLedgerClass),
				deciding(coord.MaxSaveLedgerBytes), raw(coord.MaxSaveLedgerBytes)},
			{"", jetstream.AllKeys, 0, raw(coord.MaxUploadRecordBytes)},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, store.uploads)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("the uploads bucket's %q %s is read at a term of %v, want %v", c.key, reach, got,
						c.term)
				}
			}
			if got := store.uploads.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over the uploads bucket's %q %s is quiet after %v, want %v", c.keys, reach,
					got, c.idle)
			}
		}
		// THE CHAT READS BUCKET'S CLASS, whose every read decides a flush a
		// person waits on; a pass or a watch over the bucket waits its raw
		// crossing.
		for _, c := range []struct {
			key, keys  string // key "" for a filter no read names
			term, idle time.Duration
		}{
			{coord.ChatReadKey("founder"), coord.DocumentFilter(coord.ChatReadClass),
				deciding(coord.MaxChatReadBytes), raw(coord.MaxChatReadBytes)},
			{"", jetstream.AllKeys, 0, raw(coord.MaxChatReadBytes)},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, store.chatReads)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("the chat reads bucket's %q %s is read at a term of %v, want %v", c.key, reach,
						got, c.term)
				}
			}
			if got := store.chatReads.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over the chat reads bucket's %q %s is quiet after %v, want %v", c.keys,
					reach, got, c.idle)
			}
		}
		// THE SECRETS BUCKET'S CLASS, whose reads decide a put, a create, a
		// rekey and a grant's renewal; the boot's snapshot and every other
		// pass over the bucket wait its raw crossing.
		secret := encodeKey("GITLAB_TOKEN")
		read, err := newLeaderReader(store.js, store.secrets)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := read.leaderFor(secret).Term(), deciding(coord.MaxSecretRecordBytes); got != want {
			t.Errorf("the secrets bucket's %q %s is read at a term of %v, want %v", secret, reach, got, want)
		}
		if got, want := store.secrets.(settledBucket).quiet(jetstream.AllKeys), raw(coord.MaxSecretRecordBytes); got != want {
			t.Errorf("a pass over the secrets bucket %s is quiet after %v, want %v", reach, got, want)
		}
		// THE ATLAS BUCKETS' CLASSES: a source's standing deciding a
		// person's ask, its misses read by the duty alone, and every ask and
		// code scan standing at the term; the standings' listing waits the
		// raw crossing of a standing, never of a source's misses.
		for _, c := range []struct {
			bucket     jetstream.KeyValue
			key, keys  string // key "" for a filter no read names
			term, idle time.Duration
		}{
			{store.atlasSources, coord.AtlasSourceStandingKey("cloudflare"),
				coord.DocumentFilter(coord.AtlasSourceStandingClass),
				deciding(coord.MaxAtlasSourceStandingBytes), raw(coord.MaxAtlasSourceStandingBytes)},
			{store.atlasSources, coord.AtlasSourceMissesKey("cloudflare"), coord.AtlasSourceMissesKey("cloudflare"),
				carrying(coord.MaxAtlasSourceMissesBytes), raw(coord.MaxAtlasSourceMissesBytes)},
			{store.atlasSources, "", jetstream.AllKeys, 0, raw(coord.MaxAtlasSourceMissesBytes)},
			{store.atlasRefresh, encodeKey("cloudflare"), jetstream.AllKeys, jsprovision.ReadTerm, passIdle},
			{store.atlasCode, coord.AtlasCodeScanKey("github", ""), coord.AtlasCodeScanFilter("github"),
				jsprovision.ReadTerm, passIdle},
			{store.atlasCode, coord.AtlasCodeScanKey(coord.AtlasCodeGitlet, turnID), jetstream.AllKeys,
				jsprovision.ReadTerm, passIdle},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, c.bucket)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("%s's %q %s is read at a term of %v, want %v", c.bucket.Bucket(), c.key, reach,
						got, c.term)
				}
			}
			if got := c.bucket.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over %s's %q %s is quiet after %v, want %v", c.bucket.Bucket(), c.keys, reach,
					got, c.idle)
			}
		}
		// THE INTEGRATIONS BUCKET'S CLASSES: a surface's status at the term,
		// every read of it deciding a write or keeping a beat's or a turn's
		// window, and its findings, which no reader decides on, their arrival;
		// a pass over the statuses keeps the pass's idle.
		for _, c := range []struct {
			key, keys  string // key "" for a filter no read names
			term, idle time.Duration
		}{
			{coord.IntegrationStatusKey("gitlab"), coord.DocumentFilter(coord.IntegrationStatusClass),
				jsprovision.ReadTerm, passIdle},
			{coord.IntegrationFindingsKey("gitlab"), coord.DocumentFilter(coord.IntegrationFindingsClass),
				carrying(coord.MaxIntegrationFindingsBytes), raw(coord.MaxIntegrationFindingsBytes)},
			{"", jetstream.AllKeys, 0, raw(coord.MaxIntegrationFindingsBytes)},
		} {
			if c.key != "" {
				read, err := newLeaderReader(store.js, store.integrations)
				if err != nil {
					t.Fatal(err)
				}
				if got := read.leaderFor(c.key).Term(); got != c.term {
					t.Errorf("the integrations bucket's %q %s is read at a term of %v, want %v", c.key, reach,
						got, c.term)
				}
			}
			if got := store.integrations.(settledBucket).quiet(c.keys); got != c.idle {
				t.Errorf("a pass over the integrations bucket's %q %s is quiet after %v, want %v", c.keys,
					reach, got, c.idle)
			}
		}
		// THE NODE STATUS BUCKET'S CLASSES: every row at the term, and every
		// pass over a class, or over one node's rows of a named class, keeps
		// the pass's idle.
		for _, class := range coord.NodeStatusClasses {
			key := coord.NodeStatusRowKey{Class: class, Node: "n1"}
			if class.Named() {
				key.Name = "github"
			}
			read, err := newLeaderReader(store.js, store.nodeStatus)
			if err != nil {
				t.Fatal(err)
			}
			if got := read.leaderFor(key.Key()).Term(); got != jsprovision.ReadTerm {
				t.Errorf("the node status bucket's %q %s is read at a term of %v, want %v", key.Key(), reach,
					got, jsprovision.ReadTerm)
			}
			for _, keys := range []string{coord.DocumentFilter(string(class)),
				coord.DocumentFilter(string(class), "n1"), jetstream.AllKeys} {
				if got := store.nodeStatus.(settledBucket).quiet(keys); got != passIdle {
					t.Errorf("a pass over the node status bucket's %q %s is quiet after %v, want %v", keys,
						reach, got, passIdle)
				}
			}
		}
		standing, err := beginPass(t.Context(), store.js, store.runs, runRecordKeys, "the runs' standing", true)
		if err != nil {
			t.Fatalf("begin a pass of the runs' standing %s: %v", reach, err)
		}
		info, err := standing.w.(*bucketWatcher).sub.ConsumerInfo()
		standing.stop()
		if err != nil {
			t.Fatal(err)
		}
		if standing.quiet != quiet || info.Config.Heartbeat != quiet {
			t.Errorf("a pass of the runs' standing %s is quiet after %v and beats at %v, want both %v",
				reach, standing.quiet, info.Config.Heartbeat, quiet)
		}
	}
	for _, h := range openedHoldings(t, nc) {
		classes := []class{h.rest}
		for _, k := range h.classes {
			classes = append(classes, k.class)
		}
		for _, c := range classes {
			if c.bound <= 0 || c.bound > coord.MaxValueBytes {
				t.Errorf("a class is held to %d bytes, want within 1..%d, what one key holds", c.bound,
					coord.MaxValueBytes)
			}
			// undeclared is exempt by value: a class equal to it IS it.
			if c.reply == replyTerm && c != undeclared && c.bound > coord.SmallValueBytes {
				t.Errorf("a class of %d bytes is read at the term, past the %d that cross inside it",
					c.bound, coord.SmallValueBytes)
			}
		}
	}
}

// openedHoldings is the holding of every bucket a fleet store and a lease
// store settle as they open, read off the handles themselves, so a bucket
// opened with a holding of its own is held to the class rules unlisted.
func openedHoldings(t *testing.T, nc *nats.Conn) []holding {
	t.Helper()
	store := openFleetForTest(t, nc, fmt.Sprintf("h%d", bucketSeq.Add(1)))
	fields := reflect.ValueOf(store).Elem()
	var found []holding
	for i := range fields.NumField() {
		if fields.Type().Field(i).Type != reflect.TypeFor[jetstream.KeyValue]() {
			continue
		}
		// Read without Interface: reflection hands out no unexported value.
		found = append(found, reflectedHolding(fields.Field(i).Elem().FieldByName("holding")))
	}
	leases := openStore(t, nc, time.Minute)
	for _, kv := range []jetstream.KeyValue{leases.leases.kv, leases.duties.kv, leases.counter.kv} {
		found = append(found, kv.(settledBucket).holding)
	}
	for _, want := range []holding{runsHolding, configHolding, positionsHolding, uploadsHolding,
		chatReadsHolding, secretsHolding, atlasSourcesHolding, atlasSmallHolding, integrationsHolding,
		nodeStatusHolding, undeclaredHolding} {
		if !slices.ContainsFunc(found, want.same) {
			t.Fatalf("premise: no opened bucket holds %+v, so the walk read the handles wrong", want)
		}
	}
	return found
}

// reflectedHolding is the holding a settled handle's reflected field holds.
func reflectedHolding(v reflect.Value) holding {
	classOf := func(c reflect.Value) class {
		return class{bound: int(c.FieldByName("bound").Int()), reply: reply(c.FieldByName("reply").Int())}
	}
	h := holding{rest: classOf(v.FieldByName("rest"))}
	keys := v.FieldByName("classes")
	for j := range keys.Len() {
		k := keys.Index(j)
		h.classes = append(h.classes, keyClass{keys: k.FieldByName("keys").String(),
			class: classOf(k.FieldByName("class"))})
	}
	return h
}

// A SMALL CLASS'S REPLY CROSSES THE FLOOR INSIDE THE READ TERM: at
// coord.SmallValueBytes it is 23,328 bytes, 0.67 s at the slowest pace a link
// is allowed. Held here, since internal/coord cannot import the pace.
//
// Mutation: raise coord.SmallValueBytes to 32 KiB (1.32 s), and it is red.
func TestASmallClassCrossesTheFloorInsideTheReadTerm(t *testing.T) {
	t.Parallel()
	if reply := jsapi.ReplyBytes(coord.SmallValueBytes + kvHeaderBytes); reply != 23_328 {
		t.Errorf("the reply to a read of a small value at its bound is %d bytes, want 23,328", reply)
	}
	if crossing := jsapi.Crossing(coord.SmallValueBytes+kvHeaderBytes, objstore.MiBPace); crossing >= jsprovision.ReadTerm {
		t.Errorf("a small value's reply crosses a link at the floor in %v, not inside the read term %v",
			crossing, jsprovision.ReadTerm)
	}
}

// A SETTLED WRITE IS HELD TO ITS CLASS'S BOUND, refused before anything is
// sent: the activation pointer past coord.MaxPointerBytes beside a body of the
// same size written; an atlas wake and a node's positions past
// coord.SmallValueBytes beside a model check; the reservation ledger past
// coord.MaxUploadLedgerBytes beside an upload record; a chat read record past
// coord.MaxChatReadBytes; a credential's record past MaxSecretRecordBytes; an
// atlas standing beside its misses and a code scan standing; an
// integration's status beside its findings; and every node status row class.
//
// Mutation: hold every value to coord.MaxValueBytes in carriedBack, and every
// one of them but the body and the findings is written.
func TestASettledWriteIsHeldToItsClassBound(t *testing.T) {
	t.Parallel()
	d := &deadlined{}
	b := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: d, bucket: "config"}, AcrossLink, configHolding)
	value := make([]byte, coord.MaxPointerBytes+1)
	_, err := b.Put(t.Context(), activationKey, value)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "8192") {
		t.Errorf("a pointer of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 8192",
			len(value), err)
	}
	if len(d.writes) != 0 {
		t.Fatalf("the refused pointer reached the client %d times, want never", len(d.writes))
	}
	if _, err := b.Put(t.Context(), "body.x", value); err != nil {
		t.Errorf("a body of %d bytes put = %v, want it written", len(value), err)
	}
	if len(d.writes) != 1 {
		t.Errorf("the body reached the client %d times, want once", len(d.writes))
	}

	// AND IN THE POSITIONS REGISTER: an atlas wake and a node's positions,
	// small values read at the term, past coord.SmallValueBytes, while a
	// model check beside them, under its own class, is written.
	p := &deadlined{}
	positions := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: p, bucket: "positions"}, AcrossLink,
		positionsHolding)
	small := make([]byte, coord.SmallValueBytes+1)
	for _, key := range []string{coord.AtlasWakeKey("sre", "entry"), coord.PositionKey("node-a")} {
		_, err = positions.Put(t.Context(), key, small)
		if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "16384") {
			t.Errorf("%s of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 16384", key,
				len(small), err)
		}
	}
	if len(p.writes) != 0 {
		t.Fatalf("the refused small values reached the client %d times, want never", len(p.writes))
	}
	if _, err := positions.Put(t.Context(), coord.ModelCheckKey("anthropic"), small); err != nil {
		t.Errorf("a model check of %d bytes put = %v, want it written", len(small), err)
	}
	if len(p.writes) != 1 {
		t.Errorf("the model check reached the client %d times, want once", len(p.writes))
	}

	// AND IN THE UPLOADS BUCKET: the reservation ledger past its bound,
	// while an upload's record of the same size, under its own, is written.
	u := &deadlined{}
	uploads := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: u, bucket: "uploads"}, AcrossLink,
		uploadsHolding)
	ledger := make([]byte, coord.MaxUploadLedgerBytes+1)
	_, err = uploads.Put(t.Context(), coord.UploadLedgerKey, ledger)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "73728") {
		t.Errorf("a reservation ledger of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 73728",
			len(ledger), err)
	}
	if len(u.writes) != 0 {
		t.Fatalf("the refused ledger reached the client %d times, want never", len(u.writes))
	}
	if _, err := uploads.Put(t.Context(), coord.UploadRecordKey("u-1"), ledger); err != nil {
		t.Errorf("an upload record of %d bytes put = %v, want it written", len(ledger), err)
	}
	if len(u.writes) != 1 {
		t.Errorf("the record reached the client %d times, want once", len(u.writes))
	}

	// AND IN THE CHAT READS BUCKET: a seat's record past its bound, while a
	// key of another shape, the rest, takes the same bytes.
	c := &deadlined{}
	reads := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: c, bucket: "chat_reads"}, AcrossLink,
		chatReadsHolding)
	record := make([]byte, coord.MaxChatReadBytes+1)
	_, err = reads.Put(t.Context(), coord.ChatReadKey("founder"), record)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "147456") {
		t.Errorf("a chat read record of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 147456",
			len(record), err)
	}
	if len(c.writes) != 0 {
		t.Fatalf("the refused record reached the client %d times, want never", len(c.writes))
	}
	if _, err := reads.Put(t.Context(), "other", record); err != nil {
		t.Errorf("a key of another shape of %d bytes put = %v, want it written", len(record), err)
	}
	if len(c.writes) != 1 {
		t.Errorf("the other key reached the client %d times, want once", len(c.writes))
	}

	// AND IN THE SECRETS BUCKET, every key a credential's record: one past
	// its bound refused, one at it written.
	k := &deadlined{}
	credentials := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: k, bucket: "secrets"}, AcrossLink,
		secretsHolding)
	sealed := make([]byte, coord.MaxSecretRecordBytes+1)
	_, err = credentials.Put(t.Context(), encodeKey("GITLAB_TOKEN"), sealed)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "98304") {
		t.Errorf("a secret's record of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 98304",
			len(sealed), err)
	}
	if len(k.writes) != 0 {
		t.Fatalf("the refused record reached the client %d times, want never", len(k.writes))
	}
	if _, err := credentials.Put(t.Context(), encodeKey("GITLAB_TOKEN"), sealed[1:]); err != nil {
		t.Errorf("a secret's record of %d bytes put = %v, want it written", len(sealed)-1, err)
	}
	if len(k.writes) != 1 {
		t.Errorf("the record at its bound reached the client %d times, want once", len(k.writes))
	}
	// AND IN THE ATLAS BUCKETS: a source's standing past its bound, while
	// its misses take the same bytes; and a code scan standing past
	// coord.SmallValueBytes.
	a := &deadlined{}
	sources := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: a, bucket: "atlas_sources"}, AcrossLink,
		atlasSourcesHolding)
	standing := make([]byte, coord.MaxAtlasSourceStandingBytes+1)
	_, err = sources.Put(t.Context(), coord.AtlasSourceStandingKey("cloudflare"), standing)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "65536") {
		t.Errorf("a standing of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 65536",
			len(standing), err)
	}
	if _, err := sources.Put(t.Context(), coord.AtlasSourceMissesKey("cloudflare"), standing); err != nil ||
		len(a.writes) != 1 {
		t.Errorf("misses of %d bytes put = %v and reached the client %d times, want written once", len(standing),
			err, len(a.writes))
	}
	s := &deadlined{}
	scans := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: s, bucket: "atlas_code_scans"}, AcrossLink,
		atlasSmallHolding)
	_, err = scans.Put(t.Context(), coord.AtlasCodeScanKey("github", ""), make([]byte, coord.SmallValueBytes+1))
	if !errors.Is(err, coord.ErrValueTooLarge) || len(s.writes) != 0 {
		t.Errorf("a code scan standing past its bound put = %v and reached the client %d times, want "+
			"coord.ErrValueTooLarge before it", err, len(s.writes))
	}

	// AND IN THE INTEGRATIONS BUCKET: a surface's status past its bound,
	// while its findings, a class of their own, take the same bytes.
	g := &deadlined{}
	statuses := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: g, bucket: "integrations"}, AcrossLink,
		integrationsHolding)
	status := make([]byte, coord.SmallValueBytes+1)
	_, err = statuses.Put(t.Context(), coord.IntegrationStatusKey("gitlab"), status)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "16384") {
		t.Errorf("a status of %d bytes put = %v, want coord.ErrValueTooLarge naming its bound, 16384",
			len(status), err)
	}
	if len(g.writes) != 0 {
		t.Fatalf("the refused status reached the client %d times, want never", len(g.writes))
	}
	if _, err := statuses.Put(t.Context(), coord.IntegrationFindingsKey("gitlab"), status); err != nil {
		t.Errorf("findings of %d bytes put = %v, want them written", len(status), err)
	}
	if len(g.writes) != 1 {
		t.Errorf("the findings reached the client %d times, want once", len(g.writes))
	}

	// AND IN THE NODE STATUS BUCKET: every class of row one byte past its
	// bound, and at it written.
	n := &deadlined{}
	rows := settled(termed{term: jsapi.RequestTerm}, named{KeyValue: n, bucket: "node_status"}, AcrossLink,
		nodeStatusHolding)
	for _, class := range coord.NodeStatusClasses {
		key := coord.NodeStatusRowKey{Class: class, Node: "n1"}
		if class.Named() {
			key.Name = "github"
		}
		_, err := rows.Put(t.Context(), key.Key(), make([]byte, coord.MaxNodeStatusRowBytes+1))
		if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "16384") {
			t.Errorf("a %s row past its bound put = %v, want coord.ErrValueTooLarge naming 16384", class, err)
		}
		if _, err := rows.Put(t.Context(), key.Key(), make([]byte, coord.MaxNodeStatusRowBytes)); err != nil {
			t.Errorf("a %s row at its bound put = %v, want it written", class, err)
		}
	}
	if len(n.writes) != len(coord.NodeStatusClasses) {
		t.Errorf("the node status rows reached the client %d times, want once each at its bound",
			len(n.writes))
	}
}

// A MEMBER'S CHANGE OF A RUN'S VALUE ASKS ITS READ AGAIN INSIDE THE SHORTEST
// REASON IT IS BOUNDED BY.
//
// In process a change is bounded by its caller's reason alone: five seconds at
// the shortest (internal/sandbox's resumeCarryRecordBudget, internal/engine's
// sessionStartRecordBudget and the steer's pull mark). A member behind a lost
// leader drops a read silently and the new leader answers the re-ask two
// seconds later, so a nine-second term (read term plus a reply's handling)
// would ask once and refuse the change. Unanswered here, it asks at none, two
// and four ([jsapitest.Held.Fence] counts them). Exact, in a bubble.
//
// Mutation: give the runs reads a reply's handling in process; it asks once.
func TestAMembersChangeOfARunsValueAsksItsReadAgainInsideItsReason(t *testing.T) {
	t.Parallel()
	const reason = 5 * time.Second
	ns, nc := embeddedServer(t)
	store := openFleetVia(t, jsOf(nc), InProcess, fmt.Sprintf("x%d", bucketSeq.Add(1)))
	held := jsapitest.Hold(t, ns, jsapitest.MsgGet(bucketStream(store.runs)))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), reason)
		defer cancel()
		_, _, err := store.MutateSandboxRun(ctx, "t1", func(coord.Record) (coord.SandboxRunChange, error) {
			return coord.SandboxRunChange{}, nil
		})
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("a change whose read nobody answered ended with %v, want the store unavailable", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	asked, err := held.Fence(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 3 {
		t.Errorf("a change of a run's value bounded by %v asked its read %d times, want 3: at none, "+
			"two and four seconds", reason, asked)
	}
}

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
