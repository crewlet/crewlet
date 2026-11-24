package jetstream

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
)

// A PROBE THAT COULD NOT ANSWER IS NOT AN ANSWER: a cancelled start, an
// address this host does not have and a privileged port each come back as an
// error, never as a port somebody holds; three remedies, two of which would
// send the reader hunting for a process that does not exist.
func TestAPortProbeSaysWhetherItCouldAnswerAtAll(t *testing.T) {
	t.Parallel()

	// A FREE PORT answers free, with no error.
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	port := l.Addr().(*net.TCPAddr).Port

	// HELD: free is false and err nil, since the probe ran and the port is
	// taken.
	switch free, err := PortAvailable(t.Context(), "127.0.0.1", port); {
	case err != nil:
		t.Errorf("a held port reported a probe failure: %v", err)
	case free:
		t.Error("a held port reported itself free")
	}

	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	switch free, err := PortAvailable(t.Context(), "127.0.0.1", port); {
	case err != nil:
		t.Errorf("a released port reported a probe failure: %v", err)
	case !free:
		t.Error("a released port reported itself held")
	}

	// AN ADDRESS THIS HOST DOES NOT HAVE is a configuration error of a
	// different kind, and it comes back AS one rather than as "in use".
	// 192.0.2.0/24 is TEST-NET-1 (RFC 5737) and is never a local address.
	free, err := PortAvailable(t.Context(), "192.0.2.1", port)
	if free {
		t.Error("an address this host does not have reported itself bindable")
	}
	if err == nil {
		t.Error("an unbindable address was reported as a port in use, which " +
			"sends the reader looking for a process that does not exist")
	}

	// A CANCELLED CALLER gets the cancellation back, not a verdict about
	// the port: nothing was learned about it.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if free, err := PortAvailable(ctx, "127.0.0.1", port); free || err == nil {
		t.Errorf("a cancelled probe answered (free=%v, err=%v) rather than "+
			"reporting that it never ran", free, err)
	}
}

// A CONNECTION TO THIS PROCESS'S OWN BROKER HANDSHAKES WITHIN THE LISTENER
// BUDGET ([listenTimeout]), never nats's two-second default for a remote dial:
// the server already answered ReadyForConnections, so a slow handshake is only
// host load, and failing it fails the queue against a broker that is up. Held
// on the pipe [embeddedServer.connect] dials, not an option list a dial could
// stop passing, clustered and solo alike. Its reconnects span the same budget
// ([embeddedReconnects]), to THIS server alone (IgnoreDiscoveredServers), or a
// node whose broker stopped lands on a peer and never stops itself;
// jetstreamtest's TestAMembersOwnClientDiscoversNoPeer holds that on a cluster.
//
// Mutation: drop the budget from connect, the reconnect options or
// IgnoreDiscoveredServers, and every row goes red.
func TestAConnectionToItsOwnBrokerHandshakesWithinTheListenerBudget(t *testing.T) {
	t.Parallel()
	broker, err := startEmbedded(t.Context(), Config{})
	if err != nil {
		t.Fatalf("start an in-process broker: %v", err)
	}
	t.Cleanup(broker.shutdown)

	for _, clustered := range []bool{false, true} {
		e := *broker
		e.clustered = clustered
		nc, err := e.connect(watched{}, e.auth.engine)
		if err != nil {
			t.Fatalf("clustered=%v: connect: %v", clustered, err)
		}
		got := nc.Opts.Timeout
		reconnects, wait := nc.Opts.MaxReconnect, nc.Opts.ReconnectWait
		ownOnly := nc.Opts.IgnoreDiscoveredServers
		inProcess := nc.Opts.InProcessServer != nil
		nc.Close()
		if !inProcess {
			t.Errorf("clustered=%v: the connection dialled a socket, where every "+
				"connection to this process's own broker is a pipe", clustered)
		}
		if !ownOnly {
			t.Errorf("clustered=%v: the connection takes the peers a member could "+
				"advertise into its reconnect pool, so a reconnect could land on a "+
				"peer and a node whose own member stopped would run on without it",
				clustered)
		}
		if want := listenTimeout; got != want {
			t.Errorf("clustered=%v: the handshake budget is %v, want the listener "+
				"budget %v — nats's own default is %v, a figure for a remote server "+
				"rather than one in this process", clustered, got, want,
				nats.DefaultTimeout)
		}
		// THE RECONNECTS SPAN THE SAME BUDGET: nats's own sixty attempts two
		// seconds apart leave a node whose broker stopped two minutes dark.
		if span := time.Duration(reconnects) * wait; reconnects <= 0 ||
			span != listenTimeout {
			t.Errorf("clustered=%v: %d reconnects %v apart span %v, want the "+
				"listener budget %v", clustered, reconnects, wait, span,
				listenTimeout)
		}
	}
}

