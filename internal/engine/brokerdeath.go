package engine

import "context"

// brokerDeath is what the engine reads of its own embedded broker declaring
// that it cannot go on ([jetstream.Death]): closed once, with nats-server's
// reason. Declared here, by the consumer.
type brokerDeath interface {
	Done() <-chan struct{}
	Err() error
}

// brokerDuty is the name a broker's death is reported under, on the exit
// notice and in the log.
const brokerDuty = "embedded broker"

// watchBroker ends this process when its embedded broker declares itself dead,
// from now until the teardown begins. A nil death — an external stream — is
// watched by nothing.
//
// # Why the process ends, and why at once
//
// The broker is this node's: every seat's mailbox, every acknowledgement, the
// event log and, on an embedded fleet, the coordination store all ride it. A
// nats-server that called Fatalf has returned from the step that failed and
// left the rest undone, and nothing in it promises to recover — stock
// nats-server's own logger exits the process at that call. A node left running
// on it answers /health, holds its seats' leases until they lapse, and runs
// turns whose results cannot be acknowledged or recorded and will be worked
// again elsewhere once their ack window passes.
//
// A drain is not the answer, for the reason the seat watchdog does not drain
// either: every step that makes a drain graceful — acknowledging, publishing
// the stop, giving the leases back — rides the thing that is dead, so a drain
// could only spend the time a turn takes on work none of which lands. So the
// node ends through the engine's one unilateral exit, [seat.Watchdog.Fail]:
// the reason on stderr, the watchdog's exit code, and an orchestrator that
// restarts it at once — and a restart is what brings the broker back, from
// whatever its store holds.
//
// FROM NEW RATHER THAN FROM START, because a broker can declare itself dead
// in the window between its own boot and the engine's, and a boot that went on
// provisioning against it would fail later on a timeout naming something else.
// UNTIL THE TEARDOWN, which shuts this broker down itself (a nats-server
// shutting down reports what fails as an Error, never a Fatalf) or hands a
// lent one back to the caller that owns it.
//
// ctx is the boot's, and only its values are kept: the watch outlives the
// call that arms it.
func (e *Engine) watchBroker(ctx context.Context, death brokerDeath, die func(error)) {
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
		// LOGGED BEFORE THE EXIT, through the configured handler, so the
		// reason reaches wherever this node's log goes — a log file
		// included. The exit's own notice goes to stderr alone, which a
		// deployment that discards stderr never sees.
		log.ErrorContext(ctx, "embedded_broker_died", "error", cause,
			"detail", "this node's embedded broker declared it cannot go on, "+
				"so nothing it carries — mailboxes, acknowledgements, leases — "+
				"can be relied on; the node ends now rather than serve on it",
			"hint", "the cause is the broker's own; a restart recovers the "+
				"broker from its store, and the same cause on every restart is "+
				"a setting or a disk to fix")
		die(cause)
	}()
}

// brokerDeathExit is the production answer to a broker's death: the seat
// watchdog's exit, which ends the process.
func (e *Engine) brokerDeathExit(cause error) { e.watchdog.Fail(brokerDuty, cause) }
