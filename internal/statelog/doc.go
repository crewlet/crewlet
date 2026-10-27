// Package statelog is the engine's replicated state-log framework: one
// ordered stream per domain, one deterministic applier, N identical copies in
// N node databases, with the checkpoint committed in the same transaction as
// the rows.
//
// This is ADR-0002, and the sentence above is the whole of it: the stream is
// the write-ahead log and the SQL estate is derived. A write that reaches
// those rows any other way is the one failure nothing on the node that makes
// it can see.
//
// # This is NOT the store's write-ahead log
//
// journal_mode=WAL is the database engine's own local transaction journal:
// per file, invisible to any peer, and a thing the store manages without
// anybody here knowing. This is an APPLICATION log — a fleet-wide, ordered
// record of committed mutations from which every node derives its own SQL
// state. The two are different layers and the only thing they share is a
// word.
//
// # The write authority, stated once because it is the whole safety argument
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// What it buys is that a behind node cannot corrupt anything — it can only
// fail to write. That is [internal/coord]'s three-valued discipline one layer
// down: "held" is a PubAck, "definitively not held" is a wrong-last-sequence
// refusal, and "the store could not be reached" is no answer at all. Reading
// local rows to decide is safe here — the inversion a projection-backed key
// mint forbids in its own words — for exactly one reason: the BROKER checks the
// expectation, so a stale snapshot is a claim that gets refused rather than a
// decision that gets committed. That reason holds only while the expectation
// travels with the decision it was read beside.
//
// # Three values, three questions, and no caller reads two of them
//
//	what does the broker arbitrate against?   the arbitration ANCHOR
//	                                          — the publisher, nothing else
//	did the caller decide from what it read?  the row's VERSION
//	                                          — if_match, the applier's
//	                                            guard, every tool result
//	is this row fresh enough to read?         MAX(version, scoped_through)
//	                                          — the coverage probe
//
// They are equal only while every accepted record produces rows, and an apply
// GATE is by definition the rule that makes them differ. Folding them back
// into one integer is what produced the wedge this separation exists to
// prevent: an evicted node's accepted-then-gated append leaves the broker's
// last sequence above every node's version, and a writer forming its
// expectation from the row re-reads the number that produced its own
// rejection until it runs out of rounds.
//
// # The checkpoint commits with the rows
//
// The rows a record produces, the record's operation id, and the checkpoint
// that covers it commit in ONE transaction. The acknowledgement is OUTSIDE
// it, because the store may re-run a transaction's body and a publish inside
// one would happen twice. The same holds for everything the body SAYS rather
// than writes — a warning, a count of records retained or gated — so the body
// notes what it saw, resets the notes with its tally on every attempt, and
// the loop announces them once the commit lands. Two properties follow: a
// node can only be BEHIND, never inconsistent; and a transaction ends at a
// RECORD boundary, never inside one.
//
// The shape this must not copy is the one this framework replaced, and it was
// correct for its own estate. internal/projection — deleted by node migration
// 0025, and named here because the reasoning is what matters rather than the
// package — committed its batch and THEN wrote its cursor, on the argument
// that "a crash between the two replays the batch, which is free". That is
// true while the source can always redeliver. It is false for a log that gets
// trimmed: a checkpoint behind its rows is a node that will one day ask for a
// position the stream no longer holds, and the only way back is a full
// snapshot from a peer.
//
// # Retry is three layers and only one may be relied on
//
//	layer 1  the domain's ops table, written in the apply transaction
//	layer 2  the message id and the stream's duplicate window
//	layer 3  the applier's conflict guards and the row's version guard
//
// The fact that decides this: the dedupe check and the expectation check run
// in DIFFERENT ORDERS on the two topologies. Clustered, dedupe is checked
// first and a retry inside the window gets a duplicate acknowledgement.
// Solo — which is the engine's DEFAULT — the expectation is checked first, so
// the same retry is refused instead. Layer 2 is therefore an optimisation and
// never a mechanism.
//
// Layer 1 is read BEFORE the domain decides — in the snapshot's own
// transaction ([Snap.Held]) — and not only when a lost acknowledgement is
// resolved. A retry of an operation that landed takes a snapshot that already
// holds the first application, and a decision taken there is about the wrong
// world: a purge finds its task gone and refuses, an update conditioned on a
// version finds it moved by its own first copy and refuses as stale, a
// counter moves twice. So an operation this node's ledger records is ANSWERED
// — applied, at the first copy's position, marked [Result.Collapsed] — and
// never decided again, however long after the first copy it comes. A
// duplicate acknowledgement from layer 2 is marked the same way, because its
// position is equally not one this call's decision produced, and a caller
// whose answer is computed inside the decision (a minted key) must not report
// it. So is a lost acknowledgement the ledger resolves, unless the broker
// acknowledged this call's append at the very position the ledger names: the
// row may be an earlier copy's, which a node that was behind decided against
// other rows than this call did.
//
// A copy the broker collapsed an append onto need not be this node's at all:
// an operation handed to another node — a write refused `evicted` by one the
// fleet put out, retried through one it still counts — lands on the first copy
// while the window lasts, and when that copy applied nowhere there is no
// ledger row to answer from. Whether a gate dropped it is a question
// about the node that WROTE it, so the resolution reads the record off the log
// and asks the gates about its writer, answering that writer's refusal; asked
// about itself, the retrying node found no gate and a ledger that vouched, and
// reported a contract violation. The id stays spent on that log until the
// window passes, which is what the refusal tells its caller.
//
// # An operation id carries the instant it was minted
//
// Layer 1 has a hole the other two cannot fill: the ops table can LOSE ROWS.
// The retention sweep deletes every row applied more than [OpsRetention] ago. A
// retry of such an operation — a turn re-run under its derived id, a caller
// repeating an `unknown` — finds no row, decides again on rows that already
// hold the first application, and publishes a second copy the broker has no
// reason to refuse: the expectation is current and the duplicate window long
// past.
//
// So the ledger keeps a WATERMARK, beside it in the same file: the instant
// before which it may have lost rows ([Rows.LostBefore], ledgerloss.go), moved
// only forward, by the sweep that loses them and in the same transaction —
// and, for a subject kind a domain sweeps sooner than the rest
// ([OpsHorizon.Kinds]), that kind's own, moved by that kind's own sweep, so an
// hourly sweep of sessions never reads as the whole ledger losing everything
// older than an hour.
// Before a decision is published the publisher asks whether the ledger can
// VOUCH for the operation (Publisher.vouches): it cannot for one minted before
// the watermark whose row it does not hold, and such a write is answered
// `unknown` instead — the one answer that is true, and the one a retry can do
// nothing wrong with. A decision that REFUSES asks the same question before
// the refusal is returned, because rows holding the first application are
// exactly what makes a re-run refuse — its root already moved, its guarding
// row already there — and only a refusal about this node itself (an
// [Unavailable]) stands without it. The resolution of a lost acknowledgement
// asks it too.
//
// THE LEDGER TRAVELS INSIDE A SNAPSHOT, and so do its watermarks
// ([Domain.OpsTable]). That is what keeps an adoption from costing anything:
// the adopter holds a row for every operation its donor applied, so a retry of
// one is answered from it, and an operation neither holds never applied — so a
// turn woken by a trigger from before the join, whose ids carry that trigger's
// instant, has its first attempts published like anyone's. Scrubbed out of
// the snapshot, the ledger could vouch for nothing minted before the adoption,
// and a recovering node would answer its own backlog `unknown`.
//
// The instant is the operation id's OWN — a UUIDv7 whose leading bits are its
// mint time, recovered with [OpMintedAt] and minted only by [NewOpID],
// [DeriveOpID] and [StepOpID] — because every retry reuses the id, and a value
// a caller stamped beside it was the CALL's: later than any loss on every
// retry, which is precisely the case the question exists for. An id that
// carries no instant is read as minted before every loss. opid.go states the
// grammar, the clock it is read off and what that assumes of the fleet.
//
// A DERIVED id's instant is the start of the work it belongs to, and that can
// be further back than any ledger remembers — a trigger dispatched a month
// late, a turn resumed a month after it parked — where every new write under it
// would be answered `unknown` on every node for good. [MintAt] is the rule an
// attempt applies to the instant it inherits, against its own clock, and
// mint.go says why the line is [MintHorizon] rather than the retention. That an
// attempt past it rebases, records the instant for the fleet and is judged at
// its own clock rather than an earlier attempt's is ADR-0029.
//
// # Every record is SIGNED, and no domain is handed an unverified byte
//
// This is ADR-0031. The broker has no authentication of its own and a record
// is what the next node APPLIES, so the framework frames and signs on the way
// to the appender and verifies before any domain decodes. The key is Tier A's
// keyring (ADR-0011), and a node with no keyring refuses to start.
//
// The rule is stated for READERS rather than for appliers, and the clause
// earns its place: the applier is not the only one. The change feed is a
// second consumer over the same bytes, and what travels onward from either is
// the BODY. [Verifier.Open] is that one reading; `signature.go` is where the
// whole argument lives, including why the answer is three-valued.
//
// # A record this build cannot decode is RETAINED, with one exception
//
// It is kept at its position, never skipped and never stopped on, and
// reprocessed by the same applier when a build that can read it arrives.
// [internal/events] already holds this contract for the envelope, and for the
// same reason: a rolling upgrade puts unknown records on the wire, and
// dropping them would make every upgrade an outage.
//
// The exception is a record that INSTALLS AN APPLY GATE — a rule under which
// a durable record produces no rows on ANY node. That is a STOP: the applier
// halts, health goes false immediately, and the node takes its copy of THAT
// DOMAIN out of service (ADR-0049) — that domain's operations go to another
// data node that can decode it, and its seats stay. A deferred gate does not
// postpone one record's effect on one node; it silently licenses every record
// above it.
//
// # A domain's fault takes only its own copy out of service
//
// Every domain's copy is judged on every reading, by the one predicate that
// judges any ([Health.Healthy]), and a copy that is WRONG — not behind — goes
// out of service for that domain alone: the node answers none of that
// domain's operations from it, and every other domain it holds keeps
// answering from its own copy. Which domains a SEAT depends on is a separate
// declaration ([Domain.GatesSeatAdmission]): admission waits for those to be
// level, and a node whose copy of one of them is out of service routes its
// seats through its peers. Whether a copy is judged is never declared, because
// a domain allowed to opt out is a domain whose wrong copy is served for ever.
// This is ADR-0049; the engine computes the verdict per domain and the
// estate's router asks it of each operation's own domain.
//
// The consequence every reader of an ABSENCE inherits: the checkpoint moves
// past a retained record, so "this node has consumed the log through here" is
// never "every record through here wrote its rows here". A reader that decides
// from a row being missing — nobody owns this key, this session has ended —
// asks [PrefixIn] for the prefix its own snapshot has APPLIED and [DeferredIn]
// whether a retained record is about what it is reading, both inside the
// transaction that reads the rows (`prefix.go`).
//
// # Retention rests on the ENVELOPE, so a writer can ask who reads what
//
// A build retains what it can file: the envelope must decode. A kind whose
// envelope an older build REFUSES — one that validated a field a newer build
// widened — is a stop on that build, not a deferral, and no version number on
// the record changes that. So every node's position heartbeat names, per log,
// the highest record version its build reads ([NodePosition].RecordVersion),
// and [Readers] reads it across the trim's own counted set: a writer about to
// publish such a kind waits until every node that applies the log reads it —
// a node that has not reported yet reading as one that reads nothing newer.
// No record this build writes is such a kind: the advertisement is what a
// successor's writer of one reads.
//
// # A record is stamped with the lowest version that reads it
//
// The retain rule decides by the record's version, so the version is a
// promise to the builds that PRECEDE the writer: a record stamped below what
// its fields need is applied by an older node with the field dropped, and one
// stamped at the writer's own version is retained by every older node whether
// it carries anything new or not. A domain therefore declares a
// [RecordFields] table — each field its records gained since the base format,
// with the version that introduced it — and its encoder stamps
// [RecordFields.Minimum]. The build's own [Domain.RecordVersion] is exactly
// the table's highest version: [RecordFields.Check] and the conformance suite
// hold both halves.
//
// A column an applier computes from its rows is invisible to a record
// version, since what differs between builds is the rule; a [Deriver]
// versions its rules on the checkpoint row and re-derives once, at the first
// boot whose rules differ.
//
// # THE FLOOR THEOREM
//
// The trim floor is load-bearing for the WRITE path, not only for recovery,
// and any consumer of this framework inherits the hazard the moment it uses
// per-subject conditional append — which is why the theorem lives here and
// not in a feature's documentation.
//
// The hazard: object X was last written at sequence 4 000 and its row records
// that. The trim purges below 9 000, so X's subject holds nothing. A writer
// forms an expectation of 4 000; the server loads the subject's last message,
// finds none, and rescues only the case where the expectation is ZERO — so
// the append is refused. The writer re-reads, gets 4 000 again, retries, is
// refused again. LIVELOCK: a quiet object becomes permanently unwritable.
//
// The rescue is to retry ONCE at an expectation of zero. The dangerous case
// that makes that unsafe in general is: this node's row is stale — it says
// 4 000 while a peer has since written 6 000 — and 6 000 is also trimmed, so
// the expectation of zero succeeds and the peer's write is silently
// overwritten.
//
// Let C be the checkpoint this node's rows were at when the write DECIDED —
// read in the snapshot's own transaction, beside the decision
// ([Snap.Checkpoint]) — and F the bound a writer reads, at C's generation:
// the HIGHER of the published trim floor and the log's own first surviving
// sequence. C is NOT the applier's live position. That only moves forward,
// so it is the permissive answer: a record on this subject applied after the
// snapshot and trimmed before the check passes against it, while the
// decision about to be published at zero never saw it. The proof uses one
// property of F — every sequence the log has lost, or may yet lose to a
// purge already licensed, is below it — and each half of the maximum keeps
// that true where the other cannot. The published floor is written BEFORE
// the purge it licenses and never moves down within a generation, so it
// covers a purge in flight, one that failed and will be retried, and one the
// stream's own answer has not caught up with; the first sequence covers what
// the floor cannot see, a floor published at another generation reading as
// zero. What neither may be is the trim's per-tick conclusion: that is zero
// on every blocked tick and falls to the lowest counted node's own position,
// and a bound derived from a minimum that includes a node can never refuse
// it.
//
// For a node the trim counted at every tick that raised F, each of those ticks
// having read a position this node held by its snapshot, the applied term
// gives F <= C+1. THREE ENFORCED CLAUSES close the gaps that leaves, and the
// proof needs all three, because C is the prefix SETTLED rather than APPLIED,
// and because a counted set is a thing an operator can change — a node can be
// counted BELOW F, readmitted, restored from an old backup, or up on its own
// history because no peer could donate:
//
// (i) On every object this node holds no deferred SCOPE term for, settled
// implies applied here, or the record produced no domain rows on any node.
// The write path's step 0 probes the deferred-scope index with the subject's
// own qualified path and refuses before an expectation exists. THE PROBE IS
// OVER THE SCOPE AND NOT THE SUBJECT, and that is not a refinement: a record
// deferred under object A that also rewrote neighbour B's row leaves nothing
// on B's subject to probe, so a subject-keyed probe asks the wrong question,
// B's next writer takes the retry-at-zero branch, and the deferred mutation
// is overwritten — silently, with nothing anywhere reporting it, and no later
// reprocess able to undo it. The second disjunct is the gate's: a gated
// record produces no rows on any node by its own determinism, so the
// conclusion holds vacuously for it.
//
// (ii) F <= C+1 is VERIFIED WITHIN THE CALL by every writer that reaches an
// expectation of zero, rather than being true on average because a heartbeat
// checked it. That is [Replayable](C, F) — the next record this node needs is
// one the trim has not licensed removing — and C+1 rather than C because it is
// all the proof below uses: a node one below the floor holds everything that
// may be gone. No detection cadence closes a window, and the window a
// fifteen-second cadence leaves admits a genuine LOST UPDATE rather than a
// stale read or a refused write. The heartbeat check stays, demoted to what
// it actually is: the mechanism that stops a below-floor node SERVING, which
// is a readiness concern. A writer that fails the check is refused either way;
// WHICH refusal is the log's first sequence's to say, of this node's live
// position, exactly as it is for a read — `behind` while the log still holds
// everything the node lacks, since the floor is published before the purge it
// licenses and the node is replaying up to it, and `below_floor` once the
// node's next record is gone ([ZeroFence]).
//
// (iii) An evicted node's records are dropped by the applier's eviction gate
// whatever it manages to publish, so the conclusion holds even when (i) and
// (ii) are both defeated — by a frozen clock, or by a coordination read that
// answered stale. This clause depends on nothing but the log's own order and
// on the gate record being decodable by the applier that must obey it, which
// is why a gate is a stop rather than a deferral, and why it is the layer
// that makes the fence complete rather than merely deep.
//
// Given all three: suppose a commit at sequence S on this object's subject
// has been trimmed. Then S < F <= C+1, so S <= C, and by (i) it has been
// APPLIED in the rows the decision read; therefore the decision already
// reflects S. Contradiction.
//
// A purge that lands between the check and the append is licensed by a tick
// that published its floor first, from an applied term over a counted set
// that holds every node from its presence lease at boot until its eviction
// ([CountedSet]) — so it removes nothing this node had not applied when that
// tick read its position, and the only writer it can pass is an evicted one,
// which is (iii)'s. That bounds a purge by what this node had APPLIED, which
// may be past C, so the term alone does not exclude a record on this subject
// that this node applied after its snapshot. The ORDER OF THE WRITE'S OWN
// READS does: a write learns the subject is empty from LastSeq — answered by
// the stream's leader, because a log allows no direct gets — and only AFTER
// that does the fence read either bound, on both roads to an expectation of
// zero. A record R on this subject that the decision did not read, so R > C,
// is one of three kinds:
//
//   - Still on the log when LastSeq answers: the subject is not empty, so the
//     write waits for the subject's last record and decides again rather than
//     taking zero.
//   - Purged before LastSeq answered: the floor that licensed the purge was
//     published before it, so the fence reads it afterwards as at least R+1 —
//     as it does the stream's first sequence, once that answer has caught up —
//     and F > C+1 fails (ii). A coordination read that answered stale here is
//     (iii)'s case.
//   - Appended after LastSeq answered: it is younger than the trim's minimum
//     age — a day, at the least — by all but this write's own duration, and
//     the age term, one of the six on every tick, never lets a purge cross a
//     record that young; the broker still holds it when the append arrives,
//     and refuses the expectation of zero. That rests on the trim holder's
//     clock agreeing with the broker's storage timestamps to within the
//     minimum age, and a clock that does not is (iii)'s frozen one.
//
// So a writer's expectation can only ever be stale ABOVE the floor, where the
// anchor still exists and the broker arbitrates with a genuine sequence;
// below it every node's row is current by construction, and an expectation of
// zero is the truthful statement "this subject holds nothing", arbitrated
// between concurrent writers exactly as a create is.
//
// # An anchor below the floor is removed by a record
//
// Below the floor an anchor names a record the log no longer holds, so all it
// can do is send its subject's next write to be refused before the
// discriminator takes the zero path — and kept, the table grows by every
// subject the log ever carried. The trim removes them (ADR-0050): after each
// tick's purge it appends a framework [TrimmedKind] record naming the log's
// first surviving sequence ([PlanSweep], [SweepPlan.Sweep]), and every node's
// applier removes that log's anchors below it in the transaction that applies
// the record, at most [AnchorsPerTrimmedRecord] a record, lowest first — never
// a sweep beside the applier, because the table is replicated state
// (ADR-0002). The trim
// counts them only from a copy that has applied the whole log, appends each
// record only once its own node has applied the one before, and stops when a
// time budget is spent: a write or a read queued behind the records waits one
// record's apply on the node that appended them, at most about k − 1 budgets
// on a node that applies them k times slower, and the trim's own goroutine is
// held for at most the budget ([SweepPlan.Sweep]). A write to a subject whose
// anchor went takes the road to zero an object never written takes — the
// subject's last sequence from the log, then the fence — rather than the one
// through the broker's refusal of a stale anchor, and both are the roads the
// proof above covers: the removal moves a write between them and never off the
// fence. That is why it is safe at any bound, and why the bound — what the log
// no longer holds, rather than anything higher — is chosen for cost: an anchor
// of a record the log still holds saves its next write a leader read.
//
// The deferral probe, the per-tick floor check and the per-write fence are
// therefore LOAD-BEARING rather than defensive. None may be optimised away as
// a rare case.
//
// # And the premise under all of it: ONE STREAM
//
// C, F, every anchor and every answer the broker gives are sequences, and the
// proof above — like the ordinary arbitration before it — compares them as
// numbers in one space. A stream deleted and rebuilt under the same name
// breaks that without breaking any number: it keeps the generation and counts
// from 1 again, so this node's C still clears F, an empty subject still reads
// as a trimmed anchor, and an expectation still equals the rebuilt log's last
// sequence wherever the two histories happen to meet. So the premise is
// enforced like the clauses rather than assumed: every write asks the
// applier's stream identity ([Identity]) before anything else and again before
// every append, and on a write at an expectation of zero the ask before the
// append comes after the fence's own read of the log, which is the read that
// carries the stream's creation instant to the applier — so on the branch where
// a rebuild is a lost update, it is caught within the call, as (ii) is. A node
// that knows its log is not the one its rows are keyed to refuses every write,
// `wrong_stream`, and every read with the same word — until an operator
// re-anchors THAT log ([Reanchor]), which moves that log alone into a new
// generation on the live stream and re-keys the runner to it, so every number
// above is in one space again. A generation is per LOG for exactly this
// reason: it is a coordinate in one stream's number space, and moving another
// domain's log would key it to a stream it never read. WHICH instant
// identifies a stream is ADR-0053: the one its creator stamped into its
// configuration ([CreatedAtKey]), which a snapshot restore keeps and the
// broker's own does not, read by every consumer through [StreamCreatedAt] and
// by nothing else.
//
// The instant is not the only way the premise fails. A broker restored from a
// copy older than this node's rows keeps its stream, instant and all, and the
// log ENDS below this node's checkpoint: then C clears every bound for the
// very reason that it is past all of them, and the retry at zero lands at a
// sequence this node's applier has already passed. The same read of the log
// carries the end, so the zero fence compares it within the call
// ([ZeroFence]) — against the applier's LIVE checkpoint rather than C, because
// where an append lands against where the applier stands is a property of the
// node, and the applier only leads C — and the identity answers for it on
// every other pattern ([Runner.ObserveEnd]). Once something writes that log
// past the checkpoint — a node whose rows were not ahead of the copy — the end
// is an ordinary end again and every sequence compares; what still breaks the
// premise is the RECORD at the checkpoint, which on one stream never changes.
// So a checkpoint names its record by the broker's instant for it, and the
// applier verifies it before applying anything past the checkpoint whenever
// what follows may be another history — at every start, after a failed fetch,
// after a reading found the end below — and the heartbeat every interval
// ([Runner.VerifyCheckpoint]): another record there is [ErrLogDiverged], which
// refuses like the rest and, unlike an end, never lifts on a later reading —
// nor across a restart. The log can lose the record the verdict was found by
// (an evicted node's checkpoint stops pinning the trim; a stream is purged by
// hand; a broker is restored again from an older copy still), after which a
// restarted node would have nothing to compare, so the verdict is recorded in
// the node estate when it is reached and recalled while the checkpoint names
// the same record ([NodeEstate]): only a reanchor or an adoption, which move
// the checkpoint, ends it. A checkpoint that names NO record — one a reanchor
// placed where the log held none — has nothing to compare, and the first batch
// the node commits names its own; it is never named from the log's record,
// which is the thing in question.
// The nodes whose rows are the copy's age see none of it — the log is their
// own history — so what stops them writing records the restored reanchor of a
// newer peer would apply nowhere is that peer's published position or flag
// ([ErrLogTruncated], [Identity]): their writes refuse, their reads do not.
// Its reanchor is the RESTORED case
// ([ReanchorRestored]): up to the copy the log is a prefix of the history the rows came from,
// so the new generation's checkpoint goes at the log's END rather than one
// below its first record, and replaying none of it is what keeps every object
// from rolling back to the copy. Following it from there applies nothing below
// the end, so a restored log holding a record that writes rows these do not —
// written after the restore — is re-anchored only on the operator's word, the
// newest such record named ([UnheldTail], [ReanchorGuard.Discard]). And what the
// fleet's other nodes write in the old generation AFTER the reanchor's own
// record, before they learn of it, is decided from the rows it did not keep:
// the checkpoint says where that record is, and such records are void wherever
// the checkpoint is followed from ([ReanchorPlan.StaleAfter]).
//
// And a reanchor breaks the premise for every OTHER node of a domain that claims
// identity: it opens its generation from one node's rows, so the rest hold a
// history the log no longer continues from, whatever their own readings of it
// say — a node below a restored broker's end sees nothing wrong at all. That
// is the identity's third finding ([ErrGenerationPassed]), established from the
// fleet's published generation or from a record written in a generation the
// node never entered, and refused exactly as the other two are. Its repair is
// the node's own: it adopts a snapshot from the new generation, and the join
// re-keys its runner to what it adopted ([Runner.Rejoined]). Where no node in
// the new generation survives to donate — the node that opened it was
// decommissioned first — the operator's eviction of that node is what releases
// the rest: an evicted node's generation is not the fleet's, it can be read off
// the log by a node whose applier never reaches the eviction ([EvictedOnLog]),
// the eviction is the one write such a node may still make
// ([Request.NodeGate]), and a reanchor then opens the generation after the
// abandoned one with its records void ([ReanchorAbandoned]).
//
// # A domain declares its own stream
//
// A [Domain] declares its stream ([Domain.Stream]) beside its tables and its
// gates, and a node runs exactly that stream at the byte ceiling its own Tier A
// sized — held to the declaration in everything else
// ([StreamSpec.Instantiates]). Every piece of per-stream machinery here is the
// domain's log's: a runner, a publisher, a reader and a read index each take
// its [StreamSpec]; the checkpoint, the generation, the anchors and the stream
// identity are keyed by the stream's name; a position row, a floor and a
// manifest name the log by the domain's; and a barrier proves where that one
// log ends, single-flighted per node per log ([ReadIndex]).
//
// # What a write costs, measured
//
// `make bench`'s BenchmarkArbitratedWrite runs ONE WRITE END TO END on a node
// whose every seam a healthy write takes is real — the fence's read of this
// node's own eviction row, the session wait for the writer's own last write,
// one snapshot of the rows, the decision, the append the broker arbitrates,
// this node's applier reaching it, WaitApplied — on a test domain that writes
// one row a record, so its figures are the FRAMEWORK'S FLOOR, and a real
// domain's decision and apply add to them. On four vCPUs of a KVM Intel Xeon
// at 2.8 GHz, Go 1.27.2, nats-server v2.15.0, tursogo v0.8.1, 2026-10-10, at
// `stream.sync: always` unless said, p50 / p99:
//
//   - SOLO, one subject's serial chain — each write arbitrated against the
//     anchor the one before it advanced — took 3.2–3.3 / 7.1–7.2 ms, about
//     295 writes a second; sixteen subjects written at once, 1,335–1,430 a
//     second together. With the fsync declined, 3.0 ms and 310 a second.
//   - AT THREE REPLICAS — the three members, their brokers, appliers and
//     estates all on that one machine, so pessimistic per node and with no
//     network hop — the serial chain took 7.0–7.6 / 15–18 ms from the
//     stream's leader and from a follower alike, 124–133 writes a second on
//     one subject; sixteen subjects, 520–570 a second. With the fsync
//     declined the chain took 4.1–5.1 ms, 183–228 a second.
//   - A LINEARIZABLE READ is a barrier append — 0.57–0.59 ms solo,
//     3.5–3.9 ms at three replicas — and then this node's applier committing
//     every record before the barrier, never the barrier itself, which writes
//     no row on any node. Measured while a read still waited for its own
//     barrier's commit, and so an upper bound on today's: the whole wait
//     1.7–1.8 / 4.3–4.6 ms solo and 4.8–5.5 / 10–12.5 ms at three, 535–555
//     reads a second from one reader solo and 171–197 at three.
//   - SIXTEEN READERS AT ONCE on one node SHARE their barriers
//     ([ReadIndex.Read]): 0.12 appends a read for the barrier alone, and
//     0.14–0.23 for whole reads, whose waits for the applier spread their
//     arrivals. A read may use only a barrier that started after it arrived,
//     so readers calling back to back divide into two halves, each served by
//     every other barrier — about eight a barrier. The node served
//     4,600–4,700 linearizable reads a second solo and 1,120–1,610 at three
//     replicas — eight times one reader's solo and six to eight at three —
//     each read waiting at most two barriers: 3.3 / 8.3–8.5 ms solo and
//     9.6–12.5 / 19–31 ms at three.
//
// The ranges are two runs on a VM shared with other work, where one run can
// land a third away from another: compare two trees on one idle machine with
// benchstat rather than against these.
//
// BenchmarkApplierDrain prices the apply lane every domain on a node shares:
// it drained a burst of one-row records at about 5,900 a second — each
// record's anchor, ledger row and checkpoint are what a small record costs —
// and internal/store's BenchmarkLogApplyDrain prices large ones, 52,000–63,000
// rows a second as multi-row statements and 16,000–17,400 one statement a row.
// The serial chain on one subject is what one object, or one repository's
// committer, can do before it batches; the drain is the node's ceiling across
// every domain at once.
//
// # Who may write a log: every writer is counted by construction
//
// The floor theorem above rests on a premise none of its three clauses states:
// EVERY WRITER IS COUNTED — the trim never removes a record that a node which
// may still decide a write has not applied. It holds by construction, not by a
// gate a write passes:
//
//   - A publisher exists only on a node with the `data` role. Such a node holds
//     the replicated estate and runs every log into it, the identity estate's
//     included, from boot, company or none; a node without `data`
//     holds no estate and builds no publisher, so it has no write to decide —
//     its tools reach the estate through a data node that does.
//   - Every live data node is in every log's counted set ([CountedSet]): by
//     its presence lease from boot, at position zero until its first heartbeat
//     reports one, and by its positions row after that, which never expires —
//     until an eviction's tombstone on that log is older than the fence
//     window. So a node that can decide a write is a node the trim is already
//     waiting for, from before its first decision.
//
// The one way out of the set while still running is an eviction, and an
// evicted node's records are what clause (iii) drops on every node, whatever it
// manages to publish. So nothing a write asks has to say whether this node may
// write; what refuses a node that should not decide is its own state — an
// eviction (`evicted`), a stream it is not on (`wrong_stream`), a floor it is
// below (`below_floor`) — each a fence of the write authority above.
//
// # Which log a coordination record is about
//
// ONE RULE, for every record the fleet shares about a log — a node's row in
// the positions register, a published trim floor, a trim hold, a backup point,
// a capacity operation and its lease: a record names its log by one of the two
// names the log has, and each record keeps the one it has always had:
//
//   - the log's STREAM NAME — what a hold, a backup point and a capacity
//     operation are keyed by, since each is a statement about one stream's
//     number space;
//   - the log's KEY, which is its domain's name — what the positions
//     register's rows and the published floors are keyed by, since the key is
//     what an operator reads and a row describes every log a node runs at
//     once.
//
// An EVICTION is not a coordination record at all, and needs no such name: it
// is a record on the very log it gates, applied into that log's rows in the
// replicated estate, so it is about that log by construction — the same way a
// checkpoint, an anchor and a generation are.
//
// # The alarm table borrows every threshold it fires at
//
// It is in this package rather than beside any one subsystem because an alarm
// is the framework's answer to "is this node doing its job", and everything
// above it asks the same question. The rule is ADR-0015 and alarms.go is where
// it is carried out: an alarm never invents a number, it fires at the one some
// OTHER decision already made — the grace that takes a copy out of service,
// the grace that stops a node serving a copy it cannot decode, the budget a
// caller was promised, the replay window an operator set a log's ceiling to
// hold — and ONE evaluation feeds every surface, so a gauge, a log line and a
// screen cannot disagree about whether something is wrong. A condition that
// is a property of ONE LOG — its headroom, its ceiling against its window, a
// blocked trim, a record held past the deferral grace, a floor this node
// cannot read, a read rate past its census — is evaluated once per log
// ([Report]), because its consequence and its remedy are that log's: the
// grace takes a node's copy out of service for one log and nothing for
// another, and a node behind two re-anchors needs two. The kind is still ONE
// ALARM, listing every log it stands on with that log's own measurement
// ([Alarm.Domains]), because the count, the gauge and the screen read one
// cause on four logs as one problem; and each log's onset and clearing is
// still its own pair of log lines ([Tracker]), since a log joining a kind that
// is already up, and one leaving it, are things an operator has to be told
// started and ended.
//
// # A node's alarm transition is the record its wake derives from
//
// Each of those onsets and clearings is also handed on, once, as an
// [AlarmTransition] ([Tracker.OnTransition]) whose id is derived from the
// node, the alarm, the log, the change and the instant the instance was raised
// ([TransitionID]). The engine publishes it as an inbound delivery, retried
// until the broker takes it, and the seats whose role carries the health grant
// are woken by it (internal/alarmwake). That is ADR-0065: the publish IS the
// write, so no publish trails a commit, and a republish collapses on the ids
// derived from the transition's. A transition is one node's fact about
// itself, never a record on a log — the alarms most worth a wake are about the
// logs, and a log at its ceiling would refuse the record that says so.
package statelog
