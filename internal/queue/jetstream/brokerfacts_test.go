package jetstream

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// The facts in this file belong to the VENDORED CLIENT and the broker it
// talks to, not to this package. They are asserted here because the design
// above them is built on them and because neither is visible from a checkout:
// a client bump that changed one would be a silently different engine, and
// the first symptom would be a trim that removed the wrong records or a
// catch-up replay that fetched a hundred times what it was sized for.

// A PURGE BY SEQUENCE REMOVES EXACTLY WHAT IS BELOW IT.
//
// This is the trim's whole mechanism: the fleet computes a floor — the lowest
// position any counted node still needs — and removes everything below it.
// The boundary is INCLUSIVE OF NOTHING AT THE FLOOR: WithPurgeSequence(n)
// keeps n, and a client that removed it too would delete the record a node is
// about to replay, with no error and no way to get it back.
func TestAPurgeBySequenceKeepsTheFloor(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	const total = 10
	for i := range total {
		// PUBLISHED THROUGH THE CLIENT, not through Queue.Publish: that
		// one derives a NAMESPACE stream from the subject's first token,
		// which is the seat-mailbox topology and not a domain's. A
		// domain publishes to its own stream, which is step 3's
		// publisher; here the client is enough.
		if _, err := q.js.Publish(ctx, "crewlet.probe.log.item", []byte("record")); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	stream, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if got := info.State.Msgs; got != total {
		t.Fatalf("the stream holds %d messages, want %d", got, total)
	}
	first := info.State.FirstSeq

	// Keep everything from the fifth record on.
	floor := first + 4
	if err := stream.Purge(ctx, jetstream.WithPurgeSequence(floor)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	after, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("info after the purge: %v", err)
	}
	if got, want := after.State.Msgs, uint64(total-4); got != want {
		t.Errorf("the stream holds %d messages after purging below %d, want %d",
			got, floor, want)
	}
	if after.State.FirstSeq != floor {
		t.Errorf("first_seq = %d after purging below %d: the record AT the floor "+
			"is the one the slowest node is about to replay, and a purge that "+
			"took it would remove it with no error and nothing to recover from",
			after.State.FirstSeq, floor)
	}
	if after.State.LastSeq != info.State.LastSeq {
		t.Errorf("last_seq moved from %d to %d: a purge below a floor must not "+
			"touch the head", info.State.LastSeq, after.State.LastSeq)
	}
}

// A FETCH IS BOUNDED BY BYTES, and this is what the vendored client actually
// offers rather than what the design asked for.
//
// # The finding
//
// A replay's fetch has to be bounded by SIZE, not by message count: a log
// whose modal record is a 140-byte barrier and whose largest is over a
// megabyte turns any message count into a request that is either pointless or
// enormous. The design asked for ONE pull carrying both bounds — a message
// count and a byte ceiling.
//
// nats.go v1.53.1 does not offer that, and this test is the record of it:
//
//   - Consumer.Fetch(batch, opts...) takes no byte option at all. Every
//     FetchOpt it accepts is about waiting (MaxWait, Heartbeat, Context) or
//     about priority groups.
//   - Consumer.FetchBytes(maxBytes, opts...) is the byte-bounded pull, and it
//     fixes the message count itself at one million — effectively unbounded.
//   - PullMaxBytes exists, but only for Consume and Messages, and its own
//     documentation says it is EXCLUSIVE with PullMaxMessages.
//
// So the replay pulls with FetchBytes and the message count is bounded
// elsewhere: by the consumer's MaxAckPending, which is a consumer property
// and not a per-pull one. That is a better place for it — it bounds what is
// in flight across every pull rather than within one.
func TestTheVendoredClientBoundsAFetchByBytesAndNotByBoth(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	// Records large enough that a byte bound binds before a message bound
	// would: ten of these are ~640 KiB.
	const records = 10
	body := make([]byte, 64<<10)
	for i := range records {
		if _, err := q.js.Publish(ctx, "crewlet.probe.log.item", body); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	stream, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "probe-replay",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	// A ceiling that holds two of these records and not three.
	batch, err := consumer.FetchBytes(150<<10, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("FetchBytes: %v", err)
	}
	var got, bytes int
	for msg := range batch.Messages() {
		got++
		bytes += len(msg.Data())
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got == 0 {
		t.Fatal("a byte-bounded fetch returned nothing")
	}
	if got >= records {
		t.Errorf("a 150 KiB fetch returned all %d records (%d bytes): the byte "+
			"bound did not bind, so a replay of a log whose largest record is "+
			"over a megabyte would pull the whole backlog into one transaction",
			got, bytes)
	}
}

// CREATING A DURABLE CONSUMER THAT ALREADY EXISTS WITH A DIFFERENT START
// SEQUENCE IS AN ERROR, NOT A NO-OP.
//
// This is what makes a clustered restart able to fail on the state-log
// consumer, and therefore what [Queue.DomainConsumer]'s ErrConsumerExists
// branch exists for. That consumer's name carries the node id, so no PEER can
// race it — but the node itself made it on an earlier boot, and inside the
// metadata propagation window the lookup that precedes the create answers
// not-found. The create then meets the consumer that is really there.
//
// If it merely returned the existing consumer, the branch would be dead code
// and a restart would need no tolerance at all. It does not, because the
// configs differ: OptStartSeq moves with this node's checkpoint between every
// boot, and the broker treats a durable's start sequence as immutable.
//
// Both halves are asserted, because the branch turns on the difference: an
// IDENTICAL config comes back as the existing consumer with no error.
func TestCreatingAnExistingConsumerErrsOnlyWhenTheConfigDiffers(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	base := jetstream.ConsumerConfig{
		Durable:       "statelog__probe__node_a",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   1,
	}
	if _, err := q.js.CreateConsumer(ctx, spec.Name, base); err != nil {
		t.Fatalf("create the consumer: %v", err)
	}

	// THE SAME CONFIG is not an error — which is why the branch cannot
	// simply treat every re-create as a recovery.
	if _, err := q.js.CreateConsumer(ctx, spec.Name, base); err != nil {
		t.Errorf("re-creating an identical consumer failed with %v, so a "+
			"restart whose checkpoint had not moved would fail too", err)
	}

	// A MOVED START SEQUENCE is, and this is the shape every real restart
	// takes: the node has applied records since, so its checkpoint is
	// higher than when the consumer was made.
	moved := base
	moved.OptStartSeq = 4
	_, err := q.js.CreateConsumer(ctx, spec.Name, moved)
	if !errors.Is(err, jetstream.ErrConsumerExists) {
		t.Fatalf("re-creating with a moved start sequence gave %v, want "+
			"ErrConsumerExists — DomainConsumer's recovery branch keys on "+
			"that error and would be unreachable", err)
	}
}

// A LIVE RELOAD OF A LOWER max_payload CLOSES A CONNECTED CLIENT FOR GOOD, AT
// ITS FIRST MESSAGE PAST THE NEW LIMIT, UNLESS THAT MESSAGE IS STILL BEING
// WRITTEN WHEN THE SERVER CLOSES: THEN IT MAY RECONNECT ONTO THE LOWER LIMIT.
//
// Three facts, and the deployment guide's "never lower it with a live reload"
// rests on them, as does what carriesTheContract says it cannot see. The server
// applies the reloaded limit to the connections it holds WITHOUT an INFO, so
// the client goes on reading the old figure and no check on this side can
// learn the new one. A message the socket takes whole is refused with
// `Maximum Payload Violation`, which the client reads and treats as final, so
// the connection is CLOSED rather than reconnected, whatever its reconnect
// policy (reconnect for ever here, as this package dials). And a message the
// socket takes only part of is still being written when the server closes the
// connection; since nats.go 1.55.0 a write that fails part way closes the
// socket on the client's side and drops the refusal unread, so the client
// reconnects instead and reads the lowered limit. Which of the two the larger
// message meets is a race between the client's own reader and writer, so that
// row holds the client to one or the other and to nothing else.
//
// A bump that announced a reload, that reconnected after a refusal it read,
// or that left a connection open on the old figure turns a row red, so the
// warning is rewritten rather than left standing over a hazard that has
// changed.
func TestALiveReloadOfALowerMaxPayloadClosesAClientForGood(t *testing.T) {
	t.Parallel()
	const before = 8 << 20
	for _, tc := range []struct {
		name string
		// after is the reloaded limit, and the message is twice it.
		after int32
		// reconnects marks the size whose write the server's close can
		// fail part way, which may end in a reconnect.
		reconnects bool
	}{
		// 8 KiB on the wire, which any socket takes in one write.
		{"a message the socket takes whole", 4 << 10, false},
		// 6 MiB, above the 4 MiB Linux and macOS grow a socket's send
		// buffer to at most by default, and inside the 8 MiB the client
		// still reads.
		{"a message still being written when the server closes", 3 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true,
				NoSigs: true, MaxPayload: before}
			ns, err := server.NewServer(opts)
			if err != nil {
				t.Fatalf("configure a server: %v", err)
			}
			go ns.Start()
			t.Cleanup(func() {
				ns.Shutdown()
				ns.WaitForShutdown()
			})
			if !ns.ReadyForConnections(30 * time.Second) {
				t.Fatal("the server never became ready")
			}
			nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1),
				nats.ReconnectWait(50*time.Millisecond))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(nc.Close)

			lowered := *opts
			lowered.MaxPayload = tc.after
			lowered.Port = ns.Addr().(*net.TCPAddr).Port
			if err := ns.ReloadOptions(&lowered); err != nil {
				t.Fatalf("reload a lower max_payload: %v", err)
			}
			if err := nc.FlushTimeout(5 * time.Second); err != nil {
				t.Fatalf("a round trip after the reload: %v", err)
			}
			if got := nc.MaxPayload(); got != before {
				t.Fatalf("after the reload the client reads a max_payload of %d: "+
					"the server now tells a connected client about a reload, and "+
					"the guide's warning is to be rewritten", got)
			}

			_ = nc.Publish("crewlet.fact.oversized", make([]byte, 2*tc.after))
			_ = nc.Flush()
			reconnected := func() bool {
				return nc.Stats().Reconnects > 0 && nc.IsConnected() &&
					nc.MaxPayload() == int64(tc.after)
			}
			deadline := time.Now().Add(10 * time.Second)
			for !nc.IsClosed() && !reconnected() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			switch {
			case nc.IsClosed():
				if err := nc.LastError(); err == nil ||
					!strings.Contains(err.Error(), "Maximum Payload Violation") {
					t.Errorf("the client closed on %v, not on the server's "+
						"max_payload refusal", err)
				}
			case !reconnected():
				t.Fatalf("a message past the reloaded limit left the client %v "+
					"after %d reconnects, reading a max_payload of %d: neither "+
					"closed nor reconnected onto the lowered limit, and the "+
					"guide's warning is to be rewritten", nc.Status(),
					nc.Stats().Reconnects, nc.MaxPayload())
			case !tc.reconnects:
				t.Fatal("a message the socket took whole left the client " +
					"reconnected rather than closed: it now survives a refusal " +
					"it read, and the guide's warning is to be rewritten")
			default:
				// AND THE RECONNECT ENDS IT: the client has read the
				// lowered limit, so the same message sent again is
				// refused before a byte is written, never sent for the
				// server to close the connection over again. Every
				// publisher in this module writes through that check, so
				// a producer that retries meets record_too_large rather
				// than a reconnect per attempt.
				if err := nc.Publish("crewlet.fact.oversized",
					make([]byte, 2*tc.after)); !errors.Is(err, nats.ErrMaxPayload) {
					t.Fatalf("after reconnecting onto the lowered limit the "+
						"client sent the same message with %v rather than "+
						"refusing it with ErrMaxPayload: every retry would cost "+
						"a reconnect", err)
				}
			}
		})
	}
}

