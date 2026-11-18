package engine

import natsjs "github.com/nats-io/nats.go/jetstream"

// WrapCoordinationJetStream replaces b's coordination client with what wrap
// makes of it, for an outside test that must hold the seat memory changelog
// ([Backends.CoordinationJetStream]): memory replays and flushes on the
// coordination connection, never the queue's (ADR-0001), so a queue stand-in
// cannot reach it. Stores already opened keep the old client; whatever is
// built from b afterwards, [New] included, gets the wrapper.
func WrapCoordinationJetStream(b *Backends, wrap func(natsjs.JetStream) natsjs.JetStream) {
	b.coordination = wrap(b.coordination)
}
