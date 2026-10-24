package jetstream

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
)

// connectionLoss records the first connection a queue depends on that NATS
// closed FOR GOOD, which is what [Queue.Lost] and [Queue.LostCause] report.
//
// # Why a node has to be told
//
// This package dials an operator's server to reconnect for ever, so a broker
// that goes away and comes back costs a node nothing it does not get back. A
// close for good is the other outcome, and the client reaches it on its own:
//
//   - a message past a max_payload the server lowered by a LIVE RELOAD, which
//     the server answers with `Maximum Payload Violation` and the client treats
//     as final ([carriesTheContract] says why nothing on this side can see
//     such a limit before it is crossed);
//   - an authorization error the server repeats on the reconnect after it —
//     credentials revoked or expired, or the server's own authorization
//     reloaded under a running node — since two identical ones in a row are
//     where the client stops asking;
//   - any other -ERR the client does not recognise as transient;
//   - and the reconnect attempts running out, which is how a connection to
//     this node's own embedded broker ends once that broker has stopped.
//
// Nothing was told. The process stayed up with no connection to publish,
// consume or renew a lease over — healthy to a liveness probe, its seats lapsing
// to peers while it still believed it held them — until a person noticed and
// restarted it. The honest move for a node whose broker connection is gone is
// the seat watchdog's for a wedged event loop: stop, and let whatever
// supervises the process start it again. That restart is also where a server
// still below the contract is refused BY NAME ([carriesTheContract]), so the
// one recovery path ends at the sentence naming the setting.
//
// # Why only a close this package did not ask for
//
// Every connection this package hands out is closed by its owner at shutdown,
// and the client calls a closed handler for that close too unless it is told
// not to — so each watched connection is dialled with
// [nats.NoCallbacksAfterClientClose], which is the client's own line between a
// close user code made (a [Queue.Stop], an owner's Close, a Drain, which ends
// in one) and a close the client made because the server or the reconnect
// budget left it no other move. Recording a node's own shutdown as a lost
// broker would turn every graceful stop into a failed exit.
//
// # Why once
//
// The first close is the cause. A node that lost its connection to an operator's
// server for one of the reasons above tends to lose the second one for the same
// reason a moment later — the coordination store's on an embedded broker, or
// the next connection a peer-facing subsystem dials — and every such close is
// LOGGED, but the node is stopping already, and a cause that changed between
// the reader looking and the reader reporting would name the wrong one.
//
// # Why the queue's own connection is told apart
//
// The node stops for any of them; what differs is what the turns it is still
// running can achieve on the way down. Every delivery this
// queue made is acknowledged, nacked or deferred over the queue's OWN
// connection, so once that one is gone nothing a running turn concludes can be
// recorded: the broker hands the message to whichever node takes the seat once
// its ack window elapses, and that node runs it again — every chat post, every
// comment, every tracker write a second time. Its loss therefore ends the drain
// at once ([Queue.AcksLost]), while the coordination store's connection closing
// alone leaves every ack still landing, and the drain waits for the turns as a
// signal's does. A second [sync.Once] rather than a flag on the first close,
// because the queue's own connection is routinely the SECOND to go — the
// coordination store's can close first on an embedded broker that stopped —
// and the first close names the cause while this one decides the drain.
type connectionLoss struct {
	once  sync.Once
	done  chan struct{}
	cause error

	acksOnce sync.Once
	acks     chan struct{}

	// conns is every connection whose close for good is recorded here, so a
	// start that failed can wait for the handler of one the client has
	// already closed ([connectionLoss.settle]). Appended once a dial hands
	// the connection out, and never pruned: a queue holds two.
	mu    sync.Mutex
	conns []*nats.Conn
}

func newConnectionLoss() *connectionLoss {
	return &connectionLoss{done: make(chan struct{}), acks: make(chan struct{})}
}

// record keeps the first cause and closes done, and closes acks as well when
// the connection that closed is the one deliveries settle over. The cause is
// written before done is closed, so a reader that has seen done closed reads
// the cause it was closed for.
func (l *connectionLoss) record(cause error, settles bool) {
	l.once.Do(func() {
		l.cause = cause
		close(l.done)
	})
	if settles {
		l.acksOnce.Do(func() { close(l.acks) })
	}
}

// lostCause is the recorded cause, or nil while nothing has been lost.
//
// Read only once done is closed, which is what makes it safe without a lock:
// [connectionLoss.record] writes the cause before it closes the channel.
func (l *connectionLoss) lostCause() error {
	select {
	case <-l.done:
		return l.cause
	default:
		return nil
	}
}

