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
// one would happen twice. Two properties follow: a node can only be
// BEHIND, never inconsistent; and a transaction ends at a RECORD boundary,
// never inside one.
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
// it.
//
// # An operation id carries the instant it was minted
//
// Layer 1 has a hole the other two cannot fill: the ops table can LOSE ROWS.
// The retention sweep deletes every row applied more than [OpsRetention] ago,
// and a snapshot adopted from a donor that scrubbed its ledger — every build
// before the ledger travelled — arrives with none of the donor's. A retry of
// such an operation — a turn re-run under its derived id, a caller repeating
// an `unknown` — finds no row, decides again on rows that already hold the
// first application, and publishes a second copy the broker has no reason to
// refuse: the expectation is current and the duplicate window long past.
//
// So the ledger keeps a WATERMARK, beside it in the same file: the instant
// before which it may have lost rows ([Rows.LostBefore], ledgerloss.go), moved
// only forward, by whatever loses them and in the same transaction or file.
// Before a decision is published the publisher asks whether the ledger can
// VOUCH for the operation (Publisher.vouches): it cannot for one minted before
// the watermark whose row it does not hold, and such a write is answered
// `unknown` instead — the one answer that is true, and the one a retry can do
// nothing wrong with. The resolution of a lost acknowledgement asks the same
// question.
//
// THE LEDGER TRAVELS INSIDE A SNAPSHOT, and so does its watermark
// ([Domain.OpsTable]). That is what keeps an adoption from costing anything:
// the adopter holds a row for every operation its donor applied, so a retry of
// one is answered from it, and an operation neither holds never applied — so a
// turn woken by a trigger from before the join, whose ids carry that trigger's
// instant, has its first attempts published like anyone's. Scrubbed, the
// ledger could vouch for nothing minted before the adoption, and a recovering
// node answered its own backlog `unknown`.
//
// The instant is the operation id's OWN — a UUIDv7 whose leading bits are its
// mint time, recovered with [OpMintedAt] and minted only by [NewOpID],
// [DeriveOpID] and [StepOpID] — because every retry reuses the id, and a value
// a caller stamped beside it was the CALL's: later than any loss on every
// retry, which is precisely the case the question exists for. An id that
// carries no instant is read as minted before every loss. opid.go states the
// grammar, the clock it is read off and what that assumes of the fleet.
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
// halts, health goes false immediately, and the seats move to a node that can
// decode it. A deferred gate does not postpone one record's effect on one
// node; it silently licenses every record above it.
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
// re-anchors THAT log ([Reanchor]), which moves its domain alone into a new
// generation on the live stream and re-keys the runner to it, so every number
// above is in one space again. A generation is per domain for exactly this
// reason: it is a coordinate in one stream's number space, and moving another
// domain's would key that domain to a stream it never read.
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
// refuses like the rest and, unlike an end, never lifts on a later reading.
// The nodes whose rows are the copy's age see none of it — the log is their
// own history — so what stops them writing records the restored reanchor of a
// newer peer would apply nowhere is that peer's published position or flag
// ([ErrLogTruncated], [Identity]): their writes refuse, their reads do not.
// Its reanchor is the RESTORED case
// ([ReanchorRestored]): up to the copy the log is a prefix of the history the rows came from,
// so the new generation's checkpoint goes at the log's END rather than one
// below its first record, and replaying none of it is what keeps every object
// from rolling back to the copy.
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
// # The alarm table borrows every threshold it fires at
//
// It is in this package rather than beside any one subsystem because an alarm
// is the framework's answer to "is this node doing its job", and everything
// above it asks the same question. The rule is ADR-0015 and alarms.go is where
// it is carried out: an alarm never invents a number, it fires at the one some
// OTHER decision already made — the grace that sheds a node, the grace that
// moves its seats, the budget a caller was promised — and ONE evaluation feeds
// every surface, so a gauge, a log line and a screen cannot disagree about
// whether something is wrong.
package statelog
