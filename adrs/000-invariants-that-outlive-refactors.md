# adr-000 — The invariants that outlive any refactor

Status: **Accepted** · Applies to: the whole tree

Idiom is free to change. Every rule below is not idiom, and every one of them
replaced a real production incident. A refactor may reshape anything else in
this repository; if it reaches one of these, it is wrong until it has argued
its way past the ADR that records the cost.

## What must not be tidied away

- **The tri-state.** `unknown` is not `lost`. Every "do I hold this?" answers
  held / definitively-not / unknown, and each contract's fail-open or
  fail-closed polarity is chosen deliberately, per contract — never "fixed" to
  one uniform default. See [adr-201](201-coordination-contract.md).
- **Epoch monotonicity across a release, and every fenced write.** See
  [adr-404](404-hot-reload-epochs.md).
- **The ordering and the differing destructiveness of the four attachment
  verbs.** A single `unsubscribe` never said which one it was, and the
  difference is whether a seat's mail survives. See
  [adr-101](101-queue-contract.md) and [adr-105](105-the-queue-lifecycle-verbs.md).
- **Defer rather than nak-or-republish on a lost seat.** A republish sends an
  event to the topic tail while its prefetched siblings replay from the head,
  which reorders the conversation.
- **The measured constants and their provenance.** A constant carries the
  rationale for its value where it is defined, and a broker-specific one is
  re-derived against that broker rather than inherited from another. See
  [adr-102](102-jetstream-redelivery.md) and
  [adr-104](104-pulsar-redelivery-economics.md).
- **The non-promises.** Bounded duplication, not exactly-once. A doc that
  quietly upgrades one of these is a bug in the doc.

When idiom and invariant appear to conflict, the invariant wins and the comment
explains why the idiomatic form was rejected.

## A default is a type decision, not a value

A field whose zero value is a legitimate *setting* must not use the zero value
to mean "unset". The two questions are different and collapsing them is
silent: `pause_ttl_seconds: 0` read as "never pause" tore down the checkout of
every seat that simply said nothing about the knob.

So a field that has to distinguish "the operator chose zero" from "the operator
said nothing" takes a pointer or an explicit `Unset` sentinel — never a
comment. The same rule governs a model parameter a provider may legitimately be
asked to set to zero: the absence has to be representable, or the caller cannot
express it. See [adr-103](103-payload-pointer-invariant.md), where this was
learned against a payload type rather than a config field.
