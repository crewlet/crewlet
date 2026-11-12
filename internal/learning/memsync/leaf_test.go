package memsync

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A `[SEATS]` SATELLITE READS THE CHANGELOG ACROSS A LEAF LINK.
//
// A node without the `data` role joins the fleet's broker as a LEAF: its own
// broker runs no JetStream and reaches the members' across one leaf link, in
// the fleet's domain ([jsapi.Domain]), and it holds that link to its own
// max_payload — a larger message arriving on it closes the link (nats-server
// leafnode.go, processLeafMsgArgs), and the satellite's lease renewals cross
// it. The cases below start a member and a leaf joined to it, as the embedded
// fleet does, publish a seat's rows through the member and hydrate them
// through the leaf.

// leafFleet starts a member carrying memberPayload bytes a message, holding
// every stream the queue provisions in the embedded fleet's domain, and a leaf
// joined to it carrying leafPayload, and returns a connection to each once the
// leaf's clients reach the member's JetStream.
func leafFleet(t *testing.T, memberPayload, leafPayload int32) (member, leaf *nats.Conn) {
	t.Helper()
	dir := t.TempDir()
	hub := startServer(t, &server.Options{
		ServerName: "memsync-member", JetStream: true, JetStreamDomain: jsapi.Domain,
		Port: -1, DontListen: true, StoreDir: dir, MaxPayload: memberPayload,
		JetStreamMaxStore: brokertest.StoreMaxBytes,
		LeafNode:          server.LeafNodeOpts{Host: "127.0.0.1", Port: -1},
	})
	member = connectTo(t, hub)
	client := jsOn(t, member)
	for _, stream := range js.EngineStreams(js.Config{StoreDir: dir}, time.Now()) {
		if _, err := client.CreateStream(t.Context(), stream); err != nil {
			t.Fatalf("provision %s as the queue provisions it: %v", stream.Name, err)
		}
	}
	// THE PORT THE MEMBER'S LEAF LISTENER BOUND, as nats-server reports it:
	// it writes the port it chose back into its options before it is ready,
	// and offers no listener address for leaves.
	varz, err := hub.Varz(nil)
	if err != nil {
		t.Fatalf("read the member's leaf listener: %v", err)
	}
	link, err := url.Parse(fmt.Sprintf("nats-leaf://127.0.0.1:%d", varz.LeafNode.Port))
	if err != nil {
		t.Fatalf("the member's leaf listener: %v", err)
	}
	edge := startServer(t, &server.Options{
		ServerName: "memsync-leaf", Port: -1, DontListen: true, MaxPayload: leafPayload,
		LeafNode: server.LeafNodeOpts{Remotes: []*server.RemoteLeafOpts{{URLs: []*url.URL{link}}}},
	})
	leaf = connectTo(t, edge)
	reachable(t, embeddedOn(t, leaf))
	return member, leaf
}

// startServer starts an in-process broker with opts, stopped when the case
// ends.
func startServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("configure %s: %v", opts.ServerName, err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatalf("%s did not become ready", opts.ServerName)
	}
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })
	return ns
}

// connectTo is an in-process connection to ns, closed when the case ends.
func connectTo(t *testing.T, ns *server.Server) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// embeddedOn is a JetStream client over conn in the embedded fleet's API, its
// domain: the one a leaf's clients reach the members' JetStream in.
func embeddedOn(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	client, err := jsapi.Embedded().Client(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return client
}

// reachable waits until client reaches the changelog: across a leaf, the link
// is made and the member's interest in its domain's API carried over it a
// moment after both brokers say they are ready, and until then a request
// through the leaf has no responder.
func reachable(t *testing.T, client jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		attempt, stop := context.WithTimeout(ctx, time.Second)
		_, err := client.Stream(attempt, topics.MemoryStream)
		stop()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the leaf's clients never reached the member's changelog: %v", err)
		case <-time.After(10 * time.Millisecond): //fixedwait:liveness
		}
	}
}

// noteOfSize is a diary note of the seat that is exactly size bytes on the
// wire, its content filling what the rest of the row leaves.
func noteOfSize(t *testing.T, id string, size int) Row {
	t.Helper()
	note := Row{Table: "agent_diary", Values: map[string]any{
		"id": id, "agent_id": seat.AgentID, "kind": "diary_long", "content": "",
		"source": "reflect", "created_at": 1,
	}}
	body, err := encode(note)
	if err != nil {
		t.Fatal(err)
	}
	note.Values["content"] = strings.Repeat("x", size-len(body))
	if body, err = encode(note); err != nil || len(body) != size {
		t.Fatalf("the note encodes to %d bytes (%v), want exactly %d", len(body), err, size)
	}
	return note
}

