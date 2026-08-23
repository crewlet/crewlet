# q — what protocol does a claim carry when the caller sets none?

Raised by: auditing `coordtest` against a teammate's finding — *a conformance
suite must not require the happy path of the backend it was written against*,
and its corollary, that a suite must not settle a question the contract left
open. This is the one place in the coordination suite where it does.

## The silence

`AcquireOptions.Protocol` is an `int`. Its zero value is `0`, and `coord.go`
never says what a claim carrying `0` means. Every backend has to decide, because
the gate compares protocols numerically.

Storing the zero verbatim is not one of the options. `0` is lower than every
real protocol, so a single such record refuses **every gated claim in the
fleet** until it lapses — a fleet-wide stall from one unset struct field. So a
backend must normalise. The contract just does not say to what.

## The two answers

**(a) Unset means the OLDEST protocol (1).** Fail-closed: the record gates newer
claims exactly as a real v1 hold would, and it converges because it lapses like
any other lease. This is what `coordtest` enforces today
(`protocol/an_unset_protocol_reads_as_the_oldest`), and what the Python engine
did — `protocol: int = 1` as the keyword default plus
`int(row.get("protocol") or 1)` on read, against real PostgreSQL.

**(b) Unset means THIS build's `ProtocolVersion`.** A record with no stated
protocol, in a fleet where every build stamps one, did not come from an ancient
node — it came from a caller on the current build that forgot the field.

## Why porting Python's answer is not automatically right

The languages differ in a way that inverts the risk, and this is the part worth
a decision rather than a port.

In Python the default was **a keyword argument default of 1**: a caller that
omitted `protocol=` got 1, and 1 was also the honest floor, so omission was
*safe*. In Go the same value arrives as **a struct zero**, and omission is now
the dangerous case: under (a), one caller anywhere in the engine writing
`coord.AcquireOptions{Owner: o, TTL: t}` and forgetting `Protocol:` stalls every
newer node in the fleet until that lease expires. Nothing in the type system
catches it, and the symptom — newer nodes claiming nothing, visibly, with the
floor reporting 1 — looks exactly like a legitimate rolling upgrade that is
simply not finished.

Under (b) that same omission is invisible and harmless. But (b) pays for it with
the failure the gate exists to prevent: a record written by a build that
predates the field reads as current, the gate never fires, and two builds that
disagree about what holding a lease means hold seats side by side — silently,
which is the whole reason the gate is asymmetric in the first place.

## What the suite does meanwhile

Enforces (a): a claim with no protocol reads back as `1`, contributes `1` to
`FleetProtocolFloor`, and gates a claim at a higher protocol. Fail-closed is the
right default for an undecided question, and it matches the Python semantics
that ran in production.

Recorded rather than gated behind a capability flag, because unlike a genuine
backend disagreement there is no evidence any backend wants (b) — both current
implementations do (a). But note the circularity: `internal/coord/kv` does (a)
because the suite says so, not independently. The suite manufactured that
agreement, which is exactly why this needs a contract owner's answer and not a
second backend's vote.

If the answer is (a), `AcquireOptions.Protocol` should say so, and the
omission footgun deserves a mitigation — the obvious one being that the engine's
own callers never construct `AcquireOptions` without it. If the answer is (b),
the suite case is wrong and should be inverted.
