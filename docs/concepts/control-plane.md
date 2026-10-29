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
    ACT["activation<br/>(PUT /config, revert, crewlet config import,<br/>or a booting node publishing what it knows)"]
    DB[("company_config<br/><b>each node's own copy</b>")]
    PTR[("coordination: <code>activation</code><br/><b>pointer + payload, revision = epoch</b>")]
    NUDGE(["broadcast nudge<br/>revision_activated"])
    N1["node-0<br/>reconcile poll"]
    N2["node-1<br/>reconcile poll"]
    STATUS[("coordination: status bucket<br/><b>one key per node</b>")]
    ACT -->|stored as history| DB
    ACT -->|pointer and payload, one write| PTR
    ACT -.->|best effort| NUDGE
    NUDGE -.->|wake early| N1
    NUDGE -.->|wake early| N2
    PTR --> N1
    PTR --> N2
    N1 -.->|"kept as the fleet's on first sight,<br/>booted on once applied"| DB
    N1 -->|outcome, and what it serves| STATUS
    N2 --> STATUS
    STATUS --> N1
    STATUS --> N2
```

**The `activation` key is the authoritative pointer**, and it lives in the coordination store rather than in a node's own database. The split is the whole point: *which revision is current* is a question the fleet has to agree on, and a pointer each node reads out of its own file is a fleet of one.

**The payload travels with it — inside it.** A revision is written to the database of whichever node served the write, so every *other* node meets it for the first time when the pointer names it, and a peer with no copy has nothing to apply. So `Activate` writes the sealed body **in the pointer's own record**, one write that either lands whole or does not land. The body used to be a key of its own, written just before the pointer, and the order was chosen so that a crash between the two left a body nothing pointed at. What the order could not stop was a second writer. Two nodes activating at once each wrote a body to the one key, and the flip that landed first was then pointed at a body the other had overwritten, so every peer answered "no such revision" for the epoch the fleet was on until somebody activated again. Two nodes booting onto a lost pointer, each restoring the company it knew, did exactly this, and so did an edit that *lost* its compare-and-set, because its body had already landed before its flip was refused. One record cannot come apart. The price is the body's bytes on every read of the pointer, which a reconcile tick makes; a company document is kilobytes.

**On the wire the body is JSON.** It is carried as the object it is: the envelope `{"__encrypted__":"enc:v1:<key id>:<ciphertext>"}` sits as the record's `payload` field, compacted and otherwise byte for byte what was sealed. It used to be carried as bytes, which the record spelled as a base64 string over an envelope whose ciphertext was base64 already, so every body was encoded twice and a third larger for nothing. Anything that is not a JSON object is refused before it is written, because a pointer naming a body no node can decode is a fleet that converges on nothing.

Only the **current** revision's body is kept there, in the pointer's own record. A node that has fallen behind needs exactly the revision the pointer names and never an older one, so a per-revision history in a bucket with no retention would be unbounded growth for rows nothing would ever read. A node that fetches a revision **keeps** it in its own `company_config` as soon as its seal opens, before it validates or builds it — which is where its history, its diffs and its revert targets are read from, so a node that applied without keeping a copy would serve an epoch its own operator surface cannot show. The copy is history and a record of what the fleet is on, nothing more: what the node boots on moves only once its engine has applied the revision (see [A node's own store keeps three facts apart](#a-nodes-own-store-keeps-three-facts-apart)).

**So does the revision's author.** Every revision records who wrote it (`created_by`), **what** that is (`created_by_kind`, one of `agent`, `human`, `operator` and `system`) and the credential it came through (`operator_id`), exactly as every other write in the engine names its caller (`iam.ActorFor`): a person writes as the human seat they hold — every person holds one — and so does a token the identity directory binds to a seat, kind `human`; anybody else on `/config` or `/setup` writes under their own login — `token:<id>` for a Tier A token, `svc:<name>` for a service account bound to no seat — kind `operator`, as does the login running an offline `crewlet config import` or `rekey`; and the engine's own write is kind `system` with no credential — a node seeding the store from its `-company` file (recorded under the node's id), or the reconcile loop reloading after it sealed a credential (recorded as `reconcile loop`). A disconnect the loop carries out is recorded under whoever **asked** for it rather than as the loop's own, because the loop only carries it out (see [Integration Reconcile](integration-reconcile.md)). The kind is stated by the writer, because a label cannot say it: nothing stops a Tier A token being given the id `node`. The pointer carries the revision's **origin** — author, kind, credential, source and the instant it was written — so a node keeping the revision records the same author as the node that stored it first, and the history answers the same whichever node serves it. Before it did, every other node recorded its copy of a revision as written by `peer`, from `fleet`, at the moment it was activated, and the history answered differently depending on which node served it.

The origin is **required**: the coordination store refuses an activation whose origin leaves out the author's kind, the source or the creation instant, so no pointer reaches a peer naming nobody — and a node refuses to keep a copy of one that arrives incomplete rather than record an author it cannot name. A node's copy dates the revision's activation (`activated_at`) by the pointer's own instant, not by when the revision was written — the two differ on every re-activation of an older revision. An author a node already knows is never overwritten, because the node that stored its own write is the authority on it: the first record of a revision is kept. The revision's *parent* is deliberately not carried: it names a revision the copying node may never have held.

**The body is authenticated by its seal.** It is sealed under the fleet's Tier A keyring — which every node holds — so the coordination store holds ciphertext exactly as the node's database does, and a node opens it with the keyring it was deployed with. The seal is AES-GCM, so opening it is also the proof that a node of this fleet wrote it: the broker authenticates nothing, and anything that can reach it can write this bucket. A body that does not open — **stored unsealed**, or sealed under a key this node does not hold — is refused before it is kept: the apply is recorded as an error, the epoch is not reached, and the body never enters the node's own `company_config`, where it would be shown, diffed and offered as a revert target. The recorded error says what fixes it, which depends on where the bytes came from. Every node seals what it publishes, so a body that arrived unsealed could have been forged by anything that reaches the broker. A node whose **own** copy of the fleet's revision is unsealed is told to import the document again, which stores it sealed, and a body sealed under a key the node does not hold means its `secrets.keys` is missing a key the fleet seals under. What the seal does not authenticate is the pointer beside it: a party that can write the bucket can still point the fleet at a revision it has already seen sealed, which is why the broker must be reachable only by the fleet (see [Deployment § Every node embeds a member of one cluster](../guides/deployment.md#every-node-embeds-a-member-of-one-cluster)).

**Its revision is the epoch.** The coordination store assigns every key write a monotonic revision, so publishing the pointer appends and flips in a single write — there is no instant where a node can read an epoch whose target has not been published, and two activations always get two different epochs rather than racing over a counter the engine keeps. It also gives the counter the property a plain revision-id pointer could never have: it moves on every activation *including re-activation of an unchanged revision*, which is the documented gesture for picking up a rotated credential (see [Secret Store § Propagation](secret-store.md#propagation)).

**Every activation names what it was built on.** A config write reads the pointer and builds on the revision it names, and its activation **expects** that revision: it lands only while the pointer still names it. Of two writes built on the same revision, one lands and the other is refused `409 revision_advanced`, its revision kept, inert, as history to revert to — in the history of the node that served the write and of no other, so the refusal names it as `stored_revision_id` beside that `node`, and its diff and revert go to that node by its own address, since any other answers `404` for it ([Concurrent writes](../reference/api-endpoints.md#concurrent-writes)). A write that read no pointer activates **create-only**, and lands only while there is still none, so a node that has not caught up with its fleet cannot replace the company the fleet runs — a replacement that renamed it would change every seat id derived from the name, and orphan all of their memory. A pointer that is gone by the time an expecting write arrives — deleted, or removed by a purge — is a lost race too, because what the write was built on has gone with it. A booting node's publish follows the same rule ([What a booting node publishes](#what-a-booting-node-publishes)). The one activation that expects nothing is `crewlet run -import-company`, which replaces the company outright from a file.

**Every activation carries a later instant than the one it replaces.** The
pointer records the instant a revision was activated, and every row a
configuration derives — a tracker project, a knowledge container — is stamped
with it and refuses an older stamp, so an older configuration applied late
cannot walk a newer one back. That guard needs a later activation to carry a
later instant, and the instant is the activating node's own clock; so inside
the same compare-and-set that replaces the pointer, an activation whose
instant is no later, to the millisecond, than the pointer's is published at
one millisecond after it, and the fleet's history of activations is in the
order the fleet made them whichever node's clock ran behind. Even an
unconditional publish is a compare-and-set underneath for this reason.
`Activate` returns the instant it published, which the writer records on its
own copy of the revision (`activated_at`, raised and never lowered, on every
row the boot row's included; the instant the boot row was *applied* at is its
separate `served_at`). What a
node boots its company with next time is the instant on the revision it boots
on: its `served_at`, that of the activation it last applied, which moves only
when its reconciler applies one — never `activated_at`, which a re-activation
this node refused raises without an apply. An [offline offer](#what-a-booting-node-publishes)
boots with no instant at all, because nothing has activated it: stamped with
the offline command's clock, the estate would carry a document the fleet may
never run at an instant later than the fleet's own, and refuse the fleet's
stamps. The first apply of the activation that publishes it stamps it.

The pointer's bucket has **no retention at all**. Everything else the fleet shares ages out; a pointer that expired would restart the epoch, and a fencing sequence that restarts is not a fence.

**Every node polls it** every ~15 s (±20 % jitter). A poll cannot miss anything, because it asks. The jitter exists only to break lock-step after a synchronized fleet restart — a rolling deploy boots every pod within the same second — and is deliberately applied to the *interval*, never to the apply.

**The `revision_activated` event survives as a nudge.** It wakes the loop so an operator's change lands in milliseconds instead of seconds. It is delivered as an **ephemeral broadcast**, never a consumer group: every node has to hear every activation, and a competing group would hand each one to exactly one node — the delivery shape that made config a fleet of one before the pointer existed.

It is also deliberately thin. The event carries the revision id and its summary and nothing a node acts on: the woken loop re-reads the *pointer*. That is what makes losing a nudge cost one poll interval and never a revision, and it is why a node that cannot subscribe at all — an attach failure is logged, not fatal — simply converges on its interval like any other. After a nudge fires, the next iteration still waits a full jittered interval, so an activation storm cannot become an apply storm.

**The status bucket is what each node managed to do** — one key per node, last-write-wins. Each record names the epoch the node last attempted, the revision that epoch named, the outcome, the epoch it last **applied**, and the revision the node **serves**: the one its last successful apply installed, or the one it booted on. The two revisions differ when the node refused a revision other than the one it runs — a refused re-activation of the revision it already runs names it twice, and only the outcome tells it apart — and that is the record an operator reads to find a node still running the previous company; the two epochs differ whenever the outcome is a failure, and only the applied one says which epoch the node runs. It names a third revision too, the one the node would **boot on** if it restarted now: what its boot row holds, read before the node's first record and after every write to the row, which is the revision it serves except while a write to that row is being retried, and none on a node whose own database is discarded at every start. This is what makes partial apply visible, and it has three outcomes rather than two:

| Status | Meaning |
|---|---|
| `ok` | Applied cleanly. |
| `error` | Refused. Nothing was rolled back because nothing the apply built was published: the one thing an apply changes before it can refuse is the secret snapshot it re-reads first — the store's newest values, which the serving epoch reads too — and the build after it touches nothing. So a revision that cannot be built leaves the previous epoch current and still correct — a legitimate degraded-but-correct state, and one work can safely route to — and the node goes on serving the revision it ran before, and comes back on the one its boot row names (`config_boot_revision_id`, [Reading a stuck node](#reading-a-stuck-node)): that same revision, except while a write to the row is being retried, and nothing on a node whose own database is discarded at every start. |
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

## The company, and what each node keeps of it

The pointer is the company. What a node's own database keeps beside it are facts only that node can know — what it starts on, what it last saw the fleet run, and a change made on it offline — and keeping them apart is what lets a node refuse the fleet's revision, restart, and still run.

### Every config answer is the pointer's

Every answer `/config` gives — `GET /config` and its entity-tag, the revision list's `is_active`, a diff against `active`, and the base every write is built on — is the revision the pointer names, on every node alike, whatever that node runs. A node that has not reconciled since the pointer moved holds no copy of that revision yet, and answers it from the body the pointer carries: no read writes anything, and the node's next tick keeps the copy. So the read after a write answers the write on every node, and an `If-Match` taken from one node's entity-tag lands on any other, one that refused the fleet's revision included. Once the fleet runs a company, a create-only write (`If-None-Match: *`) is refused `412 already_configured` on every node — one that joined and has not reconciled yet too — rather than replacing that company.

When the coordination store cannot be read, a `/config` request that needs the fleet's revision — every read above, and every write — answers `503 unavailable` with `Retry-After: 2` rather than a guess: "the fleet has no company" is the guess that sends a create-only write to replace the one it has. A write stores its revision before it moves the pointer, so a store that stops answering *between* the two is a different answer: `503` with `outcome: unknown`, the `stored_revision_id` and the `node` that stored it, because an activation whose acknowledgement was lost may have landed. `GET /config` says which once the store answers again; sending the write again makes another revision, over this one if it landed. If it did not, the revision is in that node's history alone — a write stores its revision as history on the node that took it, and only an activation carries it to the others — so its diff and the revert that makes it the company are sent to that node; every other answers `404` for it. With no pointer at all — one lost under a running fleet — it answers the company as this node knows it ([below](#a-nodes-own-store-keeps-three-facts-apart)), and a write is create-only.

### A node's own store keeps three facts apart

Each node's `company_config` holds every revision the node has met, as history, and marks three facts about them:

| Fact | Column | Moved by |
|---|---|---|
| The revision this node **boots** on: the last one its engine applied, with the instant of the activation it applied, or an offline command's choice | `is_active`, `served_at` | Online, only the reconciler, and only to the revision its engine has just applied. Offline, `crewlet config import`, `activate` and `rekey` |
| The **fleet's** revision as this node last saw it, runnable here or not | `is_fleets` | Only the reconciler, for each pointer revision whose seal opens under this node's keyring, before it is validated |
| An **offer**: an offline choice of what to boot on that the fleet has not taken, with the revision it was built on, an offer an earlier start sent and never confirmed where it was built on one, and the instant the boot publish planned for it | `offered`, `offered_over`, `offered_unconfirmed`, `offered_at` | Set by the offline command, and replaced by the next one; cleared once the fleet takes the offer, or once the node's engine applies another revision |

Every row also keeps the **latest activation** of its revision this node knows of, `activated_at`: raised by each activation the reconciler sees and by each the node publishes, never lowered. It is the instant a restore asks for and the one the retention sweep dates the row by. The boot row's `served_at` is a separate instant, the activation its engine applied, which the org chart is stamped with at the next start: a re-activation of the running revision that this node refuses — the [rotation](#rotation) gesture, on a node that lost the credential — is the fleet's newer activation all the same, and it is not what the node runs.

These used to be one flag, and each reader took it for the fact it needed. A peer marked the fleet's revision as the one it boots on as it fetched it, before validating it, so a node that refused that revision was refused again at its next start, before its engine ran. A write marked its revision the node's own the moment the pointer moved, and a reconciler that had read the pointer a moment earlier walked it back. And an offline command marked its revision as of "now", which the next start published over whatever the fleet had moved on to.

Now a node that refuses the fleet's revision goes on running the one it served, records the refused one as the fleet's, and at its next start boots on what its boot row holds: the one it served, except while a write to that row is being retried, and nothing on a node whose own database is discarded at every start. Its record in the status bucket names all three — the revision it refused, the one it serves and the one it would boot on. A write through the API stores its revision as history on the node that took it and moves the pointer, and each node boots on the revision once that node has applied it. An engine follows the pointer one reconcile tick at a time, from its read of the pointer to its record of what it serves, so what it boots on moves in pointer order, and an apply that finishes after a newer write has landed never walks it back. `crewlet config revisions` shows the three in its STATE column — `boots`, `fleet's` and `offered over <id>` (see [the CLI reference](../reference/cli.md#crewlet-config-revisions)).

