# q — must a backend serve a claim it just granted?

Raised by applying a teammate's finding: the Pulsar author showed that a
conformance case can encode the SYNCHRONY of the backends it was written
against, invisibly, because on those backends the weaker and stronger readings
of a wait coincide. `coordtest` has one of those, and it is load-bearing
everywhere rather than in a single case.

## The assumption

Every case reads back what it just wrote — `claim` then `mustHold`, `claim`
then `floor`, `claim` then `ListLive`. `coord.go` promises nothing about when a
granted claim becomes visible to a subsequent read.

Both certified backends make it true for free and neither can demonstrate it:
`memory` is a mutex over a map, and `kv` runs over a single connection where a
`Create` is ordered before the `Get` that follows it. So no case can distinguish
a store that GUARANTEES read-your-own-write from one that merely happens to
provide it — the same blindness the meta wire-shape gap has, and reached the
same way.

A backend that replicated asynchronously, or served reads from a follower,
would fail case after case for a property nobody ever stated.

## Why the suite enforces it anyway

A claim that cannot be read back cannot provide mutual exclusion, which is the
entire primitive. And the seat host acts on reads taken immediately after
claiming: `ListLive(NodePrefix)` for the fleet size it divides seats by, and
`FleetProtocolFloor` to tell a gate refusal from a lost race. A lease granted
but not yet visible makes both of those answer with a fleet that does not exist.

## A second consequence, found the same way

It is not only that reads would go stale — an entire GROUP of cases would pass
vacuously. The negative-path cases (`a_rejected_renew_leaves_the_lease_
untouched`, `a_refused_claim_leaves_the_holders_lease_untouched`,
`a_gate_refused_claim_leaves_the_record_untouched`) prove that a refusal wrote
NOTHING, and there is no signal to wait on for that — a write that did not
happen produces no event. They read immediately and compare, which silently
assumes that a write, had it happened, would already be visible.

On a backend whose reads lag its own completed writes, a real corruption is
simply not visible yet and all three pass. That is not a wrong answer, it is an
empty one, and it is indistinguishable from a green. All four assertions read
back the SAME resource the refused operation acted on, so a per-resource
guarantee is exactly what makes that group mean anything.

## The question

Should `Lease` or `Backend` say so — that a granted claim is visible to every
subsequent read through the same backend handle?

- **For.** It is already required in practice, and stating it tells a backend
  author to serve reads from wherever the write landed, before they discover it
  through twenty failing cases with no common theme.
- **Against.** It forecloses a design nobody has asked for yet, and the honest
  scope may be narrower than "every read" — a per-resource guarantee is enough
  for the seat host, while `ListLive` across a prefix is a weaker need.

Recommendation: state it, scoped per resource, and say that prefix listings may
lag. That matches what callers actually depend on and leaves a replicated
backend room to exist.
