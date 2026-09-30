# ADR-0002 — The stream is the write-ahead log; the SQL estate is derived

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/store.TestOnlyTheApplierWritesTheReplicatedEstate`
- **Measured:** three domains on the framework — the tracker, the knowledge base, the embeddings — and one apply transaction that commits the rows, the record's operation id and the checkpoint together
- **Cost-when-tried:** the shape this must not copy is still in the tree's history. `internal/projection` committed its batch and *then* wrote its cursor, reasoning that a crash between the two replays the batch for free. That is true while the source can always redeliver, and false for a log that gets trimmed.
- **Tag-status:** unreleased

## The decision

A mutation to the company's own tracker, knowledge base or embeddings is
published as **one record on its domain's ordered NATS stream**, arbitrated by
the broker on the subject of the object it changes, and reaches SQL only
through that domain's deterministic applier — which writes N identical copies
in N node databases, with the checkpoint committed in the same transaction as
the rows.

The stream is the write-ahead log. The SQL estate is the derived durable state.
Never the reverse, and never both: local rows are what a node has applied, not
what the company has decided.

Two consequences travel with it and are the ones most often missed:

- **A read of local rows is not an authority.** The write authority is one
  sentence — take ONE snapshot of your own rows, decide and form the
  expectation inside it, publish, let the broker arbitrate, never guess — and
  reading local rows to decide is safe *only* because the broker checks the
  expectation, so a stale snapshot is a claim that gets refused rather than a
  decision that gets committed. That safety holds only while the expectation
  travels with the decision it was read beside.
- **A write has three outcomes**, `applied` / `pending` / `unknown`, never a
  bool. This is [ADR-0005](0005-ownership-is-three-valued.md) one layer down.

## Why the obvious alternative is wrong

The obvious alternative is to write the SQL first and publish afterwards, so
the caller sees its own change immediately. It is wrong in a way that has no
symptom on the node that does it: the write succeeds, the rows look right
locally, and the divergence appears later as two nodes answering one question
differently — by which point the write that caused it is thousands of records
behind in the log, and no amount of reading the current rows says which node is
correct. There is no reconciliation to write afterwards, because neither copy
is the original.

The second obvious alternative is to keep the publish but write the checkpoint
*after* the rows commit. That is what `internal/projection` did, and its
reasoning was sound for its own estate: a crash between the two replays the
batch, which is free. It is not free here, because this log is trimmed. A
checkpoint behind its rows is a node that will one day ask for a position the
stream no longer holds, and the only recovery from that is a full snapshot
transfer from a peer.

## What this does not decide

It does not decide where a fact lives. A row that reaches SQL through an
applier is replicated; a fact the company has to agree on *now* — a lease, the
activation pointer, a counter — is not a record on any log and belongs in
coordination, which is [ADR-0003](0003-fleet-agreement-state-lives-in-coordination.md).

It does not make the applier the only thing that may ever touch a replicated
row. Two writes in the tree are not records, each because no record could own
what it touches, and each is named with its reason in
`allowedReplicatedWriter`. Adding a third is a change to what this record
means, not a line in a table. (There were three; the version reset after a
reanchor was the other, and it went when the arbitration anchor stopped being
the row's `version` and the reset stopped buying anything.)

It says nothing about the store's own `journal_mode=WAL`. That is Turso's local
transaction journal, per file and invisible to any peer. The two share a word
and nothing else.
