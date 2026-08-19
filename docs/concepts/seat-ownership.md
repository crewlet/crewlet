# Seat Ownership

A **seat** is a role in the org chart, addressed by its handle. **Seat ownership** is how a fleet of Crewlet nodes decides which node runs which seat — and, more importantly, how it guarantees that no two of them run the same one.

The rule the whole design serves is one sentence: **a seat is not a thing you can half-own.** A node either holds a seat's lease, runs its agent, consumes its inbox and answers its sandbox completions, or it does none of those things.

---

## The problem

Every seat has a durable inbox topic, `crewlet.agent.{handle}.inbox`, consumed under a **Shared** subscription named `agent-{handle}`. Shared means competing consumers: each message goes to exactly one attached member.

That is exactly right when the members are one node's consumer. It is catastrophic when two nodes both attach: the broker splits the seat's traffic between them, so one agent's conversation runs as two interleaved turn streams on two processes, each unaware of the other. Turn exclusion is in-process state; neither node can see the collision, and nothing raises.

So attachment has to be exclusive, and exclusivity has to be provable across processes. That is what a lease is for.

---

## The lease

Ownership is a row in the `leases` table (`crewlet.db.leases`) with a TTL and a monotonic `epoch`:

```
seat:{handle}   owner=node-a:9f3c1e70   epoch=7   expires_at=…   preferred=node-a
```

Three properties carry everything above it:

- **The owner is a process incarnation, not a machine.** `{node_id}:{random}`, minted fresh at boot. A live lease is renewable by its own owner string, so two processes sharing an identity would both hold the seat at the same epoch — and the default node id is the shared constant `node-0`. The *stable* node id goes in `preferred`, where restart-stability is what you actually want.
- **The epoch is a fencing token, monotonic for the resource's lifetime.** Releasing expires the row in place rather than deleting it: a deleted row would restart the counter at 1 and hand the next owner a token its predecessor is still using.
- **A lapsed lease cannot be renewed, only re-acquired** — and re-acquiring bumps the epoch even for the same owner, because during the gap that owner's in-flight work was unprotected and must be fenced against its own past self.

## Placement

Placement is deliberately dumb, and lives in `crewlet.seat.host`:

- Every node holds a `node:{id}` presence lease, renewed on the same heartbeat as its seats. Counting the live ones is how a node learns the fleet size. It cannot be inferred from seat ownership: a fleet where nobody has claimed anything yet would read as zero nodes, and every node would then take every seat.
- A node claims up to `ceil(seats / live nodes)` — its **fair share** — trying `preferred`-hinted seats first for stickiness, and never more than `SEAT_CLAIM_LIMIT_PER_SWEEP` per pass, because each takeover costs an MCP spawn.
- A node holding **more** than its share hands the excess back, at most `SEAT_RELEASE_LIMIT_PER_SWEEP` per pass. Claiming alone converges only for a fleet that shrinks: a node that booted alone holds every seat, and a peer joining later computes a share it can never reach. Without the give-back, scaling out does nothing until something dies.

The share is a ceiling, so shares sum to at least the seat count and a node at its share has no room to re-claim what it just released. Rebalancing converges rather than oscillating.

```mermaid
sequenceDiagram
    participant A as node-a
    participant L as leases (PostgreSQL)
    participant B as node-b
    A->>L: acquire node:node-a
    A->>L: acquire seat:ceo, seat:eng, seat:ops
    Note over A: alone — share is 3
    B->>L: acquire node:node-b
    A->>L: list_live("node:") → 2
    Note over A: share is now 2
    A->>A: release seat:ceo (voluntary)
    A->>L: expire seat:ceo in place
    B->>L: acquire seat:ceo → epoch 2
    Note over B: spawn, budget, MCP, sandbox recovery,<br/>THEN attach the inbox
```

## Establishing a seat, and giving it back

`on_acquire` establishes the seat in a known state and attaches the consumer **last**: agent instance, budget cap, per-role MCP children, interrupted sandbox-run recovery, *then* the inbox and control subscriptions. A seat that starts receiving work before its MCP children are up runs its first turn with an empty tool surface.

Releasing has **two modes**, because losing a lease and choosing to let go are opposites:

