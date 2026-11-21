// Package statelog is the engine's replicated state-log framework: one
// ordered stream per domain, one deterministic applier, N identical copies in
// N node databases, with the checkpoint committed in the same transaction as
// the rows.
//
// This is ADR-0002: the stream is the write-ahead log and the SQL estate is
// derived. A write that reaches those rows any other way is the one failure
// nothing on the node that makes it can see.
//
// # This is NOT the store's write-ahead log
//
// journal_mode=WAL is the database engine's local, per-file journal; this is an
// APPLICATION log, fleet-wide and ordered.
//
// # The write authority, stated once because it is the whole safety argument
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// A behind node cannot corrupt anything, only fail to write: [internal/coord]'s
// three-valued discipline one layer down (a PubAck, a wrong-last-sequence refusal, no
// answer). Reading local rows to decide is safe only because the BROKER checks the
// expectation, provided it travels with the decision it was read beside.
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
// They are equal only while every accepted record produces rows; an apply GATE
// makes them differ. Folded into one integer, an evicted node's gated append
// leaves the broker's sequence above every version, and a writer expecting from the
// row re-reads the number that caused its rejection for ever.
//
// # The checkpoint commits with the rows
//
// Rows, operation id and checkpoint commit in ONE transaction. The acknowledgement, and
// anything the body SAYS (a warning, a count), is OUTSIDE it: the store may re-run a body,
// and a publish inside would happen twice. A node can only be BEHIND, never inconsistent;
// a checkpoint committed after its rows would, on a trimmed log, ask for a position it no
// longer holds.
//
// # Retry is three layers and only one may be relied on
//
//	layer 1  the domain's ops table, written in the apply transaction
//	layer 2  the message id and the stream's duplicate window
//	layer 3  the applier's conflict guards and the row's version guard
//
// Dedupe and expectation checks run in DIFFERENT ORDERS on the two topologies:
// clustered, dedupe first (a retry gets a duplicate acknowledgement); solo, the
// DEFAULT, expectation first (the retry is refused). Layer 2 is only an optimisation.
//
// Layer 1 is read BEFORE the domain decides, in the snapshot's own transaction
// ([Snap.Held]): a retry would otherwise decide against rows holding its first
// application. An operation the ledger records is ANSWERED at the first copy's
// position, marked [Result.Collapsed], and never decided again; so is a layer-2
// duplicate acknowledgement, since a caller whose answer is computed inside the
// decision must not report it.
//
// # Resolving a durable record
//
// [Publisher.resolve] answers what a durable record at a position produced, for every
// acknowledged write, a successful wait included: an evicted node's applier drops its own record
// under its own gate well inside the budget. It needs no coordination read or clock, the applier
// being contiguous: passing p applies everything below it, and any eviction or deletion marker
// that could gate p is below it. A gate drops a record by its
// WRITER, so a record this call did not append is read off the log (a VerifiedReader) and
// the gates asked about its writer ([Unavailable.CopyWriter], [Reason.BlamesWriter]). A
// failed read answers what is known, never "did not land".
//
// # An operation id carries the instant it was minted
//
// Layer 1 can LOSE ROWS ([OpsRetention]), and a retry then decides again on rows holding the
// first application and publishes a second copy. So the ledger keeps a WATERMARK in the same file, the instant before which it may
// have lost rows ([Rows.LostBefore], ledgerloss.go), moved only forward by the sweep in the
// same transaction. The publisher asks whether the ledger can VOUCH for an operation
// (Publisher.vouches): it cannot for one minted before the watermark whose row it lacks, and
// the write, a refusal included (not an [Unavailable]), is answered `unknown`, the one answer a retry cannot get wrong. THE LEDGER TRAVELS INSIDE A
// SNAPSHOT, watermarks included ([Domain.OpsTable]), or an adopter would answer its backlog
// `unknown`.
//
// The instant is the id's OWN, a UUIDv7 recovered with [OpMintedAt] and minted only by
// [NewOpID], [DeriveOpID] and [StepOpID]: every retry reuses the id, and a value stamped
// beside it was the CALL's, later than any loss. opid.go states the grammar; the
// dashboard's minter is held to it by testdata/opname_vectors.json. [MintAt] applies a
// horizon ([MintHorizon], mint.go) to a derived id's instant, which may predate any
// ledger; that an attempt past it rebases is ADR-0029.
//
// # Every record is SIGNED, and no domain is handed an unverified byte
//
// This is ADR-0031. The broker's admission vouches for a connection, never a record (an
// embedded fleet's route credential crosses its network in clear, ADR-0094), so the
// framework signs on the way to the appender and verifies before any domain decodes, under
// Tier A's keyring (ADR-0011). The rule is for READERS, since the change feed reads the
// same bytes ([recordseal.Verifier.Open], argued in [internal/recordseal]): no record
// reaches a domain unopened ([VerifiedLog]).
//
// # A record this build cannot decode is RETAINED, with one exception
//
// It is kept at its position, never skipped or stopped on, and reprocessed when a build that
// can read it arrives ([internal/events] does so for the envelope): dropping unknown records
// would make every rolling upgrade an outage. The exception is a record that INSTALLS AN APPLY
// GATE, a rule under which a record produces no rows on ANY node. That is a STOP: the applier
// halts and the node takes its copy of THAT DOMAIN out of service (ADR-0079). A deferred gate
// would silently license every record above it.
//
// # A domain's fault takes only its own copy out of service
//
// Every domain's copy is judged on every reading by one predicate ([Health.Healthy]), and a
// copy that is WRONG, not behind, goes out of service for that domain alone
// ([Domain.GatesSeatAdmission] names those a SEAT needs). Whether a copy is judged
// is never declared: a domain allowed to opt out is one whose wrong copy is served for ever. This
// is ADR-0079. The checkpoint moves past a retained record, so a reader of an ABSENCE asks
// [PrefixIn] for the prefix its snapshot has APPLIED and [DeferredIn] whether a retained record
// is about what it reads.
//
// # Retention rests on the ENVELOPE, so a writer can ask who reads what
//
// A kind whose envelope an older build REFUSES is a stop there, whatever its version. So each data
// node's heartbeat names the highest record version it reads per log
// ([NodePosition].RecordVersion), and a writer of such a kind waits on [Readers] until every
// node applying the log reads it.
//
// # A record is stamped with the lowest version that reads it
//
// The version is a promise to the builds that PRECEDE the writer: below what its fields need,
// an older node applies it with the field dropped; above, every older node retains it. A domain declares a [RecordFields] table and its
// encoder stamps [RecordFields.Minimum]; [Domain.RecordVersion] is exactly the table's highest
// ([RecordFields.Check]).
//
// # A record's broker instant is one value, however the record arrived
//
// The broker's instant is the one instant an applier may write of its own
// ([ApplyOptions.StoredAt]). Live it arrives in nanoseconds and local zone, from the deferred
// table in the store's encoding (UTC, microsecond), and a document keeps what it is handed. So
// the loop hands every applier the second shape on both paths ([Record.StoredAt]). No record
// version and no [Readers] gate ride on it: no record changed.
//
// # THE FLOOR THEOREM
//
// The trim floor is load-bearing for the WRITE path, not only recovery; any consumer of
// per-subject conditional append inherits the hazard.
//
// The hazard: X was last written at sequence 4 000 and its row says so; the trim purges below
// 9 000, so X's subject holds nothing. The server rescues only an expectation of ZERO, so a
// writer expecting 4 000 is refused and re-reads 4 000: LIVELOCK. The rescue, one retry at zero,
// is unsafe in general: if this node's row is stale (a peer wrote 6 000, also trimmed), the zero
// succeeds and silently overwrites the peer.
//
// Let C be the checkpoint this node's rows were at when the write DECIDED, read in the
// snapshot's own transaction ([Snap.Checkpoint]), and F the bound a writer reads at C's
// generation: the HIGHER of the published trim floor and the log's first surviving sequence. C
// is NOT the applier's live position, which only moves forward and so is the permissive answer.
// The proof uses one property of F: every sequence the log has lost, or may yet lose to a purge
// already licensed, is below it. The published floor is written BEFORE the purge it licenses and
// never moves down within a generation; the first sequence covers a floor from another generation
// reading as zero. Neither may be the trim's per-tick conclusion.
//
// For a node counted at every tick that raised F, the applied term gives F <= C+1. THREE
// ENFORCED CLAUSES close the gaps, all needed, because C is the prefix SETTLED, not APPLIED, and
// a counted set is something an operator can change:
//
// (i) On every object this node holds no deferred SCOPE term for, settled implies applied here,
// or the record produced no rows on any node. The write path's step 0 probes the deferred-scope
// index. THE PROBE IS OVER THE SCOPE AND NOT THE SUBJECT: a record deferred under A that also rewrote B's row leaves nothing on B's subject, so
// B's next writer would retry at zero and silently overwrite the deferred mutation.
//
// (ii) F <= C+1 is VERIFIED WITHIN THE CALL by every writer reaching an expectation of zero
// ([Replayable](C, F)); C+1 because a node one below the floor holds everything that may be
// gone. No detection cadence closes the window; the heartbeat check stays only to stop a
// below-floor node SERVING. A failing writer is refused:
// `behind` while the log holds everything the node lacks, `below_floor` once its next record is
// gone ([ZeroFence]).
//
// (iii) An evicted node's records are dropped by the applier's eviction gate whatever it
// publishes, so it holds though (i) and (ii) are defeated (a frozen clock): a gate is a stop.
//
// Given all three, a trimmed commit at S has S < F <= C+1, so S <= C, so by (i) it is APPLIED in
// the rows the decision read.
//
// A purge landing between check and append is licensed by a tick that published its floor first,
// over a counted set holding every data node ([CountedSet]); it can pass only an evicted writer,
// (iii)'s, or one booting ("Who may write a log"). THE ORDER OF THE WRITE'S OWN READS excludes a
// record R > C on this subject: the write learns the subject is empty from LastSeq (the stream
// leader's answer) and only AFTER that reads either bound. R was then on the log (the write
// decides again), or already purged (its floor came first, so (ii) fails), or younger than the
// trim's minimum age (the broker refuses the zero).
//
// # An anchor below the floor is removed by a record
//
// An anchor below the floor names a record the log no longer holds; kept, the table grows by every
// subject the log carried. The trim removes them (ADR-0080): after each purge it appends a
// [TrimmedKind] record naming the log's first surviving sequence ([PlanSweep]), and every applier
// removes that log's anchors below it in the applying transaction, never a sweep beside the
// applier (replicated state, ADR-0002). A write whose anchor went takes the road to zero an
// unwritten object takes, which the proof covers. The deferral probe, the per-tick floor check and
// the per-write fence are LOAD-BEARING: none may be optimised away as a rare case.
//
// # And the premise under all of it: ONE STREAM
//
// C, F, every anchor and every broker answer are sequences compared as numbers in one space. A
// stream rebuilt under the same name counts from 1 again, so C still clears F and an expectation
// can match by coincidence. So every write asks the applier's stream identity ([Identity]) before
// anything and again before every append, and a node whose log is not the one its rows are keyed
// to refuses every write and read, `wrong_stream`, until an operator re-anchors THAT log
// ([Reanchor]), moving it alone into a new generation. WHICH instant identifies a stream is
// ADR-0083: the one its creator stamped into its configuration ([CreatedAtKey]), which a snapshot
// restore keeps and the broker's own does not, read only through [StreamCreatedAt].
//
// A broker restored from an older copy keeps stream and instant, and the log ENDS below this node's
// checkpoint ([ZeroFence] compares that end against the applier's LIVE checkpoint). What still
// breaks the premise is the RECORD at the checkpoint, which never changes on one stream: the
// applier verifies it by broker instant before applying past it whenever what follows may be
// another history ([Runner.VerifyCheckpoint]), first GIVING BACK everything held past it.
// Another record there is [ErrLogDiverged], kept in the node estate and ended only by a reanchor
// or adoption; a reanchor leaves every OTHER node of the domain
// ([ErrGenerationPassed]) to adopt a snapshot of the new generation.
//
// # A reanchor's generation record skips the publisher
//
// It goes straight to the broker ([appendGeneration]): every fence of the write authority is about
// the stream the rows are keyed to, and this write exists to leave it. Identity is the operator's
// confirmation of the LIVE creation instant, checked before the append and again after; eviction is
// asked by [Reanchor]; the floor theorem does not apply, nor [Reservation.Bytes] (a gate record).
//
// # A domain declares its own stream
//
// A [Domain] declares its stream ([Domain.Stream]), run at the node's Tier A byte ceiling
// ([StreamSpec.Instantiates]). A domain is also a site in several per-domain lists across the tree,
// and a list that misses a new one fails SILENTLY: add it wherever an existing domain appears.
//
// # Recovery is three loops on every data node, and the trim a fourth
//
// Adopting a peer's snapshot is real only while every data node runs the POSITION HEARTBEAT (its
// rows are the counted set; a node publishing none is one the trim passes), the SNAPSHOTTER and the
// DONOR; the TRIM ([Trim]) is the fourth.
//
// # What a write costs, measured
//
// A LINEARIZABLE READ is a barrier append, then this node REACHING the record ([Runner.Reached]).
//
// # Who may write a log: every writer is counted, but for its own boot
//
// The floor theorem rests on a premise none of its clauses states: EVERY WRITER IS COUNTED, so the
// trim never removes a record a node that may still decide a write has not applied. A publisher
// exists only on a `data` node, and every data node is in every log's counted set ([CountedSet]) by
// its never-expiring positions row until an eviction's tombstone is older than the fence window.
// The one gap is a node's own boot, until its first row lands; a write it decides there is still
// fenced by (ii) and its read order.
//
// # The rule every gate reader keeps
//
// Each domain spells its own [Gates] queries, and every one answers by ONE rule, certified by
// statelogtest.RunGates (RunEvictionGates for a domain with no deletion marker), since readers that
// merely look alike drift apart.
//
//  1. The deletion gate is reported first: a marker is a fact about the OBJECT for every writer for
//     ever, an eviction about one WRITER that a readmission ends, so a record both hold is reported
//     `deleted`, the answer that stays true.
//  2. The marker has no position, and covers the subjects the applier's does.
//  3. The purge's own record is exempt from its own marker, by operation id, and from that gate
//     alone.
//  4. The eviction window is half-open at both ends.
//
// # The gate reserve
//
// The EVICTION that unpins a trim held by a gone node is a record on a log that may be full. So
// ordinary appends are refused `log_full` at a SOFT ceiling, [Reservation.Bytes] below the broker's,
// and only records that install or lift a node's gate land between: an eviction, its readmission,
// a reanchor's generation record. A PURGE IS NOT one: it frees nothing until the trim runs and
// nothing bounds how many an operator issues, so it could spend the room an eviction is kept for. Only a log that claims identity keeps a reserve
// ([ReservationOf]), the larger of the FLEET TERM ([GateReserveFleet] peers' largest appends in
// flight, plus [GateRoom]) and a sixteenth of the ceiling ([GateReserveDivisor]).
//
// # The capacity operation's state machine
//
// A resize is unsafe while anything publishes, so the fleet restarts into a mode with every broker
// and no publisher, and the operation's record IS the exclusion ([coord.MaintenanceOperation]). It
// records who is running BEFORE issuing anything, and is SEALED once every participant's process
// has been replaced by one booted into seal mode; only a sealed operation is verified, only a
// verified one confirmed and deleted. The transitions are stated here only, a restated one being a
// second state machine ([SealRestarts], [Retire]).
//
// # The alarm table borrows every threshold it fires at
//
// The rule is ADR-0015 (alarms.go): an alarm never invents a number, it fires at one some OTHER
// decision already made (the grace that takes a copy out of service, the replay window a ceiling
// was set to hold), and ONE evaluation feeds every surface.
//
// # A node's alarm transition is the record its wake derives from
//
// Each onset and clearing is handed on once as an [AlarmTransition] with a derived id
// ([TransitionID]), published as an inbound delivery that wakes the seats (ADR-0095). It is never
// a record on a log, which at its ceiling would refuse it.
//
// # A run waiting for an operator is raised fleet-wide
//
// The give-up never ends such a run ([KindExecutorResumeBlocked]), so its alarm is fleet-wide.
//
// # Whose failure a refusal is
//
// [Reason.RefusesTheWrite] sets apart the refusals for WHAT THE WRITE IS OR NAMES, which every node
// and attempt meets alike (record_too_large, deleted, op_reused, superseded); a refusal of the node,
// log or broker it went through, or of a reason this build does not name, is not one: another node,
// attempt or operator gesture gets past it. Decided per reason beside [Reason.BlamesWriter]
// (TestOnlyARefusalOfTheWriteItselfIsTheWorks).
package statelog
