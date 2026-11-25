# Read Consistency

Every data node holds its own copy of the company's work, built from one ordered log. Copies converge but sit at different points along it. This page answers: **how fresh does this answer have to be?**

## The four levels

| Level | What it promises | What it costs |
|---|---|---|
| `linearizable` | No answer from before this read arrived. | An append to the log and a wait for it to come back. |
| `session` | No answer from before **your own** last write. | Nothing when you are caught up, the common case. |
| `stale` | Whatever this node holds, with its lag reported. | No broker call. |
| `consistent_prefix` | A coherent point in the log's own order, possibly behind. | No broker call. |

**`linearizable` is for a decision, so an agent reads at it.** Use it when the answer decides something (an admission check, a gate, a number acted on irreversibly). It is the only level that establishes a position at the log's current end first. Every seat tool read is one; a seat has no screen on which to notice a stale copy.

**`stale` is what the dashboard and the read API use.** The answer carries its lag; a barrier append per redraw would put the fleet's whole read rate on the log.

**`session` is the write path's level, and a read surface offers it only with a position.** It waits for *your own* high-water mark, which you must supply; bare, it would serve whatever the node held under a wrong `session` label. So the read grammar accepts `read_level=session` **only beside `min_position`** (the position the write you are reading back answered with) and otherwise refuses it, naming the key and the two other honest asks, `linearizable` or `stale` with `max_lag_seq`. A seat's tools carry no position, so there it is unreachable. Inside the engine a write waits for its own last write to be applied before it opens the snapshot it decides from.

That wait is **per log, not per object**: a node that just published a burst (a cross-project move re-keying a subtree) waits for it to apply locally before its next write on that log, any subject, and may be refused `behind`, naming its own record. It costs nothing when caught up.

**`consistent_prefix` is the default for nobody; it is asked for or resolved to.** It promises a *named prefix* of the log (everything up to a stated position, nothing after) and **no statement about age**, which makes it weaker than `session` in a way the answer cannot show, so nothing defaults to it. The dashboard may ask for it, and a replication answer resolves to it when the lag is unmeasurable, and says so.

## Which surface reads at which level

The level belongs to the **surface asking**, never to a caller who omitted the key.

