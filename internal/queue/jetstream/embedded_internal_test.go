package jetstream

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// droppingInfoJS drops the account report it is asked for — returning only
// when the request's own term ends, as a request a metadata group with no
// leader never replies to does — until it has dropped `drop` of them, and
// then answers it.
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
	return f.JetStream.AccountInfo(ctx)
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
	nc, err := e.connect()
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
