# d-103 — Event.Data is always the pointer form, and a twin is a wire

Status: decided
Supersedes: nothing
Related: `rewrite/decisions/000-go-native-rewrite.md` (the safe-zero rule this
extends), `rewrite/decisions/101-queue-contract.md`

## The bug this closes

`events.New(TaskAssigned{...})` stored `Data` as a **value**. Decoding an event
off a broker produces a **pointer**, because a decoder must have something to
write into. So the same event carried a different Go type depending on whether
it had crossed a wire yet, and `events.DataAs[*TaskAssigned]` — the only way
callers are supposed to reach typed fields — answered `false` on the publishing
node and `true` everywhere else.

Nothing about that looks wrong when it happens. `DataAs` returns a boolean, not
an error, so the handler simply reads a zero payload and carries on. It was
found by the fleet suite: a criterion asserting a seat's backlog arrived in
order reported `backlog arrived as [ ]` — two events delivered, both with empty
work.

## The rule

**`Event.Data` holds the pointer form of a payload, always.** Three
construction paths, one type:

| path | mechanism |
| --- | --- |
| `events.New(T{...})` | generic, constrained by `PayloadPtr[T]` — a compile error otherwise |
| `events.Register[T]()` | same constraint; the registry constructor returns `*T` |
| `events.NewFrom(p)` | run-time boxing, for the interface-typed case inference cannot reach |

`Register` takes the type rather than a prototype instance, so
`Register[*TaskAssigned]()` — which would build a registry entry handing back an
unusable `**TaskAssigned` — fails to compile instead of at decode time.

`New` also **copies** the payload it is given, so a caller reusing one struct
across a publish loop does not retroactively rewrite events it already sent.

## The second half: a twin is a serialization boundary

The same defect had a mirror in `internal/queue/memory`, which handed consumers
the publisher's own `*events.Event`. That is not a shortcut, it is a different
contract, and because the twin is what every unit test runs on it does not
merely fail to catch bugs — it certifies them. Three it was hiding:

- a payload keeping a Go type it loses in transit (the bug above, invisible
  because the twin never converted it),
- a JSON number arriving as an `int` rather than a `float64`,
- one consumer group's handler mutating what another group is about to read.

The twin now marshals once per publish and decodes a private copy for every
subscription and every stream subscriber. Publish listeners still receive the
publisher's event, which is what real backends do — they are a local hook that
runs before anything reaches a wire.

`queuetest`'s **Wire** group certifies this for every backend. Mutation-checked:
reverting the twin to hand out the publisher's pointer fails
`the_publishers_event_is_not_the_delivered_one` and `each_group_gets_its_own_copy`.

## What this does NOT require

Any particular encoding. The Wire group asserts only what a consumer can
observe — an event that survives intact, decoded into this build's types, in a
copy nobody else holds. A backend is free to use whatever wire format it likes.

## The generalisation

This is the same shape as the `AcquireOptions.Protocol` escalation and belongs
with it under d-000: **wherever the port turns a Python default into a Go type
decision, the default has to be made safe on purpose.** Python had one payload
representation because a decoder there mutates the object it was handed; Go has
two, and nothing chose between them. Choosing one, at the contract, is the fix —
not a convention each caller is expected to follow.
