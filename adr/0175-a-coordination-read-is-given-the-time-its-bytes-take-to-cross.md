# ADR-0175: A coordination read is given the time its bytes take to cross

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/coord/kv.TestAPointerReadAcrossALeafsLinkCarriesNoBody`, `internal/coord/kv.TestABodyAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/coord/kv.TestABodysReadIsGivenTheArrivalOfItsRecordedSize`, `internal/coord/kv.TestAFlipLandsWithinHalfTheBodysRetentionOfItsWrite`, `internal/coord/kv.TestAFlipIsGivenTwiceTheBodysWriteAtItsBound`, `internal/coord/kv.TestABodysCrossingIsHeldToWhatAKeyHolds`, `internal/maintenance.TestTheBodySweepRunsUnderTheDutyAtItsOwnHorizon`, `internal/engine.TestAPostureAcrossASlowLinkIsReadNotAssumed`, `internal/engine.TestAReconcilerAcrossASlowLinkAppliesARevisionAtItsBound`
- **Measured:** at the slowest link pace allowed (30 s a MiB), a leader read's reply is a third larger than the value, so no record above 47.1 KiB is read inside a probe's 1.875 s and above 127.1 KiB every read without a deadline fails at the five-second read term; a company at its bound made the activation pointer an 8 MiB reply, 240 s. The pointer at its 8 KiB bound is a 12,144-byte reply, 0.35 s; a typical one about 0.06 s.
- **Cost-when-tried:** the company document inside the activation pointer (#1149): every probe, heartbeat and tick on every node read it whole, so on a leaf a large company failed `/ready`, every reconcile tick and the boot, and above about 50 KB the pointer reads alone filled a floor link.
- **Tag-status:** unreleased

## The decision

**A coordination read is given the time the bytes it may carry take to cross
the node's link at the slowest pace allowed.** Three clauses:

1. Every key class declares the most its value may be, and a write past it is
   refused.
2. Every way the class is read is given the crossing of that bound at the
   node's reach: a key's leader read, a listing's pass and its certifying
   reads, a watch's consumer.
3. A reader with a reason of its own that cannot wait that long (a probe, a
   boot, a hot listing) reads a small record, and the large bytes are a class
   of their own, named on it by size and digest. Only the reader that needs
   them pays their crossing, sized to the size recorded.

The activation pointer is the first case of clause 3. It is at most 8 KiB
(`coord.MaxPointerBytes`), its summary at most 4 KiB as it encodes
(`coord.MaxSummaryBytes`, refused past it, never cut), and it names the sealed
document, `body.<sha256>` in the same bucket, held to what one key holds. A
write puts the body, then flips the pointer within half of
`coord.BodyRetention` of that write; a node reads the body only when it lacks
the revision, given the arrival of the recorded size, and refuses one whose
length or digest differs. A sweep of its own removes a body no pointer names
once it is `coord.BodyRetention` (an hour) old, through the revision its pass
saw: half of it covers a body's write at its bound across a floor link and the
flip behind it, the other half clock skew. Each activation's body is kept that
long past the next activation, plus a 15-minute tick, so a company at its bound
activating N times an hour holds up to about 1.25 × N × 6.29 MB more per
replica. The
posture's grace (`configplane.LagGrace`) grows by the body's crossing. ADR-0009
stands: the pointer is still append-only and its revision is the epoch.

## Why the obvious alternatives are wrong

**A longer deadline for every read.** A probe's budget is the orchestrator's,
not ours: no deadline makes an 8 MiB reply cross a floor link in 1.875 s.

**Cutting the document smaller.** The bound on a company is what one key
holds; the pointer is small only if the document is not in it.

**Keying the body by revision, or one shared body key.** Two writers' bodies
then meet on one key, and a flip can name a body the other overwrote, which is
why the body once moved into the pointer. Content addressing gives each set of
bytes its own key.

## What this does not decide

The other record classes and read shapes the rule binds (a listing's pass, a
watch, the runs' standing at boot, presence rows) follow in the rest of #1814,
each declaring its bound and the read it is given.