// startJoined starts ns the way [startEmbedded] does — on a goroutine whose
// end the returned channel reports — so a cleanup through [shutdownAndClean]
// waits for the Start it may overlap rather than racing it.
func startJoined(ns *server.Server) <-chan struct{} {
	started := make(chan struct{})
	go func() {
		defer close(started)
		ns.Start()
	}()
	return started
}

// AN EXTERNAL SERVER'S HANDSHAKE IS NOT GIVEN THE EMBEDDED MEMBER'S BUDGET: a
// remote server has not already answered, and the reconnect loop waits that
// long on every silent pool member, time a node spends not renewing its seats'
// leases. Mutation: dial with listenTimeout and it goes red.
func TestAnExternalDialKeepsItsOwnHandshakeBudget(t *testing.T) {
	t.Parallel()
	opts, _, err := dialOptions(Config{}, nil)
	if err != nil {
		t.Fatalf("dialOptions: %v", err)
	}
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			t.Fatalf("applying an option: %v", err)
		}
	}
	if applied.Timeout != externalHandshake {
		t.Errorf("an external dial's handshake budget is %v, want %v — the "+
			"embedded member's listener budget (%v) would hold a reconnect on a "+
			"partitioned member for that long",
			applied.Timeout, externalHandshake, listenTimeout)
	}
}

// AN EXTERNAL DIAL GIVES EVERY SOCKET WRITE THE CROSSING OF THE LARGEST ONE THE
// CLIENT MAKES before it cuts the connection under it.
//
// The client writes a large message whole, in one Write under one deadline,
// and a reconnect flushes its buffer the same way; the client's minute would
// break the connection on a link at the floor. The largest Write is a message
// at the contract's ceiling behind either buffer short of its limit, at the
// object store's floor, computed off the options the dial applies so a larger
// buffer raises the bound.
//
// Mutation: dial without FlusherTimeout, and the client's minute holds every
// Write.
func TestAnExternalDialGivesEveryWriteTheLargestWritesCrossing(t *testing.T) {
	t.Parallel()
	opts, _, err := dialOptions(Config{}, nil)
	if err != nil {
		t.Fatalf("dialOptions: %v", err)
	}
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			t.Fatalf("applying an option: %v", err)
		}
	}
	buffered := applied.WriteBufferSize
	if buffered <= 0 {
		buffered = nats.DefaultWriteBufSize
	}
	largest := max(buffered, applied.ReconnectBufSize) + queue.MaxPayloadBytes
	if crossing := jsapi.Sending(largest, objstore.MiBPace); applied.FlusherTimeout < crossing {
		t.Errorf("an external dial's client cuts a socket Write after %v, and the largest it "+
			"makes, %d bytes, takes %v to cross a link at the floor: a write the link was "+
			"carrying at a pace the engine allows would break the connection",
			applied.FlusherTimeout, largest, crossing)
	}
}

// droppingInfoJS drops the account report it is asked for — returning only
// when the request's own term ends, as a metadata group with no leader never
// replies — until it has dropped `drop` of them, and then answers at once.
//
// THE ANSWER IS ITS OWN, never the real server's: how often the wait asks is
// the assertion, and a real round trip under a loaded race build can outrun the
// cases' 20 ms term and make the wait ask again.
type droppingInfoJS struct {
	jetstream.JetStream
	drop  int32
	asked atomic.Int32
}

