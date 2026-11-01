// Package code is THE CODE DOMAIN: the company's git repositories as records on
// one ordered log, CREWLET_CODE_LOG — the state log's eighth domain, strict and
// identity-claiming.
//
// # A repository is rows
//
// A repository's settings, its protection rules and their merge gates, its path
// owners, its refs and the list of packs it holds are replicated tables, and
// only this package's applier writes them:
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
// ADR-0026 and ADR-0051), so the collector keeps every object a live row names,
// the audit checks each one and a backup carries them. A git node's bare
// repository is never the record: it is a cache derived from these rows,
// disposable, rebuilt from them whenever it is lost or wrong — which holds
// only because a repository's refs and packs move by records on its own
// subject and by nothing else, receive-pack never runs in a cache, and git is
// told `ok` only from a push's applied row (ADR-0054).
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
// a push, a compaction, a settings change, a deletion, a restore, a purge — is
// a record on repo.<id>, ONE SUBJECT A REPOSITORY. Two pushes to one repository therefore
// contend at the broker and exactly one record lands per round, and the loser
// re-decides against the winner's rows. One subject a repository rather than
// one a ref, because every write on a repository updates its row too (its ref
// count, its bytes, its version), so a per-ref subject would never narrow a
// write's contention — it would only add a second arbitration the first
// already makes. A NAME is claimed on name.<token>, so two creates of one name
// contend there, where two fresh repository ids never would ([NameToken]).
//
// # A change is rows on its repository's subject
//
// A change (web!42) is code_changes, code_change_bodies, code_change_authors
// and code_revisions (replicated migrations 0058 and 0066), written by records
// on its REPOSITORY's subject and on no subject of its own, so its lifecycle
// and the pushes that move its branches meet at one committer and one
// arbitration, and its number — the repository's code_repos.next_change, read
// and advanced by the apply that opens it — follows the log's order and never
// repeats. A table is written by the records of one subject only: what is said
// about a change on a subject of its own goes to tables of its own. Its state
// is a [ChangeState], the live ones the predicate two partial indexes hold
// ([ChangeState.Live]), and a merged one names how in a [MergedVia].
//
// ITS BODY IS A ROW OF ITS OWN. A change's description, up to
// [MaxChangeBodyBytes], is the one value of a change that outgrows a page, and
// this driver reads a row whole, overflow pages and all, whichever columns a
// statement asks for: in code_changes it more than doubled the read and the
// rewrite every head move makes of the row. So code_change_bodies keeps it,
// written only where a change has one, and the change's body_bytes says what
// it weighs, so nothing that walks the changes reads a body to learn its
// width.
//
// EACH WRITE OF ITS ROW SETS WHAT IT MOVES AND NOTHING ELSE ([changeWrite]).
// This driver rewrites a row's entry in every index whose key or partial
// predicate holds a column a write names, whatever the value, so a write of
// the whole row paid for nearly every index of code_changes on every write of
// a change — and the write a change meets most, a push to its source, moves
// its head, which is in the key of one index alone. So a head move rewrites
// that one entry (code_changes_target_idx); a write of the state — a draft's,
// a close, a reopen, a merge — every index the state is in; an edit of the
// text none, and one of the work item the item's index; and
// TestAChangeWriteRewritesOnlyTheIndexesItMoves holds each kind to the
// indexes its program rewrites.
//
// The bodies, revisions and authors carry no repo_id, reached by their
// change's key, so a purge takes a repository's changes WHOLE in number order,
// each one's dependents — its conversation's among them — before its row, and
// the bounded count reaches them as the final purge does ([purgeChanges]). A
// part weighs the bodies and the conversations' comments in ONE BYTE BUDGET
// with the push rows, [PurgePartBytes], since deleting any of them frees
// overflow pages a count of rows does not see.
//
// ITS LIFECYCLE IS THE COMMITTER'S. An open, an edit, a draft's state, a
// retarget, a close and a reopen are each a [ChangeProposal] the repository's
// committer decides ALONE, in the queue its pushes wait in, as a record of its
// own op ([ChangeOps], decide_change.go): its operation id the asker's — from
// which an open's change id is derived ([OpenChangeID]), so an open asked
// again is the same change — its session the asker's Seen, its rules read from
// one snapshot (who may: whoever may push an open's source branch, and for
// every other write the change's owner, a principal that maintains the
// repository, or a person; a branch heading one live change at a time; the
// version the write was formed against), and its answer read from the change's
// row. A write that asks for what the change already says is answered from the
// row with no record. A seat's is judged from the chart of the deciding node's
// epoch ([SeatRules]), and a node that cannot judge one decides none. A
// LANDING ([ChangeLanded]) is the repository's home git node's own write, of a
// change whose target came to hold its head by a push past it — which no
// push's apply can see, since only git says one commit holds another — naming
// no asker and judged by no chart, decided against the change's head and the
// target's tip the home read.
//
// ITS HOME'S WORK LIST IS ITS ROWS (changesdue.go): a live change whose head
// revision holds no facts, or no mergeability, against the branch it merges
// into — or whose stored readiness was judged before what it reads moved, or
// whose mergeability its moved target asks again, once its home's refresh has
// passed — on an active repository, its target a live branch, is due its
// home's change work ([ReadChangesDue], the engine's own read), which records
// the facts ([Revise]) and whether the change merges cleanly into its target's
// tip with its readiness beside it ([Mergeability], mergeability.go), refreshes
// that readiness alone, or lands the change — each taking its part off the
// list by its own row.
//
// ITS REVISIONS ARE ITS PUSHES' (revisions.go). The apply of the push record
// that moves a live change's source writes the change's next revision and
// makes its pusher an author; one that sets a change's target to exactly its
// head merges it, by push, at most [MaxPushMerges] a record; one that deletes
// its source closes it ([SourceDeleted]) — each entry in the record's order,
// its head moves before its merges before its closes, every change one
// version a record, pure over the record and the rows it finds — the changes,
// and the authors its pushers already are of them ([changeBook]). The push
// decide moves the same model by each entry it admits, so it counts exactly
// the rows the apply will write about changes beside the entries' own and
// holds the record to [MaxRecordRows] with them: a push whose changes cannot
// fit even a record of its own is refused [RefusalTooManyChanges], and one
// that fits only alone is carried, with the pushes behind it, to lead the
// committer's next batch.
//
// A PUSH MAY OPEN ONE, IN ITS OWN RECORD (pushchange.go). `git push -o change`
// asks for a change on the one branch the push sets ([ParseChangeOptions],
// [ChangeSource]), and the push decide opens it in the push's own entry
// ([PushEntry.Change]), on the state the push leaves: the record's apply
// numbers it from next_change and writes it after the entry's own moves, so
// the push is its first revision and its pusher its owner. A push to a branch
// already heading a live change NAMES that change instead, which its move
// revised; and options that are no change's, a push of no branch or of
// several, and a change its rules refuse — a target that is its source, no
// live target, a source at its target's commit — leave the push to land as
// one that asked for none, its pusher told why ([ChangeAnswer]): a change's
// options never fail its push.
//
// ITS READS ARE ITS REPOSITORY'S (changeread.go). A listing ([ReadChanges])
// names a repository, a work item, a state, a reviewer or what its changes
// wait on, and walks the ONE index its filters lead — a reviewer's standing
// asks, a work item's changes, an owner's in a state, a repository's in a
// state (replicated 0069), a repository's, the company's in a state —
// newest first, testing every filter that index does not seek on the rows the
// page read, so a page is the rows it read and never scans past its limit
// for the few it keeps. One change ([ReadChange]) is read by its number on
// its repository, its revisions paged newest first beside it. Both are held to
// the asker's grant on the change's repository ([JudgeRead]): a listing that
// names a repository, and a read of one change, are refused whole where the
// asker or the seat it reads for may not read it; a listing that names none
// leaves out the changes of every repository either may not, and of a
// deleted one. Both carry each live change's readiness, judged in the read's
// own transaction (readiness.go); and what waits on a person ([NeedPerson])
// walks the changes whose stored readiness does, keeping those the read
// judges still do. What a surface reads of a change's CODE — a revision's
// files from the base its facts record, what changed between two revisions,
// where each finding is in the revision asked about, and the floor those
// reads by object id are asked at — is decided once, over those rows
// (revisionread.go), for every surface that shows it.
//
// # A change's conversation is rows on its own subject
//
// What is said ABOUT a change — who is asked to review it, its comments and
// the threads they form, its reviews and the findings they raise — is
// code_conversations, code_reviewers, code_participants, code_comments,
// code_reviews, code_findings and code_finding_texts, written by records on
// the CHANGE's own subject ([KindChange], change.<id>) and by no record on its
// repository's: one table, one subject's writer. Each write is a
// [ConversationWrite] the estate asks of the repository's committing data
// node, whose [ChangeWriter] — the committer's sibling, sharing none of its
// queue — decides it in ONE snapshot under PatternArbitrated, its session the
// asker's Seen, as a record of its own op ([ConversationOps],
// decide_conversation.go): who may ask for review and who may be asked (rule
// D21, the reviewers judged by the deciding node's chart whoever asks —
// [ConversationWrite.JudgesChart]), a reply only to a thread's root or a
// finding, a thread resolved by its starter, an author, a maintainer or a
// person; a review only by one who reviews the repository and did not write
// the change, ONE RECORD with every finding it raises, superseding its
// reviewer's standing findings of earlier revisions it does not carry (D22);
// and a finding answered by an author, dismissed by its raiser, a maintainer
// or a person — a blocker cleared by its dismissal or its raiser's later
// review alone (S40, conversation.go). The writer takes ONE CHANGE'S WRITES
// ONE AT A TIME, so two of them never lose rounds to each other at the
// broker, and a comment's id, a review's and its findings' are derived from
// the write's operation id ([CommentID], [ReviewID], [FindingID]), so a write
// asked again of the repository's runner-up is the write that landed. Every
// record names the repository's path and the change's in its scope, since its
// decide reads the repository's rows; the deletion gate reads the repository
// off that scope ([gatedRepo]), which the apply holds the payload's to; its
// apply is pure over the record — a
// reviewer's kind, stamped from the chart by the writer, rides it
// (apply_conversation.go) — and keeps the conversation's counters (its
// comments, reviews, open findings, standing blockers and requests for
// changes) on one narrow row with what its wide text weighs (text_bytes), so
// a purge takes it with its change, cutting a heavy one by its comments,
// findings' texts and reviews ([purgeChanges]). It is READ WITH ITS CHANGE
// (conversationread.go): a listing naming a reviewer, or what waits on a
// review, walks the reviewer's standing asks (code_reviewers_reviewer_idx);
// what waits on an owner's fix walks the owner's changes in a live state,
// keeping those whose counters ask for one; and a change read pages one part
// of its conversation — its reviewers, its comments a thread at a time, its
// reviews or its findings — the detail on its revisions' first page counting
// it.
//
// A REVISION'S FACTS ARE ON THE CHANGE'S SUBJECT TOO (revise.go): what the
// repository's home git node read of the change's head revision against its
// target — its base, the trees of its base and its head, its patch-id, the
// files it touches and their classes ([RevisionClasses]) — and the path owners
// whose globs those files match, MATCHED ON THE HOME ([MatchOwners]) beside
// the digest of the owners matched against ([OwnersDigest]), is a [Revise]
// the change writer decides as the home's own write, naming no asker:
// recorded only of the head revision against the current target, past the
// facts held of it there, and only where the snapshot's owners digest the
// same — [ConversationRefusalSuperseded] otherwise, recorded nowhere — and a
// head its target holds is no facts at all. The writer resolves the matched
// owners on its chart, asks those who may review (code_reviewers, reason
// [ReviewerPathOwner]), skips the rest by [SkipReason], and stamps whom else
// the facts concern, so the apply judges nothing again; code_revision_facts
// keeps one row a revision, and a change read's revisions carry it.
//
// A CHANGE'S MERGE READINESS IS JUDGED, NEVER TRUSTED (readiness.go): one pure
// [ReadinessOf] over the rows one snapshot holds and the chart one epoch holds
// — a [Precondition] a [PreconditionKind]: not a draft, its head revision's
// facts recorded, one counted approval (an eligible reviewer's standing
// verdict, never an author's, carried to a later revision only by its code —
// the two revisions' recorded base and head trees equal,
// [ChangeClassNoCodeChange] — or by an equal recorded patch id), no standing
// blocker — each named with who can clear it
// (S40) — and its head merging cleanly — each met, unmet or computing, an
// unmet one naming whom it waits on. Every read of changes judges it again in
// its own transaction, so it takes the reader's chart whoever asks; and every
// [Mergeability] record's decide judges it with the merge it records and
// stamps the answer, which the applier stores beside the versions it was
// judged at — what a wake that a change became ready reads, and what
// [NeedPerson] seeks — a home refreshing it, merging nothing, once a review,
// an ask, an answer to findings, facts or the change's own row moves them
// ([Mergeability.Refresh]). It reads the change's own review objects, never a
// decision of the tracker's.
//
// AND IT IS JUDGED UNDER ITS TARGET'S MERGE GATE (gatejudge.go): every reader
// and every decide of a merge asks ONE pure [JudgeGate] over the facts one
// snapshot holds ([GateFacts], read for a page a kind of row at a time and for
// one change by [LoadGateFacts]) and the gate the rules matching the target
// combine into, the strictest of each term ([GateRulesOf]) — M4's
// preconditions, the approval only where the gate asks one, and the gate's
// own: a person's approval of a change touching its human paths — among them,
// on a gate requiring checks, the checks' own directory ([ChecksDirGlob])
// unless the rule sets agents_edit_checks (U44) — one approval
// from somebody on every touched path owner's lead chain (the head revision's
// RECORDED match, no glob matched in a judgment), the checks the gate
// requires passed on the head commit, and the merger's authority where a
// merger is named.
//
// A BRANCH WHOSE GATE REQUIRES A CHECK LANDS ITS MERGES THROUGH ITS QUEUE
// (queue.go): an enqueue, decided by the change's merge gate from one snapshot
// with its principal as the merger, and a dequeue are records on the
// REPOSITORY's subject, decided by its committer alone
// ([Committer.ProposeQueue]) — so a change's entry, its lifecycle and the
// pushes that move its head have one writer, and every record that leaves the
// change no longer what was enqueued ends its entry in its own apply: a close,
// a draft, a retarget and a landing dequeue it, and a push that moves its head
// ejects it.
//
// A CHANGE MERGES BY ONE RECORD ITS GATE DECIDES (merge.go, ADR-0066): a merge
// ([Merge]) built on the repository's home onto the tip it read of the target
// is proposed to the repository's committer ([Committer.ProposeMerge]), whose
// decide proves in ONE snapshot that the change is open at the head the merge
// took, that the target is still at the tip the merge was built onto — read
// from the snapshot, never from the proposal — and that the change's gate is
// met with the record's principal as the merger, or records a PERSON'S
// override of it; a branch whose gate requires a check merges through its
// queue, and only a person's override merges into one directly. Its apply
// marks the change merged FIRST (`merged_via direct`, who merged it, an
// override's reason), so the push it then lands through the one apply every
// push takes ([Applier.landPushes], `via merge`) — the target onto the merge's
// commit, a seat's source branch deleted (Q4) — finds it merged rather than
// merging or closing it again; and it writes the protected target's landing
// row and, where its gate requires no check, its green row. A protected branch
// so moves only by a merge whose preconditions one snapshot proved, or by a
// person's own push under direct_push persons, which is that person's recorded
// override; never by a seat's push, which every protection refuses.
//
// # A change wakes the seats its records concern
//
// Every record about a change carries whom it concerns, stamped by its writer
// (concerns.go); the applier writes a notice row for each (apply_notices.go),
// which a person's code inbox reads, and the code log's own fleet-wide
// consumer, [FeedGroup], derives a WAKE for each seat among them (feed.go,
// wake.go) — never the writer's goroutine (ADR-0012). The log's trim waits on
// that consumer. A person is never queue-woken. A CHANGE IMPLEMENTS ONE WORK
// ITEM AND CARRIES NO SPEND (ADR-0067): every wake about it names the item
// ([Prompt.WorkItem]), so the turn it starts is charged to that item, and no
// table of this domain has a column for what a turn spent.
//
// # A repository's checks are rows on their own subject
//
// What a repository's `.crewlet/checks.yaml` defines (checksfile.go:
// ParseChecks, one strict grammar for every reader) is MEASURED on commits,
// and what became of each attempt of each check of each commit, FOR EACH
// BRANCH it is checked for — the branch whose file defines the check and
// whose protection requires it, so one commit checked for two branches is two
// checks ([CheckKey]) — is code_checks — with code_check_logs, the log a
// failed run kept, a Required object reference (CheckLogReferences) — written
// by records on the repository's CHECKS subject ([KindChecks], checks.<repo>,
// scope [ChecksPath]) and by no record on its repository's (checks.go): queued,
// started, finished, swept and dispatched ([CheckOps]) — the last the
// home's record of a target's checks file read at its tip and the change
// heads it dispatched under it, which code_check_targets and
// code_check_dispatches keep, so the home's due listing converges on what it
// decided (checksdispatch.go). The subject is ADDITIVE — no
// expectation, so no runner's claim waits behind another's — and every
// apply a JOIN (apply_checks.go, [joinCheck]), so its records commute as
// PatternAdditive requires: the state is derived and only rises, a claim
// tie keeps the lower (StartedAt, Runner) and its box is set once, an
// ending tie keeps a measurement over infrastructure's, the log follows
// the ending kept, and a dispatch's rows keep the reading of the later tip.
// Its attempts' rows are kept [CheckHistory] — a dispatch's until the
// repository's purge, one a head and a target — and every ending removes
// its own repository's oldest past its broker instant, bounded by
// [CheckHorizonRows] and [CheckHorizonBytes], and the retention duty's
// checks_sweep ([Store.ChecksSweep], [Retention.Pass]'s checks step) does
// the same for a repository whose checks stopped ending — one holding a row
// past the history by [CheckSweepGrace], which no repository whose checks
// keep ending holds, so the duty never repeats an ending's horizon and
// sweeps a repository at most once a grace. The deletion gate
// reads the repository off the subject, so a check that ends after its
// repository's purge writes nothing, and the purge's parts take its checks
// after its changes, in the same rows-and-bytes budget.
//
// A CHECK'S BOX IS ADMITTED BY ITS ATTEMPT'S ROW (internal/code/host), which
// its admission reads by the attempt's key on every request ([ReadCheckBox]):
// the engine's own read, refusing an asker and every other field, since the
// box is no principal — a kind of its own so a principal's `check` read that
// forgot its asker is refused rather than answered held to no grant. Its reads
// of its commit by object id are the engine's own too ([GitQuery.Check],
// [GitQuery.EnginesOwn]), held by [GitQuery.Validate] to that commit. Its
// token outlives the check's timeout by [CheckBoxGrace], a backstop the row
// overrides the moment the attempt ends.
//
// A CHECK IS RUN BY A GIT NODE (internal/code/checkrun), which finds what it
// may claim by a `checks_queued` read ([ReadChecksQueued]: the queued attempts
// oldest queued first, each with its definition and its repository's name),
// and a repository's home finds the attempts whose runner is gone by a
// `checks_running` read ([ReadChecksRunning]) — both the engine's own, each
// claim and ending they lead to decided again by the checks writer.
//
// A CHECK IS A MEASUREMENT OF A COMMIT, DEFINED BY THE TARGET BRANCH AND RUN
// WHERE NO AUTHOR CAN REACH IT (ADR-0068): a head's checks are defined by the
// checks file at its target's tip, and a protected branch's TIP checks by the
// file at the base of its first landing above its green row — never by the
// commit measured, which may have rewritten the file. Every landing a record
// did not prove lies above its target's green until a tip passes, and the
// repository's home owes the target its tip work (tipwork.go: a `checks_due`
// read's [ReadAnswer.DueTips], listed by code_landings_unjudged_idx): the
// newest landing dispatched, coalesced; the green row moved onto it on pass
// ([Green]); a red tip's failed checks bisected over the landing rows down to
// the first landing they failed on, recorded broken ([Broken]) and announced
// once as code_target_broken.
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
// another node. Every busy refusal names its reason on the busy counter — a
// repository's queue, the node's bound, a wait in the queue that outlasted its
// asker, a deadline spent before the proposal arrived, a lost race — and the
// first three, the committer's own BACKLOG, are what the node raises
// code_commit_busy for over [CommitBusyWindow] ([Committer.AlarmReading],
// busy.go).
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
// THE PUSH'S ROWS (BenchmarkPushHistoryBytes, the same figure every count;
// restated from one count of one iteration once each row was weighed by its
// own table, again once every id was the shape production writes, and the
// pack row again once it named the position its content dates from,
// replicated 0067, with that column's index, and once a seat's coding box's
// push was measured beside a person's and a tool's),
// measured as the replicated estate's growth after a truncating checkpoint
// over records whose refs existed before, so a ref's own row is rewritten in
// place and never counted — EACH ROW BY THE PAGES ITS OWN TABLE'S B-TREES
// GREW BY, never by the file's growth with a pack less its growth without
// one, which counted the push row's pack columns, kept 181 days, as the pack
// row's, kept while the pack is live. Every id is a production one, in
// production's order: the repository's, the push's and its pack's object's
// are UUIDs, which a push row and its index entries carry six times and a
// pack row and its nine, and a person's credential is a token's name (`pat:`
// and its id). A figure is whole pages over the rows that filled them, so a
// row a few bytes wider moves it only where it fits fewer rows a page. They
// have three lifetimes:
//
//   - its code_pushes row, which the push-history horizon keeps for the 181
//     days of usage.History: 622.6 bytes with its index entries for a
//     person's push of one commit — one command on a ref named in 40 bytes,
//     one introduced id and its pack named (4 000 records; 577.5 for a push
//     that carries no pack, a deletion or a ref moved to a commit the
//     repository holds) — 679.9 for the same commit pushed by a seat and
//     built by the engine, its row naming the seat and the run behind it at
//     a run's usual sizes (a UUID turn id, the work item `native ENG-1234`, a
//     seventeen-byte model id; 4 000 records; 622.6 with no pack) — 754.7
//     for the same commit pushed by that seat's coding box, received by
//     git, its run naming as well the launch that pushed (a UUID) and the
//     coding agent the box ran (`claude-code`), the widest a seat's push
//     leaves (push=box; 4 000 records; 679.9 with no pack; one count of one
//     iteration, Go 1.27.2, on two E-cores of an Intel Core Ultra 7 265U,
//     2026-10-31, the person's and the tool's arms measured beside it to
//     the figures above) — and 420 086 for the widest a push can leave
//     ([MaxCommandsPerPush] commands on refs of [MaxRefNameBytes],
//     [MaxIntroduced] ids; 100 records);
//   - its pack's code_packs row, 587.8 bytes with its index entries, which
//     lives while the pack is LIVE: until a compaction merges it (# A
//     compaction merges packs, decided alone), or the repository is purged;
//   - and once a compaction has merged the pack, its code_packs_retired row,
//     608.0 bytes with its three index entries (the retired/inputs=16 arm:
//     4 096 typical pushes' packs merged sixteen at a time, as a compaction
//     at the floor merges them, the same figure merged eight at a time; one
//     count of one iteration, Go 1.27.2, on two E-cores of an Intel Core
//     Ultra 7 265U, which has SHA instructions, 2026-10-27 and 28), which
//     keeps its object from the collector for [RetireGrace] until the
//     code-retention duty's sweep removes it, a [RetentionInterval] later at
//     the most.
//
// So every data node, holding the whole estate, holds 754.7 bytes for each
// push a seat's coding box landed in the last 181 days, 679.9 for each its
// tools landed and 622.6 for a person's, and pack rows that
// follow its repositories rather than its pushes. A repository's home looks
// at it at every poll, a minute apart, and merges its small packs once
// [CompactMinMerge] have gathered — a busy repository's at every poll, the
// minute's pushes taking it past them — so a repository holds at most
// [CompactTargetPacks] live pack rows plus a minute's pushes whatever its age,
// and a row more for every [FullPackBytes] its history fills: one pushed now
// and then sits near the floor (internal/code/compact's steady arms: 20 at
// the most for a repository pushed once a minute), and one pushed three
// hundred times a minute held 304 at the most — some 29 KB at the target and
// 179 KB at 304. It holds the rows merged in the last day too, retired: a
// compaction below the target takes [CompactMinMerge] packs or more into one
// output, so where the packs a repository's pushes add are what its
// compactions take, a push retires at most CompactMinMerge ÷
// (CompactMinMerge − 1) packs, 16/15, and about one where a repository is
// pushed to fast enough that a minute's roll takes hundreds (the steady arms:
// 1.05 for a repository pushed once a minute, 1.003 for one pushed three
// hundred times a minute). The
// reference company's 3 000 pushes a day, every one a seat's and priced as
// its coding box's — the widest a seat's push leaves, as the code log's day
// prices it (THE PUSH ON THE LOG, below), since which of its seats push from
// their boxes and which through their tools is no figure the reference
// company has — carry about 410 MB of push rows at the horizon (369 MB
// priced at a tool's push, as they were before a box's was measured; 34.2 GB
// and 30.8 GB at 250 000 pushes a day) and at most about 2.0 MB of retired pack
// rows — 3 200 of them, a day of 16/15 a push and the RetentionInterval past
// it — beside its repositories' live ones, where while nothing merged a
// repository's packs every push's row stayed for as long as its repository
// did, about 644 MB more a year. A seat that has
// pushed holds two rows more at the most however much it pushes, its pushes
// on its two newest company days (code_seat_days, the counter the decide's
// daily quota reads, which keeps the older for a committer whose clock has
// not reached midnight yet): each a row of about thirty bytes and its key's
// entry, where an index of the push
// rows serving that one-day question would have kept an entry of some forty
// bytes for every seat's push for its 181 days, about 1.8 GB at 250 000
// pushes a day. That is one of THREE INDEXES THE PUSH ROWS ARE NOT GIVEN,
// each an entry every push would keep on every data node for its 181 days
// to answer a question the one index the rows have already answers: that
// one; a second index of the rows in log order beside the repository's,
// where the one was reordered instead (replicated 0056; a second would have
// added about a quarter to the 395.3 bytes a push's row weighed then,
// measured, # Measured); and an index
// of each coding run's pushes, where a collected run's report scans its
// repositories' pushes since its launch ([ReadRunPushes], below) — some 40
// to 100 bytes a push each, 1.8 to 4.5 GB each at 250 000 pushes a day. A
// forced update or a deletion leaves one row more, its ref drop, a commit id and a
// ref without the history row's commands, kept [PruneGrace] — two weeks of
// such commands, against the history's half year of every push — and longer
// while the repository's records are full of them (the REF DROPS horizon
// below).
//
// THE PUSH ON THE LOG (BenchmarkPushLogBytes; restated from one count of one
// iteration once an entry named its way, once a seat's push was measured
// beside a person's, once the person pushed through a token's name, once a
// seat's entry carried the day its quota counted it on, and once a seat's
// coding box's push was measured beside them): the code log's
// growth over the records the committer forms for that same person's push —
// its principal naming the token bound to them, `pat:` and its id, its one
// command, its bundle named and its one introduced id — is 1 380 bytes a push
// in a record of its own, the same every count, and about 792 a push for
// sixty-four at once to one repository, which group commit lands in two
// records or, as its batches fall, now and then one, moving the figure by
// under a byte: a record's envelope is about 610 bytes, forty of them the
// signed frame every record is appended in (ADR-0031), and each push it
// carries about 773 more, twelve of them the way it names (`"via":"git",`).
// A SEAT's push of the same commit takes 101.0 bytes more (push=agent, the
// same every count): its principal names the seat and the run behind it — a
// UUID turn id, the work item `native ENG-1234`, a seventeen-byte model id —
// where a person's names the token, its way is `engine`, and its entry
// carries the company day its quota counted it on ([PushEntry.Day]),
// nineteen bytes (`,"day":"2031-04-02"`); so 1 481 bytes alone in its record
// and about 893 in a burst. A seat's CODING BOX's push of the commit takes
// 224.0 bytes more than a person's (push=box): its principal names, beside
// the run the engine's write names, the coding run that pushed — the
// launch's id, a UUID — the coding agent the box ran (`claude-code`) and
// the identity of the work item its run was launched for (a native task's
// UUID, [RunProvenance.WorkID], 49 of those bytes), and its way is `git`;
// so 1 604 bytes alone in its record and about 1 016 in a burst (the
// person's 792 and the same 224), the most a seat's push takes of the log, at which the code log's
// day prices every one of the reference company's (internal/tracker's
// codeLogDay). The compactions that merge the pushes' packs add
// their own records, MEASURED a push (internal/code/compact's steady arms,
// the code log's growth over every pass): 108 bytes where a merge takes the
// floor's sixteen, a quiet repository's, and 43 where it takes a minute's
// hundreds, a busy one's — a compaction's record is its envelope and its
// output's row, shared by every pack it merges, and a few dozen bytes a pack,
// so the more it merges at once the less a push pays. A day later the
// code-retention duty's sweep removes the rows a merge retired: one record of
// about 700 bytes for each repository holding a row past the grace at each of
// its quarter-hourly passes, so 44 bytes a push where every merge's rows are
// swept apart, a quiet repository's (75 sweeps of 77 merges, MEASURED by the
// same arm), and a fraction of a byte where a busy one's quarter hour of
// merges is swept together, a record for every [RetireSweepRows] of its rows
// whatever pushes they came from. The log keeps the trim's window of records
// rather than the history, so the reference company's 3 000 pushes a day,
// every one a seat's and priced as its coding box's, hold about 34 MB of it
// over the default seven-day min_age, and the records that merge their packs
// and sweep what those merges retired at most about 3.2 MB beside them —
// about 37 MB, under a twenty-fifth of the log's 1 GiB floor — and the bytes
// of a push's pack are never on it: a record names its bundle. The records
// of the changes those pushes revise come beside them, several times their
// bytes (config's DerivedCodeLogNumerator has the day).
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
// [Store.EnsureRepos], the reconcile's form, reads every declared claim, row,
// rule and path owner in the one snapshot a publish on the fence's subject
// decides from,
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
// stored (ADR-0058): there is no table of who may push where, because such a
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
// A repository's PATH OWNERS ([PathOwner]) are no row of that relation at all:
// a unit or a seat named the owner of some of its paths is whom a change
// touching them asks for a review, and holds on the repository what the chart
// gives it and nothing more — [GrantOf] reads no path owner, and none is on the
// [RepoRef] every read, push and fetch decides from. They are settings like
// the rest, carried whole by the same records; a tool's change adds and
// removes globs one owner at a time, resolved against the rows its decide
// reads ([PathOwnersChange]).
//
// A SEAT'S PUSH is held to its grant AT THE DECIDE, by the repository's
// committer from the chart and the calendar of its own epoch ([SeatRules]),
// and to three rules a person's push never meets: direct_push persons takes a
// person's push alone ([ProtectionRefusal]), a seat outside the owning unit
// fills at most [OutsideSeatShare] of the repository's refs and bytes, and a
// seat lands at most [Limits.PushesPerSeatDay] pushes a company day, counted
// by the push apply in code_seat_days and refused past it unrecorded
// ([RefusalPushQuota]) — decide.go holds each, and why. A committer that
// cannot judge a seat, holding no chart yet or confirmed behind the
// company's configuration ([CommitterOptions.Current]), decides none of its
// pushes and hands them to the repository's next committer.
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
// what the grant guards. A reader that holds NO CHART — a node between its
// code reader's start and its first epoch's install, at every boot — refuses
// a seat's read [ErrNotServing], a refusal about ITSELF, before any row is
// read: never "every seat reads", and never "this seat reads nothing", a
// definite answer that would end the estate's walk with what every peer
// holding a chart contradicts. Refused about itself, it is a node that ran
// nothing, and the walk asks the next data node. A person's read asks no
// chart and is answered there.
//
// A SEAT'S READ MADE FOR ANOTHER SEAT is held to both. Inside a turn a
// colleague seat woke, the reading seat is that colleague's deputy, and a
// deputy that read for its requester whatever it could read itself would hand
// a `visibility: unit` repository to any seat that can message one of the
// unit's. So such a read names the requester too ([ReadQuery.CeilingSeat],
// and a resolve's and a read by object id's), and every reader judges the two
// by ONE function ([JudgeRead]) from one chart and one row: a listing leaves
// out what either may not read, and a read the asker may make and its ceiling
// may not is [ErrAboveCeiling] — never [ErrNotPermitted], since the same seat
// reads the repository in a turn nobody woke. A person or a stranger as the
// requester never reaches here: a person maintains every repository, and a
// stranger reads none, both settled where the turn runs.
//
// # Every read of the rows seeks a named index
//
// Every statement this package spells that reads a code table — a SELECT, and
// the WHERE an UPDATE or a DELETE finds the rows it writes by — reaches each
// table it reads by a SEARCH on an index the schema declares or on the primary
// key, because every data node applies every record on the one write lane
// every other domain's applier waits behind, and a scan there costs what the
// largest repository holds, at every call. Only three things are read whole:
// json_each, a statement's own list, which a batch is driven from (planner.go
// says why); a subquery's or a common table expression's result, which is no
// table; and a table whose rows something other than a repository bounds —
// code_evictions, the trim's listing, one row a node an operator evicted. A
// statement names the index it is written for where the planner would not
// choose it (planner.go), and every index names its reader in its migration.
//
// IT IS A GATE, NOT A REVIEW. TestEveryCodeReadSeeksANamedIndex walks this
// package's non-test source for every text that reads a code_* table — a
// constant, an inline literal, a statement built of constants — and holds
// each to a statement plan_test.go registers by its exact text, whose plan it
// judges; a function that assembles a statement at run time registers the
// statements it can build as shapes (planShapes), objstore's builders over
// [PackReferences], [RetiredPackReferences] and [LayerReferences] among them,
// each at every width its callers build it at — the collector's batch of
// keys, a walk's page, a backup's — since each width is a statement the
// driver plans on its own; and
// TestEveryCodeIndexServesARegisteredQuery holds every registered statement to
// the index and the columns it seeks, and every index to a statement whose
// plan uses it — or, while the commit that issues its read has not landed, to
// the read its migration claims for it (claimedReads), which the gate judges
// as it will judge that statement. So a new read lands with its plan
// registered, or the build fails. What the gate does not see is the state
// log's own statements over the domain's ledger and deferred records
// (code_ops, code_log_deferred, code_log_deferred_scope): internal/statelog
// assembles them for every domain alike from the table names [Domain]
// declares, so their plans are the framework's to hold rather than this
// package's, and no test holds them.
//
// # What a seat and a git node read
//
// A repository's REFS are listed by name under a prefix — its branches, its
// tags, or a seat's own namespace — a page at a time ([MaxRefsPerPage]),
// over the LIVE refs' partial index (replicated 0054) and never the key,
// whose range holds every tombstone under the prefix too; the first page
// carries the repository in detail, so what a seat asks first of a
// repository is one read. [Reader.ResolveRef] names every ref a call needs —
// each a whole ref, or a short name as a branch and then as a tag, each
// answered on its own — in one transaction with the repository's grant, at the
// level its caller states: a comparison's two sides cost one barrier, and are
// read at one moment.
//
// A repository's PUSHES are read in LOG ORDER: code_pushes_repo_idx is keyed
// (repo_id, position, push_id) (replicated 0056), because a record's
// instant is the broker's and steps back across a stream leader's change, so
// "every push above this position" — what a git node's index of which push
// introduced each commit is built from — could not be read by it. The
// history listing pages newest first by position; the `introduced` read,
// the engine's own, which names no asker and states its level, pages
// ascending over a range or descending below a position; and a delta
// snapshot asked for its PUSHES PHASE serves every push applied in (Since,
// its position], each once across its pages, so the index a git node folds
// them into is level with its copy at every round. Those two send each push
// CONDENSED to the refs it landed, and hold a page to the commands it read as
// well as to the rows it sends, since a push row stores every command its
// client sent, the refused ones too: a run of pushes that each landed one ref
// beside hundreds refused is read in more pages, never in one that decodes
// gigabytes of commands to send four mebibytes. A coding run's pushes
// ([ReadRunPushes], the engine's own as well, read when it collects the run)
// are the rows naming the run among each of its repositories' pushes above
// its launch: the same index's range, each row looked up to test its run —
// no index of the run's own, which would cost every box push an entry on
// every data node for the history's 181 days to serve one read a run — and
// a page held to what it READS, the pushes it looks up and what their rows
// weigh as the index says before any is touched, rather than to the few it
// answers: so the scan reads a week of a repository taking 70 pushes a
// minute, 705 600 rows, in 0.49 s and 71 pages of at most 10 ms, and an
// hour there with 200 pushes of 403 KB beside the run's in 19 pages of at
// most 4.3 ms, where one page had read all of their 80 MB
// (BenchmarkRunPushes, one run of each arm on two E-cores). Every
// reader of the index's first shape — the listing, the history horizon, a
// purge part and its count — reads the reordered one, so it is one index,
// not two.
//
// A read of a repository's CONTENT is a git node's (internal/code/query), and
// this package holds only what both ends of one import: its wire — [GitQuery]
// and its answers, every revision an object id ([ValidRev]) and every path one
// no git grammar reads ([GitPath]) — the node's [GitShare], half a git node's
// read lane, which every kind of seat git work takes its unit of, so a burst
// of seat reads never takes the half a person's push scans wait in, and the
// instant a served read's answer is due ([AnswerBy]), a reply's margin before
// its asker stops listening, at which every wait of the read ends and is
// answered unready rather than late ([AnswerDue]). A
// git node judges the asker's grant from its own chart as a reader of the rows
// does, and one that cannot answer for a reason about ITSELF — a copy still
// syncing, its share full, no chart yet, its configuration behind, a failure
// of its own, or no copy at all where the read is not its walk's filler's
// ([GitQuery.Home]) — says [ErrGitUnready], which another git node may
// answer, with a reason that tells the walk whether it may let the next node
// fill a copy instead ([UnreadyReason.PassesFill]). Which git node a read is
// asked of first is the estate router's: the repository's HOME, the first of
// its rendezvous order over the live git nodes (ADR-0059).
//
// Two of those reads answer with text git leaves unbounded: a path's HISTORY
// ([GitLog], its literal [LogQuery.Grep] matched by the git node, never by
// git) and a file's BLAME ([GitBlame]). Each such field — a commit's subject,
// its author's name and address, its parents, a blamed line — is DESCRIBED BY
// ITS SIZE past its bound, never cut ([MaxLogSubjectBytes] and its kin, each of
// the field's encoded size), so the widest commit, and the widest line with its
// commit, always fit one page. A kind is an estate request's and no record's,
// so a kind added moves no [RecordVersion].
//
// # An engine write is built on a git node and lands as a push
//
// A seat's or a person's change made through a tool is BUILT on a git node
// holding the repository (internal/code/host's Server.Write) and lands as a
// push through the one landing every push takes (ADR-0062). This package
// holds what both ends of one import, as it does for a read by object id: the
// write ([EngineWrite]), its answer ([EngineWriteResult]) and the change's own
// refusals ([WriteRefusal], UNRECORDED — decided on the git node before
// anything is stored), the ONE rule every party holds a write to
// ([EngineWrite.Validate], every new path to [ValidTreePath] and every path it
// names to [MaxTreePathDepth] components), and the bounds that size it — the
// depth among them, since what a build reads, holds and runs grows with it.
// Its push id is an unnamed derivation of the caller's operation
// ([WritePushID]), so a retry anywhere is the same push, and its commit's
// dates are read off the instant that operation carries
// ([EngineWrite.Instant]), so a retry is the same commit. Nothing of a write
// rides the code log but its push: a proposal [ViaEngine], which moves one ref
// and carries no pack past [MaxWritePackBytes].
//
// # A change's revisions are read by the ids their rows record
//
// Three of those reads are of a change's revisions: a revision's FACTS against
// its target ([GitRevisionFacts]) — its merge base, the trees of its base and
// its head, the files it touches and their classes, and the patch-id of its
// diff, the engine's own read, which its repository's home asks naming no
// asker ([GitReadKind.EnginesOwn]) — where lines anchored in
// one revision are in another ([GitReanchor]), and what the change changed
// between two ([GitInterdiff]). Each names its commits by the whole ids the
// rows record, never an abbreviation: an interdiff compares two revisions'
// bases as ids, which an abbreviation would make two spellings of one commit.
// A revision's touched list is bounded by [MaxTouchedPaths] and by
// [MaxTouchedBytes] of JSON, so the facts that carry it are one page; past
// either the revision touches all. And [Reanchor] is the one rule that places
// lines anchored in a blob, by the zero-context hunks of its diff ([Hunk]):
// what the read answers a reader, and what a commit built from a finding's
// suggestion splices by, so the two never place a line apart.
//
// # A change's class is lexed, and a directive is code
//
// A change's CLASS names what a review of it need not read — whitespace, or
// comments — and a class only ever relaxes: a rule may exempt a change of a
// class from an approval it would otherwise ask, so a class is given only
// where it is true of the program and of every tool that reads the file, and
// whatever cannot be proved so gets none. [ClassifyGo] judges two versions of
// a Go file with the language's own lexer, go/scanner: [ChangeClassWhitespaceOnly]
// where their tokens and comments are the same, [ChangeClassCommentOnly] where
// their tokens are once the comments are set aside — the semicolons the lexer
// inserts at a newline counted among the tokens, because a newline that ends a
// statement is one. Never `git diff -w` or a pattern over the text: the first
// calls such a newline a space, and the second cannot tell a comment from a
// string that holds one.
//
// A DIRECTIVE IS CODE, AND SO IS THE LAYOUT AROUND ONE. A comment the
// compiler, cgo, go generate, go test, the build or a linter reads changes
// what the file builds, runs or reports, and whitespace alone changes what a
// directive does: a //line indented off its line's start is ignored, a blank
// line parts an //export from its function, and one under a // +build block
// decides whether it is a constraint at all. So no class holds where a
// directive, what follows it up to the next token, or where any comment of
// its group sits changed; a function body holding an example's Output: and a
// cgo preamble are code whole; and a file's build constraints are compared as
// go/build reads them from its header. Each is read as its tool reads it:
// go generate's commands as go generate does, a line at a time whatever
// lexeme a line sits in — a command in a block comment or a raw string is
// one — and a directive's text with the carriage returns the compiler reads
// and the lexer drops. And since a linter's reach is lines — a nolint's group
// and the statement under it, the first line of the node a lint:ignore is
// attached to, revive's next line or every line to its enable — a file
// holding a linter's directive holds the line each code token sits on: a
// token moved onto or off a line there is code. Documentation — a doc
// comment above a pragma, a license above a //go:build line — changes
// freely. A class is judged a path at a time and gives none to a version
// past [MaxClassedBlobBytes]. The walk holds one gap of a version's comments
// at a time — those between two code tokens, as offsets into it — and keeps
// only the verdict of one a later token decides, so what a path costs
// follows its bytes, whatever they hold: a file of nothing but comments takes
// some three times the time of code as long ([MaxClassedBlobBytes] has the
// measure), where keeping every comment to the walk's end took nine, and
// thirty times the allocation.
//
// A REVISION'S CLASSES RIDE ITS FACTS ([Revise], revise.go): the home's git
// node lexes each touched Go file changed in place — a regular file on both
// sides, in one mode — within [MaxClassedBytes] of Go a revision, and the
// facts name each classed path by its place in the touched list
// ([RevisionClasses]), INCOMPLETE where a bound or the read's budget cut the
// lexing short or the revision touches all, so a path no list names is one
// of no class only where they are complete. [ChangeClassNoCodeChange] is no
// path's: it is a relation between two revisions, which the facts make
// judgeable by recording the trees of the base and the head — equal pairs
// hold the same code, which an approval carries across ([ReadinessOf]).
//
// # A protection rule's merge gate rides it whole
//
// A branch rule carries its MERGE GATE ([MergeGate]) — who may merge into the
// branches it matches, the approvals it waits for and the paths only a person
// approves, the checks that must pass, how a queue lands a change — as ONE
// value on the rule ([Protection.Gate]), stored as one column of its JSON
// (code_protections.gate) rather than a column a field: nothing filters on a
// field of a gate, and whatever judges a merge reads a target's matching
// rules and each gate whole. A record carries every field of it explicitly,
// its writer having resolved each default, in the one order [CanonicalGate]
// forms; [RepoSettings.Validate] holds it to that order, to its bounds
// ([MaxHumanPaths], [MaxRequiredChecks]) and to [MaxGateBytes] of JSON, and
// refuses one on a tag rule — a tag never moves, so no merge reaches it. A
// class that exempts a change from an approval is a PATH class: an approval
// already carries across a revision that changed nothing, so no_code_change
// has nothing to exempt.
//
// TWO RULES ARE COMPARED BY VALUE ([Protection.Equal]), never by ==: a gate is
// a pointer, and two reads of one rule are two pointers. Compared by ==, every
// reconcile of an unchanged declaration would write a settings record for each
// gated repository ([RepoSettings.equal]).
//
// NO ROW OF A REPOSITORY CARRIES A GATE. A push is judged by its rules' flags
// and direct push and never by a gate, and a repository's row ([RepoRef]) is
// what the admission of every git request reads ([Reader.Resolve]), every page
// of a git node's sync carries ([Reader.Snapshot]) and its cache's state file
// keeps, rewritten and made durable at every round that changes anything — on
// every push, on every git node. So the row's rules are each rule as a push is
// judged by it ([PushRule]), read without the gate's column ([pushRulesIn]),
// as the push decide's are and a listing's: at their bound a repository's
// gates are half a mebibyte of JSON, which each of those would decode, ship
// and write to disk for nothing. TYPED APART, rather than a Protection whose
// gate a read left nil, so no consumer can take a gate nobody read for a rule
// that has none. A read that presents the gates asks one repository's detail
// for them ([ReadQuery.Gates], [RepoDetail.Gates]) — the API's question of a
// repository, code_get_repo — and a decide that rewrites the settings reads
// the rules whole beside the row, as it reads the path owners
// ([protectionsIn]): the lifecycle decides and the reconcile.
//
// A TOOL CHANGES A RULE A CONTROL AT A TIME AND A GATE A FIELD AT A TIME
// ([ProtectChange], [GateChange]): a protect request names the push controls
// and gate scalars it replaces and the items it adds to or removes from a
// gate's lists ([ListChange], [HumanPathsChange]), and the lifecycle decide
// puts them in the rule its own snapshot holds for the pattern, every other
// control, field and item kept — or at its least or default, on a pattern
// with no rule or a rule with no gate — since a rule, a gate or a list whole
// is one a caller could form only from a read made elsewhere, and two
// requests changing two of them at once — two checks added — would each undo
// the other's. A seat's change is weighed field by
// field ([MergeGate.LoosenedBy]): it raises a field and only a person lowers
// one. The decide judges and resolves against the rules it reads whole beside
// the row, which alone carry a gate, and the request's answer names the
// pattern's rule whole beside the row ([LifecycleResult.Rule]) — read back by
// the table's key with it ([ruleIn]), or as the decide held it where the
// request needed no record — so its caller learns what the gate became
// without reading the repository's every rule.
//
// # A repository names how it is searched
//
// A repository's [SearchMode] is a setting like its visibility, carried whole
// by every record of its settings ([RepoSettings.Search]) and answered on its
// row ([RepoRef.Search]): indexed, read from an index of its default branch by
// a search across the company's repositories, or grep, read only by a search
// naming it. Its default is applied where the settings are formed — indexed for
// a declaration, grep for a tool's create — and a SCRATCH repository is grep
// for good: [RepoSettings.Validate] refuses its index, and a settings change
// asking for one is refused not_permitted naming why, a person's included. The
// rows carry the mode in code_repos.search, beside code_repos_search_idx over
// the active and archived repositories by mode and then name — the walk of the
// [ReadIndexed] listing, by which a git node's indexer finds the repositories
// whose code index it may keep, the first [IndexHolders] of each one's git
// order (internal/code/cache). This package records the mode, serves it and
// lets a maintainer change it; it indexes and searches nothing.
//
// # What this package does not do
//
// It runs no git and serves no HTTP. It decides and applies records, commits
// the pushes a receiving node proposes, the compactions and expunges a git
// node proposes, the purges a person asks for and the lifecycle requests a
// tool sends (lifecycle.go: a repository made, renamed, changed, retired or
// held by a tool, and the code-retention pass's release of an old name and
// expiry of an idle scratch repository), the same pass's pushes deleting
// seats' idle branches (retention_expiry.go), answers reads of the rows,
// holds a bundle's bytes to the row that names them when a caller tees them
// through a [SealWriter], and holds a published layer's files to theirs
// ([CheckLayer]). It moves object bytes in TWO places, each the one way its
// kind of object moves: [StoreBundle], the upload of a pack bundle
// [LayBundle] laid out from the files a git wrote, sealed on its way — the one
// store every writer of a stored pack takes, through the [Putter] it is
// handed, so none lays a bundle out or seals it by a copy of its own — and a
// published layer's parts, cut by one rule both ways: [StoreLayer] uploads
// them at once, and [FetchLayerPart] lays each back where it came from, read
// through the store's verified read.
//
// # Five horizons, each a function of the record that applies it
//
// The applier keeps five horizons inside a record's OWN repository, each
// chosen by the record's broker instant ([statelog.ApplyOptions.StoredAt]) or
// by what the record itself names, and never by a node-local sweep, because a
// sweep would be a second writer of the replicated estate:
//
//   - PUSH HISTORY, [PushHistory] (181 days, usage.History): a push record
//     removes up to [HorizonDeletesPerRecord] of its repository's push rows
//     older than that, and no more once their bytes reach
//     [HorizonBytesPerRecord] — the most a record can add, so removal keeps
//     pace, and no more, because a push row is the widest row the domain
//     keeps and its delete frees its overflow pages — so "what happened over
//     the last half year" has one answer across the engine. It reads them
//     oldest POSITION first and stops at the first inside the horizon, so a
//     row stamped earlier than the one before it (a leader change) waits for
//     that one, at most the members' clock skew past its own horizon;
//   - REF TOMBSTONES, [RefTombstoneHorizon] (48 hours), or sooner — oldest
//     first — past [MaxTombstonesPerRepo] (100 000): a deleted ref is kept
//     as a tombstone so a cache syncing a delta learns of the deletion, and a
//     push record sweeps its repository's due ones — as many as the record
//     leaves of [MaxRecordRows], because one record can make over a
//     thousand and a fixed share fell behind any repository that deletes
//     refs in bulk, and never fewer than the record's own deletions, for
//     each of which the committer keeps a row in a record's bound — and
//     raises the mark past which a delta is no longer complete; a record
//     whose deletions its sweep cannot make room for, which no committer of
//     this build forms, removes their rows outright and raises the mark to
//     its own position. So a deletion never counts against the live refs'
//     [MaxRefsPerRepo] and never refuses a push, the refs table holds at
//     most the two bounds' sum, and a delta is answered only while it
//     carries no more rows than the full answer (so a cache synced before a
//     clean-up larger than what the repository keeps reads what is left,
//     never every deletion);
//   - REF DROPS, [PruneGrace] (14 days, git's own gc.pruneExpire): a forced
//     update or a deletion records the object it moved its ref off
//     (code_ref_drops, one row an object, keyed by its record's broker
//     instant and position), because no ref names that object any more and
//     dropping a repository's unreachable objects must keep what such a push
//     left for the grace — history force-pushed away or deleted stays
//     fetchable by its id. A push record forgets its repository's drops past
//     the grace, oldest first by instant, with whatever its rows leave of
//     [MaxRecordRows] once the tombstones have had theirs: after them, since
//     a drop forgotten late costs a row where a tombstone's room taken costs
//     every git node a full read, and no fixed share, which falls behind even
//     one push of [MaxCommandsPerPush] forced updates. That keeps pace with
//     one such push a record, and with deletions while their tombstones are
//     not yet due; it falls behind a record the committer fills with them —
//     some 1 900 drops a record of forced updates, up to 1 246 a record of
//     deletions whose tombstones come due in turn, four behind one push of a
//     thousand of those — and what it falls behind by waits for records with
//     room, up to MaxRecordRows-4 a record, so the drops past the grace grow
//     while a repository's records stay full of them and drain once they do
//     not ([Applier.expireRefDrops]). The drops are the record's own rows, so
//     the committer counts them in its bound: a forced update two rows, a
//     deletion three;
//   - A PURGE, in bounded parts: a repository deleted longer ago than its
//     retention is removed [PurgePartRows] rows a record, its push rows and
//     then its changes' bodies only until together they weigh
//     [PurgePartBytes] ([Store.PurgeStep]), its changes last and each whole,
//     a change's revisions, authors and body before its row
//     ([purgeChanges]), and the final record — formed only once what is left
//     fits a part, in rows and in bytes — leaves only its deletion marker,
//     which drops every later record about the repository on every node, for
//     ever. It is a horizon no record of the repository's own can reach, since
//     nothing else is written to a deleted repository, so the
//     code-retention duty's pass ([Retention]) publishes its steps: every
//     purge already begun first, whatever the retention, then the deleted
//     oldest first. It publishes RECORDS and never touches a row, so the
//     applier is still the estate's only writer, and it PACES them,
//     because every data node applies every one on its single write lane:
//     a pass waits twice a step's duration after each
//     ([RetentionLaneShare], a third of the lane) and takes steps for
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
//     empty repository; and an id no repository had is none;
//   - RETIRED PACKS, [RetireGrace] (a day): a pack a record retires leaves
//     code_packs for code_packs_retired, a RETIRED reference
//     ([RetiredPackReferences], ADR-0051) that keeps its object from the
//     collector and does nothing else, for longer than the longest legal read
//     of the largest bundle a row can name — so a reader that began on the
//     pack before its retirement finishes it (ADR-0060). Like a purge it is no record's
//     own horizon, so the code-retention duty's pass sweeps it, before it
//     purges anything ([Store.RetireSweep]): a retire_sweep record removes,
//     oldest first, the rows whose broker instant the duty's clock puts past
//     the grace — at most [RetireSweepRows] a record, never part of one
//     retirement — and raises the repository's retired_through mark. The
//     record names the position it cut at, so the apply reads no clock; the
//     duty's clock against the broker's is what the grace's margin past the
//     longest read is for. A purge takes a repository's retired rows with
//     the rest, whatever their grace, since no read begins on a deleted
//     repository — and its ref drops, whose objects go with its packs. Until its sweep a retirement is what a cache holding the
//     pack learns it from: a snapshot serves it in a RETIRED phase — a delta
//     every retirement above its floor, a full answer those above its
//     caller's [StateQuery.RetiredSince] when it can serve every one, at
//     most [MaxFullRetirements] (a page, against a commit-graph every git
//     node holding the repository would otherwise drop and rewrite) — and
//     the mark is the one past which a delta is no longer complete, so a
//     delta from below it is answered in full, as below the tombstone mark.
//     Page 1 weighs a delta against the full answer with the retirements
//     each would serve, so those both would serve — every one, when the
//     caller asks from the delta's own floor, the position it holds — cancel,
//     and a delta is never replaced by a full answer carrying them again
//     beside every row; only those the full answer cannot serve weigh against
//     the delta alone.
//
// # A compaction merges packs, decided alone
//
// Every push stores a pack, so a busy repository's pack count grows with its
// pushes, and every cold sync pays a GET a pack and every request a lookup a
// pack. A COMPACTION merges some of them into fewer: the repository's HOME git
// node (internal/code/compact, ADR-0059) builds the outputs, stores each as one
// bundle and proposes ONE record — OpCompact on the repository's subject
// ([Compaction]), naming the packs it retires and the rows of the outputs it
// adds — which the repository's committer decides
// ([Committer.ProposeCompaction]). Merged without judging reachability and
// retired for longer than the longest read: ADR-0060.
//
// IN THE PUSHES' QUEUE, DECIDED ALONE: a compaction waits in the repository's
// one committer queue in its arrival order, held to a push's bounds there —
// the queue's count and bytes, the node's bytes, its asker's deadline — and
// answered busy past them the same way (crewlet.code.commit.busy, kind
// compaction); but no batch carries it beside a push, or beside another, so
// its decision is its own and its refusal refuses nothing else, and what it
// should cost the pushes queued behind it is a record's slot, its decide
// reading its inputs' rows where a batch reads its pushes' — a claim, not a
// figure: codecluster's BenchmarkCommitChain is there to measure it, its
// mix=compaction arm beside mix=packs. Its OPERATION
// ID IS THE COMPACTION'S ID, minted once by the node that built it, so a
// compaction asked again of the repository's runner-up after a failover is
// collapsed by the ledger onto the record that landed rather than decided
// again — which would find its inputs retired and answer it stale; its
// SESSION is the code log's position the builder planned at
// ([Compaction.Seen]), so it is decided from rows at least as new as the
// plan's — a committer that cannot reach them in time decides nothing, and
// the estate router asks the runner-up. Its rules, in
// their order, are compaction.go's: the repository not gone (archived is
// decided); every input one live row of part pack, and not full
// ([PackRow.Full]) — else stale; every output a bundle [PackRef.Check] admits
// — else pack_too_large — whose key was minted within objstore.RecordWithin —
// else upload_expired, the push's own rule; and no more objects out than the
// inputs' rows hold, else a builder's bug ([ErrInvalidProposal]), refused
// before any record. It is answered applied only from the outputs' rows at
// its record's position.
//
// THE APPLY MOVES ROWS, NEVER BYTES: every row of an input leaves code_packs
// for code_packs_retired, stamped with the record's position and broker
// instant; each output joins code_packs, a live pack of kind compact, its
// content dated from the oldest input's ([PackRow.Content]) — a merge moves
// content and judges none of it, so a purge requested before it still asks
// about what it holds; and the repository's counters move by the rows the
// apply found and wrote, never by the record's lists, so a replay moves
// nothing. A retired pack keeps its
// object for [RetireGrace] (the RETIRED PACKS horizon above), so a reader
// that began on it before the compaction finishes it.
//
// A MERGE MAKES NO REACHABILITY JUDGEMENT (ADR-0060): every object of the
// inputs is in the outputs, so a push decided against the inputs never lands a
// ref whose closure a compaction removed, and two compactions planned from
// different snapshots never each drop what the other relied on. Removing what
// no ref reaches is no compaction's to do.
//
// ITS BOUNDS, each with its reason where it is defined: at most
// [MaxCompactInputs] (1 024) inputs and [MaxCompactOutputs] (16) outputs, so
// a compaction's record writes at most 2 065 rows (compactDecidedRows), about
// half of [MaxRecordRows] — a publication's, the op's widest, 2 609 (below) —
// and every output can be uploaded at once within RecordWithin; outputs cut
// at [CompactPackBytes], the 1 GiB push ceiling, and a pack FULL — merged by
// no later compaction — past [FullPackBytes] of pack or [FullBundleBytes] of
// bundle; and the plan's own, [CompactFactor], [CompactMinMerge],
// [CompactTargetPacks] and [MaxPacksPerRepo].
//
// THE BOUND HOLDS PUSHES BACK, never a compaction: a push carrying a pack into
// a repository that holds [MaxPacksPerRepo] live packs, its batch's earlier
// entries' packs counted in, is refused [RefusalCompactionBehind] WHOLE and
// undecided by the push decide (decide.go's pack bound) — and before its
// bundle is uploaded by the receiver, which reads the row again before it
// refuses (internal/code/host) — so a repository's pack count, and with it
// every cold sync's GETs and every request's pack lookups, stays bounded
// whatever its pushes do. A compaction adds no pack and is decided at the
// bound as below it: it is what makes room.
//
// A git node finds the repositories a compaction may be due in by the
// [ReadCompactionDue] listing: the active and archived repositories holding
// at least MinPacks live packs, MOST FIRST, each with its row's version and
// the objects its packs hold, one backward walk of code_repos_packs_idx
// (replicated 0060) — the engine's own read, which names no asker.
//
// # A publication puts a published layer on the chain, decided as a compaction
//
// A git node serving a repository answers a fetch from a multi-pack-index and
// its bitmap, and writing them costs the history whatever was added since — so
// the code host writes them ONCE for the fleet and every git node installs
// them: a repository's PUBLISHED CHAIN, git's own incremental multi-pack-index
// layers with their bitmaps and commit-graph layers, root first, its rows in
// code_layers and its bytes the parts code_layer_parts names
// ([LayerReferences], DERIVED under ADR-0051: kept while named, never audited,
// never carried by a backup, since any git node can build a layer again). A
// layer is put on the chain by a PUBLICATION: a compaction carrying a
// [LayerRef] ([Compaction.Layer]), so the repository's committer — the one
// writer of its packs — is the one writer of its chain, its repack (when it
// has one) and its layer in one record. Its rules are publication.go's: no
// compaction retires a pack a layer names unless it replaces that layer; the
// layers it replaces are the chain's top and its base the one below; the
// chain it leaves at most [MaxPublishedLayers]; every pack it names live; and
// every pack live at its plan ([Compaction.Seen]) that its base does not
// name, it names — the closure git's bitmap needs, since a layer over fewer
// packs, a cut's full packs alone among them, is refused by git and writes no
// bitmap. Its parts are held to the push's upload rule, at most
// [MaxLayerParts] of [LayerPartBytes] each. Its record is the op's WIDEST:
// beside a repack at the compaction's bounds, a layer of MaxLayerParts parts
// replacing [MaxPublishedLayers] − 1 layers of as many is 2 609 rows
// (compactDecidedRows), about two thirds of [MaxRecordRows], and its apply
// alone reads the chain whole and the live packs again — held, measured, to
// the third of statelog.ApplyTxTimeBudget a compaction's apply is (# Measured).
//
// THE APPLY REPLACES THE TOP: the replaced layers and their parts go — at
// once, a derived row owes no grace — the layer takes its base's place plus
// one, and the repository's published packs and objects are RECOUNTED from the
// rows, so live_packs − published_packs is the tail no layer covers — as of
// that recount: a push moves only the live counts, so a pack pushed since that
// is byte-identical to a published one is in the tail until the next
// publication's (replicated 0062 says why no push recounts). A publication
// that does not fit the chain it meets applies as nothing, whole, on every
// node; one applied already finds its layer and changes nothing. A snapshot
// serves the chain WHOLE on page 1, first, whenever it moved above the
// delta's floor ([RepoSnapshot.Layers]), and the publisher finds the
// repositories a publication may be due in by the [ReadPublicationDue]
// listing — every active and archived repository whose tail is not empty:
// each holding a pack no layer covers, and any holding only such a
// byte-identical pack, which has nothing to publish — LEAST RECENTLY
// PUBLISHED FIRST, one walk of code_repos_unpublished_idx (replicated 0062),
// the engine's own read; it names none with an expunge open, whose
// publication the expunge's rule refuses until its commit (below). A purge removes the chain with the rest of a
// repository, in its final record: the chain is bounded apart from what a
// part counts.
//
// # A layer's files are git's, read without git
//
// A git process trusts a layer's files by their names — a layer's
// multi-pack-index and its commit-graph layer are named by their own trailing
// checksum, and git reads neither checksum when it opens one — so every git
// node holds a layer's three files to its row before any git process is
// handed them, and the maintainer states a row's byte fields from the files
// its build wrote, by ONE reading of git's three formats (layer.go,
// ADR-0008): [ParseMidx], [ParseBitmap] and [ParseGraph], each rule citing
// the git 2.56.0 reader it follows, as exact as git's writer about what every
// file it writes has and as lenient as git's reader about what that reader
// ignores. [CheckLayer] is the install's half — each file exactly its
// section, the multi-pack-index's trailer the row's id and its packs the
// row's, the bitmap that multi-pack-index's, the commit-graph layer's bases
// the GraphIDs of the rows below it and its filters version 2 — and
// [LayLayer] the publisher's, which holds the files to each other and to the
// base the build was written over by the same comparisons, so a build whose
// files no node would install — a commit-graph layer over another base among
// them — is refused before a byte is stored. A
// refusal is [ErrBadLayer], definite and never retried, apart from a read
// that failed. No reader holds a file: each reads a header, a table of
// contents and the chunks a count is read from, every one held to the file's
// size before it is read, and hashes the rest as it streams past. A layer's
// bytes, the three files back to back, are stored as [LayerPartBytes] parts
// ([LayerContentType]), every part uploaded at once so every key is named
// within objstore.RecordWithin.
//
// # Git's trailer is one rule
//
// Every file git writes through its hashfile — a pack, its index, a
// multi-pack-index, its bitmap, a commit-graph and its layers — ends in the
// hash of every byte before it, which git never reads back, and a git node
// holds such files to it in two places: its cache's check and scrub
// (internal/code/cache) and a published layer's check. Both judge by ONE
// implementation, [HashTrailer] over the piece loop [HashPieces]
// (hashfile.go, ADR-0008), because the same layer files reach both — fetched,
// then kept. It reads a file a PIECE at a time, each in a slot its caller's
// hold takes ([Pieces]: a git node's read lane, a piece of the cache's
// CheckPieceBytes), checking its context before every read: the root layer of
// the largest repository the defaults admit is some 2.6 GB, seconds of a core
// warm and minutes cold, which a node stopping, or a request queued in the
// lane behind it, would otherwise wait out whole.
//
// # An expunge drops what no ref reaches, in two records
//
// Nothing else ever removes an object a push stored — a force-push or a
// deletion leaves the history behind it in the repository's packs — so an
// EXPUNGE is the one rewrite that drops objects, in two records of their own
// on the repository's subject, each decided alone by its committer in its one
// queue ([Committer.ProposeExpunge], decide_expunge.go), as a compaction is:
//
//   - its BEGIN ([ExpungeBegin]) rewrites some live packs — a full pack among
//     them — into packs holding what a ref reached at its maintainer's scan,
//     which stay live (kind expunge), and packs holding the rest, which are
//     DROPPING (kind cruft, [PackDropping]): still rows of code_packs naming
//     their objects, still served, and never a pack a new push may rely on.
//     It keeps every object of its inputs, so no push decided against them
//     loses one to it; it moves the repository's DROP EPOCH, which every push
//     carries as its receiver's copy held it ([Proposal.DropEpoch]), so a
//     push checked before the begin is refused expunging
//     ([RefusalExpunging]), whole and undecided, rather than land a ref onto
//     an object the begin made dropping and the commit retires; and it
//     removes the published layers over its inputs, from the lowest naming
//     one up;
//   - its COMMIT ([ExpungeCommit]) retires the dropping packs into
//     code_packs_retired, where each keeps its object for [RetireGrace], and
//     keeps as a live RESCUE (kind rescue) whatever of them a push that
//     landed between the scan and the begin made reachable again. Its one
//     precondition is its expunge, open at its epoch with exactly those
//     dropping packs — no ref — so a repository pushed to all day still
//     commits.
//
// A person may ask for one: a REQUEST ([ExpungeRequest]) records a purge and
// nothing else, the repository holds it open — a later request WIDENS it to
// its own position, since it asks about more: a pack pushed since, and what a
// ref dropped since left — and it is SERVED, cleared, once no pack of the
// repository dates its CONTENT from before it, at a commit or, for a
// repository holding no pack, at once. A pack row names the position its
// content dates from as a purge reads it ([PackRow.Content], replicated 0067):
// a push's pack its own; a compaction's outputs, an expunge's that keeps a
// ref drop's grace and every rescue the oldest of the packs they replace,
// since moving content judges none of it; and a purge's kept and dropping
// packs its scan, where it judged them. So a rewrite that judges nothing —
// a compaction in flight when the person asked, the maintainer's own expunge
// keeping a grace — never serves a request, or strands one, by adding its
// outputs after it, and a purge's cycle takes the live packs that date from
// before the request it serves. While an expunge is open, a second begin is
// stale and so is a publication (publication.go's rule 6g); a compaction of
// live packs is decided as ever. A pack row also names WHEN its newest
// content arrived ([PackRow.Arrived], replicated 0100): a push's and a queue
// merge's pack its record's broker instant, and every rewrite's output the
// latest of the rows it retires — the record's own instant where it retires
// none — so the maintainer's own expunge keeps every pack that arrived within
// [PruneGrace] whole by a column, never a position, which says nothing of
// time. The repository's maintainer finds the expunges it owes by the
// [ReadExpungeDue] listing, in two arms: first the active and archived
// repositories with an expunge open or a request waiting, by id, one walk of
// code_repos_expunging_idx (replicated 0064); then, asked with
// [ReadQuery.ExpungedBefore], every other active or archived repository of
// any kind last expunged at or before that instant or never, least recently
// expunged first, one seek of code_repos_expunge_due_idx (the same
// migration) — the engine's own read, its maintainer's weekly schedule — and
// reads what a cycle keeps beside the refs by the [ReadExpungeTips] read: the
// objects the repository's ref drops left since an instant, one covering seek
// of code_ref_drops' key, which replicated 0061 led with the instant for it;
// the grace an expunge keeps is the instant's distance from its scan, none for
// a purge (internal/code/compact builds and sorts the packs). Its
// bounds: at most [MaxExpungeInputs] inputs and [MaxExpungeOutputs] outputs a
// begin, so its record writes at most 2 625 rows (expungeBeginRows), the
// widest record of the domain, held to the third of
// statelog.ApplyTxTimeBudget a compaction's apply is (# Measured).
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
// sixty-four (29 MB) and added 60–82 ms; 10–12 ms for a group commit of
// [MaxPushesPerRecord] pushes of one command each, which is 0.16–0.18 ms a
// push against 3.4–4.8 ms for one push alone — the commit, not the rows, is
// a small record's cost, and group commit is what pays it once; and 53–61 ms
// for one small push whose record sweeps a whole row budget of expired
// tombstones (3 995).
//
// THE DELETIONS, since each records the object it left (code_ref_drops,
// keyed by its record's instant since replicated 0061) and the committer
// counts it three times in its bound — its tombstone, the sweep's room for
// one and its drop — measured in one bounded run (-benchtime 5x, one count:
// five iterations an arm) on four E-cores of a Core Ultra 7 265U at a load
// average of 0.8–1.1, each arm interleaved with the build before the drops
// and the build that gave them a second index, where the sweep of 3 995
// tombstones above took 40 ms in all three against the Xeon's 53–61: 57 ms
// for the most deletions a record the committer forms carries (1 310 in two
// pushes) at a repository holding [MaxTombstonesPerRepo] expired tombstones,
// which writes its 1 310 tombstones and as many drops and sweeps the 1 374
// its rows leave — every row of MaxRecordRows touched, as the build before's
// shape of 1 965 deletions counted twice did in 67 ms in the same run (96–105
// ms on the Xeon); 73 ms with the widest expired push rows waiting as well,
// the longest shape of a record that moves no live change (one that moves
// the most is longer: below), under half of statelog.ApplyTxTimeBudget; and
// 79 ms for a record whose deletions are
// REMOVALS — three pushes of [MaxCommandsPerPush] deletions at a repository
// already holding [MaxTombstonesPerRepo] fresh tombstones, which writes the
// 3 000 objects they left as drops beside removing their refs — a record no
// committer of this build forms, whose cost grows with what it carries, and
// which stays under the budget. What its 3 000 drops add: 29 ms in one
// bounded run of both builds on the same machine (the review of the change
// that added them) — 56 ms for the build before the drops, 85 for the one
// that wrote them beside their second index, some 10 µs a drop — and the run
// above put that build at 86 ms and this one, which writes each drop into one
// b-tree fewer, at 79. One count of five bounds a shape from above rather
// than states it: a stall in one iteration moves the mean by a fifth of the
// stall, and the build before's own removal took 116 ms in the run above,
// against the 56 the review's run measured.
//
// THE DROPS' EXPIRY (the expiry and forced arms, the same run): 22 ms for one
// small push whose record forgets a whole row budget of drops past their
// grace (3 996), against 40 ms for the sweep of 3 995 tombstones, since a
// drop is one index entry and a tombstone three; and 67 ms for one push of
// [MaxCommandsPerPush] forced updates at a repository holding as many expired
// drops, which writes its thousand refs and drops and forgets the 1 997 its
// rows leave — every row of MaxRecordRows touched, the most drops a record
// both writes and forgets.
//
// THE PUSH INDEX IN LOG ORDER (code_pushes_repo_idx reordered by replicated
// 0056) moved none of it past the noise: interleaved with the build before
// it on four vCPUs of a 2.1 GHz Xeon under a load average of 20–40 — six runs
// of thirty — a group of [MaxPushesPerRecord] pushes is 18.8–32.4 ms against
// 18.1–29.4, 0.29–0.51 ms a push, and one push alone 6.7–14.2 ms against
// 8.1–12.4; in four runs of five of every shape, each arm's figures
// overlapped the build before's. And a push row's bytes are unchanged to the
// byte
// (BenchmarkPushHistoryBytes, both builds): 395.3 a push of one commit,
// 752.6 with its pack row, 420 045 and 420 454 at the widest — a reorder
// moves the columns of the one entry a row has, where a second index would
// have added about a quarter to the 395.3.
//
// THE LIVE REFS' INDEX (code_refs_live_idx, replicated 0054, which the push
// decide's [RefusalRefConflict] probe reads — the decide below) is an entry
// more for every live ref a record writes, and one moved for every ref it
// deletes or creates again. Measured interleaved with the build before it,
// ten counts of five each on four vCPUs of a 2.8 GHz Xeon at a load average
// of 1.3–2.1: the most ref creations a record carries went from 93–127 ms
// (median 105) to 100–151 ms (median 125), about 5 µs a created ref; the
// most deletions (the 1 965 a record carried before each wrote its drop)
// from 97–116 ms to 101–154 ms (median +6 ms); the removals (before they
// wrote theirs) from 67–115 ms to 69–93 ms (median +9 ms); and the group,
// the one push and the sweep of tombstones, which the index holds none of,
// moved nothing past the noise (four counts). The slowest record measured
// with the index there, 154 ms, is three fifths of
// statelog.ApplyTxTimeBudget — and the slowest without it, 150 ms, as much:
// on that machine the history arms' spread is the machine's, not the
// index's.
//
// THE CHANGES A RECORD MOVES (the changes and group+changes arms, each
// decided over its rows before it is applied, so each is a record the
// committer forms; one bounded run, -benchtime 5x and one count, on two
// E-cores of a Core Ultra 7 265U under other sessions' load — a load average
// of about 4–7 — interleaved with the build before it, two rounds): 172–189
// ms for the most live changes one record moves — a push of
// [MaxCommandsPerPush] fast-forwards of changes' sources by their author and
// one deleting the sources of the 232 more its rows leave, 1 232 changes
// written back beside a thousand revisions, the refs and the deletions'
// drops — the longest shape a record the committer forms has, at seven
// tenths to three quarters of statelog.ApplyTxTimeBudget; and 19–26 ms for a
// group of
// [MaxPushesPerRecord] pushes each moving one change's head, where the same
// group moving none took 10–18 ms. The build before wrote each change with an
// UPDATE of its own: 276–334 ms for the first, past the budget, and 28–36 ms
// for the second. What the batch leaves is the driver's: every value a
// statement binds is a call into the engine of its own, and a change is
// written back whole — its upsert's VALUES carry every NOT NULL column — so
// the book reads every column of every change the record moves.
//
// EACH CHANGE WRITTEN BY WHAT MOVED ([changeWrite]), so a head move rewrites
// its change's entry in code_changes_target_idx alone, where the record's one
// write of every column a push may move rewrote five entries of every live
// change it moved — 4 000 fewer index entries deleted and inserted again for
// the thousand heads of the first shape. In the same bounded form, one run of
// each build back to back under a load average of 5–8: 186 ms for the first
// and 25 ms for the second, against 192 ms and 28 ms for the build before —
// inside what this machine moves between two runs, since the benchmark's
// estate is small enough to hold every index in the page cache, where an
// entry rewritten costs a few microseconds of the engine and no read; what a
// node pays for the entries beyond that, the index leaves a commit dirties
// and writes, grows with the indexes, which grow with the company's changes.
//
// A CHECK RECORD (BenchmarkApplyACheckRecord, the record applied and
// committed; one bounded run, -benchtime 5x and one count, on two E-cores of
// a Core Ultra 7 265U under other sessions' load), every one of which every
// data node applies, four an attempt: 2.1 ms for a claim of the widest queued
// attempt — its row read whole and written back, a definition of some 200 KB
// of escaped text among it; 4.8 ms for the widest ending a writer forms, with
// its log and nothing of its repository past the history; 14 ms for that
// ending removing [CheckHorizonRows] narrow rows and their logs, the most
// rows an ending's apply touches (1 002); and 6.9 ms for one removing four
// rows of the widest a check row can be (some 290 KB each) and their logs,
// the first past [CheckHorizonBytes] — a mebibyte of overflow pages and the
// entries of the five secondary indexes a row carries, some 2 ms, the push
// rows' 2.4 ms a megabyte. Every shape is under a tenth of
// statelog.ApplyTxTimeBudget: the horizon is bounded by its rows' share of a
// record, never by its time (CheckHorizonRows).
//
// A PURGE RECORD (BenchmarkApplyAPurgePart): 56–60 ms for a part of
// [PurgePartRows] push rows of a kilobyte; 81–88 ms for a part over the
// widest push rows, which [PurgePartBytes] stops at seventy-three (34 MB) —
// bounded by rows alone, a part of 1 000 such rows held the lane 1.05–1.10 s,
// about 2.4 ms a megabyte, and one of [PurgePartRows] about three seconds;
// 76–103 ms for the most one part can take, the widest rows short of
// PurgePartBytes and refs for the rest of the rows; and 57–65 ms for the
// final purge over the same, with [MaxProtectionsPerRepo] rules. A part of a
// repository's CHANGES, each with a body at [MaxChangeBodyBytes] (the
// changes, wide+changes, wide+bodies, final+bodies and cut arms, three
// iterations of one count beside the rest, on two E-cores of a Core Ultra 7
// 265U, where narrow took 44 ms and wide+refs 56 ms): 50 ms for 750 changes
// of a revision, an author and a body each, taken whole — PurgePartRows rows
// and 24 MiB of bodies, the most a part's walk counts and its four deletes
// take; 53 ms for the most one part can take of push rows and bodies, both
// bounds at once — nineteen of the widest push rows and 745 such changes —
// and 43 ms for the final purge over the same, with the rules; 41 ms where
// the widest push rows short of PurgePartBytes leave room for six bodies, of
// which by their rows alone the part would take 732, 24 MB more (the review
// that weighed them measured that shape, its bodies then on the change's
// row, at 112 ms against 64 ms without bodies); and 23 ms for a part that
// cuts one change of [PurgePartRows] revisions, oldest first.
//
// A RETIRE SWEEP (BenchmarkApplyARetireSweep, the record applied and
// committed; BenchmarkRetireSweepStep, the whole step — the decide's read,
// the append and the holder's own apply, the duration the retention pass's
// pacer times — on four E-cores of a Core Ultra 7 265U shared with another
// session's builds, one count of five iterations): the record holds the
// write lane 1.5 ms for a sweep of eight rows, whose commit is its cost, and
// 40 ms for [RetireSweepRows], under the third of
// statelog.ApplyTxTimeBudget a purge part is sized to; the step is 4.9 ms
// for eight rows, and 69 ms for RetireSweepRows (three iterations — five,
// under the other session's disk load, averaged 378 ms). A sweep of a few
// rows is a seventeenth of a purge part, and [RetentionPassBudget] states
// what the sweeps take of a pass before the purges have the rest.
//
// A CHECKS SWEEP (BenchmarkChecksSweepStep, the whole step as the retention
// pass's pacer times it; one bounded run, -benchtime 5x and one count, on
// two E-cores of a Core Ultra 7 265U under other sessions' load): 3.5 ms for
// a sweep of eight rows, each with its log, and 13 ms for [CheckHorizonRows]
// rows and their logs, the most one step removes. [RetentionPassBudget]
// prices the checks step from them: listed only past [CheckSweepGrace], a
// repository is swept at most once a day.
//
// A COMPACTION RECORD (BenchmarkApplyACompaction, the record applied and
// committed against a repository of [MaxPacksPerRepo] live packs, each arm
// writing the most outputs its inputs allow, so the most rows its input count
// can touch, and each shape Validated and decided over the rows before it is
// applied; on two E-cores of a Core Ultra 7 265U at a load average of about
// 3.5, in one bounded run, -benchtime 5x and one count: five iterations an
// arm): 3.3 ms for eight inputs, 8.0 ms for sixty-four, and 62 ms for
// [MaxCompactInputs] — 2 065 rows. A PUBLICATION over a chain of
// [MaxPublishedLayers] layers naming every live pack, sixty-four a layer and
// each of [MaxLayerParts] parts, whose layer of as many parts replaces every
// layer but the root: 18.5 ms with no repack — 545 rows, the chain's 2 048
// checksums read whole before and the live packs again for the recount — and
// 68 ms beside a repack of MaxCompactInputs inputs into [MaxCompactOutputs]
// outputs, every bound at once: 2 609 rows, the widest record of the op,
// which its rows alone would have priced at 78 ms (2 609 at the 1 024-input
// arm's rate): a layer's rows cost less each than an input's. Both
// 1 024-input arms are under the third of statelog.ApplyTxTimeBudget (83 ms)
// that bound is held to, the publication by 15 ms: an arm past the third on a
// slower machine halves MaxCompactInputs — or, where the publication's alone
// is past it, bounds a publication's repack below it — until it is under.
//
// AN EXPUNGE'S RECORDS (the same benchmark's expunge arms, one bounded run on
// the same two E-cores at a load average of 3.4–4.7, -benchtime 5x and one
// count): 75 ms for a BEGIN at every bound at once — [MaxExpungeInputs]
// inputs into [MaxExpungeOutputs] outputs, half kept and half left dropping,
// dropping a chain of [MaxPublishedLayers] layers of [MaxLayerParts] parts
// against [MaxPacksPerRepo] live packs: 2 625 rows, the widest record the
// domain forms, the chain read whole before and the live packs again for the
// recount after — under the third by 8 ms; and 13.5 ms for the widest
// COMMIT, retiring [MaxExpungeOutputs] dropping packs into as many rescue
// packs, 97 rows and the open request's one seek. A begin's arm past the
// third halves MaxExpungeInputs below MaxCompactInputs until it is under.
//
// A RECONCILE (BenchmarkAnUnchangedActivation): an activation that changes
// nothing, over ten thousand declared repositories — the most a configuration
// may declare — is 471–593 ms of one node's reads through
// [Store.EnsureRepos], whose one publish reads every claim, row and rule in
// one transaction, against 1.7–2.0 ms a repository through [Store.EnsureRepo]
// — seventeen to twenty seconds of every data node's reads at ten thousand,
// for an activation that appends one fence record. Measured before that read
// took each repository's path owners too: one more statement, driven from the
// same list and seeking code_path_owners' key once a repository, as the rules'
// statement seeks theirs — the benchmark's repositories name none.
//
// A CHANGED ACTIVATION (internal/engine's BenchmarkAChangedActivation, the
// reconcile end to end on one booted node, its fence, walk and batch read
// included, one count each, at thirty-two in flight — [reconcileInFlight]
// then, fifteen since the path owners' rows made the widest record a
// reconcile writes 257 rows, a width inside the eight to a hundred and
// twenty-eight below that all measured the same): over the same ten
// thousand, the activation that creates them all is 18.3–19.5 s, one that
// deletes them all 17.2–18.5 s, one that restores them all 16.3–17.2 s, and
// one that changes nothing 0.45–0.53 s (its batch read, too, before the path
// owners' statement above). By width, creating them took 67 s
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
// the LIVE refs' index ([liveRefsKey], replicated 0054) for every gap: the
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
// A ROUND OVER LIVE CHANGES (the changes arm, in the bounded run that
// measured the changes a record moves above, interleaved with the build
// before it, two rounds): one push moving the heads of [MaxCommandsPerPush]
// live changes beside sixty-three one-command pushes that move none is 47–50
// ms a round and 11 MB, where the group's sixty-four pushes reaching no
// change are 3.0–3.4 ms — the difference the reads of the thousand changes
// and of the authors their pusher already is, which the round cannot do
// without. The build before tried each entry on a copy of the round's change
// book, every change the batch reached copied for every entry: 79–80 ms and
// 64 MB the same round. The journal ([bookTrial]) holds what each entry
// reached and nothing more, so the sixty-three that reach no change cost the
// round nothing of the book.
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
// full answer reads the most for a delta from below every row of that
// repository beside [MaxFullRetirements] retirements, the most a full answer
// serves: it counts every one, then the delta's refs one row past the full
// answer's own rows deep when asked as a git node's cache asks, from one
// position for both floors, and past those retirements too when the full
// answer's are asked from below the delta's floor, every one of them between
// the two — the deepest any ask reads. Measured in one run beside the
// interleaved page (ten iterations, one count, on two cores), the two cost
// 7.3 and 8.3 ms against the page's 44.6 ms, a sixth and under a fifth of
// what is then served; after a push the choice reads a handful of index
// entries. Measuring the page's rows costs 0.08–0.09 µs a ref and no
// allocation, where encoding each one to measure it cost 0.78–0.83 µs and
// three (BenchmarkServeASnapshotPage).
package code
