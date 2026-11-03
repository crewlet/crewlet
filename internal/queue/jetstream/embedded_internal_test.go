package jetstream

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// A PROBE THAT COULD NOT ANSWER IS NOT AN ANSWER, and telling the two apart is
// what keeps the caller's message honest.
//
// This collapsed every listen failure into "not free", so a cancelled start,
// an address this host does not have and a privileged port all reported
// themselves as a port somebody else was holding — three different remedies
// behind one sentence, and two of them send the reader hunting for a process
// that does not exist.
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

	// HELD: in use, and that IS the answer this exists to give — free is
	// false and err is nil, because the probe ran and the port is taken.
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
// BUDGET, never nats's two-second default for a remote dial.
//
// The server a connection is opened to has already answered
// ReadyForConnections, so a slow handshake measures only host load — and on a
// loaded host the default failed the queue's own connection with `read pipe:
// i/o timeout` against a broker that was up. The budget has to be the one a
// started server is given to take a connection ([listenTimeout]), or the two
// drift.
//
// HELD ON THE CONNECTION [embeddedServer.connect] OPENS — through the
// in-memory pipe, its one dial on every topology — rather than on an option
// list read beside it: a list a test reads certifies nothing about a dial that
// stopped passing it. Asked of a clustered member's flag as well as a solo
// server's, because the two budgets this replaced were picked by that flag,
// and a member given its own number again would be the drift.
//
// AND ITS RECONNECTS SPAN THAT SAME BUDGET, since running them out is how a
// node whose own broker has stopped stops itself ([embeddedReconnects]) — and
// they are made to THIS server alone, ignoring any peer a pool could be told
// of, or a reconnect lands on a peer and never runs out. That half is asserted
// on the option here, and as behaviour against a real cluster in
// jetstreamtest, which is where a cluster may be formed.
//
// Mutation: drop the budget from the dial in connect and every row goes red;
// drop the reconnect options and every row does (nats's own sixty attempts two
// seconds apart span two minutes); drop IgnoreDiscoveredServers and every row
// does. (That a real clustered member's connection is in process too is
// jetstreamtest's TestAMembersOwnClientDiscoversNoPeer: this package forms no
// cluster.)
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
		nc, err := e.connect(watched{})
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
		// AND THE RECONNECTS SPAN THE SAME BUDGET, one wait at a time:
		// running them out is how a node whose own broker stopped stops
		// itself, and nats's own sixty attempts two seconds apart were two
		// minutes dark whatever the topology.
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

// AN EXTERNAL SERVER'S HANDSHAKE IS NOT GIVEN THE EMBEDDED MEMBER'S BUDGET.
//
// A connection to this process's own member takes the listener budget because
// that server has already answered; a remote one has not, and the reconnect
// loop waits this long on every silent member of its pool before asking the
// next, which is time a node spends not renewing its seats' leases. The two
// option lists sit a screen apart, so this holds the one a shared list would
// break. Mutation: dial with listenTimeout and it goes red.
func TestAnExternalDialKeepsItsOwnHandshakeBudget(t *testing.T) {
	t.Parallel()
	opts, _, err := dialOptions(Config{})
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

// droppingInfoJS drops the account report it is asked for — returning only
// when the request's own term ends, as a request a metadata group with no
// leader never replies to does — until it has dropped `drop` of them, and
// then answers at once.
//
// THE ANSWER IS ITS OWN, never the real server's. The wait gives every ask
// the same term, and the cases shorten it to 20 ms so three dropped asks cost
// little; a real round trip under a loaded race build ran past that term, the
// wait rightly asked a fifth time, and the case reported the count it exists
// to pin as a defect (1 run in 900 beside three busy CPUs, and once in 20 runs
// of this package). How often the wait asks is the assertion, so the latency
// of an answer must not decide it.
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
// "Current" is a local flag, and a member that knows of no leader can hold it
// — so provisioning that started on it sent its first lookup to a group that
// dropped it, and the lookup waited out a whole fifteen-second ask term before
// anything asked again. Every fleet boot paid that by timer. The wait now
// ends on an ANSWER, and a dropped question is asked again rather than waited
// on; when none ever comes, the refusal says that it was the leader that never
// answered, which is a different remedy from a member that never caught up or
// was never routed.
//
// A member named into a cluster of its own, so the two local halves hold at
// once and what the wait does is decided by the answer alone.
func TestAClusteredMemberIsReadyOnlyOnceTheLeaderAnswers(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{ClusterName: "alone", ServerName: "a"})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(e.shutdown)
	nc, err := e.connect(watched{})
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
