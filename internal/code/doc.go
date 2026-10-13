// Package code is THE CODE DOMAIN: the company's git repositories as records on
// one ordered log, CREWLET_CODE_LOG — the state log's fifth domain, strict and
// identity-claiming.
//
// # A repository is rows
//
// A repository's settings, its protection rules, its refs and the list of packs
// it holds are replicated tables, and only this package's applier writes them:
// every change is ONE record on the code log, arbitrated at the broker and
// applied into N identical SQL copies with the checkpoint in the same
// transaction as the rows. That is ADR-0002 — the stream is the write-ahead
// log, these tables are derived from it — and [internal/statelog] is the
// record's authority.
//
// The BYTES are not rows. Every push stores ONE object in the object store —
// its pack BUNDLE, the pack and the two indexes the receiving node's
// receive-pack wrote for it, back to back (bundle.go) — and a code_packs row
// names it. That table is declared [objstore.Required] ([PackReferences],
// ADR-0026 and ADR-0033), so the collector keeps every object a live row names,
// the audit checks each one and a backup carries them. A git node's bare
// repository is never the record: it is a cache derived from these rows,
// disposable, rebuilt from them whenever it is lost or wrong.
//
// # The bundle is the permanent format, and no stored pack reaches 2 GiB
//
// One object a push — one PUT, and one GET on every git node that installs it,
// with no index-pack anywhere but inside the receiving receive-pack — because a
// cold sync from S3 is bound by how many GETs it makes, and re-indexing what it
// downloaded would make it bound by CPU on every git node. The format stays
// EXACT for ever by a rule rather than by luck: a pack index grows a table of
// 64-bit offsets only past 2 GiB, and NO WRITER OF A STORED PACK MAY PRODUCE
// ONE. A push is held to [MaxPackBytes], under 2 GiB, and whatever later
// rewrites stored packs — a compaction — caps what it writes with
// pack-objects --max-pack-size below the same line. So both indexes' sizes are
// pure functions of the object count ([IndexBytesFor], [ReverseBytesFor]) and
// [CheckBundle] compares sizes rather than parsing an index to learn them.
// TestTheBoundsFitTheReadAndPurgeBudgets pins MaxPackBytes below 1<<31.
//
// # The subject is the arbitration unit
//
// Everything that moves a repository's refs or its pack set, or governs them —
// a push, a settings change, a deletion, a restore, a purge — is a record on
// repo.<id>, ONE SUBJECT A REPOSITORY. Two pushes to one repository therefore
// contend at the broker and exactly one record lands per round, and the loser
// re-decides against the winner's rows. One subject a repository rather than
// one a ref, because every write on a repository updates its row too (its ref
// count, its bytes, its version), so a per-ref subject would never narrow a
// write's contention — it would only add a second arbitration the first
// already makes. A NAME is claimed on name.<token>, so two creates of one name
// contend there, where two fresh repository ids never would ([NameToken]).
//
// # Group commit
//
// Because every push to a repository contends on its one subject, a
// repository's pushes are committed by ONE goroutine on the deciding node
// ([Committer]), in the order they arrive, and each record carries every push
// that was waiting when the last one finished — without lingering for more, up
// to [MaxPushesPerRecord] pushes, [MaxRecordBytes] of entries and
// [MaxRecordRows] of writes: an idle repository pays one record a push and a
// burst one record a batch. A batch is decided in ONE snapshot, each push
// against what the ones before it did (decide.go's order of rules), and
// answered from the push rows its record applied — never from the decision,
// so an ok is only ever reported for a ref that moved. A push id the
// repository's history already holds is answered from its row, which is what
// makes a push forwarded twice, or decided again on another node after a
// failover, one push. A queue holds at most [CommitterInbox] pushes and
// [CommitterInboxBytes] of entries, and the node at most [CommitterNodeBytes]
// across its repositories — from a push's queueing to its caller's answer —
// and past any of them a push is refused busy at once; the estate answers a
// node's proposals on a lane of their own as wide as one repository's whole
// queue, so these bounds, never that lane's, are what a burst meets. A batch
// is bounded by [CommitBudget], half the write attempt the estate
// router gives the node it asked, and a push starts one only while a budget and
// [ReplyMargin] are left before its caller's deadline — the asker's attempt,
// which a routed proposal carries — and is refused busy rather than decided the
// moment they are not, while it waits as well as when its batch forms; one
// asked with no deadline may wait less than a budget, and one whose caller has
// gone is never decided, so every push a committer decides is answered inside
// its asker's attempt — never after the asker has handed the same push id to
// another node.
//
// WHICH NODE DECIDES is the estate router's: a push is asked of its
// repository's committer, the first live data node in the rendezvous order of
// the repository's id, so every node routes one repository's pushes to the
// same committer and its group commit holds them (internal/estate's "A push is
// asked of its repository's committer"). A data node serves this package to
// the router through ONE adapter, [Served] — its committer and its reader —
// and a committer that decided nothing answers [ErrNotServing], which the
// router reads as a node that ran nothing and asks the runner-up.
//
// # The activation fence
//
// A repository code.repos declares is created, settled, deleted and restored
// by the reconcile of each configuration activation, and every data node runs
// that reconcile. Without an order between activations, a node applying an
// OLDER activation late would walk a newer one's settings back. The order is
// one record per activation rather than one per repository: [Store.Activate]
// publishes an [Activated] record on the company subject FIRST, and its apply
// raises one replicated row, code_activation, to the activation and never
// lowers it. The applier then refuses — applies as nothing, identically on
// every node, because the fence is derived from the log — every config-managed
// create, settings record and lifecycle record whose activation is below the
// fence at its position.
//
// WHY THAT IS SAFE. Every decision of an activation is taken from a snapshot
// that already holds its own fence record (each decide waits for this node to
// apply it — the session floor of its own append). So every config-managed
// record an OLDER activation decided is either EARLIER in log order than this
// fence — and then it is in the rows this activation decides from, and any
// difference it made is written back by a settings record — or LATER, and then
// the fence refuses it on every node. Nothing an older activation decided can
// survive a newer one, and the newer one therefore needs to write nothing for
// a repository whose declared settings did not change: an activation that
// changes nothing costs its one fence record, not one record per declared
// repository. A deletion and a restore keep their own comparison against the
// activation that last wrote the row, because whether a removed repository
// comes back is a question about which activation removed it.
//
// AND IT COSTS ONE READ OF THE DECLARED SET, not two publishes a repository:
// [Store.EnsureRepos], the reconcile's form, reads every declared claim, row
// and rule in the one snapshot a publish on the fence's subject decides from,
// and sends only what changed — a new name, different settings, a restore —
// through the per-repository path ([Store.EnsureRepo]). That snapshot passes
// every check an empty per-repository decision passes, and its deferred-scope
// probe covers the whole domain, so the answer it gives is the one those
// decisions would have given. So is its refusal: a deferred record is the one
// a narrower scope answers differently, and sends every declaration down the
// per-repository path; any other — the activation's session floor out of
// reach, a later activation's fence, the node's own fences — is every
// declaration's answer from that one publish, rather than one wait of the
// write budget each for the same refusal.
//
// # What this package does not do
//
// It runs no git, moves no object bytes and serves no HTTP. It decides and
// applies records, commits pushes a receiving node proposes, answers reads of
// the rows, and holds a bundle's bytes to the row that names them when a
// caller tees them through a [SealWriter].
//
// # Three horizons, each a function of the record that applies it
//
// The applier keeps three horizons inside a record's OWN repository, each
// chosen by the record's broker instant ([statelog.ApplyOptions.StoredAt]) and
// never by a sweep, because a sweep would be a second writer of the replicated
// estate:
//
//   - PUSH HISTORY, [PushHistory] (181 days, usage.History): a push record
//     removes up to [HorizonDeletesPerRecord] of its repository's push rows
//     older than that, and no more once their bytes reach
//     [HorizonBytesPerRecord] — the most a record can add, so removal keeps
//     pace, and no more, because a push row is the estate's one wide row and
//     its delete frees its overflow pages — so "what happened over the last
//     half year" has one answer across the engine;
//   - REF TOMBSTONES, [RefTombstoneHorizon] (48 hours): a deleted ref is kept
//     as a tombstone so a cache syncing a delta learns of the deletion, and a
//     push record sweeps its repository's expired ones — as many as the
//     record leaves of [MaxRecordRows], because one record can make three
//     thousand and a fixed share fell behind any repository that deletes
//     refs in bulk — and raises the mark past which a delta is no longer
//     complete;
//   - A PURGE, in bounded parts: a repository deleted longer ago than its
//     retention is removed [PurgePartRows] rows a record, its push rows only
//     until their bytes reach [PurgePartBytes] ([Store.PurgeStep]), and the
//     final record — formed only once what is left fits a part — leaves only
//     its deletion marker, which drops every later record about the
//     repository on every node, for ever. It is the one horizon no record
//     of the repository's own can reach, since nothing else is written to a
//     deleted repository, so the code-retention duty's pass ([Retention])
//     publishes its steps: every purge already begun first, whatever the
//     retention, then the deleted oldest first. It publishes RECORDS and
//     never touches a row, so the applier is still the estate's only
//     writer, and it PACES them, because every data node applies every one
//     on its single write lane: a pass waits twice a step's duration after
//     each ([RetentionLaneShare], a third of the lane) and takes steps for
//     [RetentionPassBudget] at most, so a node up to three times slower
//     than the holder keeps up with no queue and one up to six times
//     slower ends a pass at most statelog.StallGrace behind. A pass cut
//     short leaves a repository purging that the next resumes from its
//     rows, and a purged name is handed on only once this node's rows hold
//     the purge.
//
// # Measured
//
// On four vCPUs of a 2.1 GHz Xeon shared with other builds, with Go 1.27 and
// the pinned Turso driver (go test -bench, four counts of five, the range
// given). A record APPLIED AND COMMITTED holds the replicated estate's one
// write lane — every other domain's applier on the node waits behind it, on
// every data node — and statelog.ApplyTxTimeBudget (250 ms) is checked only
// at a record's boundary, so each is held to it here.
//
// A PUSH RECORD (BenchmarkApplyAPushRecord): 70–75 ms at the largest record
// the committer forms (three pushes of [MaxCommandsPerPush] ref creations,
// 3 007 rows, against a repository of 10 000 refs), and 81–88 ms when expired
// tombstones wait behind it and it sweeps the 992 its rows leave; and no
// slower, 74–87 ms, with [HorizonDeletesPerRecord] expired push rows of the
// widest a row can be (461 000 bytes) waiting as well — the longest shape a
// push record has — because the history's removal stops at
// [HorizonBytesPerRecord], three of them, where a bound of rows alone removed
// sixty-four (29 MB) and added 60–82 ms; 10–12 ms for a group commit of
// [MaxPushesPerRecord] pushes of one command each, which is 0.16–0.19 ms a
// push against 3.0–4.0 ms for one push alone — the commit, not the rows, is
// a small record's cost, and group commit is what pays it once; and 53–66 ms
// for one small push whose record sweeps a whole row budget of expired
// tombstones (3 995).
//
// A PURGE RECORD (BenchmarkApplyAPurgePart): 56–60 ms for a part of
// [PurgePartRows] push rows of a kilobyte; 81–88 ms for a part over the
// widest push rows, which [PurgePartBytes] stops at seventy-three (34 MB) —
// bounded by rows alone, a part of 1 000 such rows held the lane 1.05–1.10 s,
// about 2.4 ms a megabyte, and one of [PurgePartRows] about three seconds;
// 76–103 ms for the most one part can take, the widest rows short of
// PurgePartBytes and refs for the rest of the rows; and 57–65 ms for the
// final purge over the same, with [MaxProtectionsPerRepo] rules.
//
// A RECONCILE (BenchmarkAnUnchangedActivation): an activation that changes
// nothing, over ten thousand declared repositories — the most a configuration
// may declare — is 471–593 ms of one node's reads through
// [Store.EnsureRepos], whose one publish reads every claim, row and rule in
// one transaction, against 1.7–2.0 ms a repository through [Store.EnsureRepo]
// — seventeen to twenty seconds of every data node's reads at ten thousand,
// for an activation that appends one fence record.
//
// A DECIDE ROUND (BenchmarkDecideABatch, four counts of twenty) holds no write
// lane — it is one read transaction on a connection of the replicated
// estate's reader pool, on the repository's one committer goroutine, and it
// is paid again on every compare-and-set round a peer's record forces: 15–16
// ms for three pushes of [MaxCommandsPerPush] ref creations against
// [MaxProtectionsPerRepo] rules on a repository of ten thousand refs, where
// asking each rule in its one-off form — the pattern parsed and the ref
// checked again for every command and rule, 192 000 times — took 83–87 ms;
// and 2.1–2.7 ms for a group of [MaxPushesPerRecord] one-command pushes
// (3.7–3.8 ms one-off). The rules are parsed once a round ([Protections]).
//
// A READ: one full page of a snapshot's refs ([MaxSnapshotItems]) from a
// repository of [MaxRefsPerRepo] refs is 57–64 ms to serve, 5.7–6.4 µs a
// ref, nearly all of it the driver's scan; measuring the page's rows costs
// 0.08–0.09 µs a ref and no allocation, where encoding each one to measure it
// cost 0.78–0.83 µs and three (BenchmarkServeASnapshotPage).
package code
