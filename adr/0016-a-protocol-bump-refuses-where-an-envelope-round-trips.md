# ADR-0016 — A coordination protocol bump refuses where an event envelope round-trips

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** nothing
- **Cost-when-tried:** both existing bumps were silent corruption in a mixed fleet, which is the bar for spending one. v2 = holding a seat means consulting the completion ledger; v3 = claiming a seat means this node satisfies the role's placement. In each case a node on either version was individually correct and the pair was jointly wrong.
- **Tag-status:** unreleased

## The decision

A rolling upgrade puts two builds on one broker and one coordination store at
once, and this engine answers that fact in two OPPOSITE ways depending on what
is being shared. Both are deliberate, and the pair is the decision.

**On the stream, an unknown thing round-trips.** ADR-0006: an event type this
build has never heard of decodes, survives and re-publishes losslessly, because
dropping it would make every upgrade an outage. A newer peer's records flow
through an older node untouched.

**On a lease, an unknown thing REFUSES.** `coord.ProtocolVersion` is the
seat-host protocol a build speaks, and a node will not claim anything while a
live lease is held at a LOWER protocol. The rule is asymmetric on purpose:
older nodes keep working, because they cannot know about a check that postdates
them; newer ones wait, visibly, until the last old lease lapses.

The difference is what the two things carry. An event is DATA, and data a build
does not understand is still data it can pass on. A lease is an AGREEMENT about
what holding it MEANS, and two nodes that disagree about the meaning are each
individually correct and jointly wrong — one runs a seat believing the
completion ledger was consulted while the other believes it was not, and both
are right about their own half.

Two consequences travel with it. Schema evolution on the lease payload is
additive-only: a field the older build ignores is invisible to it, and one it
REQUIRES is a crash. And a downgrade across a bump needs a full drain, because
an older build has no protocol check at all and will happily take over a newer
node's expired leases.

The version is bumped when the MEANING of holding a lease changes, never when
something merely gains a field.

## Why the obvious alternative is wrong

The obvious alternative is one policy for both, and it fails in whichever
direction it is unified.

Make the lease round-trip like the envelope, and a mixed fleet silently runs
two different ownership protocols. Nothing errors; that is the whole problem.
Both bumps in the history above are exactly this shape, and neither would have
produced a symptom until a seat had already been run twice or a turn had
already been repeated.

Make the envelope refuse like the lease, and every rolling upgrade is an outage
by construction: the older half of the fleet stops processing the newer half's
events, on a broker where the two are interleaved on one stream, for the whole
duration of the deploy.

## Why Enforced-by is `nothing`

The rule is about WHEN the constant is raised, not about any shape in the
source. `ProtocolVersion` is one integer, every comparison against it is
already covered by `internal/coord`'s own suite, and what a gate would have to
judge is whether a change to the MEANING of holding a lease arrived with a
bump — which is a reading of the rest of the diff, not of any file it could
open. The nearest static formulation, "the constant moves whenever a lease
payload's fields change", asserts the opposite of the decision: a field is
explicitly not a bump, so the gate would fail the correct change and pass the
dangerous one.

What would earn a gate is a second consumer of the version with its own idea
of the comparison. There is one, and it is the same expression in one place;
the day a second appears, holding the two against each other is the check —
the shape `internal/clientsource` already exists to provide for a value the
dashboard and the engine both declare.

## What this does not decide

It does not decide when to bump — only the bar, which is that a mixed fleet
would be silently corrupt rather than merely inconsistent. A field, a new
verb, or a change nobody's correctness depends on is not a bump.

It does not extend to the fan-out's own wire, which takes a third answer for a
third reason: a peer that cannot decode a slice request answers nothing, and
the coordinator reports a missing assignment. That degrades a ranking rather
than a search, so refusing would cost more than it protects.