// watched is what a dial is told about the connection it opens: where NATS
// closing it for good is recorded, and whether it is the queue's OWN.
//
// The zero value is an UNWATCHED connection, whose owner is the one to notice
// its loss — a snapshot donor's ([Queue.DialOwned]).
type watched struct {
	loss *connectionLoss
	// settles marks the queue's own connection: the one every delivery the
	// queue made is acknowledged, nacked and deferred over, whose loss
	// [Queue.AcksLost] reports. Exactly one connection per queue carries
	// it; a second one [Queue.DialWatched] opens does not, because a node
	// whose coordination connection is gone still settles what it runs.
	settles bool
}

// track notes nc as a connection whose close for good is recorded where at
// says, once a dial is about to hand it out — after the contract check, which
// closes a connection it refuses itself and so leaves no handler to wait for.
// Nothing for an unwatched one.
func (at watched) track(nc *nats.Conn) {
	if at.loss == nil {
		return
	}
	at.loss.mu.Lock()
	at.loss.conns = append(at.loss.conns, nc)
	at.loss.mu.Unlock()
}

// during is err as a start that failed beside a loss says it: the classified
// sentence first, because it names what to change, and the failing step's own
// error after it, because it says where the start was — both in the chain, for
// a caller that asks either. err itself, unchanged, where nothing was lost: a
// failure that had nothing to do with the broker is not dressed as one.
//
// ONE COMPOSITION for every start that can meet a loss — this queue's own open
// ([newQueueOn]) and an engine's boot over it ([Queue.LostDuring]) — so the two
// cannot word the same fact two ways.
//
// It SETTLES first ([connectionLoss.settle]), so a connection the client has
// already closed is named whether or not its handler has run yet — which is
// why it is asked BEFORE the start's own cleanup closes anything.
func (l *connectionLoss) during(err error) error {
	if err == nil {
		return nil
	}
	l.settle()
	return l.recorded(err)
}

// recorded is [connectionLoss.during] without the settle: the composition over
// whatever is recorded now. For a queue whose own Stop has closed its
// connection, where no handler is coming and a settle would wait for one.
func (l *connectionLoss) recorded(err error) error {
	if err == nil {
		return nil
	}
	cause := l.lostCause()
	if cause == nil {
		return err
	}
	return fmt.Errorf("%w; the start failed on it at: %w", cause, err)
}

// settle returns once the loss is recorded where a watched connection is
// already closed, and at once where none is.
//
// THE HANDLER IS QUEUED BEFORE IsClosed CAN ANSWER TRUE. The client closes a
// connection for good under its own lock and queues the closed handler before
// it lets go, and IsClosed reads under that lock — so a watched connection that
// reads as closed to a caller who has closed none of them is one whose handler
// WILL run, on the client's own goroutine, and the wait for it ends. Without
// this the cause was read whenever it happened to be there: the step that met
// the close returns once the client releases it, the handler runs a moment
// later, and a caller whose own cleanup was short — a queue's open closing its
// one connection — composed the step's bare "connection closed" while the
// sentence naming why was a moment from being recorded.
//
// HENCE BEFORE THE CALLER'S CLEANUP, never after it. A connection its owner
// closed reads as closed too, and under [nats.NoCallbacksAfterClientClose] its
// handler never runs, so asked after the cleanup this would wait on a loss
// nobody will record.
func (l *connectionLoss) settle() {
	l.mu.Lock()
	conns := slices.Clone(l.conns)
	l.mu.Unlock()
	for _, nc := range conns {
		if nc.IsClosed() {
			<-l.done
			return
		}
	}
}

// lossWatch is the closed handler a watched connection is given: what a
// sentence about that connection names, and where its close is recorded.
//
// A METHOD rather than a closure, for [reconnectWatch]'s reason: a case can
// then tell the handler a connection was given is this one.
type lossWatch struct {
	loss     *connectionLoss
	settles  bool
	log      *slog.Logger
	embedded bool
	// server names the server for a sentence. Fixed at the dial, because
	// the handler runs once the connection has closed and a closed
	// connection no longer says which member it was talking to — and an
	// operator's remedy is the same on every member stream.url reaches.
	server string
}

