# ADR-0017 — A turn id names one RUN; the work key names the unit of work

- **Status:** accepted
- **Authority:** `internal/workkey`
- **Enforced-by:** `internal/engine.TestARedeliveredTriggerRunsUnderItsOwnIdentity`
- **Cost-when-tried:** they were one value, and a redelivered trigger re-ran under the identity the failed attempt already occupied. A seat whose coding CLI was not logged in failed on `auth`, was NAK'd, and came back after the operator logged in: the retry spent 327k tokens and was invisible for the whole four minutes it ran. Its phase rows collided with the dead attempt's, the live projection folded its rounds into that attempt's frozen failed call, and `store.Turns` folded both into one row — permanently `failed`, with the two attempts' tokens summed. The only phase the screen could show was `review`, because the first attempt never reached it.
- **Tag-status:** unreleased

## The decision

A turn has **two identities and they are different values**.

- **The run id** names ONE EXECUTION. It is minted per dispatch
  (`Dispatcher.Dispatch`), it is the `turn_id` on every event the turn
  publishes, and it is what a detached sandbox row, an MCP bridge session and
  every phase record are keyed on. `(turn_id, phase, iteration)` is the
  identity every reader files a phase under, so it has to be unique per
  execution.
- **The work key** names the UNIT OF WORK — `internal/workkey`'s digest of the
  trigger events a dispatch derived it from. It is stable across a re-run and
  across nodes, and it is what a write that must happen once per unit of work
  is keyed on: the completion ledger, the episode row, the counterparty
  interaction count, the conversation entry, and every derived tracker
  operation and comment id.

Both travel together — on `turnctx.Turn`, on `runner.Turn`, on the turn and
phase events, and as two columns on `crewlet_events`. A leaf takes whichever
answers its own question: *which execution was this?* or *which work was it?*

The rule that decides every site is one sentence: **if a redelivery must
reproduce the value, it is the work key; if a redelivery must not, it is the
run.**

## Why the obvious alternative is wrong

The obvious alternative is the shape this replaces: one identity, because a
turn feels like one thing. It is not, and the reason is upstream of any
naming choice.

A turn that fails without reaching outside the engine is NAK'd rather than
abandoned (`Dispatcher.abandon` states why), so the broker redelivers the very
same trigger — up to twenty-five times, backing off from one second to thirty.
**A re-run is ordinary, not exceptional.** So "the turn" is genuinely two
quantities: the work somebody asked for, which happened once, and the
executions of it, which happened N times. A single value has to pick one
meaning and be wrong for every reader that wanted the other.

It picked the work key, so every per-execution reader was wrong, silently and
in the same direction — each one *kept the older attempt*:

| Reader | What it did with two attempts |
|---|---|
| `livestate` | `sameCall` matched, so the retry's rounds folded into the frozen failed call |
| `mergePhases` (dashboard) | kept whichever record the mount query applied last, which is the oldest |
| `store.Turns` | one row: `MAX(failed)`, `SUM(tokens)`, concatenated models |
| `sandbox` pending row | one row, one box, for two runs |
| `mcpbridge` | one session id for two runs |

The mirror alternative — make it the run and let the once-per-unit-of-work
writes key on that — fails in the other direction and worse, because those
writes FAIL OPEN: `ON CONFLICT DO NOTHING` simply stops firing. A retry would
write a second episode, re-count an interaction, post a second comment and
file a second tracker operation, and nothing would report it.

## What a rolling upgrade sees

Two builds on one stream disagree about `turn_id` for the length of a rollout,
and the consequence is bounded duplication rather than corruption.

A NEW node reading an OLD node's `turn_completed` is covered: the payload
carries no `work_key` and its `turn_id` IS one, so `learning.Turn.WorkKey`
falls back to it and the episode and the interaction count still collapse.

An OLD node reading a NEW node's event is not, and cannot be: it reads
`turn_id` as the work key, and that is now a per-run value, so two attempts at
one trigger write two episodes and count two interactions. The window is the
rollout, the effect is a duplicate row the recall and the synthesis then weight
twice, and nothing reports it. It is the same bounded duplication
`internal/learning` already promises in place of exactly-once, which is why
this is stated rather than engineered around: the alternative is a second
identity on the wire for old readers to mistake in some other way.

Two pieces of STATE outlive the rollout rather than crossing it, and each
reads its own era from the SHAPE of the value rather than from a flag.

A `sandbox` pending row is written once and re-entered minutes or days later,
possibly by another node: a run parked before the split has no `work_key`
field and its `TurnID` IS one, and nothing rewrites a parked row. So every
reader of a row goes through `PendingRun.UnitOfWork`, never the raw field —
the resume, the board, and each of the three announcements a detached run's
identity travels on (its completion, its question, its failure).

The event store's `work_key` COLUMN is backfilled from `turn_id` by
`schema/0029`; the `tags` blob beside it is not, because that blob records
what the writer extracted from an event whose JSON carried no such field, and
rewriting every one would restate history and still leave the stored payload
disagreeing. The column is therefore the single authority — `EventRecord`
reads it directly, which makes it the one promoted value that is not a copy of
a tag — and a reader going through the tags answers "no unit of work" for
exactly the history the backfill exists to preserve.

## What this does not decide

It does not make a turn id meaningful outside this engine, and it does not
make either identity a trace: one trace spans several turns, and a turn
resumed after a restart spans several traces.

It does not change what `internal/workkey` derives, only what else exists
beside it. An empty work key is still the honest default for a turn with no
ledgerable trigger.

It says nothing about a SUSPENDED turn, which is not a re-run: a detached
coding run re-enters the run that parked it, keeping the id from its own row.

And it does not decide the `turn_id` column's history. Rows written before the
split carry a work key there, which is what `schema/0029` backfills across
rather than reinterprets.
