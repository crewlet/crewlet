package engine

import natsjs "github.com/nats-io/nats.go/jetstream"

// WrapCoordinationJetStream replaces the client b's coordination connection is
// addressed through with what wrap makes of it, for a test outside the package
// that must hold what rides that connection — the seat memory changelog
// ([Backends.CoordinationJetStream]) — which a stand-in for the queue cannot
// reach: memory replays and flushes on the coordination connection, never on
// the queue's (ADR-0001).
//
// The stores already opened over the client keep it; whatever is built from b
// afterwards, [New] included, gets the wrapper.
func WrapCoordinationJetStream(b *Backends, wrap func(natsjs.JetStream) natsjs.JetStream) {
	b.coordination = wrap(b.coordination)
}