// watchClose is the option pair that makes a connection's close for good a
// node's loss — or nothing, where the connection is unwatched and its owner is
// the one to notice (a snapshot donor's: see [Queue.DialOwned]).
//
// The pair, never the handler alone: without NoCallbacksAfterClientClose the
// client calls the handler for its owner's own Close as well, and every
// graceful stop would read as a lost broker.
func watchClose(at watched, embedded bool, url string) []nats.Option {
	if at.loss == nil {
		return nil
	}
	w := lossWatch{loss: at.loss, settles: at.settles,
		log: logging.Get("queue.jetstream"), embedded: embedded,
		server: lostServer(embedded, url)}
	return []nats.Option{nats.NoCallbacksAfterClientClose(), nats.ClosedHandler(w.closed)}
}

// lostServer is how a sentence about a lost connection names its server: this
// node's own for the embedded broker, and for an operator's the addresses
// stream.url names — never the URL itself, which may carry a password or a
// token as its userinfo (see [serverAddresses]).
func lostServer(embedded bool, url string) string {
	if embedded {
		return "this node's embedded NATS server"
	}
	return "the NATS server stream.url reaches (" + serverAddresses(url) + ")"
}

// closed records the close and says so.
//
// LOGGED ON EVERY WATCHED CONNECTION and recorded only for the first, for the
// reason [connectionLoss] gives: each close is a fact an operator may want, the
// cause a node stops for is one.
//
// The line says WHETHER IT WAS THE QUEUE'S OWN, because the two outcomes differ
// for the turns still running: with the queue's own gone none of them can be
// acknowledged any more, and with only a second one gone every ack still lands.
func (w lossWatch) closed(nc *nats.Conn) {
	cause := lostConnection(w.server, nc.LastError(), w.embedded)
	w.log.Error("jetstream_connection_closed_for_good",
		"server", w.server, "carries_acks", w.settles, "error", cause.Error())
	w.loss.record(cause, w.settles)
}

// lostConnection is the sentence a node stops with: what closed, why, and what
// to change.
//
// FOUR SENTENCES, because the causes have four remedies and one sentence for
// all of them would send an operator after the wrong one: a max_payload lowered
// under the node is fixed in the server's configuration, an authorization
// refusal in the credentials this node presents or the user they name, an error
// the client does not recognise in whatever the server's own log says it was —
// and the embedded broker has no setting at all, so its sentence sends nobody
// to a file.
func lostConnection(server string, cause error, embedded bool) error {
	said := "no reason given"
	if cause != nil {
		said = cause.Error()
	}
	if embedded {
		return fmt.Errorf("jetstream: %s closed this node's connection to it for "+
			"good (%s), so this node has no connection to publish, consume or "+
			"renew a lease over: the engine starts and configures that server "+
			"itself and no setting reaches it, so a restart, which starts it "+
			"again, is the remedy — the server's own lines (queue.nats.server) "+
			"say what happened to it", server, said)
	}
	switch {
	case cause != nil && strings.Contains(strings.ToLower(said), "maximum payload"):
		return fmt.Errorf("jetstream: %s closed this node's connection for good "+
			"(%s): a message this node sent was larger than the max_payload the "+
			"server now holds the connection to, a limit lowered under the running "+
			"node — a live reload, which nats-server applies to the connections it "+
			"holds without telling them — below the %d bytes this build sends at "+
			"most. Set max_payload: %s in the configuration of every server "+
			"stream.url reaches, and wherever the account this node signs in to, "+
			"or its user, states a payload limit of its own, raise that too; a "+
			"node restarted against a server still below it refuses to start, "+
			"naming the setting", server, said, queue.MaxPayloadBytes,
			natsSize(queue.MaxPayloadBytes))
	case isAuthRefusal(cause):
		return fmt.Errorf("jetstream: %s closed this node's connection for good "+
			"(%s): it refused this node's credentials on the connection and again "+
			"on the reconnect after it, which is where the client stops asking. "+
			"What stream.credentials or stream.token presents — or the user and "+
			"account it signs in as — was changed, revoked or expired under the "+
			"running node: give this node credentials that server accepts, and a "+
			"restart signs in with them", server, said)
	default:
		return fmt.Errorf("jetstream: %s closed this node's connection for good "+
			"(%s), an error the NATS client does not retry, so this node has no "+
			"connection to publish, consume or renew a lease over: the server's "+
			"own log says why it sent it, and a restart dials it again", server, said)
	}
}

// isAuthRefusal reports whether cause is one of the authorization refusals
// the client gives up on once a server repeats it.
func isAuthRefusal(cause error) bool {
	for _, auth := range []error{nats.ErrAuthorization, nats.ErrAuthExpired,
		nats.ErrAuthRevoked, nats.ErrAccountAuthExpired} {
		if errors.Is(cause, auth) {
			return true
		}
	}
	return false
}