// THE REFUSALS [jsprovision.Refused] TAKES AS ANSWERS ARE THE PINNED SERVER'S
// OWN CODES, and so are the race's shapes it leaves to the read-back.
//
// The classifier spells each code as a number, because nats.go names few of
// them, and a number copied from a server is one a server bump can move
// without a word. It is held here rather than beside the classifier because
// this is the package that links the server, and its error table is the only
// authority on what each identifier is.
func TestTheRefusalsNotReadBackAreThePinnedServersCodes(t *testing.T) {
	t.Parallel()
	answer := func(id server.ErrorIdentifier) *jetstream.APIError {
		e, ok := server.ApiErrors[id]
		if !ok {
			t.Fatalf("the pinned server has no error %d", id)
		}
		return &jetstream.APIError{ErrorCode: jetstream.ErrorCode(e.ErrCode),
			Code: e.Code, Description: e.Description}
	}
	for name, id := range map[string]server.ErrorIdentifier{
		"no suitable peers":    server.JSClusterNoPeersErrF,
		"no room in the file":  server.JSStorageResourcesExceededErr,
		"no room in memory":    server.JSMemoryResourcesExceededErr,
		"no applicable limit":  server.JSNoLimitsErr,
		"an invalid stream":    server.JSStreamInvalidConfigF,
		"overlapping subjects": server.JSStreamSubjectOverlapErr,
		"the most streams":     server.JSMaximumStreamsLimitErr,
		"the most consumers":   server.JSMaximumConsumersLimitErr,
	} {
		if !jsprovision.Refused(answer(id)) {
			t.Errorf("the server's %s (%d) is read back after a create it "+
				"refused, and reported as an object that is not there", name, id)
		}
	}
	for name, id := range map[string]server.ErrorIdentifier{
		"a name already in use":           server.JSStreamNameExistErr,
		"a consumer configured otherwise": server.JSConsumerAlreadyExists,
		"the generic consumer failure":    server.JSConsumerCreateErrF,
		"a group that is not available":   server.JSClusterNotAvailErr,
		"a stream not found":              server.JSStreamNotFoundErr,
		"a consumer not found":            server.JSConsumerNotFoundErr,
	} {
		if jsprovision.Refused(answer(id)) {
			t.Errorf("the server's %s (%d) is taken as a refusal, so the node "+
				"that lost a create race to a peer refuses to boot instead of "+
				"reading the peer's object back", name, id)
		}
	}
}
