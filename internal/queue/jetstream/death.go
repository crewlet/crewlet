package jetstream

import (
	"errors"
	"fmt"
	"sync"
)

// ErrBrokerDied is an embedded broker that declared itself unable to go on:
// nats-server called its Fatalf.
//
// A sentinel because its readers act on it: a boot refuses at once, and a test
// harness ends the case rather than retrying with fresh ports. A fatal is a
// fact about the configuration or the host, and only a port somebody else
// holds is fixed by a different number, so only that one also carries
// [ErrRoutePortTaken] or [ErrLeafPortTaken].
var ErrBrokerDied = errors.New("embedded nats server declared itself dead")

// Death is an embedded broker's own declaration that it cannot go on.
//
// nats-server reports such a failure through its logger's Fatalf and then
// RETURNS, leaving the server half started; stock nats-server's logger exits
// the process, but [natsLogger.Fatalf] does not, since the engine owns the
// process. Logged only, the boot would wait out its readiness budget for a
// server never going to be ready and then blame a route port. So Fatalf
// declares it here: [startEmbedded] fails the boot at once with nats-server's
// reason, and after the boot the engine ends the node ([Server.Death]).
//
// ONCE: the first declaration is the cause; later ones are the server failing
// further from the state it left.
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
// IT TOUCHES NOTHING BUT ITSELF: nats-server calls Fatalf with its own locks
// held (the listeners' advertise failures, under the server's mutex), so
// asking the server anything here would deadlock.
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
// THE VALUE, NEVER THE SENTENCE: a bind failure hands Fatalf its
// *net.OpError, naming the address and wrapping the errno, which is all
// [deathError] needs to tell a taken port without matching nats-server's words.
func firstError(v []any) error {
	for _, a := range v {
		if err, ok := a.(error); ok {
			return err
		}
	}
	return nil
}
