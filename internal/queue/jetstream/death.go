package jetstream

import (
	"errors"
	"fmt"
	"sync"
)

// ErrBrokerDied is an embedded broker that declared itself unable to go on:
// nats-server called its Fatalf.
//
// A SENTINEL because the two places that read it have to act on it rather
// than print it. A boot refuses at once, with nats-server's own reason, and a
// test harness ends a case on it rather than retrying it with fresh ports —
// everything nats-server calls fatal is a fact about the configuration or the
// host (a store directory it cannot use, an advertise address it cannot
// parse, a listener it cannot bind), and only the last of those is worth a
// different number, which is why a port somebody else holds also carries
// [ErrRoutePortTaken] or [ErrLeafPortTaken].
var ErrBrokerDied = errors.New("embedded nats server declared itself dead")

// Death is an embedded broker's own declaration that it cannot go on.
//
// # Why the declaration has to be carried rather than logged
//
// nats-server reports a failure it cannot continue from through its logger's
// Fatalf — the store directory JetStream cannot use, a listener that cannot
// bind, an advertise address it cannot resolve — and then RETURNS from
// whatever it was doing, leaving the server half started. Stock nats-server's
// logger exits the process on Fatalf, so nothing in it needs to say so any
// other way. Here the engine owns the process ([natsLogger.Fatalf] does not
// exit), so the declaration reached nothing but an ERROR line: a boot whose
// JetStream had refused its store waited out its whole readiness budget for a
// server that was never going to be ready, and a clustered member was then
// told its route port had been taken, because Start had returned before it
// reached the route listener. The reason was in the log; the boot reported
// something else.
//
// So [natsLogger.Fatalf] declares the death here, and the two readers act on
// it: [startEmbedded] fails the boot at once with nats-server's own reason,
// and after the boot the engine ends the node — see [Server.Death].
//
// ONCE: the first declaration is the cause, and everything after it is the
// server failing further from the state the first one left it in.
type Death struct {
	once  sync.Once
	done  chan struct{}
	cause error
}

func newDeath() *Death { return &Death{done: make(chan struct{})} }

// Done is closed when the broker has declared itself dead, and never
// otherwise. A nil Death — a node with no embedded broker, or one already shut
// down — answers a nil channel, which never closes.
func (d *Death) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.done
}

// Err is the broker's own reason once [Death.Done] is closed, and nil until
// then. It is an [ErrBrokerDied], and it unwraps to the error nats-server
// handed its Fatalf when it handed one, so a listener's bind failure is still
// the [syscall.EADDRINUSE] it was.
func (d *Death) Err() error {
	if d == nil {
		return nil
	}
	select {
	case <-d.done:
		return d.cause
	default:
		return nil
	}
}

// declare records the first Fatalf.
//
// IT TOUCHES NOTHING BUT ITSELF, and that is a constraint rather than a
// preference: nats-server calls its logger with its own locks held — the route
// and leaf listeners' advertise failures call Fatalf under the server's
// mutex — so anything here that asked the server a question would deadlock
// the call that is reporting the failure.
func (d *Death) declare(format string, v ...any) {
	d.once.Do(func() {
		d.cause = &fatalError{reason: fmt.Sprintf(format, v...), cause: firstError(v)}
		close(d.done)
	})
}

// fatalError is a Fatalf as an error: nats-server's sentence, and the error
// value among its arguments.
type fatalError struct {
	reason string
	cause  error
}

func (e *fatalError) Error() string { return "nats-server: " + e.reason }

// Unwrap is the sentinel and the error nats-server reported, so a caller can
// ask for either without reading the sentence.
func (e *fatalError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrBrokerDied}
	}
	return []error{ErrBrokerDied, e.cause}
}

// firstError is the first argument of a Fatalf that is an error.
//
// THE VALUE, NEVER THE SENTENCE: every listener's bind failure hands Fatalf
// the *net.OpError it got, which names the address it could not bind and
// wraps the errno, and that is everything [deathError] needs to tell a taken
// port from anything else without matching a word nats-server wrote.
func firstError(v []any) error {
	for _, a := range v {
		if err, ok := a.(error); ok {
			return err
		}
	}
	return nil
}
