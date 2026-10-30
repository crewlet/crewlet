# Control Plane

The **control plane** is how every Crewlet node in a deployment converges on the same company config — and, when one cannot, how it decides what to do about its own traffic.

It is two keys in the fleet's [coordination store](coordination.md) and a poll loop. The interesting part is not delivery; it is the decision a lagging node makes, where the obvious answer turns every successful rollout into an outage.

---

## The problem

Config activation used to be delivered as an event over a **competing-consumer** subscription — group `engine-config` for the engine, `api-config` for the API — with no reconcile loop anywhere.

Competing consumers mean exactly one member of a group receives each message. With one engine process that is invisible. With N, exactly **one** applied any given revision and the other N−1 kept running the previous company indefinitely:

- a deleted role kept answering Slack;
- a rotated credential kept being used;
- a rotated webhook signing secret meant HMAC verification failed on the stale nodes — and verification failure is a *skip plus an ack*, so those nodes silently ate their share of every inbound message;
- and the dashboard reported success, because the one node that did apply it published `revision_applied(ok)`.

Broadcasting the event fixes the fan-out but not the reliability. An ephemeral broadcast consumer starts at the latest message, so anything published while a node reconnects is gone and there is no cursor to replay from. A node that misses one is stale forever, which is the same bug with a smaller window.

## The design

```mermaid
flowchart TD
    ACT["activation<br/>(PUT /config, revert,<br/>crewlet config import)"]
    DB[("company_config<br/><b>each node's own copy</b>")]
    PTR[("coordination: <code>activation</code><br/><b>pointer + payload, revision = epoch</b>")]
    NUDGE(["broadcast nudge<br/>revision_activated"])
    N1["node-0<br/>reconcile poll"]
    N2["node-1<br/>reconcile poll"]
    STATUS[("coordination: status bucket<br/><b>one key per node</b>")]
    ACT --> DB
    ACT -->|pointer and payload, one write| PTR
    ACT -.->|best effort| NUDGE
    NUDGE -.->|wake early| N1
    NUDGE -.->|wake early| N2
    PTR --> N1
    PTR --> N2
    N1 -.->|adopt on first sight| DB
    N1 --> STATUS
    N2 --> STATUS
    STATUS --> N1
    STATUS --> N2
```

**The `activation` key is the authoritative pointer**, and it lives in the coordination store rather than in a node's own database. The split is the whole point: *which revision is current* is a question the fleet has to agree on, and a pointer each node reads out of its own file is a fleet of one.

**The payload travels with it — inside it.** A revision is written to the database of whichever node served the write, so every *other* node meets it for the first time when the pointer names it, and a peer with no copy has nothing to apply. So `Activate` writes the sealed body **in the pointer's own record**, one write that either lands whole or does not land. The body used to be a key of its own, written just before the pointer, and the order was chosen so that a crash between the two left a body nothing pointed at. What the order could not stop was a second writer. Two nodes activating at once each wrote a body to the one key, and the flip that landed first was then pointed at a body the other had overwritten, so every peer answered "no such revision" for the epoch the fleet was on until somebody activated again. Two nodes booting together each publish the revision they hold, so they do exactly this, and so did an edit that *lost* its compare-and-set, because its body had already landed before its flip was refused. One record cannot come apart. The price is the body's bytes on every read of the pointer, which a reconcile tick makes; a company document is kilobytes.

**On the wire the body is JSON.** It is carried as the object it is: the envelope `{"__encrypted__":"enc:v1:<key id>:<ciphertext>"}` sits as the record's `payload` field, compacted and otherwise byte for byte what was sealed. It used to be carried as bytes, which the record spelled as a base64 string over an envelope whose ciphertext was base64 already, so every body was encoded twice and a third larger for nothing. Anything that is not a JSON object is refused before it is written, because a pointer naming a body no node can decode is a fleet that converges on nothing.

Only the **current** revision's body is kept there: in the pointer, and in its mirror beside it. A node that has fallen behind needs exactly the revision the pointer names and never an older one, so a per-revision history in a bucket with no retention would be unbounded growth for rows nothing would ever read. A node that fetches a revision **adopts** it into its own `company_config` — which is where its history, its diffs and its revert targets are read from, so a node that applied without adopting would serve an epoch its own operator surface cannot show.

