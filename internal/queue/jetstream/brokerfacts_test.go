package jetstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// The facts in this file belong to the VENDORED CLIENT and its broker, not to
// this package: asserted here because the design rests on them and neither is
// visible from a checkout, so a bump changing one would silently trim the wrong
// records or make a catch-up replay fetch a hundred times its sizing.

// A PURGE BY SEQUENCE REMOVES EXACTLY WHAT IS BELOW IT.
//
// The trim removes everything below a floor, the lowest position any counted
// node still needs. WithPurgeSequence(n) KEEPS n: a client that removed it too
// would silently and irrecoverably delete the record a node is about to replay.
func TestAPurgeBySequenceKeepsTheFloor(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	const total = 10
	for i := range total {
		// THROUGH THE CLIENT, not Queue.Publish, which derives a NAMESPACE
		// stream from the subject's first token: the seat-mailbox
		// topology, not a domain's.
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

// A FETCH IS BOUNDED BY BYTES, the bound a replay needs: its records run from
// a 140-byte barrier to over a megabyte, so no message count fits.
//
// The vendored nats.go offers no pull carrying both bounds: Fetch takes no
// byte option, FetchBytes fixes the count at one million (unbounded), and
// PullMaxBytes (Consume and Messages only) is exclusive with PullMaxMessages.
// So the replay pulls with FetchBytes and the consumer's MaxAckPending bounds
// the count, across every pull in flight rather than within one.
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
// [Queue.DomainConsumer]'s ErrConsumerExists branch rests on it: the
// state-log consumer's name carries the node id, so no peer races it, but the
// node made it on an earlier boot, and inside the metadata propagation window
// the lookup before the create answers not-found. OptStartSeq moves with the
// checkpoint between boots and the broker holds a durable's start sequence
// immutable, so the create errors. Both halves are asserted: an IDENTICAL
// config comes back as the existing consumer with no error.
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

	// THE SAME CONFIG is not an error, so the branch cannot treat every
	// re-create as a recovery.
	if _, err := q.js.CreateConsumer(ctx, spec.Name, base); err != nil {
		t.Errorf("re-creating an identical consumer failed with %v, so a "+
			"restart whose checkpoint had not moved would fail too", err)
	}

	// A MOVED START SEQUENCE is: every real restart's shape, the
	// checkpoint having advanced since the consumer was made.
	moved := base
	moved.OptStartSeq = 4
	_, err := q.js.CreateConsumer(ctx, spec.Name, moved)
	if !errors.Is(err, jetstream.ErrConsumerExists) {
		t.Fatalf("re-creating with a moved start sequence gave %v, want "+
			"ErrConsumerExists — DomainConsumer's recovery branch keys on "+
			"that error and would be unreachable", err)
	}
}

// A RELOAD REACHES A CLIENT WHOSE HANDSHAKE IS STILL IN FLIGHT.
//
// The INFO ending a handshake answers its first PING, behind the PONG
// nats.Connect returns on, and states the limit the server holds the
// connection to as that INFO is built, so a reload landing first is in it. A
// case staging a reload its client is not told about finishes the handshake
// first, with announcedMaxPayload's round trip. Spoken by hand, so the reload
// lands between an acknowledged CONNECT and that PING every time.
//
// Mutation: reload after the PING's INFO rather than before the PING and this
// goes red.
func TestAReloadReachesAHandshakeStillInFlight(t *testing.T) {
	t.Parallel()
	const before, after = 8 << 20, 4 << 10
	srv := externaltest.Start(t, before)
	conn, err := net.Dial("tcp", srv.HostPort())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("bound the exchange: %v", err)
	}
	in := bufio.NewReader(conn)
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(conn, line+"\r\n"); err != nil {
			t.Fatalf("send %s: %v", line, err)
		}
	}
	expect := func(op string) string {
		t.Helper()
		line, err := in.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, op) {
			t.Fatalf("read %q (%v), want %s", line, err, op)
		}
		return line
	}
	maxPayload := func(line string) int32 {
		t.Helper()
		var info server.Info
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "INFO ")), &info); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		return info.MaxPayload
	}

	if got := maxPayload(expect("INFO ")); got != before {
		t.Fatalf("the INFO on accept states a max_payload of %d, want %d", got, before)
	}
	send(fmt.Sprintf(`CONNECT {"verbose":true,"protocol":1,"auth_token":%q}`,
		externaltest.Token))
	expect("+OK")
	srv.Reload(after)
	send("PING")
	expect("PONG")
	if got := maxPayload(expect("INFO ")); got != after {
		t.Fatalf("a reload between CONNECT and the first PING left the INFO "+
			"answering it at a max_payload of %d, want %d: the server no longer "+
			"tells a handshake in flight, and the round trip before each staged "+
			"reload is to be revisited", got, after)
	}
}

// A LIVE RELOAD OF A LOWER max_payload CLOSES A CONNECTED CLIENT FOR GOOD, AT
// ITS FIRST MESSAGE PAST THE NEW LIMIT, UNLESS THAT MESSAGE IS STILL BEING
// WRITTEN WHEN THE SERVER CLOSES: THEN IT MAY RECONNECT ONTO THE LOWER LIMIT.
//
// The deployment guide's "never lower it with a live reload", and what
// carriesTheContract says it cannot see, rest on this. A handshaken connection
// gets the new limit WITHOUT an INFO (one mid-handshake is told, so this case
// handshakes first): no client check learns it. A message the socket takes
// whole meets `Maximum Payload Violation`, read as final: CLOSED, even
// reconnecting for ever. One taken in part fails its write as the server
// closes; nats.go (since 1.55.0) drops the refusal and reconnects onto the
// lowered limit. A race decides which, so that row admits either.
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
			// Finished before the reload, which otherwise can reach the
			// client in the INFO ending its handshake.
			if got, err := announcedMaxPayload(nc); err != nil || got != before {
				t.Fatalf("finish the handshake: read a max_payload of %d (%v), "+
					"want %d", got, err, before)
			}

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
			for !nc.IsClosed() && !reconnected() && time.Until(deadline) > 0 { //fixedwait:measure a poll's own deadline
				time.Sleep(20 * time.Millisecond) //fixedwait:liveness
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
				// lowered limit, so the same message is refused before a
				// byte is written. Every publisher here writes through
				// that check, so a retrying producer meets
				// record_too_large, not a reconnect per attempt.
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
// The classifier spells each code as a number (nats.go names few), which a
// server bump can move silently. Held here, not beside the classifier, because
// this package links the server, whose error table is the only authority.
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
