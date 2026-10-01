package jetstream

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
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

// A READINESS FAILURE NAMES THE ROUTE LISTENER OR THE PEERS, NEVER BOTH AND
// NEVER THE WRONG ONE.
//
// # Why this branch is worth a case of its own
//
// Because it was wrong in the direction that is invisible. The read of "did
// the route listener bind" used to happen AFTER the server was shut down, and
// shutting a server down closes that listener and clears the address — so the
// answer was nil on every clustered member, and a member whose peers were
// simply unreachable was told its port had been taken. That sends an operator
// to hunt for a process holding a port nobody is holding, while the peer they
// needed to look at goes unmentioned.
//
// Nothing caught it: the pre-bind probe covers the ordinary taken-port case
// and returns long before here, so this branch had no test at all and the
// message it produces is the only thing that distinguishes the two failures.
func TestAReadinessFailureBlamesTheRouteListenerOnlyWhenItNeverBound(t *testing.T) {
	t.Parallel()

	const (
		budget = 90 * time.Second
		port   = 6222
		host   = "127.0.0.1"
	)

	// THE ROUTE LISTENER NEVER BOUND: the port is the finding, because it
	// is the one fact an operator cannot derive from anywhere else.
	lost := notReadyError(budget, true, port, host, false)
	if !strings.Contains(lost.Error(), "no route listener") {
		t.Errorf("a member that never bound its route listener is not told so: %v", lost)
	}
	if !strings.Contains(lost.Error(), strconv.Itoa(port)) {
		t.Errorf("the failure does not name the port that was taken: %v", lost)
	}

	// THE LISTENER BOUND: the routes and the metadata group are what is
	// left, and naming the port here is the wrong lead.
	peers := notReadyError(budget, true, port, host, true)
	// THE FIELD IS stream.cluster.peers, which is the one an operator can
	// grep their Tier A for. It read `stream.cluster.routes` — a key this
	// config has never had, so the remedy sent them looking for something
	// that does not exist.
	if !strings.Contains(peers.Error(), "stream.cluster.peers") {
		t.Errorf("a clustered member waiting on peers is not pointed at them: %v", peers)
	}
	if strings.Contains(peers.Error(), "no route listener") {
		t.Errorf("a member whose route listener DID bind is blamed for it: %v", peers)
	}

	// A SOLO MEMBER HAS NO ROUTE LISTENER TO BLAME, whatever it answers
	// about one: it configures no route port, so the question does not
	// arise and the port branch must not fire on it.
	solo := notReadyError(budget, false, 0, "", false)
	if strings.Contains(solo.Error(), "no route listener") {
		t.Errorf("a solo member is blamed for a route listener it never wanted: %v", solo)
	}
	if !strings.Contains(solo.Error(), "clustered: false") {
		t.Errorf("the failure does not say this member had no peers to wait for: %v", solo)
	}
}

// A CONNECTION TO THIS PROCESS'S OWN BROKER HANDSHAKES WITHIN THE ACCEPT
// BUDGET, never nats's two-second default for a remote dial.
//
// The server a connection is opened to has already answered
// ReadyForConnections, so a slow handshake measures only host load — and on a
// loaded host the default failed the queue's own connection with `read pipe:
// i/o timeout` against a broker that was up. The budget has to be the one the
// server was given to start, solo and clustered alike, or the two drift.
//
// HELD ON THE CONNECTION [embeddedServer.connect] OPENS, over both of its
// dials — the in-memory pipe and the loopback port — rather than on an option
// list read beside them: a list a test reads certifies nothing about a dial
// that stopped passing it. The clustered budget is asked of the same two
// servers, since the member's flag, not its listener, is what picks it.
//
// Mutation: drop the budget from either dial in connect and its rows go red.
func TestAConnectionToItsOwnBrokerHandshakesWithinTheAcceptBudget(t *testing.T) {
	t.Parallel()
	inProcess, err := startEmbedded(t.Context(), Config{})
	if err != nil {
		t.Fatalf("start an in-process broker: %v", err)
	}
	t.Cleanup(inProcess.shutdown)
	if !inProcess.inProcess {
		t.Fatal("a solo broker listens on a socket, so the in-memory dial goes unexercised")
	}
	loopback := listeningBroker(t)

	for _, c := range []struct {
		name   string
		server *embeddedServer
	}{
		{"through the in-memory pipe", inProcess},
		{"over the loopback port", loopback},
	} {
		for _, clustered := range []bool{false, true} {
			e := *c.server
			e.clustered = clustered
			nc, err := e.connect(nil)
			if err != nil {
				t.Fatalf("%s: connect: %v", c.name, err)
			}
			got := nc.Opts.Timeout
			nc.Close()
			if want := acceptBudget(clustered); got != want {
				t.Errorf("%s, clustered=%v: the handshake budget is %v, want "+
					"the accept budget %v — nats's own default is %v, a figure "+
					"for a remote server rather than one in this process",
					c.name, clustered, got, want, nats.DefaultTimeout)
			}
		}
	}
}

// listeningBroker is a solo broker that DOES listen, on a loopback client
// port — the dial a clustered member's connections take — so
// [embeddedServer.connect]'s socket branch is exercised without forming a
// cluster in this package.
func listeningBroker(t *testing.T) *embeddedServer {
	t.Helper()
	opts, scratch, err := embeddedOptions(Config{})
	if err != nil {
		t.Fatalf("broker options: %v", err)
	}
	opts.DontListen, opts.Host = false, "127.0.0.1"
	ns, err := server.NewServer(opts)
	if err != nil {
		removeScratch(scratch)
		t.Fatalf("configure a listening broker: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() { shutdownAndClean(ns, scratch) })
	if !ns.ReadyForConnections(acceptTimeout) {
		t.Fatalf("a listening broker was not ready within %v", acceptTimeout)
	}
	return &embeddedServer{ns: ns, scratch: scratch}
}

// AN EXTERNAL SERVER'S HANDSHAKE IS NOT GIVEN THE EMBEDDED MEMBER'S BUDGET.
//
// A connection to this process's own member takes the accept budget because
// that server has already answered; a remote one has not, and the reconnect
// loop waits this long on every silent member of its pool before asking the
// next, which is time a node spends not renewing its seats' leases. The two
// option lists sit a screen apart, so this holds the one a shared list would
// break. Mutation: dial with acceptBudget(true) and it goes red.
func TestAnExternalDialKeepsItsOwnHandshakeBudget(t *testing.T) {
	t.Parallel()
	opts, err := dialOptions(Config{})
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
			"embedded member's accept budget (%v solo, %v clustered) would hold "+
			"a reconnect on a partitioned member for that long",
			applied.Timeout, externalHandshake, acceptBudget(false), acceptBudget(true))
	}
}