**So does the revision's author.** Every revision records who wrote it (`created_by`), **what** that is (`created_by_kind`, one of `agent`, `human`, `operator` and `system`) and the credential it came through (`operator_id`), exactly as every other write in the engine names its caller (`iam.ActorFor`): a person writes as the human seat they hold — every person holds one — and so does a token the identity directory binds to a seat, kind `human`; anybody else on `/config` or `/setup` writes under their own login — `token:<id>` for a Tier A token, `svc:<name>` for a service account bound to no seat — kind `operator`, as does the login running an offline `crewlet config import` or `rekey`; and the engine's own write is kind `system` with no credential — a node seeding the store from its `-company` file (recorded under the node's id), or the reconcile loop reloading after it sealed a credential (recorded as `reconcile loop`). A disconnect the loop carries out is recorded under whoever **asked** for it rather than as the loop's own, because the loop only carries it out (see [Integration Reconcile](integration-reconcile.md)). The kind is stated by the writer, because a label cannot say it: nothing stops a Tier A token being given the id `node`. The pointer carries the revision's **origin** — author, kind, credential, source and the instant it was written — so a node adopting the revision records the same author as the node that stored it first, and the history answers the same whichever node serves it. Before it did, every other node recorded an adopted revision as written by `peer`, from `fleet`, at the moment it was activated, and the history answered differently depending on which node served it.

The origin is **required**: the coordination store refuses an activation whose origin leaves out the author's kind, the source or the creation instant, so no pointer reaches a peer naming nobody — and a node adopting a revision refuses one that arrives incomplete rather than recording an author it cannot name. A node adopting a revision records it active from the pointer's own instant, not from when the revision was written — the two differ on every re-activation of an older revision. An author a node already knows is never overwritten, because the node that stored its own write is the authority on it: the first record of a revision is kept. The revision's *parent* is deliberately not carried: it names a revision the adopting node may never have held.

**A node's own active revision is a claim, so only the fleet makes it.** It is what the node's `GET /config` serves, what the node boots on, and what the node offers the fleet at its next start whenever it is newer than the pointer. So a write through the API stores its revision in the history first and marks it the node's active revision only after the pointer has moved to it: a write that loses the compare-and-set stays history, and is never served or republished by the node that took it. And once a node has applied the fleet's epoch, its reconciler keeps the fleet's revision as the node's active one on every tick, correcting a copy that disagrees, whether a local activation failed after the fleet took the write or an offline import had already been superseded when the node started.

**The body is authenticated by its seal.** It is sealed under the fleet's Tier A keyring — which every node holds — so the coordination store holds ciphertext exactly as the node's database does, and a node opens it with the keyring it was deployed with. The seal is AES-GCM, so opening it is also the proof that a node of this fleet wrote it: the broker's admission vouches for a connection, never a value — an embedded fleet's route credential crosses its network in clear, and an external cluster admits whom its operator chooses — and anything the broker admits can write this bucket. A body that does not open — **stored unsealed**, or sealed under a key this node does not hold — is refused before it is kept: the apply is recorded as an error, the epoch is not reached, and the body never enters the node's own `company_config`, where it would be shown, diffed and offered as a revert target. The recorded error says what fixes it, which depends on where the bytes came from. Every node seals what it publishes, so a body that arrived unsealed could have been forged by anything that reaches the broker. A node whose **own** copy of the fleet's revision is unsealed is told to import the document again, which stores it sealed, and a body sealed under a key the node does not hold means its `secrets.keys` is missing a key the fleet seals under. What the seal does not authenticate is the pointer beside it: a party that can write the bucket can still point the fleet at a revision it has already seen sealed, which is why the broker must be reachable only by the fleet (see [Deployment § Every node embeds a member of one cluster](../guides/deployment.md#every-node-embeds-a-member-of-one-cluster)).

**Its revision is the epoch.** The coordination store assigns every key write a monotonic revision, so publishing the pointer appends and flips in a single write — there is no instant where a node can read an epoch whose target has not been published, and two operators activating at once get two different epochs rather than racing over a counter the engine keeps. It also gives the counter the property a plain revision-id pointer could never have: it moves on every activation *including re-activation of an unchanged revision*, which is the documented gesture for picking up a rotated credential (see [Secret Store § Propagation](secret-store.md#propagation)).

**Every activation carries a later instant than the one it replaces.** The
pointer records the instant a revision was activated, and every row a
configuration derives — a tracker project, a knowledge container, a declared
repository — is stamped with it and refuses an older stamp (a repository
through the code log's one
[activation fence](code-host.md#the-reconcile-and-the-activation-fence)
rather than a stamp of its own), so an older configuration applied late
cannot walk a newer one back. That guard needs a later activation to carry a
later instant, and the instant is the activating node's own clock; so inside
the same compare-and-set that replaces the pointer, an activation whose
instant is no later, to the millisecond, than the pointer's is published at
one millisecond after it, and the fleet's history of activations is in the
order the fleet made them whichever node's clock ran behind. Even an
unconditional publish is a compare-and-set underneath for this reason.
`Activate` returns the instant it published, and a node keeps that instant on
its own copy of the revision (`activated_at`), because it is what the node
boots its company with next time.

The pointer's bucket has **no retention at all**. Everything else the fleet shares ages out; a pointer that expired would restart the epoch, and a fencing sequence that restarts is not a fence.

> **On an embedded broker the coordination store lives inside the running engine**, so an *offline* `crewlet config import` — one run while the engine is stopped — can mark a revision active locally but cannot move the pointer; it says so, and the node publishes it at its next start. Run against a **running** node the same command goes through that node's `PUT /config` instead, which moves the pointer at once. A node that starts holding an active revision the fleet has no pointer for publishes it, unless the pointer it finds is newer; a restarted single-node deployment therefore comes back pointing at what it was already serving, and a node rejoining a live fleet converges on the fleet rather than rolling it back.

**Every node polls it** every ~15 s (±20 % jitter). A poll cannot miss anything, because it asks. The jitter exists only to break lock-step after a synchronized fleet restart — a rolling deploy boots every pod within the same second — and is deliberately applied to the *interval*, never to the apply.

**The `revision_activated` event survives as a nudge.** It wakes the loop so an operator's change lands in milliseconds instead of seconds. It is delivered as an **ephemeral broadcast**, never a consumer group: every node has to hear every activation, and a competing group would hand each one to exactly one node — the delivery shape that made config a fleet of one before the pointer existed.

It is also deliberately thin. The event carries the revision id and its summary and nothing a node acts on: the woken loop re-reads the *pointer*. That is what makes losing a nudge cost one poll interval and never a revision, and it is why a node that cannot subscribe at all — an attach failure is logged, not fatal — simply converges on its interval like any other. After a nudge fires, the next iteration still waits a full jittered interval, so an activation storm cannot become an apply storm.

**The status bucket is what each node managed to do** — one key per node, last-write-wins. This is what makes partial apply visible, and it has three outcomes rather than two:

| Status | Meaning |
|---|---|
| `ok` | Applied cleanly. |
| `error` | Refused. Nothing was rolled back because nothing was mutated: the build comes first and touches nothing, so a revision that cannot be built leaves the previous epoch current and still correct — a legitimate degraded-but-correct state, and one work can safely route to. |
| `degraded` | Failed **after** a subsystem that cannot be un-applied was mutated, so this node's declared epoch would not be the whole truth — it would report the prior config while its tool surface was amputated. Never counted as converged, and never counted as somewhere work can go. **Not reachable in this build**, and the status is documented rather than quietly dropped because the ordering that keeps it unreachable is a live constraint: everything an apply cannot undo has to stay behind the epoch swap. See [`Engine.Apply`](configuration.md#the-engine-half). |

Each node **re-stamps its key every tick**, not only when it converges, and the posture decision only counts reports written in the last four intervals (~60 s). Both halves are needed together. The bucket is keyed by node rather than by event, so a node that is scaled in, redeployed or crashed would otherwise leave its last `ok` behind forever, and a surviving node that cannot apply the current epoch would read that ghost as "there is a healthy peer to shed to" and step out of rotation to hand work to a process that no longer exists. Bounding on freshness fixes that, but only if a live node keeps writing: a converged node that reported once and went quiet would age out of its own fleet's view, and a lagging peer would read `PeersOK` as 0 off a perfectly healthy fleet. One idempotent write per node per tick is what makes a key mean *"alive, at this epoch"* rather than *"was alive, once"*.

**The bucket's own age is that bound**, set to four reconcile intervals when the store is opened. Nothing sweeps it, because there is nothing to sweep: a node that stops reporting stops renewing, and the broker expires the key on its own. That is also why the value is a bucket-wide constant rather than a per-write TTL — see [Coordination § Retention is a bucket's age](coordination.md#retention-is-a-buckets-age).

A node's **coordination record** of a failure is truncated at 2 000 bytes (not
characters — the cut is applied to bytes, on a rune boundary). That record is
re-read by every peer on every posture decision and rendered on the dashboard's
**Settings › Nodes** screen, so one node returning a megabyte of Go error would be paid for
by every reader on every tick.

The **`config_revision_applied` event** carries up to 64 KiB of it — thirty
times more, and marked when it cuts. It is written once and kept for the event
store's retention horizon, so it is the copy an operator reads days later to
find out why a revision did not apply, and a 2 000-byte cut removes exactly the
end of a wrapped chain where the cause sits. It is bounded at all only so the
event can be published: one over the queue's payload ceiling is refused and
dropped, which would cost the operator the whole record rather than its tail.

---

## Posture: what a lagging node does

Reading those two tables together is what lets a node distinguish *"I am behind because propagation takes a moment"* from *"I am behind because I cannot apply this"* — which need opposite responses.

```mermaid
flowchart TD
    START{"applied ≥ target?"}
    CONF{"lag <b>confirmed</b>?<br/>(own failed attempt, or<br/>behind &gt; 45 s)"}
    PEERS{"any peer<br/>applied it?"}
    ATT{"attempts<br/>exhausted?"}
    ANY{"any peer reported,<br/>or did <i>we</i> fail?"}
    SERVE["<b>SERVE</b><br/>take work"]
    WAIT["<b>WAIT</b><br/>keep serving"]
    SHED["<b>SHED</b><br/>refuse new work"]
    STUCK["<b>STUCK</b><br/>stop retrying, fail /ready"]
    ISO["<b>ISOLATED</b><br/>keep serving, alarm"]
    START -->|yes| SERVE
    START -->|no| CONF
    CONF -->|no| WAIT
    CONF -->|yes| PEERS
    PEERS -->|yes| ATT
    ATT -->|yes| STUCK
    ATT -->|no| SHED
    PEERS -->|no| ANY
    ANY -->|yes| ISO
    ANY -->|no| WAIT

    STUCK:::danger
    ISO:::warning
    classDef danger stroke:#ef4444
    classDef warning stroke:#f59e0b
    linkStyle 5 stroke:#ef4444,color:#ef4444
    linkStyle 8 stroke:#f59e0b,color:#f59e0b
```

The rule that matters, and the one an obvious design gets backwards:

> The target is the store's activation pointer, but **lag alone is not a reason to shed.**

Every successful rollout produces lag. The first node to apply advances the pointer, and every peer is behind until it polls. A node that sheds on that makes the fastest node the cause of a fleet-wide outage — and the faster it is, the longer everyone else is down.

So lag has to be **confirmed** before it means anything: either an attempt of this node's at that epoch concluded in failure, or the lag outlasted what propagation could explain (three poll intervals, 45 s — comfortably longer than a poll plus a normal apply, short enough that a genuinely stuck node leaves rotation quickly).

**An attempt still running is neither.** A node in the middle of applying the epoch is doing exactly what propagation means, so it reports `wait` for the whole of the apply, its first company on an unconfigured node included, and moves to `serve` the moment the apply succeeds. An attempt counts toward the retry bound, and its failure counts as evidence, only when it **concludes**. Counted when it started, the attempt in flight read as a failed one: for the length of every successful apply a lone node reported `isolated`, a node beside current peers `shed`, and a node on its last retry `stuck`, so a client polling `/health` across a config write could not tell the transient from a revision that genuinely does not apply. A failed attempt stays the node's evidence while a retry runs, so a node that is shedding does not flap back into rotation for the length of each retry.

The 45 s is measured **in time**, from the moment the node first saw *that* epoch, rather than in reconcile ticks: the case it exists for is an apply that hangs, and a hung apply holds the very tick that would count it. Past 45 s a node still applying is confirmed lag like any other: `shed` if a peer has the epoch, and still `wait` if none has reported either way, because silence is not evidence.

Both the 45 s and the retry budget start again **with each epoch**, so a node is judged on the epoch the pointer names and on nothing it went through before it. That is what makes the recovery from a bad revision an ordinary apply: a revision failed everywhere, the fleet sat `isolated` on it for ten minutes, and the operator activates a corrected one — every node applying the fix reads `wait`, not `shed`, once the fastest has recorded it, and a node that ran out of retries on the bad revision is not `stuck` on the fix before it has tried it. The one thing that does carry over is an attempt that is **still running**: a node whose apply of an older epoch hangs cannot begin on the newer one, so it is late on the newer one by as long as that apply has held it, and a hang stays confirmed lag however many activations are made while it lasts.

Only then does peer health pick the action. And when *no* peer managed the epoch either, the honest conclusion is that the **revision** is bad rather than this node — so it keeps serving the epoch it already had, which a refused apply leaves untouched, and raises divergence loudly. Shedding there would take the whole fleet down over one bad revision, which is precisely what publishing rather than mutating exists to avoid.

Retry is **bounded** (three attempts, each counted when it fails). Without a bound, a revision that fails on one node only — a missing per-node env var, an MCP binary absent from that image — would re-apply every tick forever, restarting that node's MCP children each time.

Note where exhaustion sits in that chart: **after** peer health, not before it. The bound itself is unconditional — a node stops re-applying at three attempts whatever posture it reports — but `STUCK` is a claim about *this node* being the anomaly, and that claim is only true when the epoch demonstrably applies somewhere else. With no healthy peer there is nowhere for the work to go, so stepping out of rotation is not shedding, it is stopping; and every node in a fleet that cannot apply a revision exhausts its attempts at roughly the same moment, so ranking exhaustion first took the whole company dark about 45 s after a bad activation. A single-node deployment reaches the same place by a shorter path: no peer will ever report anything, so its own failure is the only evidence there is, and it stays `isolated` — serving the config it already had — rather than failing readiness over a revision nothing else in the fleet ever saw.

The budget is **per epoch**, not per process. Activating a fixed revision resets it, so the runbook's answer to a stuck node — push a corrected revision — is one the node actually acts on.

### Where the gate sits

A shedding node refuses work at **trigger admission**, not inside the turn (`Engine.runTurn`). Two reasons, both concrete:

- A stale node still *consumes* inbound messages. Signature verification happens consume-side against that node's cached secret, and a failure is a skip plus an ack, so gating later means the node silently eats its share of the fleet's inbound.
- Refusing inside the turn would permanently wedge a seat whose sandbox run just completed: the pending row is already flipped to `resumed`, the box collected, the seat still marked busy, and nothing reaps a `resumed` row in-process.

Refusal **defers**: the delivery goes straight back to the broker and this node stops consuming — on a seat's inbox and on the ingress topic alike. Never a bare NAK. The two hand the message back the same way; what a deferral adds is quiescing the consumer, and that is the whole difference. A node that NAKed and kept fetching would be handed the same event again a second later, refuse it again, and spend one of its twenty-five deliveries on every lap — so a shed that lasts minutes dead-letters a perfectly healthy event on a node that was never the problem.

And never a republish, which is the form this took first. A shed *releases* this node's seats, and a fenced release republishes nothing (see [Seat Ownership](seat-ownership.md#establishing-a-seat-and-giving-it-back)): a republished event is a **new message**, and the completion ledger's idempotency and the batch layer's aging both key on the identity a NAK preserves — so the copy is not the delivery a successor was entitled to, it is a second one nothing can collapse against the first. Worse, it lands on a subject this node is still attached to at that instant — so if the release that should follow does not happen, the copy comes straight back, is shed again and republished again, at whatever rate the broker will serve. A deferral cannot spin: the consumer stops after the first one, and the seat's release (or, if the posture recovers first, the next successful lease renew) is what starts it again.

The ingress consumer has no seat to release it, so the reconcile tick starts it: on every tick whose posture admits work, the node un-quiesces `crewlet.notifications.inbound`. That is deliberately a **convergence rather than an edge**. The refusal runs on the delivery path while the posture changes on the reconcile loop, so a recovery edge can fire just before the shed's last in-flight delivery quiesces a consumer nothing would then restart — a node that accepts webhooks, reads none of them, and reports a perfectly healthy config. Converging on "if I admit work, I am consuming" cannot lose that race.

Sandbox-driven turns bypass the gate entirely: a completion is dispatched directly by the `sandbox.Coordinator` and never passes through inbox admission. They are the tail of a turn this node already started, and refusing them destroys durable state rather than deferring it.

A shed takes **no topic pause**. Deferring quiesces the one consumer that refused, and the sandbox busy state is a separate count the coordinator keeps from the pending store, so the two cannot release each other: a node converging back to `serve` restarts consumers without un-gating a seat mid-sandbox, and a completing sandbox clears its busy count without restarting a diverged node's consumers.

The **scheduler** is gated too, and differently: a tick on a shedding node is skipped whole rather than fired. A schedule's fire identity is org-derived — its name, cron and target seat — so a stale node would fire the previous company's schedules, and unlike a delivery there is no queued copy to fall back on. The skipped window stays open, so the missed-tick catchup evaluates it once the node converges; anything a peer already fired is absorbed by the fleet's at-most-once fire claim.

The **integration reconcile loop** is gated the same way and for the same reason, and it is the one where the stale document reaches *outside* the deployment. Every reconciler reads the live company config on each pass, so a shedding node would converge a third-party app to the revision the fleet has already replaced: an account a removed seat should no longer hold is kept, a webhook is re-registered at the previous public base, and the status row says `ready`. It declines **before claiming the duty**, so its lease lapses and a peer holding the current revision takes the loop over rather than waiting behind a holder that does nothing, and it logs `integration_reconcile_shed` going in and `integration_reconcile_resumed` coming out.

The **replicated estate** a shed or stuck data node holds goes the other way: the node keeps serving it — tracker, knowledge base, usage, and the code host's reads and pushes — and is asked for it **last**. Its copy is sound, but what it adds to an operation from its own configuration is the revision the fleet replaced: the chart a people field and a wake's lead are resolved by, the unit a read renders, a search's scope, the limits a push is decided under. So every other data node is asked first, and the shed node is asked again, told to answer anyway, only when none of them runs the operation — as a data node whose copy [lags its log](scaling.md#how-a-request-reaches-the-estate) is, and after any such node, whose answer is held to the current configuration. It is never refused: a stale value of that kind is one every reader already meets when a revision changes it, while a refusal would cost a fleet whose only data node failed an apply — a stateless node beside one data node is enough for that, since the stateless one applies what the data node could not — every tracker, knowledge-base and usage operation it makes. Nor does the shed node cost a request a second round trip: it answers `behind_config`, which every asking node remembers for thirty seconds, so an asker spends one refusal on it every thirty seconds and otherwise goes straight to a current data node — or, where none runs the operation, asks the shed node once, told to answer anyway — and a push to a repository the shed node commits for goes to that repository's runner-up. And lag alone never moves a node there, so a rollout costs no extra hop: only the confirmed `shed` and `stuck` postures do — 45 s after the node first reads the new pointer (at the activation's nudge, or at its next poll), or sooner where its own apply fails while a peer has applied, so about a minute after the activation at worst — and the node's next posture decision after it applies moves it back.

**One kind of operation it refuses even asked last: one that judges a grant from its chart.** A seat's read of the code host's rows — the repositories it may list, a repository's detail or history, a ref resolved for it — is decided by what the chart lets that seat read, and the revision the node failed to apply may be the very one that revoked it, so no amount of being asked last makes that answer sound. Such a node answers it `behind_config` however it is asked, told to answer anyway or not, and the read is answered by a current data node or by nobody; a git node answers a seat's read of a repository's content unready the same way, and the read moves on to the next git node, which downloads a copy to answer it where it holds none. Each operation that judges a grant declares it in the estate (`judgesGrant`), and a gate holds every operation whose arguments name an asker to that declaration, so one added later cannot be served from a stale chart by omission. A person's read consults no chart and is served as above. So a fleet whose only data node is stuck answers every seat's code read unavailable until that node's apply succeeds — `/ready` already fails there — while its tracker, knowledge base, usage, and persons' code reads and pushes go on.

The gate stops there rather than being folded into every worker duty, and that is a decision rather than an omission. Posture and `node.roles` are decided by subsystems that do not consult each other — the shed rule counts any peer with a fresh `ok` row as somewhere the work can go, including an ingress-only node that will never claim a singleton. Applied to all of them, an `ingress` + `seats,workers` fleet whose worker node fails a single apply would end with nobody running the scheduler, the retention sweep, the sandbox waiter or the curator, `/ready` green on the node that is fine, and not one log line to say so.

One class of duty waits for more than the posture, and it is narrow on purpose: an **irreversible judgement made against the active revision**. Two exist — the [retirement of a removed seat's mailbox](seat-ownership.md#the-removed-seat), which deletes mail for a seat absent from the revision, and the [purge of a deleted repository](code-host.md#deletion-and-purge), which removes a repository deleted longer than the revision's `code.deleted_retention_days`. Each acts for the whole fleet and neither has an undo, so each is made only on a node that serves the revision the activation pointer names: the pointer read first, this node's applied epoch next, its company last, so the revision judged by can only be the pointer's or newer. A node that is behind — still propagating, refused, or stuck — judges nothing, and **nothing activated is not current** here, unlike the posture: a node boots on a company file or its store's company before its first tick activates anything, and a revision nobody activated is exactly what these must not judge by. That is stronger than the shed gate, and it costs little where the gate would cost a lot: a declined judgement is a retirement a sweep later or a purge one duty tick later, against a grace of a day and a retention of days, and the duty is given back so a node serving the revision takes it. Unlike the gate folded into every duty, it is never silent — the mailbox sweep logs why it judged nothing, and the code-retention duty logs `code_retention_duty_declined` naming this node's epoch and the fleet's, so a fleet whose only worker data node failed an apply says why its purges wait.

**Behind is a fault only when the lag is confirmed**, by the same rule the posture judges by (above): an attempt of this node's at the epoch concluded in failure, or it has been behind for longer than the 45 s of propagation. Every node is behind for a moment at every activation, and at every boot until its reconciler has been built and has ticked once — the code-retention duty's first turn runs before either — and a warning for that would be logged at every deploy and every configuration write, which teaches whoever reads the log to ignore the one a node that really failed an apply raises. So a node merely applying the revision, or still starting, declines **quietly**: the code-retention duty logs `code_retention_duty_declined` at debug with `reason` `propagating` and asks again at the reconcile cadence (15 s) rather than a whole tick later, so a restart or an activation costs a purge seconds rather than a quarter of an hour; the mailbox sweep logs `seat_mailboxes_not_judged` at debug and judges again at its next tick. Only a confirmed lag (`reason` `behind`) and a node that cannot tell at all — the pointer unreadable (`reason` `unknown`) — are warnings. A node with no reconciler yet reads as one whose reconciler has not ticked, as the posture reads it: `crewlet run` builds its reconciler straight after the engine and exits if it cannot, so no running node is without one for longer than its boot.

---

## Rotation

A config revision and the *values* its `${VAR}` references resolve to are two different things, and re-activating an unchanged revision is the documented gesture for picking up a rotated credential. The payload is byte-identical, so an apply that compared payloads would rebuild nothing on exactly the operation an operator performs to make it rebuild: the LLM providers would keep the revoked key, the trackers their old token, and every shared MCP child the credential it captured at spawn.

**Nothing compares the payload.** No step between the operator's gesture and the rebuild looks at a revision's bytes, so there is nothing for a rotation to slip through. The property falls out of the control plane's shape instead:

- The pointer's **KV sequence is the epoch**, and the store assigns a new one on every write. Re-activating a revision therefore mints a new epoch even though the value written is byte-identical.
- A node's reconciler skips on the **epoch it has already applied**, never on payload content — so a re-activation always reaches `Apply`.
- `Apply` **re-reads the secret store first**, before it builds anything, and `${VAR}` references stay verbatim in the stored revision and are resolved where a provider is *constructed*. So the rebuild that follows is against freshly resolved values, without anything having compared them.

**What that rebuild reaches, and what it does not.** A rotation is only useful where something that *captured* the old value is replaced, and an apply does not reach everything that holds one:

| Holder | Rotated by a re-activation? |
|---|---|
| LLM providers | **Yes** — the epoch's providers are constructed from the fresh resolver. |
| Jira / Confluence / GitLab / GitHub | **Yes** — each tracker is reconciled against the new epoch and re-resolves the engine credential. |
| Datadog / Atlassian | **Yes** — nothing holds their credentials between uses. Datadog's webhook token is resolved against the applied company on every delivery, and the [integration pass](integration-reconcile.md) resolves the provisioning keys (Datadog's API and application keys, Atlassian's organization API key) at the start of every pass, so a re-activation reaches the next delivery and the next pass. |
| Shared MCP children | **Yes, selectively** — see below. |
| Per-role MCP children | **Yes, selectively**, on the node holding each seat, just after the apply — see below. They belong to a seat's *lease* rather than to the epoch, but the `seat_tools` step of the [convergence](configuration.md#what-follows-a-published-company) hands every seat a node holds a reconcile of its children over resolved values, so a rotated `mcp_env` value restarts that seat's one child and leaves the rest running. |
| Slack transport | **Yes.** It is rebuilt on every apply (`Engine.reconcileSlack`); what is replaced is an HTTP client and the working-status driver, with no socket to drop. |
| Mattermost transport | **Yes, when a value it is built from moved.** It holds a websocket per seat, so `Engine.reconcileMattermost` rebuilds only when a fingerprint over the resolved URL, team, status and every seat's resolved bot token, username and channel changes, and a rotated bot token is such a change. |
| Native tracker projects | **Nothing to rotate, but they are re-stamped.** The apply stamps each project with the activation's own instant, and a re-activation is a new activation. It therefore records one "org chart re-applied" change per project, once, however many nodes apply it. Re-applying the *same* activation, which is what every restart does, writes nothing. |

The MCP children, shared and per-role, and the Mattermost transport above, are the places a comparison does happen, and it is deliberate: a child is a *process*, and restarting every one on every apply would tear down working servers to arrive back where they started. So `Bridge.Reconcile` compares the spec it is handed against the one the child is already running and leaves an unchanged server alone. What makes that safe for a rotation is *what* it compares: the spec's `env`, `headers` and `url` are resolved at the edge before the comparison, so a moved credential reads as a changed spec and restarts that one child. Comparing the stored config entry, where `${VAR}` stays verbatim, would silently stop rotation from reaching MCP children at all; the tests hold that line by re-applying the same document and asserting which children survive it, and by rotating one seat's credential and asserting its neighbour's child is untouched.

A per-role child is reconciled where its seat is held and only there: every node walks the seats *it* holds, so a rotation reaches each agent's child exactly once in the fleet, and a node never starts a child for a seat whose lease it cannot still prove. A seat the revision removes has all its children retired by that same reconcile, without waiting for placement to release the seat. The same reconcile is what lets a credential [minted by a provisioning pass](integration-reconcile.md) after a seat was claimed reach that seat's child — the pass re-activates the revision once it has sealed — and an apply that lands while the seat is still being claimed is folded into the claim, which starts the children from the snapshot that apply installed before the seat's mailbox opens. The apply itself does not wait for any of this: it refiles each held seat's tool catalogue and returns, and each seat restarts its own children just after, because a start can take a whole startup timeout and every stage after this one would wait behind it with the new epoch already current. A restart stops the old child before it starts the new one, so a turn that starts in that moment still lists the old child's tools and a call to one fails as [What a running turn sees](#what-a-running-turn-sees) describes; the next turn gets the new child.

That is the whole of the comparison, and it is over resolved values rather than a digest of them. Nothing keeps a digest of live credentials across applies, which is what a broader selective rebuild would need and what would turn a rotation into a leak the moment such a digest reached a log line or a row.

> One more surface, and it is out of the engine's hands entirely rather than merely off the apply path: a **running code sandbox** received its credentials in the box's environment at launch, and no engine-side refresh reaches a live box. There the bound is the run's duration plus any clarification pause, not seconds. Tear the run down if a rotation is a revocation.

---

## What runs before the first revision

A node started with no active revision is not an idle one. On every node that holds the estate — every `data` node — its **core runtime** is started at boot, company or not: every domain's state log, the node gate over every identity-claiming log, the [identity estate](identity-and-access.md) with the triggers that keep this node's view of it, and — on a node that publishes — the log's trim and the identity duties. That is what lets a Tier A token's session sign in on a node that has nothing else yet and create the company through it — and the company's first person be invited or created and sign in once that company declares the human seat they will hold, since a person is only ever put onto a seat. `ingress` requires `data`, so every node that signs anybody in reads its own identity rows. A node that holds no data runs no core at all: it applies no log, and reaches the estate — the identity directory included — through a node that holds it ([A node that holds no data](scaling.md#a-node-that-holds-no-data)).

What waits for the first revision is what only a company can say anything about: whether the engine keeps its own tracker and knowledge base at all, and so the writers and readers over those two logs, their lexical index, their change feeds and the embedding duty. The apply's `native` stage brings them up under the API already serving, and the surfaces over them — `/work`, `/pages`, `/operator/mcp` and the socket's work questions — read them per request, so they answer `503 no_active_revision` until then and serve from then on with no restart. On a data node the logs themselves have been applied all along: a join replaces the whole replicated file and a snapshot names every registered domain, so a node applying part of the register could neither adopt nor donate, and the trim, which counts nodes per log, would be pinned by it.

A node's seats are admitted once its state logs are hydrated: on a data node, once every core log is established, the identity log included, and the router answers for the tracker and the knowledge base; on a node that holds no data, once a data node answers for them. So a data node waiting for its first company is already caught up the moment it gets one. At a stop the order is the reverse of what depends on what: the directory's view trigger first — a registry rebuild must not outlive what it serves — then the fleet duties, the native half's embedding writer before the core's trim and identity duties because it publishes into a log the trim is deciding how far to purge, and every duty before the node gives its duty leases back; then the native halves — their change feeds, lexical index and search answerer, each of which reads what the logs write — and the logs last.

---

## What a running turn sees

**Nothing moves under a running turn, because nothing is mutated in place.** An epoch is published rather than edited, so the question is only ever *which* epoch a turn is reading — and a turn answers that once. `runTurn` pins the company in a local at the top and builds everything from that one value: the runner, the round caps, the prefetch, the telemetry. Two reads could straddle a publish, and a turn that built its runner from one revision and took its round caps from the next would be running a company that never existed. That is the failure publishing-instead-of-mutating exists to remove, and the pin is what collects the benefit.

The prompt is frozen harder still: it is **rendered to strings before the runner is built**, so the runner has nowhere to re-fetch from and a `self_iterate` loop cannot move the system prompt underneath the executor between rounds.

**What a pin cannot hold is a capability.** It holds a *catalogue* — the tool objects the epoch's registry names — and an MCP tool object holds the server it dispatches to. If the apply replaced that server (its spec moved, or the revision dropped it), the server behind a pinned tool has been stopped, and the call comes back as a failed result the model can read, classed `unavailable` and saying the server is no longer running on this node, rather than as a name that vanished mid-turn or a panic. A model that sees a failed tool result can say so; one whose tool disappeared cannot. A server that merely went down and came back is a different case: the tool object survives the restart and the next call reaches the new process (see [Tools & MCP](../guides/tools-and-mcp.md#a-server-that-dies-or-will-not-start)).

That is the whole exposure, and it is small enough that **the apply does not wait for in-flight turns at all** — there is no drain, and no seat is quiesced before an epoch is published. It stays small because of what the apply restarts: only a server — shared, or one seat's own, which the [convergence](configuration.md#what-follows-a-published-company) reconciles just after the apply — whose resolved spec actually moved, so the common config change restarts nothing a turn is holding.

---

## Operator surface

`GET /health` stays `200` through everything once the node serves — an orchestrator watching liveness must not SIGKILL a node that is finishing in-flight turns — and reports what the node concluded. (Before it serves, a starting node answers a shorter body, `503` only while it recovers what it holds: see [While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting). No posture exists yet to report.)

```json
{
  "status": "shed",
  "node": "node-2",
  "configured": true,
  "in_flight": 3,
  "shutting_down": false,
  "posture": "shed",
  "applied_epoch": 40
}
```

`GET /ready` is what steers traffic, and it fails on `shed` and `stuck` only:

| Posture | `/ready` | Why |
|---|---|---|
| `serve` | 200 | Converged. |
| `wait` | 200 | Ordinary propagation during a rollout, this node's own apply included while it runs. Failing here is the fleet-wide-outage bug. |
| `isolated` | 200 | *No* node applied the revision — taking this one out would take the fleet out over one bad revision. |
| `shed` | 503 | Confirmed: cannot apply an epoch its peers have. |
| `stuck` | 503 | Retries exhausted: the last attempt has failed, not merely started. Needs an operator. |

That is an ingress node's `/ready`. A node without the `ingress` role takes no traffic, so its `/ready` asks whether it is doing its work — the same postures fail it, beside a broken broker link, a lapsed presence lease and a withheld seat admission; see [Probes on a node without ingress](../reference/api-endpoints.md#probes-on-a-node-without-ingress).

Both probes say *why*, because "draining" and "cannot apply epoch 41" call for opposite responses. `/health` carries the posture itself, and `/ready` names what took the node out of rotation in `reason`: `draining`, `unconfigured`, `shed` or `stuck` — or, on a node whose own copy of the estate is out of service and whose view of the fleet has been unreadable for over a minute, `estate_behind`, which is not a posture at all (see [the routes table](../reference/api-endpoints.md#routes)) — and, before the node serves at all, `starting`. A drain outranks a posture in both, because it is the operator's own action.

### Reading a stuck node

Each node's status carries the `error` it failed with, so the first question — *is this the revision or is this the node?* — is answered by the **Settings › Nodes** screen, which calls out every node whose applied epoch is behind the target and prints the error it failed with. It is also one request:

```bash
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  http://localhost:8000/query/fleet | jq '.nodes[] |
    {id, config_epoch, config_status, config_error, config_reported_at}'
```

A node that stopped reporting **drops out of this list** once its status ages past the freshness bound — which is the same fact the posture decision reads, so what an operator sees and what the fleet concluded cannot disagree.

One node `error` while peers are `ok` is a per-node problem: a missing env var, an image without some MCP binary. Every node `error` on the same epoch is the revision — revert it. Any node `degraded` needs a **restart** of that process specifically: the status exists precisely for a failure past something no later apply can put back, so nothing short of a restart will. No build reports it today.

### After the fact

The fleet view above is a **live** view and deliberately forgetful: a node that stops reporting drops out of it within a minute, which is exactly the node — the one that crashed, or was scaled in mid-rollout — an incident review is looking for.

The durable answer is the event log. Every apply publishes `config_revision_applied`, with the reporting node in the event's `source`:

```bash
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  "http://localhost:8000/query/events?type=config_revision_applied&limit=50" |
  jq '.events[] | {id, source, timestamp, failed, summary}'
```

The `summary` reads as a failure for every status other than `ok` — `degraded` included, because a node that could not finish an apply has not converged and a line that hedged would let the fleet look healthier than it is.

A listing never carries payloads (see [API § An event on the wire](../reference/api-endpoints.md#an-event-on-the-wire)), so fetch the one you want by id for the detail:

```bash
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  http://localhost:8000/events/$EVENT_ID | jq '.payload'
```

```json
{
  "revision_id": "cfg-…",
  "status": "error",
  "error": "engine: apply: …",
  "applied_subsystems": ["secrets", "company"]
}
```

`applied_subsystems` is the ordered list of what this node had already rebuilt when it stopped, out of `secrets`, `company`, `native`, `sandbox_runtime`, `tools`, `learning`, `sandbox`, `integrations`, `epoch`, `maintenance`, `learning_passes`, and then the [convergence](configuration.md#what-follows-a-published-company) every published company runs — `parties`, `seat_identities`, `seat_tools`, `tracker_projects`, `knowledge_containers`, `mailboxes`, `scheduler`, `published` (the example above refused the revision's `providers.sandbox`, whose catalogue is built straight after the company and before anything else on the node moves). That is the difference between "refused before anything changed" and "torn down halfway", which is precisely what decides whether the node needs a restart. An `error` with **no** `applied_subsystems` — the field is absent rather than an empty array — means nothing on this node changed, and it still serves the previous epoch. There is one way to get one: the revision never reached the apply, because its body is in no store this node can reach, it could not be fetched, read, opened or parsed, or it is not a runnable company (`engine: store: no such config revision: …` — the activation pointer names a revision neither this node's database nor the coordination store holds — `engine: fetch revision …`, `engine: read revision …`, one of the three refusals of a body that does not open under this node's keyring, which each name the revision and the remedy, `engine: parse revision …`, `engine: revision … is not a runnable company`) — a storage, transport or authoring fault, and the `error` text says which. Every apply that starts reports at least `secrets`, because re-reading the secret store is its first stage (see [The engine half](configuration.md#the-engine-half)), and nothing about the revision is checked against this node's Tier A before it: a node whose stream would not survive a restart is refused when it starts, so it never reaches an apply to refuse.

Like every other event this lives in each node's own log, so a node whose disk is gone took its rows with it — but a node that merely stopped reporting, or was replaced, still has them.

### What an apply warns about

An applied revision can be valid and still worth a line. Each node logs these **once for every epoch it applies**, and only when the apply succeeds — a refused revision is retried, and would otherwise repeat them for an epoch the node is not serving:

- `org_admission_warning`, once for every [admission rule](configuration.md#what-a-stored-revision-is-held-to) the revision breaks;
- `org_dangling_reference`, once for every reference it resolves to nothing ([Dangling references](organization-model.md#dangling-references));
- `config_tier_warning`, once for every consequence of the revision beside **this node's own** Tier A — a company hosting code on a fleet's default object store, a git node whose cache budget cannot hold a repository at its size limit, a git node behind a disabled guard ([Warnings across the two tiers](configuration.md#warnings-across-the-two-tiers)). Each names the `path` of the field that answers it, the `revision` and the `epoch`. The cache budget compared is the one this node resolved — derived from its cache's volume where `git.cache_max_bytes` is unset — which neither document holds. Because one of the two documents is the node's own, the same revision can warn on a git node and say nothing on a node that serves no git.

A node booting on a company says these at its reconciler's first tick, which applies the very activation it booted with — not at boot as well, which said each of them twice.

### A native backend changes at a restart

Each node builds its native runtime once — the engine's own tracker and its knowledge base (and, with the code host, `code.native`) — from the company that started it, and it follows the fleet's logs rather than a revision. A revision that turns one of them on or off (`tracker.backend` or `knowledge.backend` moving to or from `native`, or `code.native` changing) activates normally and reports `ok`, and the node keeps serving what it was built with until it restarts: a tracker turned off is still served, and a knowledge base or a code host turned on is not served yet. The node says so once per activation with a `native_backend_needs_restart` warning naming the `field`, what it is `running` and what the revision `activated`:

```json
{
  "time": "2026-03-12T10:30:00.412Z",
  "level": "WARN",
  "msg": "native_backend_needs_restart",
  "component": "engine",
  "field": "tracker.backend",
  "running": "native",
  "activated": "jira",
  "detail": "this node's native runtime was built with the engine's own tracker (tracker.backend: native) and keeps serving it until the process restarts; restart each node (one at a time is fine) to serve tracker.backend: jira"
}
```

Restart each node (a rolling restart is fine) to serve the new choice. Moving between two vendors — `jira` to `none`, say — is not such a switch and takes effect at the apply, because a vendor's half is rebuilt by its own reconciler; and a node that runs no native runtime at all brings one up at the first apply that names a native backend — `code.native` among them — so it has nothing to restart for. A re-activation of a revision that still asks for the switch says it again.

A restart serves the switch because a node boots on the revision its own store holds — never on a stale `-company` file, which is ignored once a company exists ([Two equivalent bootstrap entry points](configuration.md#two-equivalent-bootstrap-entry-points)) — so the first apply after it says nothing. The one exception is a node that did not hold the switch when it restarted: one that was **down while it was activated**, or a data node **joining** the fleet with an empty store. Before its engine is up a node cannot read the fleet's pointer, so it boots on the company it had (or on its file), applies the switch at its first reconcile, says the line once more, and serves the switch after one more restart — by which point its store holds that revision.

---

## See also

- [Coordination](coordination.md) — the shared store this plane's two keys live in, and what else does
- [Scaling Out](scaling.md) — the model this sits inside, and the other four kinds of coupling a fleet had to resolve
- [Configuration](configuration.md) — the two-tier split, and the apply itself stage by stage
- [Secret Store](secret-store.md) — where rotated credentials live and how re-activation picks them up
- [Deployment](../guides/deployment.md) — running more than one node
- [Event System](event-system.md) — subscription types and why a competing group was the wrong one here