**With no pointer, the company is the one this node knows.** That is an offer built on the fleet's revision this node last saw — or on an offer an earlier start sent, which the fleet's revision may be — or built while it had seen none; otherwise that fleet revision, even one this build refused, because every node that could run it does and the older revision this node runs would roll them back; otherwise the revision it boots on. It is what `/config` answers while the pointer is gone, what the next boot restores, and what an offline command builds on.

The revision sweep that bounds the table never deletes the boot row, the fleet's row, the chain of ancestors behind each, or a revision activated inside its window: a row is dated by the later of its creation and its last activation, so an old revision the fleet re-activated yesterday stays (see [Retention](../guides/retention.md#the-configuration-archive-and-the-one-thing-a-purge-cannot-reach)).

### What a booting node publishes

A `crewlet config` command run while the engine is stopped does not open the coordination store — on an embedded broker that store lives inside the running engine — so it cannot move the pointer. What `import`, `activate` and `rekey` write offline is an **offer**: the revision this node boots on at its next start, built on the company this node knew when the command ran, and the command says which revision that is. Where that company is an offer an earlier start published and could not confirm — it crashed between the publish and the record of it — whether the fleet took it is not known offline, so the new offer is built on it **and** on the revision under it, and the boot publishes it over whichever the pointer names. Run against a **running** node, `crewlet config import` goes through that node's `PUT /config` instead, which builds on the fleet's revision and moves the pointer at once.

