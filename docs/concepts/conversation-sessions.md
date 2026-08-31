# Conversation Sessions

A seat that answered a Jira comment on Monday and is woken by a follow-up on
Wednesday used to arrive with no memory of Monday. Everything the engine
carried across turns was either *agent-scoped and similarity-addressed*
(episodes, the diary) or *content-free* (thread follows, the completion
ledger) — nothing was keyed to the conversation itself.

The **conversation session ledger** closes that. Every completed turn appends
a structured entry to its conversation, and the next turn of that same
conversation gets those entries back as an `## Earlier in this conversation`
block on its Plan and Execute user messages.

It is the cross-turn counterpart of the [prior-work ledger](turn-engine.md#prior-work-ledger-across-self_iterate-rounds),
and it deliberately follows the same doctrine one scope wider.

---

## What it is not

**Not a transcript replay.** The engine can already round-trip a whole LLM
conversation — the detached run's `execute_state` persists the full message
list, signed thinking blocks included, and splices it back into a running
loop. That is right for a turn *parked* on a question whose dangling tool call
is waiting for one answer. It is wrong here: a conversation's next turn
arrives against a thread that has **moved**, and replaying raw prior context
invites acting on state that is no longer true.

**Not episodic memory.** Episodes answer *"have I done something like this
before?"* by embedding similarity across every conversation. This answers
*"what did I already say here?"* by identity. They share a write site and
nothing else:

| | Episodes | Conversation sessions |
|---|---|---|
| Keyed by | agent + vector similarity | agent + `conversation_key` |
| Holds | two ≤2000-char summaries, tool names | plan, reasoning, calls, the reply, the verdict |
| Retrieval | cosine top-3, `done` only, no recency | the newest N of *this* conversation |
| On thin triggers | prefetch gated **off** | always rendered |
| Compaction | clusters collapse per-turn detail | none; trimmed by count and age |

The last row of that table is the sharpest difference. A Slack thread reply or
a Jira comment webhook is a *pointer*, and the engine's thin-trigger gate
skips all three aux-LLM prefetches on exactly those turns — the ones that
continue an existing conversation. The session block is deterministic (no
embedding, no aux LLM), so it renders there regardless.

**Not a second, invisible memory.** The [`cli-agent` workspace](subscription-llm-backends.md)
deletes a coding CLI's own sessions before and after every call, precisely so
that one task's context cannot leak into the next through a channel nobody can
see. This ledger is the inverse shape of what that rule rejects: engine-owned
rather than tool-private, scoped to one conversation rather than leaking
across tasks, and rendered into the prompt as a visible block — so the context
it adds is stated in the turn that uses it, where a person reading that turn
can see it, rather than applied out of sight.

---

## What an entry holds

Built at turn end from data already in hand — there is no summarisation call,
because a summariser that drops the line naming the reply the seat already
sent re-creates the duplicate-answer bug in a place nothing else can catch.

- **Triggered by** — who said what (the last constituent of a coalesced digest)
- **You planned** — the plan summary
- **Your reasoning** — the planner's own `reasoning` field
- **You called** — the tool-call lines, writes always recorded, reads marked `(read)`
- **You replied** — the turn's final text
- **Reviewer** — `completed_work`, the prose on what already landed
- **Turn ended** — only when the decision was not `done`

The reviewer's *other* field, `ReviewOutcome.notes`, is deliberately **not**
carried. It is documented as "shown to the next Plan round when the decision is
`self_iterate`" and is written as an instruction to that round — *"the next
round should retry posting X"*. Replayed into a later turn it stops being
history and becomes a standing order the reviewer never issued, aimed at a
round that already came and went. Nothing is lost by dropping it: the calls
that failed are in the tool lines and the verdict is in **Turn ended**.

Every field is elided at write time against the [ledger budgets](turn-engine.md#prior-work-ledger-across-self_iterate-rounds):
*elide payloads, never structure*.

### Reads stay marked, never merged

Inherited wholesale from the within-turn ledger, and **stronger** here: a read
from last Tuesday is stale by construction. Tool *results* are never carried,
reads render with a `(read)` marker, and the block's header tells the model it
may re-run exactly those. Telling it not to repeat a `jira_get_issue` would
push it to invent the data instead.

---

## Which turns are recorded

A turn is appended when all of these hold:

- it **completed** — a crashed turn has nothing coherent to record;
- it was not a **detached-sandbox suspend** — the resumed turn records once, for real;
- its trigger has a **reproducible conversation key** (see below);
- it has a **work key** — the constituent-trigger identity used for dedupe.

A turn that ended `failed` on a guard breach **is** recorded. "I tried this and
it did not land" is exactly what the conversation's next turn must not
rediscover the hard way.

### Identity

The row is keyed on `(agent_handle, conversation_key)` and deduped on
`work_key` — **never** `turn_id`. Two nodes completing one trigger mint two
turn ids, so a turn-keyed row would *record* the duplicate instead of
collapsing it, and the next turn would read its own reply twice.

The dedupe index is **partial**, over `work_key <> ''`: an empty work key is
the documented "a turn with no ledgerable trigger", and those turns are
legitimately distinct rows that must never collide onto one.

Separately, each row carries an `entry_id` its writer mints — the row's name
across the fleet, so [memory replication](seat-ownership.md#a-seats-memory-follows-it)
can carry the ledger with the seat. It is not a second dedupe: the table's
`id` is an `AUTOINCREMENT` that starts at 1 on every node and means nothing
off the one that wrote it, and `entry_id` says nothing about what a row
*means* — two unkeyed turns get two ids and stay two rows.

`conversation_key` is the `{source}:{local}` grammar that already partitions
every seat inbox for [coalescing](event-system.md#inbox-batching--coalescing):
`jira:POC-7`, `slack:C9:1718.001`, `github:acme/api#42`. A trigger with no
derivable conversation — a scheduled fire, a task assignment, an A2A wake —
keys as `event:{uuid}`, which no later message can reproduce; those are not
recorded, because the row could never be read back.

Two seats legitimately serve one conversation (a lead and its report on one
ticket) and each keeps its own ledger.

---

## What the next turn sees

The block is resolved **once** at turn start and frozen for the whole turn —
the same rule the Plan prefetches follow, so a `self_iterate` loop cannot
invalidate the provider prompt cache. It rides the **user** message, never the
frozen system prefix, alongside the prior-work ledger:

```
## Earlier in this conversation
…your prior turns, oldest first…

Task:
…the newest thing said…

## Already done earlier in this turn
…the within-turn ledger, on iterations after the first…
```

Oldest to newest top to bottom — the ask is the newest thing *said*, and the
within-turn ledger below it is the newest thing *done*, so the most recent
context sits nearest the model's answer. The header states the contract
explicitly: do not repeat a reply already given,
`(read)` calls may be stale, everything else already took effect, and where
the history and the task disagree the task wins.

Review does **not** receive the block. It judges *this* turn's delivery
against the plan, and its duplicate-delivery rule is already served by the
within-turn ledger; feeding it prior turns invites judging work this turn
never promised.

---

## Cost

The block is re-sent on every round of every phase, and Anthropic bills cache
reads at full token value, so its cost multiplies by rounds used. What bounds
that product is `max_entries`, at **write** time — the injected block is the
whole recorded conversation.

There is no injected-side budget. Two knobs (`injected_max_entries`,
`injected_max_chars`) used to be documented here; neither was ever threaded to
a caller, so both validated, defaulted and described a truncation that did not
happen. Rather than wire a cut into the one block that tells a seat what it
already said on this thread — which is how a seat repeats a reply it cannot
see it already gave — the bound stays on what is *kept*. The `prompt.size`
telemetry event records the delta fleet-wide.

Against that: the re-recon it displaces costs a `list_mcp_server_tools` round,
an `activate_tool` round and the read itself, in Plan **and again** in Execute,
on every turn of the conversation — and recovers only what was *posted*, never
the agent's own plan, reasoning, or the results it gathered.

---

## Configuration

```yaml
turn_engine:
  conversation_session:
    enabled: true            # the feature gate — a live kill switch
    max_entries: 20          # kept per conversation, trimmed at write time —
                             #   and the only bound; the whole kept
                             #   conversation reaches the prompt
    retention_days: 30       # matches the event store's own horizon
```

Nested under `turn_engine` rather than beside it, which is load-bearing: it
rides the live the turn-engine settings cell, so it hot-reloads through the
existing turn-engine diff handler with no extra apply-config wiring. Setting
`enabled: false` restores the previous prompt exactly — which is why it is
safe to leave on.

`retention_days` is the one retention an operator sets rather than the engine
(a company running quarter-long tickets has a real reason to keep more). It is
read when the maintenance worker is built, so a change lands at the next
process start, and it is floored at the sweep interval so a hostile value
cannot undercut the worker's own invariant.

---

## Storage

One row per recorded turn in `conversation_sessions`, a regular
table (not a hypertable), so dedupe is a plain unique index and an ordinary
`ON CONFLICT DO NOTHING` — the advisory-lock dance in
`031_work_key.sql` exists only because `episodes` is partitioned on time.

Bounded twice: `max_entries` trims on write (a chat DM keys on the whole
channel rather than a thread, so its ledger never stops receiving entries),
and the [maintenance worker](scaling.md) sweeps past `retention_days`.

Every field of a recorded entry is stored **verbatim** apart from the tool
arguments, which the [ledger budgets](turn-engine.md#prior-work-ledger-across-self_iterate-rounds)
elide. This row is the store's only record of the turn, so a field cut at write
time is not a shortened rendering — it is the only copy.

**Failure never stops a turn.** A write that fails is swallowed — it happens on
a completed turn's tail, where there is nothing left to tell. A read that fails
*raises*, and the caller decides: the turn engine renders no history (exactly
the pre-ledger prompt) and logs that it could not read it. Swallowing it in the
store would make "unreadable" and "nothing said yet" one answer, and a seat
would run without its history with nothing anywhere to say why.

Without a database the engine wires the in-memory twin, so a single node still
gets the feature; a seat that moves between nodes simply arrives with no
history, which is the same fail-open answer.

---

## Reading it

**There is no read surface, deliberately.** The ledger is prompt context: the
engine renders it into the next turn of the same conversation and nothing else
consumes it.

A dashboard tab and a `/conversations` endpoint did exist, and both were
removed. They were a viewer for somebody ELSE's threads — a Slack channel, a
Jira issue — reconstructed from what the engine happened to record about them,
always a worse version of the thread than the surface it lives on, and a
conversations screen in this product is meant to be Crewlet's own messaging
when there is one to show. Keeping a half-view until then would have promised
a chat system the engine does not have.

The entries reach a person through the prompt they shape, and through the
`conversation_key` shown on a phase, which names the external thread a turn
served so a reader can go to it.

---

## Related

- [Turn Engine](turn-engine.md) — the phases, and the within-turn prior-work ledger
- [Agent Learning](agent-learning.md) — episodes, the diary, counterparty profiles
- [Event System](event-system.md) — the conversation key and inbox coalescing
- [Scaling Out](scaling.md) — why shared state lives in the coordination slot
