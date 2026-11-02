# ADR-0037: A datastore write is decided where it is applied, from authority stamped at the API

- **Status:** accepted
- **Authority:** `internal/artifactdata`
- **Enforced-by:** `internal/artifactdata.TestAConditionalWriteIsSkippedIdenticallyOnEveryNode`, `internal/artifactdata.TestTheWriterReadsItsVerdictFromItsOwnRows`, `internal/artifactdata.TestTheApplierDecidesFromTheStampedAuthorityAlone`, `internal/artifactdata.TestTheApplierReadsNoOtherDomain`
- **Tag-status:** unreleased

## The decision

An artifact's datastore is a state-log domain of its own, `artifactdata`, with
**one sequenced log per store**: every write to one artifact's store is a record
on one subject, and every data node applies that subject's records in the one
order the log gives them.

The framework's usual write shape (take one snapshot, decide inside it,
publish with an expectation, let the broker arbitrate,
[ADR-0002](0002-the-stream-is-the-write-ahead-log.md)) is the wrong fit here,
so the datastore uses a third, named one, `statelog.PatternSequenced`:

- **The writer forms no expectation** and reads nothing to decide the effect.
  It checks what must be checked before anything is appended (the caller's
  authority, the request's shape, an early quota from a snapshot) and
  publishes.
- **The log orders.**
- **The applier decides**, as a pure function of the record and the rows of
  its own log: the grammar of collection, document and field names, the
  collection's schema and field rules, each op's version guard, the store's
  caps and the stamped limits. A batch is all or nothing, and the verdict
  (`applied`, `conflict`, `invalid`, `forbidden`, `quota`, `gone`) is written
  for every record, skipped ones included. Because it is a pure function in
  log order, every data node reaches the same verdict, and the writer learns its
  own by reading its applied rows back by operation id.

**Whatever needs IAM or the org chart is decided at the API and stamped into
the record**: the caller's effective role on the artifact, whether the admin
path admitted them, the actor and its kind (a person or an agent), the plan
quotas in force on the deciding node, the surface the write came through and
the work item the turn was charged to. The applier enforces the stamp. **It
never reads another domain**: the identity and artifacts logs apply at their
own pace on each data node, so a verdict that read them would differ between nodes
that had applied them to different points.

Datastore writes are HTTP requests with an `Idempotency-Key`, as every write
the dashboard makes is ([ADR-0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)),
never frames on the socket.

The stamp states what was true when the API decided, and that is the honest
bound on a revocation: **a share narrowed while a write is between its decide
and its apply does not stop that write.** Every write decided after the
narrowing reached the deciding node's copy of the artifacts log is refused;
the window is that log's lag on the deciding node plus the write's own flight
to the broker, and the record carries the role it was admitted under, so the
history says exactly what happened.

## Why the obvious alternatives are wrong

**One subject per document, arbitrated by the broker**, is the framework's
default and fails twice. A broker expectation lives on one subject, so a batch
touching ten documents (a pasted range, a column reorder) cannot carry ten
expectations; and putting every document of a sheet on one arbitrated subject
serialises every writer through compare-and-set rounds, so thirty people
clicking exhaust `statelog`'s sixteen rounds into "it kept changing". Most
sheet writes are blind besides (tick this box): there is nothing to decide
from a snapshot.

**An applier that reads the authority table, the chart or the share rows** is
the obvious way to keep one source of truth, and it breaks the one property an
applier must have: two nodes applying the same record must write the same
rows. Those rows live in other domains, applied at other positions on each
node.

**The coordination store**, holding documents under compare-and-set, is for
bounded, short-lived agreement with a bucket's age as its retention
([ADR-0003](0003-fleet-agreement-state-lives-in-coordination.md)); a sheet's
rows are neither. **A compacted changelog**
([ADR-0014](0014-a-compacted-changelog-is-the-fourth-answer.md)) has no total
order across subjects, so it can give a store no atomic batch and no history.

**Operational transforms for concurrent text** answer a question the motivating
case does not ask (it is checkboxes and chips) at the price of two
implementations of a subtle algorithm in two languages. A long-text field is a
guarded whole-field write plus append in this release; co-editing text later
rides the same log.

## What the spikes confirm

Spikes S2 and DS-1 measure the datastore's write rate in rows per second
through the real publisher and applier, on one node and on a three-member
embedded fleet; DS-2 measures the real on-disk footprint of a 10 000-row store.
The method and the numbers are in
[`docs/reference/artifact-capacity.md`](../docs/reference/artifact-capacity.md).
The rate limits and caps in `internal/artifacts` follow the measurement; the
decision does not.

## What this does not decide

It does not decide **who holds which role**, which is
[ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md), nor **what
the collection and field rules are**, which are `internal/artifactdata`'s. It
does not decide **watches and triggers** on data, which are a later phase and
never decide anything on anybody's behalf. And it does not decide **where an
idle store's rows live**, which is
[ADR-0039](0039-a-cold-store-lives-in-object-storage-and-returns-through-the-log.md).
