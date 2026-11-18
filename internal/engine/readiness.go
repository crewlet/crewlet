package engine

// What a node must hold to be doing its work, for a readiness probe on a node
// that serves no API. An ingress node's /ready answers from the drain, the
// company and the config posture; a node WITHOUT ingress is asked whether it
// has joined the fleet and is doing its job, which a rollout waits on. Each of
// the three facts below can be lost while the process lives, and each is read
// from what already tracks it, so a probe costs no round trip.

// BrokerLink reports whether this node reaches the fleet's broker now: nil
// while it does, and an error naming what is down otherwise.
//
// ASKED OF THE QUEUE'S OWN CLIENT, which every mailbox, publish and (on an
// embedded fleet) the coordination store ride. A queue with no link to lose
// (the in-memory twin, an in-process broker) answers nil.
func (e *Engine) BrokerLink() error {
	linked, ok := e.backends.Queue.(interface{ Link() error })
	if !ok {
		return nil
	}
	return linked.Link()
}

// HoldsPresence reports whether this node holds its presence lease now
// ([seat.Host.HoldsPresence]): without it the node is in no fleet's divisor
// and the estate's router asks it nothing.
func (e *Engine) HoldsPresence() bool { return e.node.Host().HoldsPresence() }

// SeatAdmission reports whether seat admission applies to this node, and
// whether its latest placement pass was admitted to claim.
//
// IT APPLIES WHERE THIS NODE RUNS SEATS IN A MODE THAT PUBLISHES: a
// maintenance mode withholds every claim by design ([Engine.seatsAdmitted]),
// and calling that unready would stall the rollout the mode is for.
//
// ADMITTED IS THE PASS'S OWN ANSWER ([seat.SweepResult.Withheld]), never a
// second evaluation of the gate, which asks a data node over the broker.
// Before the first pass it is not admitted.
func (e *Engine) SeatAdmission() (applies, admitted bool) {
	if !e.profile.RunsSeats() || !e.Mode().Publishes() {
		return false, false
	}
	last, swept := e.node.Host().LastSweep()
	return true, swept && !last.Withheld
}
