package engine

// What a node must hold to be doing its work, for a readiness probe on a node
// that serves no API.
//
// A node with the ingress role is ready when traffic should come to it, and its
// /ready answers that from the drain, the company and the config posture alone.
// A node WITHOUT ingress takes no traffic at all: an orchestrator probing it is
// asking whether it has joined the fleet and is doing the job it was started
// for, which is what a rollout waits on before replacing the next one. That
// takes three facts the ingress answer never needed, each of which a node can
// lose while its process stays perfectly alive — and each is read from what
// already tracks it, so a probe asking on every call costs no round trip.

// BrokerLink reports whether this node reaches the fleet's broker now: nil
// while it does, and an error naming what is down otherwise.
//
// ASKED OF THE QUEUE'S OWN CLIENT, which is what every seat's mailbox, every
// publish and (on an embedded fleet) the coordination store ride. A queue with
// no link to lose — the in-memory twin, a broker and its clients in one
// process — answers nil, the way [brokerJetStream] answers no JetStream for
// it.
func (e *Engine) BrokerLink() error {
	linked, ok := e.backends.Queue.(interface{ Link() error })
	if !ok {
		return nil
	}
	return linked.Link()
}

// HoldsPresence reports whether this node holds its presence lease now — see
// [seat.Host.HoldsPresence]. A node its peers cannot see is in no fleet's
// divisor and is asked nothing by the estate's router, however healthy it
// looks from inside.
func (e *Engine) HoldsPresence() bool { return e.node.Host().HoldsPresence() }

// SeatAdmission reports whether seat admission applies to this node, and
// whether its latest placement pass was admitted to claim.
//
// IT APPLIES WHERE THIS NODE RUNS SEATS IN A MODE THAT PUBLISHES. A node
// without the seats role claims none whatever the gate says, and a node
// started into a maintenance mode withholds every claim by design
// ([Engine.seatsAdmitted]) — that is the mode working, not a node failing to
// join, and a probe that called it unready would stall the very rollout the
// mode is for.
//
// ADMITTED IS THE PASS'S OWN ANSWER ([seat.SweepResult.Withheld]), not a
// second evaluation of the gate: the gate reads the estate's router, which
// asks a data node over the broker, and a probe must not be a second caller of
// that. Before the first pass there is no answer, which is not admitted.
func (e *Engine) SeatAdmission() (applies, admitted bool) {
	if !e.profile.RunsSeats() || !e.Mode().Publishes() {
		return false, false
	}
	last, swept := e.node.Host().LastSweep()
	return true, swept && !last.Withheld
}
