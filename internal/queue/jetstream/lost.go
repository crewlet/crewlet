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
// closed FOR GOOD, which [Queue.Lost] and [Queue.LostCause] report. A client
// that reconnects for ever still reaches that on its own: a write past a
// max_payload lowered by a live reload ([carriesTheContract]; a write cut part
// way instead reconnects and reads the new limit, [reconnectWatch]), an
// authorization error repeated on the reconnect, an -ERR it does not treat as
// transient, or reconnects running out once the embedded broker stopped. Such a
// node stays live to a probe while its seats lapse to peers, so it stops and
// its supervisor restarts it, where [carriesTheContract] names a setting still
// wrong. Only a close this package did not ask for counts ([watchClose]). The
// FIRST close is the cause: later ones, often the same reason a moment later,
// are only logged, so the cause a reader reports cannot change under it.
type connectionLoss struct {
	once  sync.Once
	done  chan struct{}
	cause error

	// acks closes when the queue's OWN connection goes: nothing a running turn
	// concludes can be settled then and the broker redelivers it elsewhere, so
	// the drain ends at once ([Queue.AcksLost]); with only the coordination
	// connection gone every ack still lands. A second Once, not a flag on the
	// first close, since the queue's own is routinely the second to go.
	acksOnce sync.Once
	acks     chan struct{}

	// conns is every watched connection, so a failed start can wait for the
	// handler of one the client has already closed ([connectionLoss.settle]).
	// Never pruned: a queue holds two.
	mu    sync.Mutex
	conns []*nats.Conn
}

func newConnectionLoss() *connectionLoss {
	return &connectionLoss{done: make(chan struct{}), acks: make(chan struct{})}
}

// record keeps the first cause and closes done, closing acks first when the
// connection is the one deliveries settle over. The cause is written, and acks
// closed, BEFORE done: a reader woken by done (`crewlet run` on [Queue.Lost])
// asks [Queue.AcksLost] at once to decide whether the drain finishes or
// cancels the running turns, so both must be settled by then.
func (l *connectionLoss) record(cause error, settles bool) {
	if settles {
		l.acksOnce.Do(func() { close(l.acks) })
	}
	l.once.Do(func() {
		l.cause = cause
		close(l.done)
	})
}

// lostCause is the recorded cause, or nil while nothing has been lost. Read
// only once done is closed, so lock-free: [connectionLoss.record] writes the
// cause first.
func (l *connectionLoss) lostCause() error {
	select {
	case <-l.done:
		return l.cause
	default:
		return nil
	}
}

// watched is what a dial is told about the connection it opens: where NATS
// closing it for good is recorded, and whether it is the queue's OWN. The zero
// value is UNWATCHED: its owner notices its loss ([Queue.DialOwned]).
type watched struct {
	loss *connectionLoss
	// settles marks the queue's own connection, which every delivery is
	// settled over and whose loss [Queue.AcksLost] reports. Exactly one per
	// queue: one [Queue.DialWatched] opens does not, since a node whose
	// coordination connection is gone still settles what it runs.
	settles bool
}

// track notes nc as watched once a dial is about to hand it out — after the
// contract check, which closes a connection it refuses itself and so leaves no
// handler to wait for.
func (at watched) track(nc *nats.Conn) {
	if at.loss == nil {
		return
	}
	at.loss.mu.Lock()
	at.loss.conns = append(at.loss.conns, nc)
	at.loss.mu.Unlock()
}

// during is err as a start that failed beside a loss says it: the classified
// cause first, since it names what to change, then the step's own error, both
// in the chain; err unchanged where nothing was lost. The one composition for
// [newQueueOn] and [Queue.LostDuring], so the two cannot word it two ways. It
// settles first ([connectionLoss.settle]), so it is asked BEFORE the start's
// own cleanup closes anything.
func (l *connectionLoss) during(err error) error {
	if err == nil {
		return nil
	}
	l.settle()
	return l.recorded(err)
}

// recorded is [connectionLoss.during] without the settle, for a queue whose
// own Stop closed its connection: no handler is coming to wait for.
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
// already closed, and at once where none is. The client queues the closed
// handler under the lock IsClosed reads, so a closed connection the caller did
// not close has a handler that WILL run; without the wait, a step's bare
// "connection closed" is composed while its cause is about to be recorded.
// Hence BEFORE the caller's cleanup: a connection its owner closed reads as
// closed too, and under [nats.NoCallbacksAfterClientClose] its handler never
// runs, so this would wait for ever.
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

// lossWatch is a watched connection's closed handler. A METHOD rather than a
// closure, for [reconnectWatch]'s reason: a case can tell the handler a
// connection was given is this one.
type lossWatch struct {
	loss     *connectionLoss
	settles  bool
	log      *slog.Logger
	embedded bool
	// server is fixed at the dial: a closed connection no longer says which
	// member it talked to, and the remedy is the same on every one.
	server string
}

// watchClose is the option pair that makes a connection's close for good a
// node's loss, or nothing for an unwatched one ([Queue.DialOwned]). The pair,
// never the handler alone: without NoCallbacksAfterClientClose the handler
// runs for the owner's own Close too (a Stop, a Drain), and every graceful
// stop would read as a lost broker.
func watchClose(at watched, embedded bool, url string) []nats.Option {
	if at.loss == nil {
		return nil
	}
	w := lossWatch{loss: at.loss, settles: at.settles,
		log: logging.Get("queue.jetstream"), embedded: embedded,
		server: lostServer(embedded, url)}
	return []nats.Option{nats.NoCallbacksAfterClientClose(), nats.ClosedHandler(w.closed)}
}

// lostServer names a lost connection's server: for an operator's, the
// addresses stream.url names — never the URL, whose userinfo may carry a
// password or token ([serverAddresses]).
func lostServer(embedded bool, url string) string {
	if embedded {
		return "this node's embedded NATS server"
	}
	return "the NATS server stream.url reaches (" + serverAddresses(url) + ")"
}

// closed logs every watched connection's close and records the first
// ([connectionLoss]). The line says whether it carried acks, since only the
// queue's own leaves running turns unable to settle.
func (w lossWatch) closed(nc *nats.Conn) {
	cause := lostConnection(w.server, nc.LastError(), w.embedded)
	w.log.Error("jetstream_connection_closed_for_good",
		"server", w.server, "carries_acks", w.settles, "error", cause.Error())
	w.loss.record(cause, w.settles)
}

// lostConnection is the sentence a node stops with: what closed, why, and what
// to change. FOUR SENTENCES for four remedies — the server's max_payload, the
// credentials presented, the server's own log, and for the embedded broker,
// which has no setting, a restart — since one would send an operator after
// the wrong one.
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
