# d-000 — This is a rewrite, not a transliteration

Status: **standing rule** · Applies to: every phase, every executor

The Python tree is the **specification of behaviour**, not a template for
structure. Port the invariants, the incident-hardened decisions, and the
measured constants. Do not port the shape Python needed to express them.

A file that reads like Python with braces has failed even when its tests pass:
it imports Python's workarounds into a language that does not need them, and
the next reader inherits both.

## Translate the intent, not the mechanism

| Python did this | Because | Go does this |
|---|---|---|
| `DeferDelivery` exception as a third handler outcome | only two outcomes fit return/raise | an explicit `Result{Outcome}` enum — three outcomes are three values |
| `LeaseError` vs `None` vs a value | exceptions were the only way to distinguish "unknown" | `(value, error)` — the tri-state IS the return signature |
| five `contextvar` channels | avoiding a parameter through 20-deep call stacks | explicit `context.Context` values and passed structs; ambient only where genuinely ambient |
| mutable `BatchOptions` dataclass | one event loop made it "atomic" | a mutex-guarded struct read through accessors |
| `dir()` reflection over a types module | dynamic registry for free | explicit `Register` calls; a typo is an unknown type, not a silent miss |
| whole-object `clear()` + `update()` to keep identity | readers held the reference | immutable snapshots swapped atomically; readers take a snapshot |
| thread-executor bridging around a sync C++ client | asyncio could not call blocking code | goroutines; the bridging layer simply disappears |
| `getattr(x, "field", default)` duck typing | structural typing at runtime | real interfaces, optional behaviour via small interfaces and type assertions |
| advisory locks around a table | many OS processes shared one Postgres | an in-process mutex; the store is single-process by design |

## What Go-native means concretely here

- **Interfaces are small and consumer-defined.** Declare the interface where
  it is *used*, listing only the methods that caller needs. A 20-method
  interface exists because it was transcribed from a Python Protocol, not
  because anything needs all 20 — the one exception is `queue.EventQueue`,
  which is a deliberate certified contract with a conformance suite.
- **Errors are values, wrapped with `%w`.** Sentinels for the cases callers
  branch on; typed errors when they need detail. No error strings parsed.
- **Concurrency is structured.** A goroutine has an owner that can stop it and
  a way to report failure. `context.Context` first parameter, always. Channels
  for handoff, mutexes for state — not one dressed as the other.
- **Zero values are useful.** `Result{}` is Ack. A nil map reads as empty.
  Callers should rarely need constructors for plain data.
- **Tests are table-driven, parallel, and race-detected.** The Python engine's
  correctness rested on there being one event loop; every one of those
  assumptions is now a real race, and `-race` is what finds them.
- **Names are Go names.** `EventQueue.SubscribeBatch`, not
  `EventQueue.subscribe_batch`. Getters drop `Get`. Acronyms stay capitalised:
  `ID`, `TTL`, `MCP`, `LLM`, `API`.
- **Generics where they remove duplication, not where they show off.**
  `PartitionByKey[T]` earns it; a generic store abstraction does not.

## What must NOT be modernised away

Idiom is free to change. These are not idiom, and every one of them replaced a
real incident:

- the tri-state (`unknown` ≠ `lost`) and each store's fail-open/fail-closed
  polarity (REWRITE_PLAN §15);
- epoch monotonicity across release, and every fenced write;
- ordering and destructiveness of the four attachment verbs;
- defer-rather-than-nak-or-republish on a lost seat;
- the measured constants and their provenance (REWRITE_PLAN §14);
- the non-promises (REWRITE_PLAN §16) — bounded duplication, not exactly-once.

When idiom and invariant appear to conflict, the invariant wins and the
comment explains why the idiomatic form was rejected.
