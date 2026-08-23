# q — what does Release answer for a lease that already lapsed?

Status: **RESOLVED — `(false, nil)`** · Raised by the coordtest port · Decided by
the contract owner · Contract: `go/internal/coord/coord.go`

## The ruling

`false`. Three independent lines agree and none of them is the suite:

1. **The contract's own wording.** "Release gives up a lease the caller HOLDS."
   A lapsed lease is not held, by the same reading that already makes `Get`
   live-only.
2. **Self-consistency within one lifecycle.** A store answering `true` to
   `Release` while answering `nil` to `Get` for the same record contradicts
   itself, and a draining node is then told it handed back a seat it had in
   fact already lost.
3. **`kv` chose it independently**, before the case existed — so for once the
   agreement between backends is not one the suite manufactured.

Checked against the consumer before ruling, because a contract answer that
breaks its only caller is not an answer: `SeatHost.finishRelease` returns this
bool straight through, and every caller already treats `false` as "not PROVEN
released" rather than "the seat moved" — the seat is torn down locally either
way and the row lapses on its own. The one visible effect is that such a seat
is not counted in a sweep's `Lost` list, which is right: it was not handed
back, it ran out.

The twin was the one that was wrong and is corrected.


Found by applying an outside author's method rather than by a failing test: they
noticed that every case in a sibling suite took its pause BEFORE anything was in
flight, so one lifecycle point was never exercised. The same enumeration here —
at which points in a lease's life does the suite actually send each operation —
turned up one, and the two certified backends had been answering it differently
for as long as both have existed.

## The divergence, measured

`Release` by the rightful owner, at the correct epoch, on a lease that simply
RAN OUT and was never re-claimed:

| backend  | answer        |
|----------|---------------|
| `memory` | `(true, nil)` |
| `kv`     | `(false, nil)`|

The epoch is unaffected either way — both advance 1 → 2 on the next claim — so
nothing about fencing was ever at risk. The disagreement is the boolean a caller
reads.

No case sent it. Every release in `coordtest` is of a LIVE lease, or of one
already taken over by someone else (where the owner predicate rejects it before
liveness matters). The gap was invisible from inside the suite because both
answers passed everything.

## Why the suite now requires `false`

- `coord.go`: "Release gives up a lease the caller **holds**." A lapsed lease is
  not held.
- It is the same reading that already makes `Get` live-only, which this suite
  certifies. Answering `true` here while `Get` answers `nil` for the same record
  is the store contradicting itself within one lifecycle.
- A drain reads it. A node shedding seats that is told it released one it had
  actually lost to a lapse records a clean handover that did not happen.
- `kv` chose `false` independently — its behaviour predates the case, so unlike
  most agreement in this suite it was not manufactured by the assertion.

`memory` was corrected to match, and `releasing_a_lapsed_lease_reports_not_held`
now certifies it. Mutation-checked: reverting the twin's liveness predicate
fails that case and only that case.

## The question

Should `Backend.Release` say so? One sentence — "reports false for a lease that
has lapsed, since a lapsed lease is not held" — closes it permanently.

The alternative reading is defensible and worth stating so it is rejected on
purpose rather than by omission: `true` could mean "the record now reflects your
intent to give this up", which is also true. It is just not what the method's
own documentation says it answers, and it is the reading that lets a drain log a
handover it did not perform.
