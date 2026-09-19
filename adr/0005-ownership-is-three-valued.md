# ADR-0005 — "Do I hold this?" has three answers, never two

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** the `(value, error)` signature — a compile error, not a test
- **Cost-when-tried:** treating *unknown* as *lost* tore a healthy company down over a two-second store blip: every node read its own failed lookup as "I no longer hold these seats", shed them, and the fleet moved every seat at once while the store was still fine.
- **Tag-status:** unreleased

## The decision

Every question of the form "do I still hold this?" returns three values, and Go
expresses it natively:

```
(lease, nil)  — held. Proceed.
(nil, nil)    — definitively NOT held: lapsed, moved, or advanced.
                Shed the work it covered, now.
(nil, err)    — UNKNOWN. The store could not be reached or did not answer.
                This says NOTHING about ownership: the record is untouched and
                probably still held. Keep the seats, stop admitting new work,
                and retry until the TTL is genuinely elapsed.
```

A bool cannot carry this, and neither can a bool plus a logged error. The
shape is `(value, error)` and the caller must branch on all three.

The same discipline appears one layer up wherever a decision has an
"I could not tell" case: a state-log write is `applied` / `pending` /
`unknown`; a maintenance gate that errors skips its job and is reported,
because "I could not tell whether there is work" is not "there is no work"; a
webhook dedupe claim **fails open**, because a delivery dropped by a store that
blinked is a message nobody ever answers.

## Why the obvious alternative is wrong

The obvious alternative is `Held(ctx) bool` with the error logged, and it is
wrong in the direction that takes a company down. Collapsing *unknown* into
*false* makes every transient failure of the coordination store look identical
to every node losing every lease simultaneously — which is precisely the
condition under which a fleet does the most destructive thing it can do.

Collapsing it the other way, into *true*, is worse in the rarer case: a node
that genuinely lost its lease keeps serving a seat a peer is now also serving,
and two nodes run one agent.

There is no third bool that fixes this. The information is genuinely ternary
and the type has to say so.

## What this does not decide

It does not say what to do with `unknown` — that is per caller, and the
answers differ. A seat host keeps its seats and stops admitting; a duty
declines to run; a dedupe claim proceeds. What the record forbids is a caller
that never sees the third case.