| Mode | When | What happens |
|---|---|---|
| **Voluntary** | drain, capacity rebalance, role decommissioned | quiesce → let the in-flight handler finish under a bounded wait → detach → release the lease |
| **Fenced** | renew returned `False`, the TTL grace expired, an acquire hook failed, config posture went `shed`/`stuck` | **detach first**, abandon in-flight work, republish nothing |

Fenced release never republishes. A peer may already be running the seat; republishing hands it a second copy of work it is already doing, and sends those messages to the topic tail while the successor replays its prefetched siblings from the head — which reorders the conversation.

**A teardown that cannot be proven does not release the lease.** If the consumer will not close, the node keeps renewing and retries. A lease held too long costs latency; one released too early costs correctness.

## Deferring a delivery

A handler has two ordinary outcomes: return (ack) or raise (negative-ack, which spends the message's dead-letter budget). Seat handoff needs a third, so the queue protocol has one:

```python
raise DeferDelivery(f"seat {handle!r} is not owned here")
```

The delivery is left **unacked** and the attachment stops consuming. Measured against a real broker, a close-driven handoff does *not* increment `redeliveryCount`: the messages return to the seat's next owner in order, at count 0. A NAK would burn the budget on messages nothing is wrong with.

## Admission: freshness, not membership

"Do I hold this seat?" is a question about a local snapshot refreshed on a 15-second heartbeat against a 45-second TTL, so the honest answer can be a full TTL stale — precisely the window an ownership check exists to close. A membership check cannot meet its own exit criterion.

What *is* provable is that a successful renew at time *t* bought exclusivity through *t + ttl*. So `SeatHost.may_start(handle)` returns the epoch only when the last successful renew is inside one heartbeat interval, and `None` otherwise. Every turn that starts is then certified owned for at least `ttl - heartbeat`.

That also gives the right answer during a database blip. The lease row is untouched by an unreachable store, so the seat is **kept** — shedding on a two-second outage would tear a healthy company down — but new turns stop at the first failed renew. The consumer is quiesced, and un-quiesced when a renew succeeds again. Both edges matter: without the second one the node comes back healthy, still owning the seat, still attached to it, and never reads from it again.

## Fencing: what it protects, and what it cannot

The epoch is threaded into the **sandbox run state** — every mutation on a live `pending_sandbox_run` row carries `WHERE owner_epoch = $epoch` — and checked in the turn loop before every round and before every write-capable tool. A zombie's late write to a run it no longer owns bounces; a zombie's turn stops within a round.

It is **not** yet on every seat-scoped write. The learning tables (`episodes`, `agent_diary`, counterparty profiles), `token_usage`, and the onboarding markers and pass-claims are written unfenced, so a zombie that completes a turn between fence checks can write a second episode and a second diary entry for one human message. That is the least visible duplicate the design admits to and the one worth naming: a doubled Slack post is obvious and recoverable, while doubled agent memory is retrieved by every later turn's episode recall. Closing it is outstanding work.

**Fencing protects database state. It cannot protect outbound effects.** `run_sandbox` makes this concrete: it acquires a real, billed sandbox *before* the pending row is written, so no epoch-fenced insert can undo a box that is already pushing commits. The property the design offers is **bounded duplication**, not none — and what bounds it is the in-turn fence.

## The completion ledger

Fencing and admission bound the window in which two nodes could be working one seat. They do not close a narrower one: a turn **finishes**, its outbound effects ship, and the node dies before the delivery is acked. At-least-once then hands that trigger to the seat's next owner, which re-runs the whole turn — the same Slack reply, the same Jira comment, from an agent with no idea it already spoke.

`turn_completions` records what finished, and is read before the next turn starts.

It is deliberately **not** a claim, and the absences are the design:

- **No `in_progress` state.** The seat lease is already the mutual exclusion — one consuming node, serial within it — so a claim's only honest disposition for a stale in-progress row is "supersede and re-run", which is exactly what you do with no row at all. An earlier design had one; five of five reviewers rejected it, because every other defect they found existed only to service that state.
- **No expiry, no supersede rule.** A row means the work is done, and done does not lapse. Rows are deleted on a retention horizon by the `maintenance` duty — garbage collection, not semantics.
- **Keyed on *constituent* event ids.** A multi-event partition is merged into one digest before the turn runs, and that digest is minted fresh on every coalesce, so a key taken from it would differ on every redelivery and match nothing. Recording constituents also means a redelivery that overlaps a previous partition only partially — A+B ran, then A+B+C arrives — skips A and B and runs C.

**Both directions fail open, and that is the whole failure policy.** An unreadable ledger cannot tell you whether work was done, and the only safe answer to that is the one the engine gave before the table existed: run it. Failing closed would park real work during a database blip — and the seat's own admission gate already refuses new turns within one heartbeat of a store it cannot reach. The write happens *after* the side effects shipped, so failing to record them cannot un-ship them.

Two exemptions, both deliberate:

- **A2A triggers are not recorded.** `_handle_a2a` drains the channel destructively and the bus is process-local until A2A moves onto the durable queue, so a re-run finds an empty channel whatever the ledger says. Recording those completions would imply a choice neither branch can honour.
- **A suspended sandbox turn IS recorded, at the suspend.** Past that point the pending run's own at-most-once flip is the authority for the rest of the work, and the trigger itself is finished with.

A short-circuited trigger publishes `TurnTriggerSkipped`. Without it, "the agent never answered" and "the agent already answered, on a node that has since died" are the same observation.

## The unowned seat

A seat is unowned during a lease gap, a claim ramp, a rebalance, or a full fleet restart. Its mail must survive all of them.

It does, because the **durable subscription** is what retains messages, and the subscription exists whether or not anything is attached to it:

- Every seat's subscription is created at boot, behind the `worker:seat-subscriptions` singleton lease, at the **earliest** message. Behind a singleton because creating one by *subscribing* joins a Shared subscription a peer may be actively serving and takes a share of that seat's live traffic into the joining process — manufacturing the very state this design exists to prevent. Creation runs over the broker's admin API instead, which needs no consumer.
- Detach is non-destructive: the subscription and its cursor survive, so unacked messages return to whoever attaches next.
- Deleting one is explicit (`delete_subscription`) and reserved for a decommissioned role, whose inbox must not accumulate undeliverable events forever.

> **The broker's reapers will delete an unowned seat's subscription.**
>
> Two Pulsar reapers are enabled by default and both destroy the thing this invariant depends on. `brokerDeleteInactiveTopicsEnabled` removes an idle topic outright; `subscriptionExpirationTimeMinutes` deletes a subscription whose last-active time is older than the threshold.
>
> Under owner-only attachment a seat's subscription has **no connected consumer for as long as the seat is unowned**, so both settings become lossy. Set subscription expiry off (or far above any credible unowned window) and inactive-topic deletion off, or to `delete_when_subscriptions_caught_up`. The repo's `docker-compose.yml` ships the correct values; an operator who upgrades the engine without changing the broker gets a fleet that quietly loses a quiet seat's mail, and nothing in the engine can detect it.

## Sandbox control is owner-routed

A detached coding run outlives the node that started it, so its completion has to reach whichever node owns the seat *now*. Each seat has a control topic, `crewlet.agent.{handle}.control`, attached and detached alongside the inbox — so routing emerges from who subscribes, exactly as it does for the inbox, rather than from any "which node" computation.

It cannot ride the inbox itself: while a seat is `AWAITING_SANDBOX` the inbox is paused, and a completion riding it would queue behind the very pause it exists to lift.

`pending_sandbox_run` carries `owner` and `owner_epoch`, so a run is recovered by the node that owns the seat, under that node's epoch, as a step inside `on_acquire`.

## Routing is org-derived, never instance-derived

Agents exist only on the seat's owner, so **any code that resolves a recipient through the local agent pool is broken in a fleet**. A fleet-wide consumer group hands a delivery to an arbitrary node; that node looks the recipient up in its own pool, finds nothing, and drops it — `(N−1)/N` of the time.

Routing needs only `handle → (inbox topic, agent id)`, and both are derivable from the org every node has in full:

```python
from crewlet.queue.topics import agent_inbox_topic

topic = agent_inbox_topic(handle)
seat = org.agent_seat_by_handle(handle)
agent_id = org.agent_id_for(seat)      # uuid5 over (org name, handle)
```

The live `AgentInstance` is an *execution* detail. It must never be a *routing* one. This applies to extensions too — see [Extensions § The agent pool is per-node](../guides/extensions.md#the-agent-pool-is-per-node).

## Singleton duties

Some work belongs to the company rather than to a seat. Running it on every node is not merely wasteful, it races — N reapers deciding independently to expire the same paused sandbox, N clustering passes writing N sets of near-identical auto-drafted skills.

Each sits behind a `worker:{duty}` lease, **claimed per tick rather than held**, so a node that dies mid-duty releases it by lapsing and a peer picks it up on its next tick with no handoff protocol. There are six:

| Duty | What it does | Why once |
|---|---|---|
| `seat-subscriptions` | Creates every seat's inbox and control subscription at boot | Only needs doing once per company; no reason for every node to walk every seat at every boot |
| `sandbox-waiter` | Polls live sandbox boxes, keeps them alive, reaps expired pauses | Each poll is a reconnect, so N nodes means N reconnects per box per tick — and N racing reapers |
| `scheduler` | Evaluates every schedule and fires what is due | The `scheduled_runs` claim already makes a fire at-most-once, so peers are not *wrong* — they lose the race on every fire, having walked the whole org to get there |
| `skill-clustering` | Synthesises skills from episodes | Reads every agent's episodes and **writes** skills: N nodes produce N sets of near-identical pages and N× the LLM spend |
| `skill-curator` | Transitions skills active → stale → archived | Publishes a lifecycle event per transition, and races its own optimistic-concurrency guard |
| `maintenance` | Retention sweeps for `webhook_deliveries`, `rate_limits`, `scheduled_runs`, `turn_completions` | Idempotent range deletes, so peers are harmless — just N times the write amplification and vacuum churn |

Without a placement host — the single-node case — the answer is always yes: there is no fleet to be a singleton within. A duty claim that *fails* (an unreachable lease store) skips the tick rather than proceeding: unknown ownership is not ownership, and assuming otherwise is how every node decides it is the singleton at once.

**Not everything periodic is a duty.** The tool-skill boot walk looks like one and is not: it populates a *process-local* registry, so every node has to run it or its agents have no tool skills at all. The test is whether the work produces shared state (a duty) or warms a local cache (not one). The episode-lifecycle worker is a third shape — it consumes a fleet-wide subscription, so the broker already delivers each request to exactly one node and a lease would add nothing.

## Mixed-version fleets

A rolling upgrade puts a vN and a vN+1 node on the same lease table and the same topics at the same time. That is fine as long as both agree on *what holding a lease means*, and catastrophic when they do not.

The rule is asymmetric: **a node refuses to claim anything while a live lease is held at a lower protocol version.** Older nodes keep working (they cannot know about a check that postdates them); newer ones wait, visibly, until the last old lease lapses. A rolling deploy converges because that is what a rolling deploy does.

Two consequences worth stating plainly:

- **Lease schema evolution is additive-only.** A column the older build does not select is invisible to it; one it *requires* is a crash.
- **A downgrade across a protocol bump needs a full drain.** An older build has no protocol check at all, so it will happily take over a newer node's expired leases. Stop the whole fleet before rolling back.
- **The wait is an outage window, and it is the point.** New nodes claim *nothing* until the last old lease lapses or is released — at the shipped 45-second TTL plus however long the old nodes take to drain. Plan the rollout for it rather than being surprised by it: the alternative is two builds disagreeing about what a lease obliges them to do, which is silent and unbounded rather than visible and finite.

The current protocol is **2**. It moved for the completion ledger: after it, holding a seat lease means consulting and settling `turn_completions`, and a v1 node cannot — it takes a seat over, never reads the completion row, and re-runs a turn whose effects already shipped.

## What ownership looks like from outside

`GET /health` reports a `seats` block per node: seats held, the computed capacity, the live node count, the last claim, the last loss, and the protocol floor when an older peer is blocking claims. The `inbox_attached` / `inbox_detached` log lines carry the seat, the epoch and the elapsed milliseconds.

`unproven_handles` is the number to watch: each one is a seat this process may still be consuming while holding a lease no peer can take.

## Single node

Everything above runs unchanged on one node — it is the degenerate case, not a second code path. With no database configured the leases live in memory, which means the process believes it owns the whole company. That is correct for one node and catastrophic for two, so the engine says so at boot:

```
seat_placement_is_process_local  node=node-0
  hint=no database configured, so seat leases are held in this process only…
```

Configure `providers.database.dsn` to run a fleet.

---

## See also

- [Control Plane](control-plane.md) — how nodes converge on one company config, and the posture a lagging node takes
- [Event System](event-system.md) — topics, groups, inbox batching
- [Code Sandbox](code-sandbox.md) — the detached run whose completion this routes
- [Deployment](../guides/deployment.md) — running more than one node
