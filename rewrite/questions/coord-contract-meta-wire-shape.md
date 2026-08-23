# q — must a Meta value survive a round trip with its Go type?

Raised by: measuring the twin against the KV backend after a teammate landed
d-103, whose lesson is that a twin's job is to BE the store and that every
shortcut in it is a bug it will certify rather than catch.

## The measurement

One payload, both certified backends, `Get` straight after `TryAcquire`:

| key        | written      | memory (before) | embedded NATS |
|------------|--------------|-----------------|---------------|
| `replicas` | `int(3)`     | `int`           | `float64`     |
| `roles`    | `[]string`   | `[]string`      | `[]any`       |
| `ratio`    | `float64`    | `float64`       | `float64`     |
| `on`       | `bool`       | `bool`          | `bool`        |

So a caller writing `meta["replicas"].(int)` or `meta["roles"].([]string)`
passed every memory-backed test in the tree and panicked against the store a
company is deployed on. The suite could not see it: every meta payload in
`coordtest` is already in the shape JSON decoding produces, so the cases cannot
distinguish a backend that encodes from one that hands the caller its own map
back.

## What the corpus already says

Not nothing, which is why this is a question about the SUITE rather than about
the twin:

- d-201 §2 — the ownership key carries "owner, epoch, preferred, protocol and
  meta **as JSON**". That is the KV mapping, not a statement about every
  backend, but it is what a deployment runs.
- `placement.rolesFromMeta` — written to accept "both the `[]string` this build
  writes and the `[]any` a JSON round trip through the lease store returns".
  The one real consumer is already bilingual, deliberately.

`coord.go` itself says only that `Meta` is `map[string]any` — "what the holder
IS". Nothing about encoding.

## What changed meanwhile (twin only)

`internal/coord/memory` now encodes meta at the door and stores the decoded
value, so it returns exactly what the KV backend returns. An unencodable
payload is an error rather than a silent Go-native pass-through, because a real
store's write would fail on it too and finding that in production is the whole
failure this closes. Evidence for doing it without waiting on an answer: three
independent sources agree (d-201, the consumer's own comment, and the
measurement), none of them manufactured by the suite.

## The open question

Should `coordtest` REQUIRE wire shapes of every backend — a case that writes an
`int` and a `[]string` and asserts `float64` and `[]any` come back?

The gap is measured, not argued: deleting the twin's encoding — reverting it to
handing the caller's own map straight back — **passes the entire contract
suite**, every case, under `-race`. Nothing in `coordtest` can tell the two
behaviours apart, so a backend that preserves Go types is certified today.

### Narrowed since first filing

The suite now separates the two properties and requires the one the contract
does back. `meta_values_survive_the_round_trip` writes a number, a bool, a
`[]string` and a nested map and compares **canonical JSON**, under which
`int(3)` and `float64(3)` are one value and `[]string{"a"}` and `[]any{"a"}`
are one list. Mutation-checked in both directions, which is the part that keeps
it honest:

| mutation | outcome | caught by |
|---|---|---|
| codec drops every non-string value | fails | 5 cases, incl. the new one |
| codec drops ONE number | fails | **only** the new one |
| Go-native codec (types preserved) | passes | — by design |

The middle row is the hole that existed before: every other meta fixture is
pre-shaped, so none of them writes a number, so none could see a codec that
loses one. The bottom row is what stops the case from being a type requirement
wearing a value requirement's name.

So what remains open is only the TYPE question below — value survival is no
longer at risk either way.

- **For.** Both current backends already behave that way, the deployed one
  cannot do otherwise, and without it a third backend may hand back Go-native
  types and be certified — which puts the panic back.
- **Against.** `coord.go` does not say meta is JSON, and an in-process or
  gob-based store could reasonably preserve types. Requiring a specific codec's
  output shape is a contract decision the suite does not own, and would be the
  suite writing one backend's implementation strategy into the contract again.

Deliberately NOT decided in the suite, and the gap is stated at the meta cases
so the next reader is not misled by their passing. If the answer is "yes",
`Lease.Meta` should say meta is JSON-shaped and the case is easy to add. If it
is "no", the contract should say that a caller may not depend on the Go type of
a meta value, because today nothing tells it either way.

**The recommendation here is "no".** Which is worth reading beside the queue
suite's write-up of the identical measurement on `Event.Payload`
(`queue-contract-free-form-payload-types.md`), because the two land differently
and the difference is structural rather than a disagreement.

An event has a TYPED path: a registered payload decodes into this build's
struct, so a `Count int` is an `int` on both sides of the wire. That makes "if
the type matters, register it" a real answer there — the free-form bag can stay
untyped precisely because a caller who needs a type has somewhere to go.

`Meta` has no such path. It is free-form by construction: node presence carries
whatever roles and labels a build advertises, and there is no registered shape
to promote it to. So a caller who needs an `int` back has nowhere to be sent,
and the only honest instruction is the negative one — do not type-assert a meta
value; read it the way `placement.rolesFromMeta` does, accepting either shape.
Saying so in `Lease.Meta` costs a sentence and removes the trap for good.
