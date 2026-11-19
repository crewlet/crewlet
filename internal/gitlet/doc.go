// Package gitlet is THE CODE DOMAIN of Gitlet, the engine's own git host: the
// company's git repositories as records on one ordered log, CREWLET_CODE_LOG —
// the state log's eleventh domain, strict and identity-claiming.
//
// # A repository is rows
//
// A repository's settings, protection rules, path owners, refs and pack list
// are replicated tables only this package's applier writes: every change is
// ONE record on the code log, applied into N identical SQL copies with the
// checkpoint in the same transaction (ADR-0002; [internal/statelog] owns the
// record). The BYTES are not rows: every push stores ONE object, its pack
// BUNDLE (bundle.go), named by a code_packs row declared [objstore.Required]
// ([PackReferences], ADR-0026, ADR-0081). A Gitlet node's bare repository is a
// disposable cache derived from the rows, which holds only because refs and
// packs move by records on the repository's own subject alone, receive-pack
// never runs in a cache, and git is told `ok` only from a push's applied row
// (ADR-0084).
//
// # The bundle is the permanent format, and no stored pack reaches 2 GiB
//
// One object a push — one PUT, one GET per installing node, no index-pack
// outside the receiving receive-pack — because a cold sync is bound by its
// GETs, and re-indexing would bind it by CPU on every node. The format stays
// exact by rule: an index grows 64-bit offsets only past 2 GiB, and no writer
// of a stored pack may produce one ([MaxPackBytes] for a push, pack-objects
// --max-pack-size for a compaction). So index sizes are functions of the
// object count ([IndexBytesFor], [ReverseBytesFor]) and [CheckBundle] compares
// sizes instead of parsing. TestTheBoundsFitTheReadAndPurgeBudgets pins
// MaxPackBytes below 1<<31.
//
// # The subject is the arbitration unit
//
// Everything that moves or governs a repository's refs or packs is a record on
// repo.<id>, ONE SUBJECT A REPOSITORY, so two writes contend at the broker and
// the loser re-decides against the winner's rows. Not one a ref: every write
// also moves the repository's row (ref count, bytes, version), so a per-ref
// subject narrows nothing and adds a second arbitration. A NAME is claimed on
// name.<token> ([NameToken]).
//
// # A change is rows on its repository's subject
//
// A change (web!42) is code_changes and its bodies, authors, revisions and
// moves, written by records on its REPOSITORY's subject only, so its lifecycle
// and the pushes that move its branches meet at one committer, and its number
// (code_repos.next_change) follows log order and never repeats. A table is
// written by one subject's records only.
//
// ITS BODY IS A ROW OF ITS OWN (code_change_bodies): the driver reads a row
// whole, overflow pages and all, whatever columns are asked. EACH WRITE OF THE
// ROW SETS ONLY WHAT IT MOVES ([changeWrite]): the driver rewrites every index
// whose key or predicate names a written column, so a head move touches
// code_changes_target_idx alone (TestAChangeWriteRewritesOnlyTheIndexesItMoves).
// A purge takes changes WHOLE, dependents first ([purgeChanges]), weighing
// bodies and comments with push rows in one byte budget, [PurgePartBytes],
// since overflow pages are what a row count misses.
//
// ITS LIFECYCLE IS THE COMMITTER'S: each lifecycle write is a [ChangeProposal]
// the committer decides ALONE in its push queue (decide_change.go), its
// operation id the asker's ([OpenChangeID]), a seat's judged by the deciding
// node's chart ([SeatRules]); a node that cannot judge a seat decides none. A
// LANDING ([ChangeLanded]) is the home Gitlet node's own write, since only git
// says one commit holds another. The home's work list is the rows
// ([ReadChangesDue]). ITS REVISIONS ARE ITS PUSHES' (revisions.go): a push's
// apply revises, merges or closes the live changes it moves, and the decide
// moves the same model ([changeBook]), so it counts those rows against
// [MaxRecordRows]. A change's options never fail its push ([ChangeAnswer]).
// Reads walk the ONE index their filters lead, held to [JudgeRead]; word
// search uses this node's own index (ADR-0156).
//
// A CHANGE MERGES BY ONE RECORD ITS GATE DECIDES (merge.go, ADR-0096): the
// committer's decide proves in ONE snapshot that the change is open at the
// merged head, the target still at the tip the [Merge] was built onto (read
// from the snapshot, never the proposal), and the gate met with the record's
// principal as merger — or records a PERSON'S override. Its apply marks the
// change merged FIRST, so the push it lands ([Applier.landPushes]) does not
// merge or close it again. A protected branch moves only by such a merge or a
// person's push under direct_push persons, never by a seat's push. A gate
// requiring a check lands through its QUEUE (queue.go), decided by the same
// committer ([Committer.ProposeQueue]), so an entry has one writer.
//
// # A change's conversation is rows on its own subject
//
// Reviewers, comments, reviews and findings are written by records on the
// CHANGE's own subject ([KindChange], change.<id>): one table, one subject's
// writer. The [ChangeWriter], sharing none of the committer's queue, decides
// each [ConversationWrite] in ONE snapshot and takes one change's writes ONE
// AT A TIME, so they never lose rounds to each other; ids derive from the
// operation id ([CommentID], [ReviewID], [FindingID]), so a retry on the
// runner-up is the write that landed. A review is ONE RECORD with every
// finding it raises.
//
// A DELETED COMMENT IS A TOMBSTONE: it keeps its place, its words and
// notices' excerpts cleared, naming who deleted it and when, so every reply
// stands. Its author, a maintainer or a person deletes it ([CommentDelete]).
//
// A revision's facts ([Revise]) are recorded only of the head against the
// current target, their owners matched on the home. MERGE READINESS IS JUDGED, NEVER
// TRUSTED: one pure [ReadinessOf] under the target's gate ([JudgeGate],
// [GateRulesOf]) over one snapshot and one chart, judged again by every read;
// a [Mergeability] record stores it for wakes and [NeedPerson]. It reads the
// change's own reviews, never the tracker's decision.
//
// # A change wakes the seats its records concern
//
// A record carries whom it concerns, stamped by its writer (concerns.go); the
// code log's fleet-wide consumer [FeedGroup] derives each seat's WAKE — never
// the writer's goroutine (ADR-0012). A person is never queue-woken. A CHANGE
// IMPLEMENTS ONE WORK ITEM AND CARRIES NO SPEND (ADR-0097): every wake names
// the item ([Prompt.WorkItem]) and no table here has a spend column. A seat's
// record names whom its write was made for (ADR-0109, writtenfor.go).
//
// # A repository's checks are rows on their own subject
//
// `.crewlet/checks.yaml` (ParseChecks) is measured on commits for each BRANCH
// ([CheckKey]), written by records on checks.<repo> ([KindChecks]) only. The
// subject is ADDITIVE, so no runner's claim waits behind another's, and every
// apply a JOIN ([joinCheck]) so records commute: state only rises, ties break
// deterministically. Rows are kept [CheckHistory], trimmed by each ending
// ([CheckHorizonRows], [CheckHorizonBytes]) and by checks_sweep past
// [CheckSweepGrace]. A box is admitted by its attempt's row ([ReadCheckBox]),
// since it is no principal. A CHECK IS A MEASUREMENT OF A COMMIT, DEFINED BY
// THE TARGET BRANCH AND RUN WHERE NO AUTHOR CAN REACH IT (ADR-0098): never by
// the commit measured, which may rewrite the file. The home's tip work
// (tipwork.go) moves [Green] and bisects a red tip to [Broken].
//
// # Group commit
//
// A repository's pushes are committed by ONE goroutine ([Committer]) in
// arrival order; each record carries every push waiting when the last
// finished, WITHOUT LINGER, up to [MaxPushesPerRecord], [MaxRecordBytes] and
// [MaxRecordRows]. A batch is decided in ONE snapshot and answered from the
// push rows applied, never the decision, so `ok` means a ref moved; a push id
// the history holds is answered from its row, so a redelivery is one push.
// Bounded queues ([CommitterInbox]) refuse busy. A batch is bounded by [CommitBudget]
// and starts only with a budget and [ReplyMargin] left before the caller's
// deadline, so every decided push is answered before its asker can hand the
// same id to another node. The backlog raises code_commit_busy (busy.go). The
// router picks the committer; [Served] is the one adapter, and
// [ErrNotServing] sends the router to the runner-up.
//
// # What a push costs, measured
//
// THE COMMIT CHAIN (gitletcluster's BenchmarkCommitChain): a lone push is a
// record's round trip, 8–12 ms, and group commit scales one repository's
// burst (2 050–2 090 a second at sixty-four on three members). Pushes sharing
// NO repository are a record each, bound by the log's record rate (201–225 a
// second), which group commit cannot raise and a data node more does not.
//
// THE PUSH'S ROWS (BenchmarkPushHistoryBytes): code_pushes gets no index
// beyond code_pushes_repo_idx: each other would cost every push 40–100 bytes
// on every data node for 181 days. THE PUSH ON THE LOG (BenchmarkPushLogBytes):
// a seat's coding box's push is the widest, so 1 667 bytes alone in its record
// (its signed frame, ADR-0031), at which internal/worklet's codeLogDay prices
// the log. Compactions add 108 bytes where a merge takes the floor's sixteen,
// and their sweeps 44 bytes a push where every merge's rows are swept apart.
// The reference company's 3 000 pushes a day hold about 35 MB of it over the
// default seven-day min_age, and the records that merge their packs and sweep
// what those merges retired at most about 3.2 MB beside them — about 38 MB. A
// pack's bytes are never on the log: a record names its bundle.
//
// # The activation fence
//
// Every data node reconciles each activation's declared repositories, and a
// node applying an OLDER activation late would walk a newer one's settings
// back. So [Store.Activate] publishes [Activated] FIRST; its apply raises
// code_activation and never lowers it, and every config-managed record below
// the fence applies as nothing, identically everywhere. Safe because each
// decision is taken from a snapshot holding its own fence: an older
// activation's record is earlier (in the rows decided from) or later
// (refused). So an unchanged activation costs one record, and
// [Store.EnsureRepos] reads the whole declared set in one snapshot, sending
// only what changed through [Store.EnsureRepo].
//
// # Who may read and push
//
// A principal's standing is DERIVED from the chart, never stored (ADR-0088):
// a table of grants is a second chart that drifts. [GrantOf] is pure over the
// repository's row and a [Roster]; a person maintains every repository, a seat
// holds the minimum of [Tier.Cap] and its chart relation. A full_access seat
// owns [SeatNamespace], refs/heads/seats/<handle>/, never refs/heads/<handle>/,
// since people name branches with those slugs; [gitref.ReservedSeatBranch].
// [Visibility] narrows SEATS ONLY, so people are never shut out. PATH OWNERS
// ([PathOwner]) grant nothing. A SEAT'S PUSH meets three rules a person's
// never does — direct_push persons, [OutsideSeatShare],
// [Limits.PushesPerSeatDay] — judged from the committer's own epoch
// ([SeatRules]); a committer that cannot judge a seat hands its pushes on.
//
// # Reads are held to who asks
//
// Every read names its ASKER ([ReadQuery.Asker]), judged by one chart a read;
// a listing pages by rows SCANNED. A reader holding NO CHART refuses a seat
// [ErrNotServing], about itself, never a definite answer peers would
// contradict. A read for another seat is held to both ([ErrAboveCeiling]).
//
// # Every read of the rows seeks a named index
//
// Every statement reading a code table SEARCHes a declared index or key, since
// every data node applies every record on one write lane and a scan costs the
// largest repository. TestEveryCodeReadSeeksANamedIndex and
// TestEveryCodeIndexServesARegisteredQuery (plan_test.go) gate it; the state
// log's own statements are internal/statelog's.
//
// # What a seat and a Gitlet node read
//
// Refs are listed over the LIVE refs' index, never the key, which holds every
// tombstone. Pushes are read in LOG ORDER (code_pushes_repo_idx), since a
// broker instant steps back across a leader change. A content read is a Gitlet
// node's; this package holds the wire ([GitQuery], [GitShare], [AnswerBy],
// [ErrGitUnready]), first asked of the HOME (ADR-0089). Unbounded git text is
// DESCRIBED BY ITS SIZE past its bound, never cut ([MaxLogSubjectBytes]).
//
// # An engine write is built on a Gitlet node and lands as a push
//
// A tool's change is BUILT on a Gitlet node and lands through the one landing
// every push takes (ADR-0092). [EngineWrite.Validate] is the one rule; its push
// id ([WritePushID]) and dates ([EngineWrite.Instant]) derive from the
// caller's operation, so a retry anywhere is the same push and commit.
//
// # A change's revisions are read by the ids their rows record
//
// [GitRevisionFacts], [GitReanchor], [GitInterdiff] and [GitExcerpts] take the
// whole recorded ids, never an abbreviation: two spellings of one commit.
// [Reanchor] is the one rule placing anchored lines, for reads and suggestions.
//
// # A change's class is lexed, and a directive is code
//
// A class only relaxes an approval, so it is given only where true for every
// tool reading the file. [ClassifyGo] compares tokens with go/scanner, never
// `git diff -w` or a pattern. A DIRECTIVE IS CODE, AND SO IS THE LAYOUT
// AROUND ONE: anything a compiler, generator, build or linter reads.
//
// # A protection rule's merge gate rides it whole
//
// A [MergeGate] is ONE value on its rule ([Protection.Gate]), in
// [CanonicalGate]'s order, compared by value ([Protection.Equal]). NO
// REPOSITORY ROW CARRIES A GATE: [RepoRef] carries [PushRule]s, TYPED APART.
// A tool changes a gate a field at a time ([GateChange]); only a person
// loosens one.
//
// # A repository names how it is searched
//
// [SearchMode] is a setting; a SCRATCH repository is grep for good. This
// package indexes and searches nothing.
//
// # What this package does not do
//
// It runs no git and serves no HTTP. It moves object bytes in TWO places:
// [StoreBundle] and a layer's parts ([StoreLayer], [FetchLayerPart]).
//
// # Five horizons, each a function of the record that applies it
//
// Each is chosen by the record's broker instant or what it names, never a
// node-local sweep, which would be a second writer of the replicated estate:
//
//   - PUSH HISTORY ([PushHistory], 181 days): a push record removes expired
//     rows oldest POSITION first, up to [HorizonDeletesPerRecord] and
//     [HorizonBytesPerRecord], the most a record adds, so removal keeps pace;
//   - REF TOMBSTONES ([RefTombstoneHorizon], or past [MaxTombstonesPerRepo]),
//     kept so a delta sync learns of a deletion: a push record sweeps what its
//     rows leave of [MaxRecordRows] (a fixed share fell behind bulk deletion)
//     and raises the mark below which a delta is answered in full;
//   - REF DROPS ([PruneGrace], git's gc.pruneExpire): the object a forced
//     update or deletion left stays fetchable for the grace. Drops are
//     forgotten AFTER the tombstones — a late drop costs a row, a tombstone's
//     room every node a full read — so they fall behind (four, for a push of a
//     thousand deletions) while records stay full ([Applier.expireRefDrops]);
//   - A PURGE, in parts ([PurgePartRows], [PurgePartBytes], [Store.PurgeStep]):
//     no record of a deleted repository reaches it, so the retention duty
//     ([Retention]) publishes RECORDS, never rows, PACED to the lane
//     ([RetentionLaneShare], [RetentionPassBudget]); its final record's
//     deletion marker drops every later record about the repository, on
//     every node, for ever; [RepoGone] names a push's remedy;
//   - RETIRED PACKS ([RetireGrace]): a code_packs_retired row
//     ([RetiredPackReferences], ADR-0081) keeps its object past the longest
//     read (ADR-0090) until [Store.RetireSweep], which names its cut position
//     so the apply reads no clock.
//
// # A compaction merges packs, decided alone
//
// Every cold sync pays a GET a pack. The HOME (internal/gitlet/compact,
// ADR-0089) builds a [Compaction] and proposes ONE record
// ([Committer.ProposeCompaction]), queued with pushes but never batched beside
// one, so its refusal refuses nothing else. Its OPERATION ID IS ITS ID, so a
// retry after failover collapses onto the landed record; its session is its
// plan's ([Compaction.Seen]). THE APPLY MOVES ROWS, NEVER BYTES; outputs date
// from their oldest input ([PackRow.Content]). A MERGE MAKES NO REACHABILITY
// JUDGEMENT (ADR-0090): every input object is in the outputs. THE BOUND HOLDS
// PUSHES BACK, never a compaction: at [MaxPacksPerRepo] a push carrying a pack
// is refused [RefusalCompactionBehind], and a compaction, which makes room, is
// decided.
//
// # A publication puts a published layer on the chain, decided as a compaction
//
// A multi-pack-index, bitmap and commit-graph cost the history to write, so
// Gitlet writes them ONCE for the fleet: a PUBLISHED CHAIN of git's
// incremental layers ([LayerReferences], DERIVED under ADR-0081, since any
// node can rebuild one). A publication is a compaction carrying a [LayerRef],
// so the packs' one writer is the chain's; it names every pack live at its plan
// that its base does not, the closure git's bitmap needs (publication.go).
//
// # A layer's files are git's, read without git
//
// git trusts a layer's files by their names, so every node holds them to their
// row before git sees them, by ONE reading of git's formats (layer.go,
// ADR-0008): [CheckLayer] installs, [LayLayer] publishes, so a build no node
// would install is refused before a byte is stored; [ErrBadLayer] is never
// retried.
//
// # Git's trailer is one rule
//
// The cache's scrub and a layer's check judge git's hashfile trailer by ONE
// [HashTrailer] (hashfile.go, ADR-0008), a PIECE at a time in its caller's
// lane ([Pieces]), checking its context before each, since a root layer can be
// 2.6 GB.
//
// # An expunge drops what no ref reaches, in two records
//
// The one rewrite that drops objects, each record decided alone
// ([Committer.ProposeExpunge], ADR-0099): a BEGIN ([ExpungeBegin]) splits live
// packs into kept and DROPPING ([PackDropping]), keeping every object and
// moving the DROP EPOCH, so a push checked before it is refused
// [RefusalExpunging]; a COMMIT ([ExpungeCommit]) retires the dropping packs,
// RESCUING what a push since reached, conditioned on no ref. A request is
// served once no pack dates its content from before it; the maintainer keeps
// what ARRIVED within [PruneGrace] ([PackRow.Arrived]).
//
// # Measured
//
// A record holds every data node's one write lane, and
// statelog.ApplyTxTimeBudget (250 ms) is checked only at its boundary, so each
// record shape's worst is benchmarked against it (apply_bench_test.go): a
// push record at most 189 ms, a purge part bounded by bytes as well as rows
// ([PurgePartBytes]), a compaction, a publication and an expunge begin each
// under the 83 ms third their bounds are held to. internal/engine's
// BenchmarkAChangedActivation sizes reconcileInFlight.
package gitlet
