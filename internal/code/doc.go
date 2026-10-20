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
// disposable, rebuilt from them whenever it is lost or wrong — which holds
// only because a repository's refs and packs move by records on its own
// subject and by nothing else, receive-pack never runs in a cache, and git is
// told `ok` only from a push's applied row (ADR-0036).
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
// # What a push costs, measured
//
// THE COMMIT CHAIN (internal/code/codecluster's BenchmarkCommitChain): a
// proposal to the committer, the batch it forms, the publisher's snapshot,
// decide and append, the committer node's applier and the answer read from
// the push's row — everything a push pays once its receiving node has stored
// its bundle. On four vCPUs of a 2.1 GHz Xeon, Go 1.27.2, NATS server 2.15.0
// and the pinned Turso driver (0.8.1), every broker fsyncing each write as
// stream.sync's default does, three counts of three seconds, the range given:
//
//   - ONE MEMBER, proposed to in process: a lone push is 7.7–7.8 ms at the
//     median (14 ms p99), 121–125 records and pushes a second; eight at once
//     are two records of four — no linger, so the first is in flight alone
//     while seven queue — 460–471 pushes a second at 115–118 records;
//     sixty-four at once are two of thirty-two, 2 670–2 730 pushes a second
//     at 83–85 records, a push's median 22–23 ms.
//   - THREE MEMBERS of one cluster on one machine — its four vCPUs and ONE
//     disk shared, so a record's three fsyncs queue on one device where a
//     deployment's members each sync their own, and its copies cross
//     loopback where theirs cross a network: what three machines reach
//     differs both ways — each record on all three before it is
//     acknowledged, proposed to the committer in process: a lone push is
//     10.5–11.1 ms at the median, 86–89 a second; 330–339 pushes a second at
//     eight and 2 050–2 090 at sixty-four, 64–65 records.
//   - A FULL QUEUE: 256 pushes at once are five records — the first goes
//     alone while the rest queue behind it, the others full to
//     [MaxPushesPerRecord] — 2 720–2 830 pushes a second; and
//     [CommitterInbox], 1 024 at once, drains in 0.33–0.34 s in seventeen
//     records, 3 010–3 090 pushes a second, its last push answered within
//     0.35–0.38 s (p99).
//   - THE ROUTER HOP, a push proposed through a member that is not its
//     repository's committer as every push received elsewhere is: about
//     1 ms at the medians on a lone push (11.7–12.3 ms, 77–82 a second),
//     306–317 pushes a second at eight, and 1 270–1 390 at sixty-four, its
//     median 38–41 ms and p99 94–97 ms where sixty-four proposals cross the
//     wire at once.
//   - MANY REPOSITORIES, every push to a repository of its own and every
//     committer reached through member 0's router, the committers spread
//     over the three members by rendezvous: a record carries one
//     repository's pushes, so eight pushes at once over sixteen
//     repositories are eight single-push records, 148–157 a second across
//     the fleet against 290–294 pushes a second when the same eight go to
//     one repository; sixty-four at once over sixteen are 146–172 records a
//     second of two pushes, and over sixty-four 201–225 records — and
//     pushes — a second, against 1 270–1 390 pushes a second to one: eight
//     times the records in flight for under half again the rate, a push's
//     median 250–280 ms, so this machine's record ceiling is near the top of
//     that range.
//
// So the chain is a record's round trip, 8–12 ms, and group commit is what
// scales it: one repository's burst of sixty-four is over twenty times a
// lone push's rate, and its full queue over thirty-five. What bounds pushes
// that do NOT share a repository is the log's RECORD rate: one stream
// carries every repository's records and a broker fsyncing each write
// appends each of them alone, about 150 to 225 records a second on three
// members of this machine whatever the members' write lanes could apply —
// sixty-four single-push records are some 900 rows a second, under the
// lane's published floor of about 2 000 (docs/guides/replication.md, the
// loaded CI runner). One repository committing sixty-four a record, by
// contrast, applies some 8 000 rows a second on every data node, past that
// floor, which this machine's applier sustains because such a record is one
// transaction of 10–12 ms (# Measured): there the lane is what a busier
// node would meet first. A company whose pushes spread over many
// repositories at once — a CI fleet tagging each one — meets the record
// rate long before either, and per-repository group commit gives it
// nothing: no record carries two repositories' pushes, and a data node more
// adds no stream, since one log has one leader.
//
// THE PUSH'S TWO ROWS (BenchmarkPushHistoryBytes, the same figure every
// count), measured as the replicated estate's growth after a truncating
// checkpoint over records whose refs existed before, so a ref's own row is
// rewritten in place and never counted. They have two lifetimes:
//
//   - its code_pushes row, which the push-history horizon keeps for the 181
//     days of usage.History: 395.3 bytes with its index entries for a
//     person's push of one commit — one command on a ref named in 40 bytes,
//     one introduced id (4 000 records) — and 420 004 for the widest a push
//     can leave ([MaxCommandsPerPush] commands on refs of
//     [MaxRefNameBytes], [MaxIntroduced] ids; 100 records);
//   - its pack's code_packs row, 357.3 bytes more with its index entries
//     (752.6 with both), which lives as long as the pack does — until the
//     repository is purged, since nothing in this release compacts a
//     repository's packs.
//
// So every data node, holding the whole estate, holds 395.3 bytes for each
// push of the last 181 days and 357.3 for each push ANY repository not yet
// purged has ever taken: a deployment of 3 000 pushes a day carries about
// 215 MB of push rows at the horizon and grows by about 390 MB of pack rows
// a year for as long as its repositories live.
//
// THE PUSH ON THE LOG (BenchmarkPushLogBytes, the same figure every count):
// the code log's growth over the records the committer forms for that same
// person's push — its one command, its bundle named and its one introduced
// id — is 1 292 bytes a push in a record of its own, and 742.7 a push for
// sixty-four at once to one repository, which group commit lands as two
// records: a record's envelope is about 570 bytes, and each push it carries
// about 725 more. The log keeps the trim's window of records rather than the
// history, so 3 000 pushes a day hold about 27 MB of it over the default
// seven-day min_age, a fortieth of the log's 1 GiB floor, and the bytes of
// a push's pack are never on it: a record names its bundle.
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
// # Who may read and push
//
// A principal's standing on a repository is DERIVED from the chart and never
// stored (ADR-0040): there is no table of who may push where, because such a
// table is a second chart, and it drifts from the first the day a seat moves
// units. [GrantOf] computes it, every time it is asked, from the repository's
// row and a [Roster] — the chart of the node that answers or decides, which
// the engine adapts and this package declares, so a grant is a pure function
// of the two and this package never reads the organization itself. A person
// (an operator credential bound to a human seat) maintains every repository
// and a token bound to nobody reads every one, which needs no chart; a seat
// holds the minimum of its tier's cap ([Tier.Cap], role.code.access) and
// what the chart relates it to the repository by. [Grant.RefRule] judges one
// command against the grant before the protections are judged — a grant is
// never a reason to skip one — and its sentence names what the principal
// holds, what the command needs and a remedy the rule itself would admit.
//
// A full_access seat owns [SeatNamespace] — refs/heads/seats/<handle>/ — on
// every repository it reads, which is where a seat whose relation is below
// write pushes, in place of a fork. Under a root of its own and NEVER at
// refs/heads/<handle>/, because a handle is a role name's slug and people
// name their branches under exactly those (docs/, release/): what is a seat's
// is then decidable from the ref alone. The root itself and one seat's
// namespace as a branch are nobody's ([StatelessRefusal] refuses both
// reserved_ref for every principal, and no repository takes either as its
// default branch), since no ref is created beside a directory of its own name
// — the push decide refuses one ref_conflict ([RefusalRefConflict]), because
// no clone can hold a file and a directory of one name — so a branch named
// seats would block every seat's namespace, and one named seats/<x> the
// namespace of the seat x ([gitref.ReservedSeatBranch]).
//
// A repository's [Visibility] is one row of that relation and no other: a
// seat the chart relates to the repository in no other way reads a
// `company` repository and holds nothing of a `unit` one. It narrows SEATS
// ONLY — a person's standing reads no visibility, as it reads no chart — so
// the people who run the company are never shut out of its code by a setting
// that exists to keep one team's seats out of another's. It is a setting like
// any other, carried whole by every create, settings and restore record, so
// changing it is one settings record and the reconcile's comparison sees it.
//
// # Reads are held to who asks
//
// Every read a principal makes names its ASKER ([ReadQuery.Asker], refused
// absent), and the reader judges it by the grant on the serving node, from
// the chart its [ReaderOptions.Roster] answers — read once a read, so every
// row of one answer is judged by one chart. A listing leaves out what the
// asker may not read and still pages by the rows it SCANNED, so a page may be
// short, or empty with a cursor, rather than end the listing early or scan
// without bound for a seat that reads little; a detail or a history of a
// repository the asker may not read is [ErrNotPermitted], judged after the
// row is read, while one that does not exist is still [ErrRepoNotFound] — a
// unit repository's name is in the company document, and its contents are
// what the grant guards. A reader with no roster holds every seat to none,
// never "every seat reads".
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
//   - REF TOMBSTONES, [RefTombstoneHorizon] (48 hours), or sooner — oldest
//     first — past [MaxTombstonesPerRepo] (100 000): a deleted ref is kept
//     as a tombstone so a cache syncing a delta learns of the deletion, and a
//     push record sweeps its repository's due ones — as many as the record
//     leaves of [MaxRecordRows], because one record can make nearly two
//     thousand and a fixed share fell behind any repository that deletes
//     refs in bulk, and never fewer than the record's own deletions, which
//     the committer counts twice in a record's rows — and raises the mark
//     past which a delta is no longer complete; a record whose deletions its
//     sweep cannot make room for, which no committer of this build forms,
//     removes their rows outright and raises the mark to its own position.
//     So a deletion never counts against the live refs' [MaxRefsPerRepo]
//     and never refuses a push, the refs table holds at most the two bounds'
//     sum, and a delta is answered only while it carries no more rows than
//     the full answer (so a cache synced before a clean-up larger than what
//     the repository keeps reads what is left, never every deletion);
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
//     the purge. A push meanwhile is refused repo_deleted, told which of
//     four states the decide found, because each has its own remedy
//     ([RepoGone]): a DELETED repository is restored by declaring it again
//     until its purge, which may begin from the instant the sentence names
//     (the deletion plus the deciding epoch's code.deleted_retention_days) —
//     the earliest the duty begins it, on a tick at or after that instant,
//     and nothing holds the purge to the instant itself; one being PURGED,
//     or purged, never comes back, and its name declared again is a new,
//     empty repository; and an id no repository had is none.
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
// A PUSH RECORD (BenchmarkApplyAPushRecord, one run): 86–94 ms for the most
// ref creations a record the committer forms carries (three pushes of
// [MaxCommandsPerPush] and a fourth of the 926 its row bound leaves, 3 935
// rows, against a repository of 10 000 refs), and no slower, 86–95 ms, with
// [HorizonDeletesPerRecord] expired push rows of the widest a row can be
// (461 000 bytes) waiting — because the history's removal stops at
// [HorizonBytesPerRecord], three of them, where a bound of rows alone removed
// sixty-four (29 MB) and added 60–82 ms; 96–105 ms for the most deletions a
// record the committer forms carries (1 965 in two pushes, each counted
// twice in its bound) at a repository holding [MaxTombstonesPerRepo] expired
// tombstones, which writes its 1 965 tombstones and sweeps the 2 029 its rows
// leave — every row of MaxRecordRows touched — and 94–114 ms with the widest
// expired push rows waiting as well, the longest shape a record the
// committer forms has, under half of statelog.ApplyTxTimeBudget; 10–12 ms
// for a group commit of [MaxPushesPerRecord] pushes of one command each,
// which is 0.16–0.18 ms a push against 3.4–4.8 ms for one push alone — the
// commit, not the rows, is a small record's cost, and group commit is what
// pays it once; 53–61 ms for one small push whose record sweeps a whole row
// budget of expired tombstones (3 995); and 65–69 ms for a record whose
// deletions are REMOVALS — three pushes of [MaxCommandsPerPush] deletions at
// a repository already holding [MaxTombstonesPerRepo] fresh tombstones, a
// record no committer of this build forms, whose cost the applier bounds all
// the same.
//
// THE LIVE REFS' INDEX (code_refs_live_idx, replicated 0045, which the push
// decide's [RefusalRefConflict] probe reads — the decide below) is an entry
// more for every live ref a record writes, and one moved for every ref it
// deletes or creates again. Measured interleaved with the build before it,
// ten counts of five each on four vCPUs of a 2.8 GHz Xeon at a load average
// of 1.3–2.1: the most ref creations a record carries went from 93–127 ms
// (median 105) to 100–151 ms (median 125), about 5 µs a created ref; the
// most deletions from 97–116 ms to 101–154 ms (median +6 ms); the removals
// from 67–115 ms to 69–93 ms (median +9 ms); and the group, the one push and
// the sweep of tombstones, which the index holds none of, moved nothing past
// the noise (four counts). The slowest record measured with the index there,
// 154 ms, is three fifths of statelog.ApplyTxTimeBudget — and the slowest
// without it, 150 ms, as much: on that machine the history arms' spread is
// the machine's, not the index's.
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
// A CHANGED ACTIVATION (internal/engine's BenchmarkAChangedActivation, the
// reconcile end to end on one booted node, its fence, walk and batch read
// included, one count each, [reconcileInFlight] the width): over the same ten
// thousand, the activation that creates them all is 18.3–19.5 s, one that
// deletes them all 17.2–18.5 s, one that restores them all 16.3–17.2 s, and
// one that changes nothing 0.45–0.53 s. By width, creating them took 67 s
// one at a time, 35 s two, 25 s four and 18–21 s at every width from eight to
// a hundred and twenty-eight — a create's round trip is 6.7 ms alone, and
// the floor is this node's applier and the decisions' own snapshots, about
// 550 records a second. Before every write's anchor read named its index
// (internal/statelog's TestEveryAnchorStatementSeeksItsKey) the same three
// took 21.6, 31.6 and 61.4 s at thirty-two, because each read walked every
// anchor the log had written and so cost more with every repository decided.
// No apply waits for any of it: the engine's reconciler runs beside the
// apply that asks.
//
// A DECIDE (BenchmarkDecideABatch, four counts of twenty, a 2.1 GHz Xeon)
// holds no write lane, and is two parts on the repository's one committer
// goroutine: the FORMATION of the batch's protection verdicts, before the
// publish, holding nothing but a read of the rules; and a ROUND, one read
// transaction on a connection of the replicated estate's reader pool, run
// again on every compare-and-set round a peer's record forces. For three
// pushes of [MaxCommandsPerPush] ref creations against
// [MaxProtectionsPerRepo] rules of a team's shape on a repository of ten
// thousand refs, the formation is 8.4–12 ms and a round 9.7–11 ms, where a
// round that formed the verdicts inside its snapshot is 19–22 ms; for a group
// of [MaxPushesPerRecord] one-command pushes, 0.64–0.96 ms and 2.6–3.8 ms.
// (Asking each rule in its one-off form — the pattern parsed and the ref
// checked again for every command and rule, 192 000 times — took 83–87 ms
// for the three pushes where parsing the rules once took 14–16 ms, and
// 3.7–3.8 ms for the group, in an earlier run.) At the costliest rules a
// repository can carry — every one of gitref's costliest shape, asked about
// as many refs of 255 bytes as a record carries (2 567) — the formation is
// 1.83–2.03 s, 11–12 µs a rule and ref, and the same whether no rule matches
// or every command is refused (1.82–1.91 s), where naming the refusing rule
// with a second walk over the rules made a refused round 3.4 s; and a round
// is 13–22 ms, the reads and nothing else. So a reader is held for the reads
// alone, where forming the verdicts inside the snapshot held one for
// seconds, and four such batches held every reader of the pool; and a
// publish's formation and sixteen rounds cost about 2.4 s of the 30 s
// [CommitBudget], where forming the verdicts every round cost 28–30 s. A
// round whose rules differ from those the verdicts were formed under forms
// them again inside its snapshot: 1.85–2.13 s at that worst.
//
// A batch that creates refs pays the round two reads more, for
// [RefusalRefConflict] — a primary-key seek for every directory its creates
// would make (and the upper bound of a create under another), and a seek of
// the LIVE refs' index ([liveRefsKey], replicated 0045) for every gap: the
// creates' ranges cut at their bounds and at every name with a live row, each
// gap asked once whichever creates hold it. Measured both ways on four vCPUs
// of a 2.8 GHz Xeon under other builds' load (a load average of 15–18, which
// roughly doubles every round above), the three pushes' round went from
// 19–21 ms to 43–44 ms, the group's from 2.4–2.5 ms to 3.7–4.7 ms, and the
// costliest rules' from 28–54 ms to 78–81 ms; reading the gaps over the live
// refs' index, once each, moved none of the three past the noise (43–59 ms
// against 47–63, 4.4–6.1 against 3.5–8.1, 77–91 against 88–117, interleaved
// with the build before it under a load average of 3–7). So the reads follow
// what the batch SPELLS, never what the repository holds — at most a seek a
// directory, one a create's bound and two a create's gaps, plus one a live
// name:
//
//   - what lies under a create costs its probe one seek: one push of the 118
//     nested creates of the deepest chain (refs/heads/a, refs/heads/a/a, …)
//     over [MaxTombstonesPerRepo] tombstones under the deepest is 7–15 ms a
//     round (BenchmarkDecideABatch's buried arm), where a probe over the key
//     stepped over every tombstone once for every create above it: 7.7–9.9 s;
//   - nesting adds a cut, never a gap a create: the 118 nested creates over
//     2 492 more under the deepest is 155–181 ms (fanned), where asking each
//     create for the gaps between the names under it was 3.4–4.5 s and some
//     600 MB of statements;
//   - the deepest batch a record carries — 2 573 creates of refs of 118
//     directories each, the most a ref of [MaxRefNameBytes] spells, some
//     303 000 seeks of the key — takes the first term to its most and is the
//     worst a record carries: 1.2–1.4 s a round (deepest; 1.2–1.5 s under the
//     heavier load above), the order of the costliest rules' formation —
//     twenty such rounds, each forced by a peer's record on the repository,
//     would spend the whole of [CommitBudget].
//
// A READ: one full page of a snapshot's refs ([MaxSnapshotItems]) from a
// repository of [MaxRefsPerRepo] refs is 57–59 ms to serve, 5.7–5.9 µs a
// ref, nearly all of it the driver's scan. A full read's walk of the refs'
// key passes every tombstone in its pages' key ranges, so the most a
// repository may hold ([MaxTombstonesPerRepo]) costs a page by where they
// sort: 62–66 ms, about a tenth more, INTERLEAVED one after every live ref —
// what every page of a whole snapshot pays — and 90–97 ms, about three fifths
// more, CLUSTERED before the first live ref, a clean-up of one prefix — the
// most ONE page pays, since the whole cluster lies in its range; either way
// the snapshot is no page longer, and its walk passes each tombstone once
// (four counts of twenty, one run). Page 1's choice between a delta and the
// full answer costs, at its dearest — a delta from below every row of that
// repository, counted one row past the full answer deep before the full
// answer is served — 8.5–10.8 ms, a sixth of one page of what it then
// serves, and after a push a handful of index entries. Measuring the page's rows costs
// 0.08–0.09 µs a ref and no allocation, where encoding each one to measure it
// cost 0.78–0.83 µs and three (BenchmarkServeASnapshotPage).
package code