Every publish also says whether it **restores** a company the fleet lost or **chooses** one. Only a restore is something a peer that knows better replaces: instants come from writers' clocks, so a pointer somebody chose — an offline `activate` on a node whose clock runs behind, or `-import-company` — would otherwise be reverted by the first peer to boot whose clock read later.

Before its first tick, a node compares what it boots on, and the fleet's revision as it last saw it, with the pointer, and publishes at most one revision, by a compare-and-set against what it was built on:

| This node boots on | The pointer | The boot |
|---|---|---|
| An offer built on *B* | names the offer, at the instant this node planned for it — or this node's reconciler saw it name the offer | Publishes nothing: the publish landed before a crash, and is recorded as taken; the boot then decides as for any revision |
| | names *B*, and is not a restore of a revision this node saw the fleet move past | Publishes the offer expecting that very activation of *B*, at an instant later than the pointer's. That is always a new epoch, so an offline `activate` of the fleet's own revision is the [rotation](#rotation) gesture |
| | names anything else | **Withholds it**: the fleet moved on from what the change was built on |
| | is gone, and the fleet revision this node last saw is *B*, or it saw none | Publishes the offer create-only |
| | is gone, or is a stale restore, and this node last saw the fleet run another revision | Withholds the offer, and restores that revision as below |
| Anything else — *R* is the fleet's revision this node last saw, or else what it boots on | is gone | Restores *R* create-only, at its latest activation. A node that knows no company publishes nothing |
| | is a **restore** of another revision this node holds, at an earlier instant than *R*'s | Restores *R* expecting that very restore: another node put back a company the fleet had moved on from |
| | names anything else — a choice, however old its instant reads, included | Publishes nothing: the pointer is the company, and the reconciler converges on it |

A publish that loses its compare-and-set re-reads the pointer and decides again. That ends: every landing moves the pointer to a later instant, and every rule that publishes needs the pointer where this node left it. So a restarted single-node deployment comes back pointing at the revision it last saw the fleet on, a pointer lost under a fleet is restored from what its nodes saw it name — by a node that refused that revision too — and a node rejoining a live fleet converges on the fleet rather than rolling it back.

**What no rule can do** is tell a stale restore from the company once somebody has *chosen* something over it: an edit made on a node that knew no better is a choice, and a peer that remembers a newer revision leaves it alone. A rebuilt coordination store is restored by the first nodes to boot, so the fleet converges on the newest revision any of them remembers only while they boot ahead of the first write. After rebuilding one, start the nodes that saw the fleet last before anybody writes.

**A withheld offer is said on every boot**, at warn, as `offline_revision_not_published`, naming the revision, what it was built on and what the fleet runs, for as long as the offer stands: a change an operator made that the fleet is not running is not something to find out by accident. The node runs the offer until it applies the fleet's revision, which then becomes what it boots on — said the same way, with the offline change left in its history. Making it the company as it stands would undo what the fleet changed since, so the warning's hint names the change itself instead: `GET /config/revisions/<revision>/diff?against=<built_on>` on that node shows what the offline change did, to make the same change on the fleet's document (through the dashboard, or `crewlet config import -api`); `POST /config/revisions/<revision>/revert` on that node puts it up as it stands, for an operator who means that.

`crewlet run -import-company` is the one exception to all of this: it replaces whatever the fleet runs with its file, unconditionally, unless the fleet already runs that file. With no pointer and a file that *is* the company this node knows, it publishes that revision again — a choice, at this node's clock, never at the instant the revision last activated, which would read as the company the fleet ran back then.

---

## Posture: what a lagging node does

Reading the pointer and the status bucket together is what lets a node distinguish *"I am behind because propagation takes a moment"* from *"I am behind because I cannot apply this"* — which need opposite responses.

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

Only then does peer health pick the action. And when *no* peer managed the epoch either, the honest conclusion is that the **revision** is bad rather than this node — so it keeps serving the epoch it already had, which a refused apply leaves untouched and which it boots on again if it restarts, and raises divergence loudly. Shedding there would take the whole fleet down over one bad revision, which is precisely what publishing rather than mutating exists to avoid.

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

The gate stops there rather than being folded into every worker duty, and that is a decision rather than an omission. Posture and `node.roles` are decided by subsystems that do not consult each other — the shed rule counts any peer with a fresh `ok` row as somewhere the work can go, including an ingress-only node that will never claim a singleton. Applied to all of them, an `ingress` + `seats,workers` fleet whose worker node fails a single apply would end with nobody running the scheduler, the retention sweep, the sandbox waiter or the curator, `/ready` green on the node that is fine, and not one log line to say so.

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
| Worklet projects | **Nothing to rotate, but they are re-stamped.** The apply stamps each project with the activation's own instant, and a re-activation is a new activation. It therefore records one "org chart re-applied" change per project, once, however many nodes apply it. Re-applying the *same* activation, which is what every restart does, writes nothing. |

The MCP children, shared and per-role, and the Mattermost transport above, are the places a comparison does happen, and it is deliberate: a child is a *process*, and restarting every one on every apply would tear down working servers to arrive back where they started. So `Bridge.Reconcile` compares the spec it is handed against the one the child is already running and leaves an unchanged server alone. What makes that safe for a rotation is *what* it compares: the spec's `env`, `headers` and `url` are resolved at the edge before the comparison, so a moved credential reads as a changed spec and restarts that one child. Comparing the stored config entry, where `${VAR}` stays verbatim, would silently stop rotation from reaching MCP children at all; the tests hold that line by re-applying the same document and asserting which children survive it, and by rotating one seat's credential and asserting its neighbour's child is untouched.

A per-role child is reconciled where its seat is held and only there: every node walks the seats *it* holds, so a rotation reaches each agent's child exactly once in the fleet, and a node never starts a child for a seat whose lease it cannot still prove. A seat the revision removes has all its children retired by that same reconcile, without waiting for placement to release the seat. The same reconcile is what lets a credential [minted by a provisioning pass](integration-reconcile.md) after a seat was claimed reach that seat's child — the pass re-activates the revision once it has sealed — and an apply that lands while the seat is still being claimed is folded into the claim, which starts the children from the snapshot that apply installed before the seat's mailbox opens. The apply itself does not wait for any of this: it refiles each held seat's tool catalogue and returns, and each seat restarts its own children just after, because a start can take a whole startup timeout and every stage after this one would wait behind it with the new epoch already current. A restart stops the old child before it starts the new one, so a turn that starts in that moment still lists the old child's tools and a call to one fails as [What a running turn sees](#what-a-running-turn-sees) describes; the next turn gets the new child.

That is the whole of the comparison, and it is over resolved values rather than a digest of them. Nothing keeps a digest of live credentials across applies, which is what a broader selective rebuild would need and what would turn a rotation into a leak the moment such a digest reached a log line or a row.

> One more surface, and no re-activation reaches it while it runs: a **running code sandbox** received its model credential in its environment at launch, and a process's environment cannot change. Every *launch* builds its environment from the current epoch's entry, so a new box, and every later segment of a running turn (a send-back, a continuation after a person's answer), already takes the new credential, and a follow-up segment in a kept box carries its own launch's environment rather than the box's. What is left is a segment running when the credential changes:
>
> - **Where the old credential still works**, the segment runs to its end on it, and its next segment launches on the new one.
> - **Where the vendor revoked it**, the segment ends refused. Each launch records the entry's sign-in identity on the run's row (`sign_in_id`, a keyed digest that names no credential), so the node collecting the run sees that it was refused on a sign-in the entry no longer uses, and relaunches the same round **once**, in the same box, continuing the run's own session, with the new credential (log line `run_relaunched_signed_in`). A relaunched segment refused again is an ordinary failure.
>
> Stop the seat's running work (pause it with *stop running work*) if a rotation must cut a running segment off at once.

---

## What runs before the first revision

A node started with no company is not an idle one. On every node that holds the estate — every `data` node — its **core runtime** is started at boot, company or not: every domain's state log, the node gate over every identity-claiming log, the [identity estate](identity-and-access.md) with the triggers that keep this node's view of it, and — on a node that publishes — the log's trim and the identity duties. That is what lets a Tier A token's session sign in on a node that has nothing else yet and create the company through it — and the company's first person be invited or created and sign in once that company declares the human seat they will hold, since a person is only ever put onto a seat. `ingress` requires `data`, so every node that signs anybody in reads its own identity rows. A node that holds no data runs no core at all: it applies no log, and reaches the estate — the identity directory included — through a node that holds it ([A node that holds no data](scaling.md#a-node-that-holds-no-data)).

What waits for the first revision is what only a company can say anything about: whether the engine runs Worklet and Doclet at all, and so the writers and readers over those two logs, their lexical index, their change feeds and the embedding duty. The apply's `native` stage brings them up under the API already serving, and the surfaces over them (`/worklet`, `/doclet/pages`, `/operator/mcp` and the socket's work questions) read them per request, so they answer `503 no_active_revision` until then and serve from then on with no restart. On a data node the logs themselves have been applied all along: a join replaces the whole replicated file and a snapshot names every registered domain, so a node applying part of the register could neither adopt nor donate, and the trim, which counts nodes per log, would be pinned by it.

A node's seats are admitted once its state logs are hydrated: on a data node, once every core log is established, the identity log included, and the router answers for Worklet and Doclet; on a node that holds no data, once a data node answers for them. So a data node waiting for its first company is already caught up the moment it gets one. At a stop the order is the reverse of what depends on what: the directory's view trigger first (a registry rebuild must not outlive what it serves), then the fleet duties, with the embedding writer of the Worklet and Doclet halves before the core's trim and identity duties because it publishes into a log the trim is deciding how far to purge, and every duty before the node gives its duty leases back; then Worklet's and Doclet's halves (their change feeds, lexical index and search answerer, each of which reads what the logs write), and the logs last.

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

Each node's status carries the `error` it failed with and the revision it runs, so the first question — *is this the revision or is this the node?* — is answered by the **Settings › Nodes** screen. It counts as behind on config every node that does not run the activated revision — one on an older epoch, one that refused the active revision or applied it only in part, one on another revision, and one that has reported nothing — and names the revision each one runs beside the error it failed with. It is also one request:

```bash
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  http://localhost:8000/query/fleet | jq '{activation, nodes: [.nodes[] |
    {id, config_epoch, config_revision_id, config_applied_epoch,
     config_serving_revision_id, config_boot_revision_id, config_status,
     config_error, config_reported_at}]}'
```

`activation` is the pointer the fleet is converging on: its `epoch`, `revision_id`, `at` and `summary`. A node's `config_epoch` and `config_revision_id` are what it last **attempted**, `config_status` how that went, `config_applied_epoch` the epoch it last **applied**, absent until it has applied one since it started, `config_serving_revision_id` what it **runs**, empty while it runs none, and `config_boot_revision_id` what it would **come back on**, absent while it would come back on nothing. A node runs the company only when it reports the activation's epoch and revision with `ok` and serves that revision. A node that refused it reports the activation's epoch with `error` while it serves the revision before, which a comparison of epochs alone reads as current.

A node that stopped reporting **drops out of this list** once its status ages past the freshness bound — which is the same fact the posture decision reads, so what an operator sees and what the fleet concluded cannot disagree.

One node `error` while peers are `ok` is a per-node problem: a missing env var, an image without some MCP binary. That node goes on running the revision named by its `config_serving_revision_id`, and comes back on the one its `config_boot_revision_id` names, so a restart with the missing piece in place brings it up on what it served and then applies the fleet's revision. The two name one revision except while a write to the node's boot row is being retried — the write is local and best effort, each tick tries again, and the node logs `boot_revision_not_moved` meanwhile — and `config_boot_revision_id` is absent altogether on a node whose own database is discarded at every start (`store.scratch`), which comes back on nothing and applies the fleet's revision from scratch. Keep whatever such a node needs in order to start, a revision's credentials among them, against the revision it would boot on rather than the one it runs. Every node `error` on the same epoch is the revision — revert it. Any node `degraded` needs a **restart** of that process specifically: the status exists precisely for a failure past something no later apply can put back, so nothing short of a restart will. No build reports it today.

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

`applied_subsystems` is the ordered list of what this node had already rebuilt when it stopped, out of `secrets`, `company`, `native`, `sandbox_runtime`, `tools`, `learning`, `sandbox`, `integrations`, `epoch`, `maintenance`, `learning_passes`, and then the [convergence](configuration.md#what-follows-a-published-company) every published company runs — `parties`, `seat_identities`, `seat_tools`, `tracker_projects`, `knowledge_containers`, `mailboxes`, `scheduler`, `published` (the example above refused the revision's `providers.sandbox`, whose catalogue is built straight after the company and before anything else on the node moves). That is the difference between "refused before anything changed" and "torn down halfway", which is precisely what decides whether the node needs a restart. An `error` with **no** `applied_subsystems` — the field is absent rather than an empty array — means the apply changed nothing on this node, and it still serves the previous epoch; at most the node recorded the revision as the fleet's, which it does before validating it. There are two ways to get one. The node was stopping, and refused before the first stage (`engine: apply: this node is stopping and applies no revision`). Or the revision never reached the apply, because its body is in no store this node can reach, it could not be fetched, read, opened or parsed, or it is not a runnable company (`engine: store: no such config revision: …` — the activation pointer names a revision neither this node's database nor the coordination store holds — `engine: fetch revision …`, `engine: read revision …`, one of the three refusals of a body that does not open under this node's keyring, which each name the revision and the remedy, `engine: parse revision …`, `engine: revision … is not a runnable company`) — a storage, transport or authoring fault, and the `error` text says which. Every apply that runs a stage reports at least `secrets`, because re-reading the secret store is its first stage (see [The engine half](configuration.md#the-engine-half)), and nothing about the revision is checked against this node's Tier A before it: a node whose stream would not survive a restart is refused when it starts, so it never reaches an apply to refuse.

Like every other event this lives in each node's own log, so a node whose disk is gone took its rows with it — but a node that merely stopped reporting, or was replaced, still has them.

---

## See also

- [Coordination](coordination.md) — the shared store this plane's two keys live in, and what else does
- [Scaling Out](scaling.md) — the model this sits inside, and the other four kinds of coupling a fleet had to resolve
- [Configuration](configuration.md) — the two-tier split, and the apply itself stage by stage
- [Secret Store](secret-store.md) — where rotated credentials live and how re-activation picks them up
- [Deployment](../guides/deployment.md) — running more than one node
- [Event System](event-system.md) — subscription types and why a competing group was the wrong one here