// A ROW AT THE TRANSPORT'S LIMIT HYDRATES ACROSS A LEAF LINK.
//
// A row crosses a satellite's link as its replay delivers it, whole and no
// larger, so a row at the link's limit fits. The leader's answer to a read of
// the row is a third larger, base64-encoded in JSON: a hydration that asked
// the leader for each table's newest row before its replay never received
// that answer for a row past three quarters of the limit, because the leaf
// closed its link to the member on it, and the read asked again closed it
// again, until the budget ran out and the seat was refused — at every sweep,
// for as long as the row was its table's newest, the satellite's leases riding
// a link that kept closing. The leader is asked once the replay has delivered
// what its copy holds, and answers that it holds nothing past it. The row here
// is a note whose message is exactly the limit, on a fleet carrying a small
// one and on one carrying the embedded fleet's 8 MiB.
//
// Mutation: ask the leader for each table's newest row before its replay, and
// this goes red.
func TestARowAtTheTransportsLimitHydratesAcrossALeafLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		limit int32
	}{
		{"on a fleet carrying 64 KiB", 64 << 10},
		{"on the embedded fleet's 8 MiB", queue.MaxPayloadBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			member, leaf := leafFleet(t, tc.limit, tc.limit)
			note := noteOfSize(t, "at-the-limit", int(tc.limit))
			publishOn(t, member, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)

			fresh := openStore(t)
			satellite, err := New(fresh, embeddedOn(t, leaf), func(string) string { return seat.AgentID }, true)
			if err != nil || satellite == nil {
				t.Fatalf("New: %v", err)
			}
			carried, err := satellite.Hydrate(t.Context(), seat.Handle)
			if err != nil {
				t.Fatalf("a row at the transport's limit refused the seat across a leaf link: %v", err)
			}
			if carried != 1 {
				t.Fatalf("the hydration carried %d rows, want the 1 note", carried)
			}
			var length int
			if err := fresh.SQL().QueryRowContext(t.Context(),
				"SELECT length(content) FROM agent_diary WHERE id = 'at-the-limit'").Scan(&length); err != nil {
				t.Fatalf("the note did not arrive: %v", err)
			}
			if want := len(note.Values["content"].(string)); length != want {
				t.Errorf("the note arrived with %d bytes of content, want its %d", length, want)
			}
		})
	}
}

// A ROW LARGER THAN THIS NODE'S CONNECTION CARRIES IS REFUSED AS THAT.
//
// A pull is bounded at one message at this node's connection's max_payload,
// and the broker sends a pull nothing past what is left of its bytes: it ends
// the pull with none (`409 Message Size Exceeds MaxBytes`, which the client
// reads as the pull's ordinary end). So a row published through a server, an
// account or a user carrying more than this node's connection is never sent to
// it, at any pull of any sweep, and the seat is refused there. That refusal
// read as a copy behind its leader whose budget ran out, said a second into a
// fifteen-second budget and naming no limit, so nothing pointed at the setting
// to align. It is refused by name, naming the limit. The fleet here is a
// member carrying 128 KiB a message and a leaf carrying 64 KiB, and the row is
// a note of 96 KiB, published through the member; the link stays up, since
// the broker never sends the row across it.
//
// Mutation: read a pull that ended with no row while its copy held more as a
// copy that stalled, and this goes red.
func TestARowLargerThanThisNodesConnectionCarriesIsRefusedAsThat(t *testing.T) {
	t.Parallel()
	member, leaf := leafFleet(t, 128<<10, 64<<10)
	note := noteOfSize(t, "past-the-leaf", 96<<10)
	publishOn(t, member, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)

	satellite, err := New(openStore(t), embeddedOn(t, leaf), func(string) string { return seat.AgentID }, true)
	if err != nil || satellite == nil {
		t.Fatalf("New: %v", err)
	}
	carried, err := satellite.Hydrate(t.Context(), seat.Handle)
	if !errors.Is(err, errRowPastPull) {
		t.Fatalf("a row larger than the satellite's connection carries answered (%d, %v), want "+
			"the seat refused for a row past its pull's bound", carried, err)
	}
	if !strings.Contains(err.Error(), "max_payload of 65536 bytes") {
		t.Errorf("the refusal does not name the satellite's limit: %v", err)
	}
}