func (f *droppingInfoJS) AccountInfo(ctx context.Context) (*jetstream.AccountInfo, error) {
	if f.asked.Add(1) <= f.drop {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &jetstream.AccountInfo{}, nil
}

// A CLUSTERED MEMBER IS READY ONLY ONCE THE METADATA LEADER ANSWERS IT, not
// once it believes itself current.
//
// "Current" is a local flag a member that knows of no leader can hold, and a
// lookup sent to a leaderless group waits out a whole ask term. So the wait
// ends on an ANSWER, a dropped question is asked again rather than waited on,
// and when none comes the refusal names the leader, a different remedy from a
// member that never caught up or was never routed.
//
// A member in a cluster of its own, so both local halves hold at once and the
// answer alone decides the wait.
func TestAClusteredMemberIsReadyOnlyOnceTheLeaderAnswers(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{ClusterName: "alone", ServerName: "a"})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(e.shutdown)
	nc, err := e.connect(watched{}, e.auth.engine)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	real, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	t.Run("a dropped question is asked again until it is answered", func(t *testing.T) {
		js := &droppingInfoJS{JetStream: real, drop: 3}
		e := *e
		e.ready = readiness{timeout: 10 * time.Second, poll: 5 * time.Millisecond,
			ask: 20 * time.Millisecond}
		if err := e.awaitClusterReady(t.Context(), js, 1); err != nil {
			t.Fatalf("a member whose leader answered on the fourth ask was not "+
				"ready: %v", err)
		}
		if got := js.asked.Load(); got != 4 {
			t.Errorf("the leader was asked %d time(s), want 4 — three dropped and "+
				"the one that was answered; none at all is a member called ready "+
				"on its local flag alone", got)
		}
	})

	t.Run("a leader that never answers is named", func(t *testing.T) {
		js := &droppingInfoJS{JetStream: real, drop: 1 << 30}
		e := *e
		e.ready = readiness{timeout: 300 * time.Millisecond, poll: 5 * time.Millisecond,
			ask: 20 * time.Millisecond}
		err := e.awaitClusterReady(t.Context(), js, 1)
		if err == nil {
			t.Fatal("a member no leader ever answered was called ready")
		}
		if !strings.Contains(err.Error(), "metadata leader answered: "+
			context.DeadlineExceeded.Error()) {
			t.Errorf("the refusal does not say the leader never answered:\n%v", err)
		}
		if strings.Contains(err.Error(), errNotAsked.Error()) {
			t.Errorf("the refusal says the leader was never asked, of a member "+
				"current and routed throughout:\n%v", err)
		}
	})
}

// A CLUSTERED MEMBER WHOSE JETSTREAM IS OFF CAN'T REPORT THE GROUP, and never
// says it has none: nats-server shuts JetStream down on a full or unwritable
// store without ending the process, and read as a solo member's answer that
// made every peer's broker look stopped to a capacity window's judgement. The
// member has a route port, so it runs a metadata group, and reports it first.
//
// Mutation: answer ErrNoMetaGroup for a disabled member, or skip the
// DisableJetStream call, and this fails.
func TestAClusteredMemberWithJetStreamOffCantReportTheGroup(t *testing.T) {
	t.Parallel()
	srv := loneMember(t)
	if _, err := srv.MetaGroup(); err != nil {
		t.Fatalf("premise: a member with a route port reports no group: %v", err)
	}
	if err := srv.embedded.ns.DisableJetStream(); err != nil {
		t.Fatalf("turn JetStream off: %v", err)
	}
	_, err := srv.MetaGroup()
	if !errors.Is(err, ErrMetaGroupDown) || errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a clustered member with JetStream off answered %v, want ErrMetaGroupDown alone", err)
	}
}

// A MEMBER WHOSE CLUSTER IS ONLY NAMED HAS NO METADATA GROUP: with no route
// port nats-server runs it standalone, so with JetStream on it answers as a
// solo member does, never as a group that is down, which would refuse every
// capacity window's judgement on it for good.
//
// Mutation: decide the group from the cluster's name, and this fails.
func TestAMemberWhoseClusterIsOnlyNamedHasNoMetadataGroup(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{ClusterName: "alone", ServerName: "a"})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(e.shutdown)
	srv := &Server{embedded: e}
	if _, err := srv.MetaGroup(); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a member whose cluster is only named answered %v, want ErrNoMetaGroup", err)
	}
	if err := srv.RemovePeer(t.Context(), PeerIDOf("b")); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("its removal answered %v, want ErrNoMetaGroup", err)
	}
}
