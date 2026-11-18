package engine

import "context"

// brokerDeath is what the engine reads of its own embedded broker declaring
// that it cannot go on ([jetstream.Death]): closed once, with nats-server's
// reason.
type brokerDeath interface {
	Done() <-chan struct{}
	Err() error
}

// brokerDuty is the name a broker's death is reported under, on the exit
// notice and in the log.
const brokerDuty = "embedded broker"

// watchBroker ends this process when its embedded broker declares itself dead;
// a nil death (an external stream) is watched by nothing. Serving on it would
// answer /health and hold leases while no turn's result can be acknowledged.
// The exit is [seat.Watchdog.Fail] ([seat.FailExitCode]), never a drain
// ([Engine.Fatal]'s included): every graceful step rides the dead broker, whose
// connections stay open, so a drain would wait as long as a turn takes; a
// restart recovers the broker from its store. Armed in New, where the watchdog is built; a death is a latch, so
// one declared earlier is acted on at the arm, and a boot step waiting on the
// broker before then ends by its own budget ([jsprovision]), since no Fatalf of
// nats-server v2.15.1 is reachable once Start returns ([jetstream.Death]).
// Watched until the teardown, which shuts the broker down or hands a lent one
// back. ctx is the boot's, kept only for its values: the watch outlives it.
func (e *Engine) watchBroker(ctx context.Context, death brokerDeath) {
	if death == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	torn := e.teardownSignal()
	go func() {
		select {
		case <-death.Done():
		case <-torn.Done():
			return
		}
		// A TEARDOWN THAT BEGAN FIRST WINS a race with the declaration:
		// past it the broker is this engine's to shut down or its lender's
		// to keep, and neither is a node serving on a dead broker.
		if torn.Err() != nil {
			return
		}
		cause := death.Err()
		// LOGGED BEFORE THE EXIT, through the configured handler, so the reason
		// reaches this node's log file too: the exit's notice goes to stderr alone.
		log.ErrorContext(ctx, "embedded_broker_died", "error", cause,
			"detail", "this node's embedded broker declared it cannot go on, "+
				"so nothing it carries — mailboxes, acknowledgements, leases — "+
				"can be relied on; the node ends now rather than serve on it",
			"hint", "the cause is the broker's own; a restart recovers the "+
				"broker from its store, and the same cause on every restart is "+
				"a setting or a disk to fix")
		e.watchdog.Fail(brokerDuty, cause)
	}()
}
