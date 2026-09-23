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
// Let C be this node's committed checkpoint and F the bound a writer reads:
// the HIGHER of the published trim floor and the log's own first surviving
// sequence. The proof uses one property of F — every sequence the log has
// lost, or may yet lose to a purge already licensed, is below it — and each
// half of the maximum keeps that true where the other cannot. The published
// floor is written BEFORE the purge it licenses and never moves down within a
// generation, so it covers a purge in flight, one that failed and will be
// retried, and one the stream's own answer has not caught up with; the first
// sequence covers what the floor cannot see, a floor published at another
// generation reading as zero. What neither may be is the trim's per-tick
// conclusion: that is zero on every blocked tick and falls to the lowest
// counted node's own position, and a bound derived from a minimum that
// includes a node can never refuse it.
//
// For a node the trim counted at every tick that raised F, the applied term
// gives F <= C. THREE ENFORCED CLAUSES close the gaps that leaves, and the
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
// is a readiness concern.
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
// APPLIED here; therefore this node's row already reflects S. Contradiction.
//
// A purge that lands between the check and the append is licensed by a tick
// that published its floor first, from an applied term over a counted set
// that holds every node from its presence lease at boot until its eviction
// ([CountedSet]) — so it removes nothing at or above this node's C, and the
// only writer it can pass is an evicted one, which is (iii)'s.
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
// applier's stream identity ([Identity]) before anything else and before every
// append, and a write at an expectation of zero asks it AGAIN after its fence's
// own read of the log, which is the read that carries the stream's creation
// instant — so on the branch where a rebuild is a lost update, it is caught
// within the call, as (ii) is. A node that knows its log is not the one its
// rows are keyed to refuses every write, `wrong_stream`, and every read with
// the same word.
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