| Surface | Default | May the caller choose? |
|---|---|---|
| A seat's own tools, inside a turn | `linearizable`, floored at its node's own writes and at the change that woke the turn ([below](#read-your-trigger-is-a-floor-not-the-mechanism)) | No |
| The operator MCP, about the company's own content: the tracker, the knowledge base and chat | `linearizable` | No |
| The dashboard and the REST read path | `stale` | Yes: `linearizable`, `stale`, `consistent_prefix`, or `session` beside a `min_position` |
| Any answer **about replication**: the retention report, Settings › Backups & retention's lag, whether a purge landed | `stale`, weakening to `consistent_prefix` | No, it is derived |
| A person's git client, for every answer that names a ref: the refs a clone, a fetch or a push is shown, a protocol v2 `ls-refs` | `linearizable` | No |
| A git fetch of objects by id | none: an object id names the same bytes on every node; a want this node has not seen takes one `linearizable` sync first. A [check's box](../concepts/gitlet.md#check-boxes)'s wants are looked for at its check's `session` instead (the position its attempt was queued at, which holds its commit), so a node behind that commit's push serves it with no barrier | No |
| A Gitlet node's [server-side read](../concepts/gitlet.md#server-side-reads) of a repository's content by object id: a tree, a file's lines, a diff, a comparison | none: an object id names the same object at every position; the copy is first brought to the asking node's own writes to that repository, where it holds any, and to the push that set each ref the asker resolved its commits through (never to where that resolve read the ref); an id it lacks takes one `linearizable` sync before it is answered as no such object | No |
| [Gitlet](../concepts/gitlet.md)'s rows, `/gitlet/repos…` | `stale`, as the REST read path's | Yes, as the REST read path: `linearizable`, `stale`, `consistent_prefix`, or `session` beside a `min_position` on the code log, the position a write to it answered with |

**Only the screen chooses**, because only the screen can see what it got: the level and lag are rendered beside the rows.

**A replication answer is the one row where the SUBJECT decides the level**, on every surface including the operator MCP. These answers ask how far behind the log this node is, and the barrier that `linearizable` needs is the instrument whose health is the subject: it cannot be served in the very incident somebody opened the page for.

It weakens to `consistent_prefix` when the node could not measure its own lag at all (broker unreachable, or coordination), because `stale` claims *age* and there is none. `crewlet retention status` and **Settings › Backups & retention** say so in a sentence.

An agent cannot choose (a model would trade correctness for latency it cannot perceive); the operator's reads cannot (a knob that only weakens gets turned once and forgotten).

Every answer reports the level it was **actually** read at.

**git has no level to choose, and needs none.** Every answer that names a ref is read at the log's head, so a node cut off from the fleet answers `503` rather than an old head. An object fetch is served from what the node holds; for an object learned of from a node further ahead, the node reads the log's head once first. A push is answered `ok` for a ref only once the push's own record has been **applied**, never on a record that is durable and not yet applied: see [A push, end to end](../concepts/gitlet.md#a-push-end-to-end).

**A client that must not conclude "not declared" from an old copy** (a script about to create or import a repository) reads `/gitlet/repos/{name}` at `read_level=linearizable`: at the default `stale`, a repository declared a moment ago may be missing from this node and answered `404`.

## Bounding staleness

`stale` alone accepts any age. A caller that will not says so with `max_lag_seconds`, `max_lag_seq`, or both, and the read refuses `too_stale` past **whichever is reached first**. They are two readings of one distance: the record count is what the broker answers, the duration is derived from it through this node's drain rate.

The lag is this node's reading of its log's ends, served from memory: re-read at most once a second, off the read's path, and **never more than three seconds old**. So a node may be up to three seconds' worth of writes further behind than it reports. Ends nobody could re-read for three seconds are not served: the read is refused.

Both are refused at every other level (`linearizable&max_lag_seq=250` is a caller who believes they asked for something they did not). A zero bound is not a bound: it accepts anything.

## Reading your own write back

Every write answers with the **position** its record landed at, `<stream>@<generation>:<sequence>`, on the tool answer, the operator MCP's answer and the retention routes alike. Every read takes it back as `min_position` and is served from no earlier than that position, **at whatever level was asked**:

- At `linearizable` it costs nothing: the read's barrier is already past any position a write on the same log answered with.
- At `session` it *is* the high-water mark the level waits for, which is what makes the level offerable outside the engine.
- At `stale` and `consistent_prefix` it turns "whatever this node holds" into "whatever this node holds, from here on". The lag is still checked against any bound and reported; a node that has not reached the position within the read budget refuses `behind`, with the same derived retry hint, rather than serving rows from before the write.

A position on a log the answer is not read from is refused, not waited for, and as a **bad request** (checked before anything about the node, so every node answers alike): `400
bad_params` over REST, and `bad_params` on the socket, the detail naming the log the position is on.

### One floor per log

**One answer can be read from more than one log**: the company feed is the tracker's work and the knowledge base's pages. A caller that wrote to both repeats `min_position` **once per log**: `?min_position=CREWLET_TRACKER_LOG@1:4711&min_position=CREWLET_PAGES_LOG@1:77` over REST, and an array of the tokens on the socket (one alone may be a plain string, or an array of one). At most one per log and four in all: two on one log, or a fifth, are refused `400 bad_params` naming the key.

Each floor holds up the log it names:

- **A question read from one log** (nearly every question) waits on the floor on that log and refuses a floor on any other as the bad request above.
- **A question read from several logs** hands each half the floor on that half's own log, so each waits for exactly the caller's write to its log. A floor on a log none of its halves reads is refused. The rest of the ask reaches every half whole: its level (save the `session` case below) and any [staleness bound](#bounding-staleness), so `max_lag_seq=250` on the company feed holds its work and its pages each to 250 records, and the feed is refused `too_stale` when either half is further behind.
- **A `session` read names a floor on each log the caller wrote**, so a half whose log it named no floor on has nothing of the caller's to wait for. That half is read `stale` rather than `session` over a wait for nothing, and the answer reports **the weakest level any half was read at**.

The dashboard keeps one floor per log it has written from a tab and names, on each read, the floor of every log that question reads (see [Writes are confirmed, not optimistic](../reference/dashboard-design.md#writes-are-confirmed-not-optimistic)).

Two spellings, one triple: inside an answer a position is the object `{stream, generation, seq}` (`seen_through`, `incomplete.from`, a listing's `position`); as a parameter (`min_position`, a cursor, `since`) it is the token `<stream>@<generation>:<sequence>`. A tool answer's `position` is already the token.

This is the wire's half of read-your-writes; a seat needs none of it, since its reads are `linearizable`.

## How `linearizable` actually works

It is a **barrier append**, not a field check.

The reader publishes one record onto the domain's `barrier` subject and waits until its own node holds every record before it that writes a row. The acknowledgement is the proof: at `replicas: 3` a `PubAck` means a majority of the raft group has the record durably, so any position at or below it is one the whole group agrees on. At `replicas: 1` it proves only that the one member has it, as strong as that deployment is.

**No barrier's commit is waited for**, neither the read's own nor an earlier one's. A barrier writes no row, so the node counts it as held the moment its applier decodes it and commits it beside the apply loop (waiting would refuse reads `behind` on a disk whose fsyncs stall, with every needed row already held). A read still waits for a commit that writes a row, and a `behind` names that position. The position an answer reports may be a barrier not yet checkpointed; the durable checkpoint (where a restart resumes, and what the node tells the fleet) moves only by a commit, so a restart is handed those barriers again.

Concurrent reads on one node **share** barriers. A read may use only a barrier that started after it arrived, so one in flight proves nothing for it; it waits that one out and joins the next, which serves every read that arrived meanwhile. A lone read costs one append; a read on a busy node waits for at most two and appends none.

Measured by `make bench`'s read arms: 2,000 reads an arm, one run each, on two shared cores of a laptop (E-cores of an Intel Core Ultra 7 265U) holding all three members, with `stream.sync: always`. A three-replica range's ends are the stream's leader and a follower. A whole read is never faster than its barrier; at three replicas the two rows came from separate arms and the lower median is noise.

| | solo | three replicas |
|---|---|---|
| the barrier append alone (median) | 0.96 ms (0.21 ms with the fsync declined) | 8.4–11.9 ms (2.4–2.6 ms declined) |
| one reader: the whole read, median / p99 | 1.4 / 5.3 ms | the barrier's within that noise (6.2–10.0 measured) / 63–71 ms |
| one reader: reads a second | 595 | 64–96 |
| sixteen readers at once: barriers appended per read | 0.12 | 0.12 |
| sixteen readers at once: reads a second, together | 5,500 | 490–1,560 |
| sixteen readers at once: the whole read, median / p99 | 2.8 / 6.3 ms | 9.8–21.9 / 22–83 ms |

A whole read adds the node's decode, not the checkpoint's commit, so the rows sit about half a millisecond apart solo. Sixteen back-to-back readers get at most about eight reads per barrier (measured, about eight and a half, whole reads included). Treat all of it as a floor: real members add a network hop and do not share one machine.

It is an append and not a `STREAM.INFO` field because a partitioned former leader would answer a field read with a last sequence the majority has moved past; an append cannot commit there.

## Which logs can be read at `linearizable`

Twelve of the fourteen state logs grant it; the vectors and the usage log do not.

| Log | `linearizable` | Why |
|---|---|---|
| The tracker (`CREWLET_TRACKER_LOG`) | Yes | It encodes the barrier as one of its own records. |
| The knowledge base (`CREWLET_PAGES_LOG`) | Yes | Likewise. |
| The identity estate (`CREWLET_IAM_LOG`) | Yes | Likewise; no surface reads it that way (identity answers are a node's own rows with their lag), so its declared read rate is zero (below). |
| The artifacts log (`CREWLET_ARTIFACTS_LOG`) | Yes | Likewise: an artifact's head, versions, shares and history are read at a position, and a seat tool reads them at `linearizable`. |
| The artifact datastore (`CREWLET_ARTIFACT_DATA_LOG`) | Yes | Likewise: a store's rows, history and state digest are read at a position, and every datastore read takes the same freshness parameters as other estate reads. |
| Ask Crewlet's log (`CREWLET_ASSISTANT_LOG`) | Yes | Likewise. No surface asks for it (a person reads their own conversation through the node their writes waited on; an approval is arbitrated at the broker); the barrier is what the gate reserve on this identity-claiming log is proved against. Declared read rate zero (below). |
| Chat (`CREWLET_CHAT_LOG`) | Yes | Likewise. A barrier contends with no message or room state. |
| The executor log (`CREWLET_EXECUTOR_LOG`) | Yes | Likewise: the turn that continues a conversation reads its head at this level, so a node that has not applied the previous turn's end, published by another node, waits for it rather than continuing an older session. |
| The access log (`CREWLET_ACCESS_LOG`) | Yes | A turn's dispatch reads what its seat was last told at `linearizable`: the previous turn may have been told on another node, and a copy that has not applied it would tell the same change twice. An audit page reads at the caller's level. |
| The atlas log (`CREWLET_ATLAS_LOG`) | Yes | Likewise: an agent's and the operator's atlas tools read it at the seat surface's `linearizable`. A person's edit decides from the entry's version in its own snapshot, where the broker arbitrates what a stale copy decided, and the dashboard reads at `session`. |
| The advisor log (`CREWLET_ADVISOR_LOG`) | Yes | Likewise. No surface asks for it: the Advisor's duty decides each write from the finding key's version in its own snapshot, where the broker arbitrates what a stale copy decided, and every read of the Advisor is at its caller's level, `session` by default. Declared read rate zero (below). |
| Gitlet (`CREWLET_CODE_LOG`) | Yes | Likewise: every answer a person's git is given that names a ref is read at `linearizable`, and the REST reads under `/gitlet/` take the level as other estate reads do (see [Which surface reads at which level](#which-surface-reads-at-which-level)). |
| The vectors (`CREWLET_TRACKER_VECTORS`) | **No**, declared not omitted | A compacted changelog keeping one message per source; every value derives from a source another log owns, so a read claims no position a barrier could prove. No surface reads them at a level: a search ranks with them and answers for the pages and tasks they came from. |
| The usage log (`CREWLET_USAGE_LOG`) | **No**, declared not omitted | A compacted changelog keeping one message per (node, company day, seat, schedule or person), each written by the one node whose own event log it derives from: nothing is arbitrated, and no node can vouch for another's day being whole until it publishes it. A barrier would prove only that this node holds every record published before it, which a day still being written never makes complete. A spend window reports the days and the `horizon` it answered from instead. |

The decision is **declared per log** in the engine's register of domains (`internal/engine/statelogregister.go`): each entry states either the barrier encoder it appends with or `NoBarrier`, and a boot refuses an entry that states neither or both.

An entry with a barrier also states **how often it is read that way**: the linearizable reads a day each agent seat puts on its log. That is 125 on the tracker's, the knowledge base's, the artifacts log's, the artifact datastore's and Gitlet's, 120 on chat's, 150 on the executor log's, 60 on the access log's, 30 on the atlas log's, and none on the identity estate's, Ask Crewlet's or the advisor log's. A boot refuses a barrier stated without it. The figure is the log's census, which [`census_drift`](../reference/alarms.md) holds it to, and a term in the log's ceiling; see [Retention § The storage forecast](retention.md#the-storage-forecast).

## The fifteen refusals

A read that cannot be served at the level asked is **refused with a code**, never downgraded. Each code names a different thing to do.

An API answer's `detail` tells a person each code, and each write reason below, in a sentence (what happened and what to do, asking them to try again in a moment where the answer carries a `Retry-After`); the dashboard draws it as it stands. A write through the operator's act route, or the human write surface's tools and tool-less verbs, answers at ADR-0060's floor with no `detail`, so the `unavailable` code's own sentence stands beside the `Retry-After`. The sentence names the node by what it is doing (the node handling a change, the node answering a question), because the refusing node may be a data node the one you asked routed to; that node's log names it (`estate_peer_refused`). A refusal of another node's copy of your write, which the refusing node finishes itself once the log's duplicate window lets go of the operation, asks you to try again in a few minutes and carries that window as its `Retry-After`.

The refusal's own detail (the broker's words, the record's size, the setting to change) names positions and settings for whoever runs the node, so it goes to the answering node's log, not `detail`. The tooling an operator drives (`crewlet retention` and the dashboard's evict and readmit dialog) still shows it, and the configuration screens say a change may or may not have landed when the engine answers a write with a `5xx`.

| Code | What happened | What to do |
|---|---|---|
| `behind` | This node has not reached the position the read needs: it has not committed every record up to it that writes a row (a barrier is held from the moment it is decoded, its own commit never waited for). Includes a node below the published trim floor whose missing records the log still holds (it is replaying). | Wait: the answer carries a retry hint derived from this node's measured drain. It clears on its own. |
| `too_stale` | This node's lag is past what the read said it would accept. | Ask a node less behind, or accept more staleness. No retry hint: a lag bound is the caller's choice, and a node past it may stay there. |
| `stalled` | This node's applied prefix has stopped moving: its applier halted, or has been retrying a failure it cannot get past for longer than the retry budget. | Its rows are frozen, so a short answer would be wrong, not old. Check the applier: `crewlet retention status` names the domain, its position and the error it is retrying. A retried failure clears the moment an attempt succeeds. A `consistent_prefix` read, which makes no statement about age, is still answered by a stalled node at or above the published trim floor (a frozen prefix is still coherent) and refused by one below it. |
| `no_quorum` | On a log **several members** store (`stream.replicas` above one), the barrier did not commit in time: a majority did not agree, or no leader answered, within the barrier's own ten seconds (one election of the log's leader at the broker's raft settings, which the barrier waits out, appending again while nobody answers or the log answers it has no leader), or, on a read another node asked for, within what that node's attempt left, less a second for the answer to reach it. Never on a log one member stores, which has no majority to miss. | Retry after the hint (4 s, the broker's own minimum election timeout). If it persists, a member is down or partitioned. |
| `broker_stalled` | On a log **one member** stores (a single node's, or any log whose stream keeps one copy), the barrier was not acknowledged within its own five seconds. No election is coming, so the member's own write did not complete: a disk not keeping up with the fsync `stream.sync: always` makes of every append, or a broker that stopped serving the stream. | Retry after the hint (5 s, one barrier budget). If it persists, look at the broker's own disk, the volume under `stream.store_dir`, first. Every node appends its barrier to that one member, so no other node answers it either. |
| `broker_unreachable` | The broker did not answer at all: the barrier a linearizable read appends on a log one member stores ended on an error before its time ran out or was answered for all of it that the log had no leader, or this node's own reading of its log's ends (not re-read for three seconds) or its eviction row. | Retry. Not `no_quorum` or `broker_stalled`; the difference is where to look. |
| `log_full` | The log is at the byte ceiling its ordinary appends are held to (on every log that claims identity (the tracker's, the knowledge base's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's, chat's, the executor log's and Gitlet's) the gate reserve below the broker's, [kept for gate records](retention.md#the-gate-reserve)), so no barrier can be written. | Raise the ceiling with `crewlet retention set-capacity`, or unblock the trim: `crewlet retention status` names the term. **`stale` keeps answering**, so a full log costs `linearizable` reads, every seat tool read among them, rather than every read. |
| `broker_refused` | The broker refused to store the barrier for a reason it named and this build has no remedy for: a sealed stream, a JetStream store with no resources left. The detail carries the broker's code and words. | Act on the broker's words; the next barrier is refused the same way. Like `log_full`, it costs only the levels that append. |
| `deferred` | This node holds a record it cannot decode covering what this read is about. | Ask another node, or upgrade this one. Waiting changes nothing. |
| `deferred_scope_unknown` | The deferred record's own scope could not be read, so nothing can be said about what it covers. | It blocks the whole domain, hence a different code. Upgrade the node behind on the record version. |
| `below_floor` | Records this node never applied are gone from the log. | No replay can supply the missing state, so the node adopts a peer's snapshot on its own: the position heartbeat requests the rejoin. Come back after the hint (one heartbeat, 15 s), or ask another node meanwhile; see [the join runbook](retention.md#the-join-runbook). |
| `floor_unknown` | This domain's published trim floor has not been read for four heartbeats (a minute). A read is served the floor this node last read, refreshed at most once a second off the read's path, so a blip of coordination refuses nothing; the refusal names how long ago the floor was last read. Each domain's floor is its own key, so one that cannot be read refuses that domain's reads alone. | Guessing would keep a node serving over a hole it cannot see. It clears the next time the floor is read, so come back after the hint (one heartbeat, 15 s); if it persists, check coordination, or, where one domain alone refuses, its floor row; see [An unreadable trim floor](retention.md#an-unreadable-trim-floor). |
| `evicted` | This node has been removed from the fleet. | Nothing it holds is authoritative. Readmit it, or route elsewhere. |
| `maintenance` | This node runs in a [capacity window](retention.md#changing-a-logs-ceiling) (`-mode maintenance` or `-mode seal`), which publishes nothing, and a `linearizable` read proves the log's end by appending a barrier. | Ask again once the fleet is back in normal mode. Every other level appends nothing and is still answered. Every node of a fleet in a window is in one, so no other node answers it either. |
| `wrong_stream` | This node's own log is not the one its rows are keyed to: its checkpoint is past the log's end (a broker restored from an older copy, which keeps the stream's creation instant); or the log holds, at that checkpoint, another record than the one it consumed there (the same restore written past this node's rows); or the stream was deleted and rebuilt (found at boot against the checkpoint, or under a running node by the position heartbeat and by any write at an expectation of zero, from the stream's own creation instant); or a peer re-anchored the log past this node's generation, so it continues from that peer's rows. A position this node derived for the read on another log (a session mark, a barrier) is the same refusal. A **state of this node**, never of the request (a `min_position` naming another domain's log is refused as a bad request instead, above). Each finding refuses the node's **writes** with the same word. | Ask another node meanwhile. A recreated stream or restored broker is an operator's re-anchor, see [Retention](retention.md#re-anchoring-a-recreated-or-restored-log); no wait clears it. A peer's reanchor clears on its own once this node has [adopted](retention.md#a-node-a-peer-re-anchored-past) a snapshot from the new generation. |

Seven are worth coming back to **this** node for: `behind`, `no_quorum`, `broker_stalled`, `broker_unreachable`, `stalled`, `below_floor` and `floor_unknown`. The rest are not: a caller that retried `deferred` would loop forever.

Eleven of the fifteen are about **this node** (its copy, its view of the log, its side of a partition), so another data node may serve the same read. The estate's router asks the next data node a read refused for any of them, and answers with the refusal only where no node does better. Asking costs a question per node and no more, because the refusal says what the read already paid for and the router hands it to the next node:

- a `linearizable` read waits for the records before the barrier the first node appended instead of appending its own (still linearizable, since it was appended after the read began; every node holds another's barrier the moment it decodes it);
- a position one node waited its whole read budget for is not waited for again (served if the node holds it, `behind` at once if not);
- a log whose barrier ran its whole budget (ten seconds, five on a log one member stores) without committing gets no second one (`no_quorum` at once, or `broker_stalled` on a log one member stores).

So a fleet one burst put behind together refuses a read after one barrier and one read budget, not a barrier and two seconds per data node.

Four are about **the whole fleet** and are answered as they come, from the first node that gives them: `log_full` (every node appends its barrier to the one log at the one ceiling), `broker_refused` (the broker that refuses one node's barrier refuses every node's), `broker_stalled` (a log one member stores takes every node's barrier through that member's one write, so its stall holds them all) and `maintenance` (every node of a fleet in a capacity window is in one).

Every refusal is about **one domain's copy**. A node whose tracker copy refuses `stalled` or `below_floor` still answers knowledge-base reads from its own pages, and the estate's router sends a seat's call on the tracker to a data node whose tracker copy is in service while the node's other logs keep answering here ([ADR-0079](https://github.com/crewlet/crewlet/blob/main/adr/0079-a-domains-fault-takes-only-its-own-copy-out-of-service.md); see [how a request reaches the estate](../concepts/scaling.md#how-a-request-reaches-the-estate)).
### A write's refusals

A **write** refused by the log names a reason from the same vocabulary, and a
reason spelled like one of the codes above agrees with it about waiting. Every
surface that writes to a state log (`/worklet`, `/doclet/pages`, `/chatlet`,
`/artifacts` with its datastore's `/artifacts/{id}/data`, `/artifact-folders`,
`/brand`, `/operator/act`, `/iam`, `/auth` and `/assistant`) answers every
reason `503`, with a `Retry-After` only for the five that clear on their own;
the other thirteen carry none, because the same write is refused the same
however often it is sent here.

| Reason | What happened | What to do |
|---|---|---|
| `behind` | This node has not yet applied your own previous write, so a decision here would read a state you have already moved. | Wait; it clears on its own. |
| `below_floor` | This node is below the trim floor, where the retry-at-zero a trimmed anchor needs could overwrite a peer's committed write. | Wait; it catches up or adopts a snapshot on its own. |
| `floor_unknown` | The trim floor could not be read, and the third value blocks: failing open here is a lost update. | Wait; it clears the next time the floor is read. |
| `usage_unknown` | This node could not read the log's usage before an ordinary append, so it could not check the room kept for gate records, and admitting blind could spend it. Nothing was appended. JetStream answers this way while a cluster's meta group has no leader (`503`, `err_code` 10008), as when a member steps down. | Wait: it clears the next time the usage reads, typically once the election ends. Another node may take the write meanwhile, and Gitlet hands a push to the repository's next committer at once. |
| `eviction_unknown` | This node could not read its own eviction state, and publishing under an eviction nobody can see produces records every node drops. | Wait; it clears when the state reads again. Deliberately not `evicted`, which no wait clears. |
| `evicted` | This node has been removed from the fleet; nothing it publishes is applied anywhere. | Readmit it, or write through another node. |
| `deferred` | This node holds a record it cannot decode whose scope covers this object, so its rows are stale. | Write through another node, or upgrade this one. |
| `deleted` | The object carries a permanent deletion marker. | It stays deleted. |
| `abandoned` | The record was written in a generation a reanchor abandoned (one only a node the fleet has since evicted held), so it produces rows nowhere. | Nothing to retry: the rows it was decided from are on no disk the fleet still has. Make the change again as a new operation. |
| `overtaken` | The record was written after a restored reanchor, by a node the move had overtaken before it learned of it, from rows the reanchor did not keep. | The same: make the change again, under a fresh operation id, on a node on the new generation. |
| `log_full` | The log is at the byte ceiling its ordinary appends are held to, and refuses appends rather than dropping records. On a log that keeps a [gate reserve](retention.md#the-gate-reserve) that is the reserve below the broker's own. | Raise the ceiling with `crewlet retention set-capacity`, or unblock the trim; `crewlet retention status` names the term. |
| `record_too_large` | The record is larger than its log takes in one message, whatever room the log has. Three limits reach it, and the detail names the record's size and which one refused it: (a) the log's own declared largest record, which this node refuses before anything is sent and the broker enforces as the stream's `max_msg_size` for a peer on another build: 8 MiB less 4 KiB on the tracker's, the knowledge base's, the vector changelog's, the usage log's and the code log's (the largest an 8 MiB message carries once signed), 128 KiB on the identity estate's, 2 MiB on the artifacts log's, 1 MiB plus 64 KiB on the artifact datastore's, 2 MiB on Ask Crewlet's, 512 KiB on chat's and 128 KiB on the executor log's; (b) the NATS server's `max_payload`; (c) the file store's per-record limit. | Splitting the change into smaller writes answers all three. Otherwise: raise `max_payload` to 8 MiB on every server of an external NATS cluster, and on the account the engine signs in to where that states a limit of its own. A node refuses to start against a server that holds it to less, so the refusing one is a server it has reconnected to since: a cluster member configured apart from the others, or one restarted with a smaller limit (the embedded broker's is 8 MiB, and nothing changes it). A limit lowered by a live reload is not told to the client: the server closes the connection over the record, and the node [stops itself](deployment.md#an-external-nats-server), unless the client reconnected without reading why; then it reads the lowered limit on the reconnect and the refusals that follow are this one. Nothing raises a log's declared largest record (its gate reserve is sized by it) or the file store's limit, and no ceiling or trim changes them. |
| `broker_refused` | The broker refused to store the record for a reason it named and this build has no remedy for, such as a sealed stream or a JetStream store with no resources left. The detail carries the broker's code and words. Deliberately **not** this: a record under the write's own operation id still being committed, and a store that closed under a record raft had committed. Each may yet land, so the write answers `unknown` with its operation id instead. | Act on the broker's words; asking again changes nothing. |
| `skew` | The broker answered a last sequence below an expectation this node formed, which a healthy stream never does. | A store or stream was restored out of step; see [Retention](retention.md#re-anchoring-a-recreated-or-restored-log). |
| `op_reused` | The write's operation id already names a record on another object: an operation id names one write. Nothing was written. | Send the write under a fresh operation id. |
| `log_truncated` | The log lost records a peer's rows hold: a broker restored from an older copy, beside a peer whose rows are newer. | Settle which history the fleet keeps; see [Retention](retention.md#re-anchoring-a-recreated-or-restored-log). Writes refuse until then, because one made now is one the restored reanchor of that peer would apply nowhere. |
| `wrong_stream` | The log under this domain's name is not the one this node's rows were derived from; the read refusal of the same name lists every finding. | The remedy that refusal names: an operator's re-anchor, or this node's own adoption where a peer re-anchored past it. |
| `superseded` | This operation's record landed, and a later record on the same object has since undone or replaced it (an eviction retried under its id after a readmission took the node back), so a retry is not a new write of it. | If you still want the effect, start a new operation under a fresh id. |

A record already durable on the log that a gate then dropped is refused under
the gate that dropped it (`evicted`, `deleted`, `abandoned` or `overtaken`),
because the gate says why; republishing it writes another record the same gate
drops.

Eight write reasons have a read twin spelled the same, agreeing with it about
waiting: `behind`, `deferred`, `below_floor`, `floor_unknown`, `evicted`,
`log_full`, `broker_refused` and `wrong_stream`. The other ten only a write
meets:

- `eviction_unknown`: a write's fence reads this node's own eviction rows
  before every append, and that read can fail; a read takes the eviction flag
  this node's health already holds, and answers `evicted` from it.
- `usage_unknown`: a write's admission reads the log's usage before every
  ordinary append. A read's barrier is admitted the same way, and when that
  reading fails the barrier is never appended, so the read is refused as a
  barrier nobody confirmed (`no_quorum` on a log several members store,
  `broker_unreachable` or `broker_stalled` on one), in a sentence saying it was
  never appended.
- `deleted`: a create that would bring back an object carrying a permanent
  deletion marker. A read of that object finds nothing.
- `abandoned` and `overtaken`: what became of a record already durable on the
  log. A read appends only a barrier, which a gate never drops.
- `record_too_large`: the one record a read appends, a barrier, is a few
  hundred bytes.
- `skew`: a read forms no expectation.
- `op_reused` and `superseded`: what the operation ledger says about the
  write's own operation id. A read carries none.
- `log_truncated`: this node's own rows are the log's history, so it keeps
  answering reads; only a write could land where a restored peer would apply it
  nowhere.

## Completeness is a different fact from freshness

Every set answer carries **`complete`** beside its level, and the two fail
independently.

An answer can be fresh and incomplete. A record this node cannot decode is
retained rather than applied, and if it touched something the read is about, a
row that would have entered the set has no local trace. The level says nothing
about that, so `complete: false` is its own field, with an `incomplete` block
giving how many records could intersect, the lowest position among them, and
the scope they declared.

It does **not** name the objects: enumerating them would disclose neither the
direction of the difference (a row that would have entered the set or one that
would have left it) nor reliably the right identifiers, and it would truncate.
`direction` is always `"unknown"`, a field rather than an omission so a reader
meets the fact instead of inferring it.

What a read is ABOUT is the object it names, however named. A listing or feed of
one knowledge-base space is scoped to that space's canonical key, so `eng` and
`ENG` are one question. A read of one page is judged on the page it resolved to
(by id or by `SPACE/Title`) in the snapshot that read it, and refused `deferred`
when a retained record covers that page. A page no row holds is refused the same
way while a retained record may be the one that creates it (one about that page
or address, or about a whole space, since a record about a space may write any
page in it), rather than answered "not found". The node decides that in the
snapshot it read the page from, so a record retained while the read was waiting
is seen too.

## What no level can strengthen

Three things are outside this vocabulary, and a stronger level does not touch
them:

1. **An apply bug.** N nodes applying one ordered log deterministically produce
   N identical copies *including of a bug*. Every level agrees on the wrong
   thing.
2. **A record this build cannot decode.** It is retained, not applied.
   `linearizable` establishes a position past it and still cannot show what it
   would have written.
3. **What another company's system did.** A level is a statement about this
   log. A Jira issue that changed a second ago is not in it.

## Read-your-trigger is a floor, not the mechanism

An agent woken by a change to Worklet, Doclet or Chatlet sees that change. The
*wake* guarantees it by carrying the record's own position, `trigger_position`
(where on its log the change was committed; see
[the event system](../concepts/event-system.md)), not the level the turn's tools
read at:

1. Before the turn's first read, the node hands that position to its
   **read-your-writes floors**: one table per node, the same one every write the
   node makes raises.
2. Every tracker, knowledge-base or chat read the turn makes carries the floor
   on its own domain's log to whichever node answers it (a tracker read the
   tracker log's, a page read the knowledge base's, a room, a thread or a
   mention list a chat read's), this node's own copy included.
3. That holder waits up to two seconds to have applied it, or answers `behind`
   and the next holder is asked. A holder asked by another node waits only as
   long as that node's attempt leaves it, less a second for the answer's trip
   back, so its `behind` arrives while the asker is still listening, never as
   the silence of a gone node.

The floor travels with the request because the node running the turn may hold no
data and have no applier to wait on. A floor on one domain's log never holds up
a read of another.

**What the floor does not reach.** The ranked searches (`search_knowledge`,
`search_work_items` and the turn-start knowledge block) read an index each node
builds behind its own rows, so no log position describes them and they carry no
floor: a seat woken by a page created a moment ago may not find it by search
yet, and finds it by listing or by its id. `search_messages` is half of each:
which rooms it may rank over is a chat read and carries the chat log's floor,
while its ranking index is the node's own and carries none, so a seat woken by a
message may not find it by search yet and finds it by reading the room. A
vendor's wake (a Slack message, a Jira issue) carries no position, because what
changed is in another company's system.

So a seat's `linearizable` reads do not make it see its own trigger. They buy
the other half: that an answer the turn *decides* on is not one from before the
read arrived. The operator's reads are `linearizable` too and not settable: the
person asking is deciding something about their own company, and operators are
few.

## The coordination store is not on this scale

Leases, budgets, the activation pointer, the trim's positions and every other
record in the [coordination store](../concepts/coordination.md) are not a log a
node applies, so none of the four levels describes them and none can be asked for. They have one fixed
level: **every read is answered from the bucket's stream leader** (a key by the
leader itself, a listing by a pass the leader closes before any row is handed
over), so it reflects every write acknowledged anywhere in the fleet before it
began: read-your-writes, whichever node made the write.

It is not `linearizable` in this page's sense, for the reason [How
`linearizable` actually works](#how-linearizable-actually-works) gives: a leader
cut off from its quorum answers from its own copy until it notices (ten seconds
without a quorum, checked every ten) while every write through it fails.
[Coordination § What a leader read does not
promise](../concepts/coordination.md#what-a-leader-read-does-not-promise) says
why no coordination read pays a barrier to close that window.

## See also

- **[Replication](replication.md)** — what the two positions in every answer
  mean, and what the log does and does not promise.
- **[Retention](retention.md)** — the trim, the terms that block it, and what a
  full log costs.
- **[API endpoints](../reference/api-endpoints.md)** — the level header and the
  refusal shape on the wire.
