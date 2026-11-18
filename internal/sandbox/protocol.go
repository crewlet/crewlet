// Package sandbox is the executor runtime: every agent seat's executor is its
// harness's own run, inside a box placed in one of the catalogue's cells.
//
// The shape that matters is that a run is DETACHED. The launch starts it, the
// turn SUSPENDS, and the engine RESUMES the same turn with the run's report
// when the run completes — possibly minutes later, possibly in a different
// process after a restart, possibly on a different node. Nothing
// here parks a goroutine on a running job: a goroutine cannot survive a
// restart, and a harness legitimately runs for longer than a deployment
// window.
//
// A run that stops to ask a person something is that same shape one step
// further: the seat goes FREE while the question waits, and the answer is
// matched back to the run by the CONVERSATION it was asked in rather than by
// the inbox batch the question arrived in. A person answers where they are
// talking, and the engine's own chat prompt routinely puts their reply in a
// finer partition than the question was asked from — so a match on the batch
// lost the answer outright. The rule is [ConversationRef.Answers], and
// [ConversationRef.Best] chooses between the runs it admits.
//
// AND BY WHEN IT WAS WRITTEN, against when the question was asked
// ([PendingRun.AskedAt]): the first reply posted after the question is the
// answer, and it is RECORDED on the run ([StatusAnswered]) before anything is
// done with it. Resuming with it is then this package's job, retried on its
// own schedule with the seat's inbox held behind it, never the inbox's — a
// person's message handed back to the broker returns behind their next one,
// which then took the question. See answerowed.go.
//
// AND IT REACHES THE SEAT ONCE, whatever becomes of the run. A recorded reply
// is either TAKEN by a resumed turn — written on the row at the last moment
// before the turn runs ([RecordedAnswer.TakenAt], [ResumeRequest.Begin]) — or
// LET GO back to the seat the way it came, through an outbox on the row
// ([PendingRun.HandBack]) — a chat reply to the seat's inbox, an answer by turn
// to its control topic ([handBackSubject]): after its attempts are spent, by an
// ending no turn reached, or by the seat's next holder reaping a claim whose
// node stopped before its turn took it ([Coordinator.RecoverSeat]) once the claims of that
// answer have died too often or for too long. Short of that, such a claim is
// REVIVED — the answer given back to the run and the run resumed with it
// ([PendingStore.ReviveAnswer], [Coordinator.revivable]) — because a claim that
// never reached its turn used nothing. The take is exclusive with both, in the
// store's own compare-and-set: an ending is DECIDED on the row before any of
// it is done ([RecordedEnding]), no turn takes an answer from a row whose
// ending is decided ([ErrRunEnding]) or from a revived run, which holds no
// claim, and a take that landed first is seen by the decision or the revival
// and the reply goes with the run as used — whichever lands first wins,
// whatever any lease says. And no ending deletes a row still
// holding a reply it owes the seat ([ErrAnswerOwed]). The reply's delivery is
// recorded as worked when the reply leaves the run, either way
// ([CoordinatorOptions.Spent]) — but a refused reply's only once a turn that
// took its refusal keeps it, since the delivery itself is what goes back to the
// seat when the refusal is let go of ([AnswerRefused]).
//
// AND A LOST RUN IS ANNOUNCED ONCE: the announcement goes out before the record
// is deleted, under the identity its decided ending recorded, so whichever node
// finishes the ending publishes the same event ([Coordinator.announceEnding]).
// So is a STOPPED one — a person's pause with `stop_running` ends the seat's
// launching and running runs at once ([Coordinator.StopSeat]) — as a stop and
// never as a loss, with who stopped it recorded in the deciding write
// ([PendingRun.Stop]), its job ENDED and its spend read off the box and charged
// before the box goes, and the turn parked on it closed as stopped
// ([CoordinatorOptions.CloseStopped]).
//
// AND A STOPPED OR LOST JOB IS ENDED BEFORE ITS SESSION IS TAKEN, and so is
// one still working when its turn's cap has passed, since the cap is an end:
// the read sends its script a request to end it — SIGUSR1 to the pid the script
// recorded in its box ([Sandbox.EndJob], [JobEnder]) — which stops its harness
// and packs its session as its own end does, and kills what is left past the
// wait ([Sandbox.KillJob]), so what is taken is final wherever the job ended.
// See endjob.go.
//
// The reap fences the row to its lease before it revives or lets go of
// anything — and the revival stamps that lease on the row itself — and a take
// under an older lease — or under none — is refused, which holds because every
// claim stamps the CLAIMANT's lease on the row ([PendingStore.ClaimForResume])
// and every launch the lease that launched it ([CoordinatorOptions.Lease]).
//
// EVERY SUBJECT A RUN IS REACHED ON IS NAMED BY THE ROW'S SEAT ID — its
// completion and start on the seat's control subject, an answer by turn and a
// reply handed back on its inbox — through one parse of the id the row carries
// as text ([controlSubject], [inboxSubject]), because a seat's mailbox is named
// by its id, derived from its immutable handle (ADR-0013). The handle the row
// carries beside it is a label and this node's bookkeeping key, never an
// address: the coordinator's per-seat counts, holds and retries are keyed by it,
// and whatever maps one of them onto a lease or a mailbox maps the handle to
// the id first. An answer by turn is stamped with that same id
// ([AnswerDesk.Deliver]) and dropped by any seat whose run does not carry it
// ([Coordinator.AnswerByTurn]).
//
// WHO ANSWERED is recorded as iam.ActorFor makes it of a principal — the
// author, its kind and the credential it came through ([AnswerWords]) —
// on the row and on every announcement of what became of the answer, never as a
// seat beside an actor.
//
// AND THE ANSWER IS SEALED: its words and who gave them are on the row only
// inside a seal under the fleet keyring, bound to the run's turn, the question
// it answers and its deliveries, like every other instruction channel
// (ADR-0031), and every reader opens it before anything of it is used. An answer
// whose seal does not open is never handed to a turn or attributed to anybody.
// See answerseal.go.
//
// AND A REPLY TOO LARGE TO BE THE ANSWER IS THE QUESTION'S REFUSAL (ADR-0114):
// past what the row carries as an answer ([MaxAnswerBytes]), decided against the
// run as the store finds it still waiting on the question
// ([PendingRun.takesAnswer]), a chat reply is not taken; what the row records as
// the question's outcome instead is its refusal — its deliveries by id, when it
// was posted and the figures, sealed, and none of its words
// ([PendingStore.RecordRefusal], [PendingRun.AnswerRefusal]) — and announced
// as `executor_answered` too_large under one identity per run, question and
// delivery ([tooLargeRecord]). The refusal is then resumed exactly as an
// answer is: the run is told, in the engine's words and through the same
// channel an answer reaches it ([AskedRun.Refused]), that the person replied
// and the reply was too large to hand it, and either asks again with a question
// of its own, whose own [PendingRun.AskedAt] leaves everything written before
// it the ordinary message it is, or carries on without it. While the refusal
// is the run's the question waits no more, so nothing the person writes
// meanwhile is taken for its answer. The reply itself stays where it was: in
// the conversation, and in its delivery, kept on the seat's inbox until the
// refusal leaves the run ([AnswerRefused]) — spent once a turn that took the
// refusal keeps it ([Coordinator.spendRefused]), and back to the seat as the
// ordinary message it is when the refusal is let go of. A refusal let go of —
// declined after its attempts, or a seal that does not open — puts the
// question back to waiting under the anchor it was asked at, as an answer let
// go of does, and the next qualifying reply answers it; the let-go is announced
// as `executor_answered` declined ([Coordinator.announceRefusalLetGo]). A
// reply within the share is the run's answer word for word, and an answer by
// turn is held to [MaxAnswerTextBytes] at both its ends, on one string.
//
// AND A CALL TO A TOOL AT NEEDS APPROVAL PARKS ITS RUN IN THE SHAPE A RUN'S ASK
// DOES (ADR-0077): the job's question is the call ([PendingRun.HeldCall]), its
// arguments sealed beside it ([CallSeal]); only its approval route answers it,
// never a chat reply ([ConversationRef.Answers]) or an answer by turn that does
// not name it ([Coordinator.AnswerByTurn]); and the resume that makes the call
// marks it first, so no attempt at the same answer makes it twice
// ([PendingStore.MarkHeldCall]). See heldcall.go.
//
// AND A RUN'S VALUES ARE HELD TO A BYTE BUDGET: one value in the coordination
// store is held to what a key holds, so the run's row and the log beside it are
// each held to [RowBytes], in the bytes their encoder writes, and every field
// has a share of its value's. The log's calls have theirs ([BridgeLogBytes]),
// each call held to its own and the calls it counts without listing kept as
// identities ([ElidedCall]); on the row, the parked state has its own
// ([ExecuteStateBytes]), which its park condenses to fit and the store refuses
// past, and the rest is every other field held by the write that puts it there
// — a person's reply, a held call's sealed arguments and a box's word on its
// session included — with room stated for what a later build adds. Every call
// carries the identity its run's call log admitted it under
// ([BridgeCall.Digest]), but for one whose arguments are withheld
// ([BridgeCall.Withheld]), so neither a cut argument nor a dropped call moves an
// operation id a resumed segment derives; and the id its harness gave it, where
// the box sent one the log keeps whole ([BridgeCall.CallID]), which is no part
// of that identity. See rowbytes.go.
//
// AND A BOX IN A `direct` OR `container` CELL IS TOUCHED ONLY ON ITS LAUNCHING
// NODE'S HOST ([PendingRun.LaunchNode], ADR-0065): that node's own waiter polls,
// keeps alive, reaps and reclaims it, a seat's holder elsewhere asks the host to
// read it ([BoxReader]), and a launching node whose presence stays lapsed for
// the box timeout has its running jobs lost with it. See boxhost.go.
//
// AND A JOB LOST WITH ITS HOST IS CARRIED ON, NEVER COLLECTED: one its
// engine's restart killed — which the box tells from one that crashed by the
// engine process that last saw it running ([JobLost], local_witness.go) — one
// whose box is gone, and one whose node left. The waiter that drives the box
// records the loss on the row ([PendingRun.Lost]), and the seat's holder reads
// what the job spent where its box is left and resumes the turn to relaunch the
// same round, at most [MaxRoundLosses] times a round. See lost.go.
//
// AND WHAT A RUN PUSHED TO GITLET is the push record's
// account, never the run's: a collection of a run given a repository there to
// push to reads the rows Gitlet wrote under its launch ([RunPushes],
// [Result.Pushed]) and the run's report states what each ref became, the run
// having been told its own report need not name them. A record that cannot be
// read is SAID to be unknown rather than retried — a collection held until
// Gitlet answers holds a finished run's seat for as long as the host is
// down — and never left out, since silence reads as "nothing pushed".
//
// AND A RUN'S HARNESS SESSION CAN LEAVE ITS BOX: a run names its session before
// its harness starts ([RunHandle.SessionID]), says whether it continued the one it
// was asked to, and why not where it did not ([Result.Continued],
// [Result.FellBack]), and packs it when the harness exits
// ([Result.Captured]); a run handed a session packed in another box
// ([RunRequest.Carried]) restores it before its harness takes it up, and a box
// takes the archive streamed rather than whole ([Sandbox.WriteStream]). See
// carry.go.
//
// # A launch's harness session is kept, and a turn's end recorded
//
// EACH LAUNCH'S HARNESS SESSION, KEPT, and each turn's END, recorded
// (ADR-0073), by session.go.
//
// # A session is taken wherever its box can still be read
//
// A run's harness keeps its conversation in its box, and a box does not outlive
// its turn. So the session is taken out at every point the box can still be
// read, ON THE NODE WHOSE HOST THE BOX IS ON (ADR-0065), before the box is
// paused or reclaimed:
//
//   - a completion's collection ([Coordinator.readBox]): the archive the run's
//     script packed when its harness exited — or, for a job that never got to
//     (one that failed before it packed, one lost with its engine whose box is
//     still there), one packed now. A job its turn's cap passed while it still
//     ran is ENDED first, as a stop's is, since the cap is an end;
//   - a stop's read of what the run had spent, which first ENDS THE JOB
//     ([Coordinator.endJob]): its script, asked, stops its harness and packs
//     the session as its own exit does, and the archive it left is taken —
//     one packed now where it left none, the job killed past the wait;
//   - the ending of a run LOST with its box still there (a launch whose engine
//     restarted before its turn could park), whose job is ended the same way
//     before the ending reclaims the box — on this node's host, or asked of the
//     host the box is on ([BoxReadLoss]).
//
// So a capture is FINAL wherever its job ended — by itself, asked, or killed —
// and taken as it stands, not final, only where the job still works: one a box
// would not end. No pack is made beside a harness still working but that.
//
// A box that is GONE — lost with its host, or with a node that left the fleet —
// has nothing left to read, and keeps nothing.
//
// # Two halves: taken before the box goes, kept off every frame
//
// What has to happen before the box is paused or reclaimed is only what reads
// it: the TAKE ([Coordinator.takeCapture]) — the box's archive, packed where it
// must be, streamed through the scrub of every secret the engine handed the box
// and the redaction every text leaving a box passes ([SessionTaker],
// [HandedSecrets]), sealed under the fleet keyring as it streams and spooled
// to this node's disk sealed ([SessionKeep.Seal]). It is bounded
// ([SessionTakeBudget]), and never past where the read it is part of must stop
// to leave its pause and its answer their room.
//
// What can take minutes — the upload of a session at its bound at the object
// store's slowest pace, and the record — is the KEEP ([Coordinator.keepCapture]),
// made off the read's frame from the moment its take is spooled, under a
// budget of its own ([SessionKeepBudget]): no read a node is waiting on, and
// no ending, holds for an upload, and no keep waits on what its read does
// after the take (a collection's record of how its job's harness ended, the
// box's pause and the pause's record), steps no capture's budget counts. The
// spool is the node's own and goes once the keep is done.
//
// # Every capture is stamped on the run's row
//
// Its place — the launch's ordinal ([PendingRun.LaunchSeq]) and its own among
// that launch's captures — is reserved on the row before a byte is spooled
// ([PendingStore.StampCapture]), with its SAVE POINT — how many of its round's
// calls the run's log had taken when it was read once the job was over, for a
// job whose harness ended its own run ([SessionCapture.Through]; every call the
// log takes is numbered as it is appended, [BridgeCall.Appended]), or, for
// the take of a job still working or one cut off, the newest its launch is
// known to hold (savepoint.go) — and the last note read the record says it
// holds, none for a session cut off and none from a take the record marks
// unanswered on ([SessionCapture.NoteCursor]): what a launch that continues the
// session would need to know of what came after it.
// How it came out is stamped beside: LANDED, REFUSED with why (no box to
// read it out of, a pack that failed, a session past its bounds, the seat's
// budget, an upload the store had no room for, a record the log will never
// take), or UNRESOLVED — uploaded, and whether its record landed unknown —
// which the turn's end, and a relaunch before it decides, asks again under the
// same operation.
//
// A SAVE, a capture of a job still working, reserves its place the same way
// and keeps a second one beside it ([PendingRun.Saving],
// [PendingStore.ReserveSave]) until it can no longer become the turn's kept
// capture, so a final take of the job that overtakes it never hides it; it is
// reserved only while its job runs undecided ([licensesSave]); "A save under
// way" below is its account.
//
// # Two reads of one job: one holds the place, the other stands by
//
// A stop's read and a collection's of one job can take its session at once,
// and the place both ask for is the store's to give one of them. The other —
// its reservation refused, or the job's capture found begun by the first —
// takes the session all the same, whole, each read streaming the archive its
// own pack made, or the job's own capture where its script left one
// (internal/sandbox/executor's PackCapture), and STANDS BY for
// that capture rather than walking away from it: named on the capture's stamp
// as its standby ([PendingStore.StandBy]) before it spools, its session sealed
// on its own node's disk, and its keep waiting to see what the capture comes
// to ([Coordinator.takeOver]). Where it LANDS, the standby keeps nothing: one
// read's upload of the job's session, never two. Where it does not — the read
// holding the place had its scrub refused, its seal or its upload fail, its
// node stop mid-keep, or its record go unanswered and then refused, asked
// again ([Coordinator.settleCapture]), or begun past its budget and not
// named by the turn's row on the executor log, read once by the standby then
// ([Coordinator.awaitStood]) — the standby takes the place after it
// ([PendingStore.TakeOver]) and keeps its own. So a session one read took
// whole is kept wherever either read could have kept it, and that holds of
// every read that finds its place taken: a stop's, a collection's and a
// loss's, its reservation refused, found begun, or never answered
// ([Coordinator.abandonPlace]). A read of the job ENDED beside a capture taken
// as the job stood — a read whose end could not end the job — takes the place
// after it instead ([Coordinator.besideAnother]), its session the more whole.
// A turn's end waits for a standby as for a capture still being kept
// ([captureInFlight]), for as long as its takeover and its keep can take
// ([Coordinator.awaitEndCaptures]); a standby that cannot keep what it took
// withdraws ([PendingStore.StandDown]), and one whose taker is gone is
// withdrawn by the wait ([Coordinator.standDownGone]). A standby or a takeover
// the store never answered is settled, never left on the row with nothing
// behind it ([Coordinator.standBy], [Coordinator.tookOver]). One standby per
// place: a third read of the same job at once stands down.
//
// # A capture whose taker stopped is settled from the log, not waited out
//
// A take dies with its process, leaving its place stamped begun and its spool
// on that process's disk. Read by its age alone ([captureInFlight]), such a
// capture would hold the turn's end for as long as a capture can take
// ([captureLife], over an hour at the bounds), and a relaunch that followed it
// would start a session of its own beside a record the log may well have taken.
//
// So every process keeps the captures it takes under a TAKER LEASE of its own
// in the coordination store ([coord.ClassTaker], taker.go): claimed when its
// coordinator is built, renewed [coord.KeptRenewals] times a TTL, held until
// its last keep has ended — through a drain, which gives presence up while the
// keeps still upload — and released then; a process that dies leaves it to
// lapse on its TTL. Every stamp names that lease: its holder, the process
// incarnation ([SessionCapture.Taker]), and its fencing token
// ([SessionCapture.TakerEpoch]), read again right before the take reserves its
// place, so a lease claimed afresh while the session was packed is the one
// named.
//
// A TAKER IS GONE where that lease has lapsed or is held at another epoch, and
// nothing else: one coordination read, three-valued, so a store that cannot
// answer is never a taker gone ([Coordinator.takerGone]). Every wait for a
// capture ([Coordinator.awaitCapture]) — the end's and the relaunch's — asks
// it of every begun stamp in sight, the newest capture and a save under way
// beside it, and settles one whose taker is gone from the turn's row on the
// executor log, read once a pass at the level a relaunch reads it, which has
// seen every record the log took before it ([Coordinator.settleGone]):
// LANDED where the row names its place, written from the row; REFUSED where it
// does not, since a taker gone sends nothing more. Written only over the begun
// stamp it was decided from ([PendingStore.SettleBegun]), so a stamp the taker
// wrote in the meantime stands — an unresolved one among them, which says a
// record was sent and may yet land, and which a wait then asks again under its
// own operation rather than settle refused.
//
// A CAPTURE THE LOG TOOK IS LANDED WHATEVER ITS TAKER'S LEASE. A host whose
// coordination store answers its leases and not its runs (the two are other
// streams) keeps its taker lease while no stamp of its own reaches the row, so
// its begun stamp is never settled by the lease, and a job lost with that host
// would leave its relaunch starting without a capture the log had kept. So a wait
// reads the turn's row for a begun stamp under a lease that holds, or that the
// store cannot say of, as well, and settles it LANDED where the row names its
// place, never refused, since a taker not read gone may still send its
// record: the turn's end at most once every [heldRowPace] of its wait, and a
// relaunch once, after its wait has run out, for a capture of the job it
// follows whatever its age ([Coordinator.ContinuedCapture]), and a read
// standing by before it takes the place after the capture it stands by for:
// once, at the pass that finds that capture begun past its budget
// ([Coordinator.awaitStood]), and once where its own wait runs out first
// ([Coordinator.waitedOut]), deciding on the row's word alone, whether or not
// the run's record then takes the landed settle, so the session is not
// uploaded a second time beside a record the log took. A stamp that names no
// taker is not read so.
//
// A SETTLE IS THIS NODE'S OWN WRITE. A wait's patience bounds its waiting and
// every read and ask it makes; each settle write it decides (a gone taker's
// capture settled, a gone standby withdrawn, the stamp of what an ask of an
// unanswered record settled) is given its own room past it, a record's budget
// and this node's crossing of the run's row ([Coordinator.settleContext]), and
// an ask that leaves a record unanswered writes nothing. Cut where the wait
// ends, a settle the store was slow to take would not be applied, and the wait
// would answer the capture still being kept.
//
// THAT IS WHAT THE FENCE BUYS. A keep checks its own lease before its upload
// and again before every attempt at its record, from its own renewals rather
// than a store read: held at the epoch its stamp names, inside the exclusive
// window of its last confirmed renewal (coord.ExclusiveThrough, one renewal
// interval its margin) — and EVERY ATTEMPT AT THE RECORD IS CUT AT THAT
// WINDOW'S END, so none outlasts it. Fenced, it uploads and records nothing,
// stamps what it knows — refused where no record was sent, unresolved where
// one was and went unanswered, which a wait asks again under its own operation
// rather than settle — and says so (`executor_session_fenced`, beside the
// `executor_session_not_kept` every unkept capture logs). So no attempt the
// taker makes outlives the window, which closes a renewal interval before any
// peer can read the lease lapsed.
//
// WHAT THE FENCE CANNOT REACH is a request the taker had already handed on
// before its window closed — to a data node the router asked, or to the
// broker connection's own buffer while it reconnects — and that the log takes
// only after a peer's read of the turn's row: the capture's record then lands
// beside a stamp that says refused. On the turn's end the log orders the two
// itself — a record it took before the end is the row's place and is kept,
// one after it is refused by the end's tombstone — and never contradicts
// itself. THAT IS DECIDED, AND NO REFUSAL GOES TO THE LOG: ordered there as the
// end's tombstone is, the settle's refusal would have the applier refuse the
// late capture and lose a session, and no session history is given up for a
// stamp's sake. So a late capture the log takes before the turn's end is kept
// and read as any other — the turn's row names it, a session read and the
// conversation's next turn take it — and one decision alone goes by the stamp
// once the log names the capture: the relaunch that follows the job whose
// capture was settled, which continues only what the run's record keeps, and
// the record never keeps a place it holds refused — so every attempt at that
// relaunch starts without the capture, the one whose wait settled it and one
// asked again after the record landed alike; and where the decision is told
// with the turn's row in hand — to a reader of the seat's sessions, and in the
// relaunch's own reason, which reads the row for a capture its record refused
// — it says the log keeps the capture, never that it was not kept, and the
// relaunch's run, starting without it, is named the turn's own as a session it
// may read ([namedThoughRefused], [ContinuedSession.KeptTurn]).
// Nothing else treats the stamp as authoritative once the log names the
// capture: the end names it unkept at the row's own place, which releases
// nothing ([executorkeep.Ending.Unkept]), and an account of how the turn ended
// tells a capture as not kept only where it is newer than the row's own place
// (internal/executordomain's Session.Unkept). The end says too that the turn
// went on, which the record holds — and where that capture is the turn's last
// word, names it so ([executorkeep.Ending.LastWord]): a row at its place is
// read as all the turn did, never as one its turn went on past
// (internal/executordomain's Session.WentOn).
//
// THAT RELAUNCH STARTS WITHOUT IT, AS ONE WHOSE JOB KEPT NOTHING BEFORE A SLOW
// KEEP DOES ([relaunchCaptureWait], [Coordinator.continuedWithout]): in the
// base the turn's first launch froze, where the turn has kept nothing and its
// row still held that base when the relaunch read it — the late record landing
// only after — and otherwise in a session of its own, or the one its job's box
// holds where it carries the run on in that box. Beside an older capture its
// job kept, which a relaunch past a slow keep continues, it starts without that
// one too: the turn's row names the newer, and a relaunch restores only the
// object the row names ([ContinuityOf]).
//
// NOT PRESENCE, which answers another question. A draining node gives its
// presence up at once while its keeps still upload, so a taker read as gone
// without it would have its capture refused under it, the turn's end would
// tombstone the turn before the draining node's record landed, and the turn's
// last session would be lost — with the older one the end's unkept capture
// lets go. Presence says whether a node may take new work; only the taker's
// own lease says whether a process still keeps what it began.
//
// A stamp that names no taker, which only a coordinator built with no taker
// lease writes (a test's, since every engine wires one), is waited for by its
// age alone.
//
// # A capture's life is counted on one clock, and read on another within twice their skew
//
// Every step between a capture's instant ([SessionCapture.At]) and its taker's
// last attempt at the stamp of what came of it is bounded, each at its bound
// at the slowest crossing a link is allowed: the rest of its take with its
// reservation, its digest, its keep with its two reads, its stamp, and for a
// takeover the takeover and, where the store did not answer it, the standby's
// withdrawal and its read of the row back ([captureLife]). Past that, nothing
// the taker sends lands. A read standing by can take the place after it no
// later than its window past its own instant ([standbyWindow]).
//
// A STAMP'S INSTANT IS ITS TAKER'S CLOCK'S, and every node that reads its age
// reads it on its own. The engine holds each machine within an hour of the
// fleet's clock, so any two within two (objstore.AssumedClockSkew); so a node
// reads a begun stamp as being kept until its life and two hours have passed
// ([SessionCaptureBudget], [SessionCapture.keeping]), and a read standing by
// as standing until its window and two hours have ([standbyBudget],
// [SessionCapture.standing]). A reader whose clock stands ahead never releases
// a stamp before its life ends; one behind holds it up to four hours longer,
// which the waits bound on their own clocks:
//
//   - A READ STANDING BY waits for the capture it stood by for on its own
//     monotonic clock, that capture's whole life and one read
//     ([standbyWait]), and then reads the turn's row on the executor log
//     once, for that capture still begun, keeping nothing where the row
//     names it ([Coordinator.namedAtWaitsEnd]), and the run's row once more:
//     that capture still begun or refused, it takes the place after it;
//     landed, or the row moved on, it keeps nothing; its record still
//     awaiting the log's answer, it stands down ([Coordinator.waitedOut]).
//   - A TURN'S END waits for each capture it sees for at most its life, and
//     each read standing by for at most its window, from the moment it first
//     sees it, on its own monotonic clock, and once every one has passed
//     reads the row once more and decides on that read
//     ([Coordinator.awaitEndCaptures]).
//
// And the sweep of spools a crash left spares a spool for as long as its keep
// may still wait to open it, and an hour more for a step of the machine's own
// clock ([Coordinator.sweepSpools]).
//
// A RECORD SENT AND NEVER ANSWERED is asked again whatever its object's age
// reads on the asking node's clock: the log decides the window a record may
// name an object in (objstore.RecordWithin) on its own clock, from the instant
// another node's clock minted the object's key ([Coordinator.settleCapture]).
// A reader holds such a capture as awaiting that answer until the window and
// two hours have passed on its own clock ([pastRecordWithin]), past which the
// log, on any clock the engine holds, refuses it.
//
// # And every turn's end, owed on the row until the log takes it
//
// How the turn ended — completed or failed, the reviewer's decision, whether
// its reply reached the party waiting, who stopped it and why, what lost it —
// is the account a transcript cannot hold ([executorkeep.Ending]), recorded on
// the turn's row of the log beside the session it kept. Whatever ends the turn
// writes it on the run's row ([PendingStore.OweEnd]): the engine's resumed
// turn, and this package's endings that no frame of the turn reaches — a loss,
// a stop ([CoordinatorOptions.TurnLost], [CoordinatorOptions.CloseStopped]).
// The run's ending records it on the executor log ([Coordinator.publishEnd])
// before it deletes the row, which the delete refuses until then
// ([ErrEndOwed]): after every capture of the turn has settled, a save under
// way that a final take overtook among them, which may still become the turn's
// kept capture ([captureInFlight]), so the end's tombstone never refuses the
// turn's own last session, with the turn's NEWEST
// capture where it was not kept ([executorkeep.Unkept]), and saying whether the
// turn WENT ON PAST the session its row names ([wentOnPast]) — wherever its
// run's record does not hold the turn's last word kept: a capture taken while
// its job still worked, an earlier launch's, or none beside a newer one that
// was not kept — and naming that last word where it is the newest capture,
// which the record did not keep ([lastWordOf]), so the log's row never names an
// older session as the turn's last without saying so: the log keeps that
// session, and says the turn went on past it unless its place is the last word
// named. On every frame somebody waits on — a completion's resume, a stop, a
// seat's recovery — the ending is decided there and the end recorded by the
// ending's own retry, armed at once ([errAskAway]): a capture still being kept,
// and the log's write after it, are not that frame's to wait on. A seat's
// retirement, which holds the seat's lease and a budget of its own, both
// decides its endings and records their ends on its own frame. So the row
// outlives the frame that decided its ending by the settling of the turn's
// captures — each capture the end sees bounded by its life from the moment
// the end first sees it and one more read of the row ([captureLife], an hour
// and twenty-five and a half minutes at the bounds), and where a read stands by
// for one that does not land, by that read's window and its takeover's life
// from the moment the end first sees the read, each with its read
// ([standbyWindow], about four hours and sixteen and a half minutes in
// all), and for a capture whose taker is gone by its lease's TTL and one
// read of the log (above) — and the end's write, and the seat's mail waits on
// it as on every ending kept for its retry ([Coordinator.awaitEndCaptures]). A turn
// with no capture still ends: its row names no object, and holds the account.
//
// # And every turn's first read of its conversation, held on the log
//
// A turn's first launch freezes the read its dispatch made of its conversation
// on the turn's run record ([PendingRun.Base]) and records it on the log as the
// turn's BEGIN ([Coordinator.recordBegin]): the turn's row from then on names
// where the read was made, the turns it handed out that the turn was told of —
// which its end absorbs once it has landed a capture — and the head it froze,
// which the row HOLDS against every release but a retirement while the turn is
// live and has landed no capture. Every capture carries the same read
// ([Coordinator.recordCapture]), which the log takes where no begin landed
// first: a turn's first landed record keeps the read.
//
// # The session switch
//
// `turn_engine.conversation_session.enabled: false` keeps nothing — no capture
// is taken or stamped ([SessionPolicy.Keep], the fleet's word) — and prepares
// nothing for one: no launch seals what it handed its box, and no loss asks
// another node's host to read a box, holding the seat's mail for a take that
// would keep nothing ([Coordinator.capturing], this node's word). It still
// records every end, which is the account and not the session; and a stop
// still ends its job before its box goes, which is how a stop ends a job.
//
// # A save under way
//
// Kept by saving.go. A SAVE is a capture of a job STILL WORKING, taken while
// its harness runs on ([SessionCapture.Final] false,
// [SessionCapture.Interrupted] true): its place is reserved on the run's row
// like every capture's, after the row's newest, and stamped there as it comes
// out. What makes it more than a take is that the job goes on, and so do the
// reads of it: the job's collection, a stop's read or a loss's take the job's
// FINAL capture in the place after the save while the save may still be
// uploading, and every stamp of that final take replaces the row's newest
// capture ([PendingRun.Capture]). Read off that section alone, a save the final
// overtook is out of sight while it may still become the turn's kept capture:
// where the final is refused, the save is the newest session of the job anybody
// kept, and an end that recorded over it, waiting on the final alone, would
// leave its record to meet the end's tombstone.
//
// SO A SAVE KEEPS A SECOND PLACE ON THE ROW, [PendingRun.Saving], from its
// reservation ([PendingStore.ReserveSave]) until it can no longer become the
// turn's kept capture: it lands or is refused, or a capture at or past its
// place is kept ([PendingRun.Kept]), so its landing could change nothing. Every
// stamp of the save's own take moves it on there, and every stamp that takes
// lets it go once that holds ([stampOnto], [savingAfter]). A READING, NOT A ROW
// INVARIANT: a build without the field carries it in [PendingRun.Extra] and
// writes it back unchanged, so a row may hold a stale save: one that build
// settled or asked again in [PendingRun.Capture], or one whose evidence its
// final take overwrote. So nothing reads the field but through one rule
// ([PendingRun.savingTake], [PendingRun.saving]), which a source walk holds
// every reader to, and the next stamp that takes reconciles it from the row as
// that stamp found it.
//
// # A save's license
//
// A save takes its place ONLY WHILE ITS JOB RUNS UNDECIDED ([licensesSave]),
// decided in one read-decide-write of the row as it stands: the run running,
// the save of its current launch, nothing of the job's end read and no refusal
// of its completion owed ([PendingRun.jobOver]), no loss marked, no ending
// decided, no capture or save of the turn that may still land
// ([PendingRun.capturesMayLand]), and at the place after the row's newest.
// Every read that takes a job's final capture writes its decision on the row
// before it reads the row for its place (the claim, a stop's ending, a loss),
// so a final take either finds the save's place reserved and takes the one
// after it, or the save is refused: A FINAL TAKE NEVER LOSES ITS PLACE TO A
// SAVE.
//
// THE PLACE ORDER IS THE CONTENT ORDER. A save's place, its save point and its
// note cursor come from ONE read of the row made before its pack, so any
// capture placed since that read moves the next place and refuses the save:
// read after the pack, a save packed before a final take could be handed the
// save point that final take's own read placed after the job's end, counting
// calls the save never held. The store holds a save that reads late to the
// job's end besides ([PendingRun.jobOver]).
//
// WHAT THE ARMS LEAVE. A save of a job whose completion is announced but not
// yet recorded on its row (on its way to the seat's holder, claimed and handed
// back before its collection heard from the box's host, or refused on a
// condition the holder hands on, [SeatLost] or [Draining], which nothing
// records) is licensed as an ordinary take of a job that may have ended: the
// collection's final take is placed after it and supersedes it, and where that
// take is not kept, the save is kept holding all the job did, told as a session
// its run went on past. It over-tells; it never holds a call the session did
// not see.
//
// # A save taken as its job works
//
// Kept by save.go and saveledger.go. A save task runs its steps CHEAPEST FIRST,
// so a save the store would refuse costs its box nothing: the box attached
// (never woken), its job's event stream read for whether it moved since this
// node's last landed save, then ONE READ OF THE RUN'S ROW, settled where a
// save of the turn was left begun by a gone taker or unanswered, and held to
// the license's pre-check; the fleet's policy, the seat's allowance, what the
// box was handed and the taker lease; and only then the pack. The place, the
// save point and the cursor all come from that one read, made before the
// pack, for "The place order is the content order".
//
// THE BOX PHASE IS THE DUTY'S, nothing after it: the attach, the stream's read
// and the pack end with the stint of duty the offer came under (an E2B duty
// handed to a peer, the coordinator stopping, a drain's end), and a save cut
// there stamps nothing and is released from its box. Past the pack the save is
// the coordinator's, as every take is: its take context starts after the pack,
// so the scrub has its whole budget, and the scrub's values are read after the
// pack too, so a token the poll recorded while the pack ran is in them.
//
// THE SEAT'S ALLOWANCE IS CHARGED BEFORE THE STAMP'S INSTANT, never after: the
// whole company agrees on it (one value per seat in the coordination store's
// uploads bucket, which ages a value out with the hold it needs), written by
// compare-and-swap, sized from the packed archive and a margin for what the
// scrub changes, keyed by an entry id so an append asked again counts once. A
// crash, a lost answer or a cut keep can then only over-count, which is the
// safe direction for a bound every artifact's bucket shares, and nothing waits
// between the stamp's instant and its reservation. A spool that seals past what
// was counted is refused before its keep, so a landed save is never
// under-counted. An entry is held for [saveLedgerHold]: a save's object is
// stored within the take's life after its entry, the collector deletes a
// superseded one within its grace and two of its passes, and the entry's end is
// read on another node's clock, so twice the engine's assumed skew.
//
// A SAVE CONTINUES NOTHING ([savePlaces.continued] nil): a run's row records no
// capture its launch continued, the saver is often not the launching process,
// and the launch's request names what it ASKED for, which a failed restore
// replaced with a fresh session. So a save is placed at its launch's newest
// landed save's point, or zero, which tells more, never less.
//
// # A save is offered by the poll, from memory
//
// Kept by save.go and the waiter's hook. THE POLL IS THE ONE TRIGGER: it
// reaches every running box each interval already, under the duty that
// authorises touching it, so each box a poll finds running within its turn's
// cap is offered ([WaiterOptions.Save]); a bridge call, a clock in the box's
// script or a signal would each be a second way into the box, under no duty.
// THE OFFER DECIDES FROM MEMORY AND NEVER BLOCKS, since the poll is the box's
// keepalive: saves on, the runner saving, the stint live and no task of the
// turn under way on this node, the save due, and one of [maxConcurrentSaves]
// slots free, taken without waiting (the next poll offers again). The task
// runs on the coordinator's own lifetime, counted for its stop, and gives its
// slot back only once its keep has ended, so the slots bound the uploads too.
//
// THE STINT is the waiter's duty, made by the pass that claimed it and ended
// by its stand-down. A task carries the stint it began under, so a poll that
// returns after a stand-down offers nothing under a duty this node lost.
//
// DUE-NESS IS COUNTED ON THIS NODE'S CLOCK ALONE: the interval
// ([SessionKeep.SaveInterval], this node's revision) after the last attempt,
// doubled for each failure in a row up to the larger of the interval and an
// hour, and never before the seat's allowance paces it. What was not there
// (the box, the license, the taker, the place) moves the clock without
// doubling; a landing resets it and remembers how far the job's stream had
// run, so a job that has not moved since is not saved again. AT FIRST SIGHT
// (a node that restarted, or took an E2B duty over) the last save is taken
// to be the later of the launch's start and this launch's newest capture,
// instants other machines stamped, clamped to within one interval of now: as
// in "A capture's life is counted on one clock", an age read across machines
// decides only a wait one interval bounds, so the first save comes within an
// interval of first sight whatever any clock says, and a paced seat takes no
// unpaced save then. A memory no offer came for in an hour is forgotten.
//
// # A turn's session is continued in whichever box its next launch gets
//
// A TURN'S SESSION, CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS — AND A
// CONVERSATION'S, BY ITS NEXT TURN, by restore.go.
//
// Every launch of a turn keeps its harness's session in the object store
// (above), named by the turn's row on the executor log. So a relaunch of
// the turn — a reviewer's send-back, a person's answer to a run that parked, a
// job lost with its engine whose box was left behind, a sign-in replaced while
// it ran — continues the session of THE JOB BEFORE IT, as that job's kept
// capture holds it, in whatever box and cell the relaunch gets: the box the
// session was last worked in, its own copy kept, or any other, the archive
// restored there before its harness takes the session up ([RunRequest.Carried]).
// And a conversation's NEXT TURN continues the session its last turn left — the
// conversation's head, read once at the turn's dispatch and frozen by its first
// launch (conversationbase.go) — in a box of its own.
//
// # Which session: the job before's own, the conversation's, or none
//
// A relaunch follows one job — the one its reviewer collected, the one whose
// question was answered, the one lost or refused — and only that job's session
// holds everything the turn's jobs were told: the round's task or the review
// that sent the round back, a person's answer, and the job's own work. A
// capture an EARLIER job took holds none of what the jobs after it were told,
// and the calls a continuation lists after it would be numbered in another
// round's order where a send-back began one. So a relaunch whose job before
// left no kept capture — its box went with its host, its take or its keep was
// refused, its record was never answered — continues none of the turn's own:
// it starts a session of its own, told what the jobs before it did. And that
// job's NEWEST KEPT capture, past newer ones of it that will not be kept
// (refused, begun longer ago than a capture can take, sent and never answered
// past the window a record may name it in), and past one still being kept, or
// whose record still awaits its answer, once the relaunch's wait for it has
// run out: the relaunch waits for that one and asks it first, and continues
// it where it lands. A newer capture went further than the kept one, and the
// run continued from the older is told every logged call its session lacks
// after it (by what the capture is read to hold, where it is read, and by its
// save point otherwise) and, where it was taken while its harness may still
// have worked, that it was cut off there, so it never takes itself to know
// what the newer one held. One that lands after the launch changes nothing
// the launch's session or its words rest on, and one that lands between the
// relaunch's choice and its launch is caught by the launch's own check
// ([Coordinator.continueKept]). And only one whose stamp states its SAVE
// POINT ([SessionCapture.Through]), a count of the round's calls the session is
// known to hold — the run's log as the capture read it once its job was over,
// where its harness ended its own run, and else the newest point its launch
// was known to hold (savepoint.go) — which is what the continuation is told the
// calls after by: one an older build stamped states only the places it read
// the log at, which cannot say which calls came after it.
//
// AND NEVER OVER A COPY THAT WENT ON PAST IT ([copyWentPast]): a capture taken
// while its job still worked is a point the job's session passed through, so
// where a relaunch carries the round on in that job's own box, offered the copy
// of the same session the box holds, it continues that copy, which holds
// everything the capture does and all the job did after it, rather than roll
// the session back to the capture. A final capture holds the ended job's whole
// session and is continued there as anywhere.
//
// EXCEPT WHILE THE TURN HAS KEPT NOTHING AT ALL: its first job lost with its
// box before any capture of it landed, its captures refused or unanswered. Then
// the relaunch continues the session the turn's first launch froze — the one
// the conversation's last turn left — told where this turn begins and then
// handed its own fresh brief or reseed, which say what the turn's jobs did:
// otherwise a first job lost with its box would end the conversation's session
// line for good. The turn's row on the log HOLDS that base while the turn is
// live and has kept nothing (its `begin`), so a sibling's end, the seat's
// budget and the retention pass it over; and once the turn keeps any session
// of its own, that one holds the base and everything after, and only the rule
// above applies. A base is continued ONLY IN A BOX OF ITS OWN: a box the turn's
// earlier job ran in holds that job's working tree and session, which are the
// rule above's to continue in place.
//
// ONE DECISION EACH, [ContinuityOf] and [BaseContinuityOf]. The first: the turn
// live and not ending, the job before's newest capture kept, stating its save
// point and named by the turn's row on the executor log, in a format and by a
// release a runner restores ([ArchiveRestorer]). The second: a base the turn
// froze — or, before its first launch, the head its dispatch read — the turn
// live, not ending and having kept nothing, its row holding that base, and a
// runner that restores it. The launch's caller asks it before it builds the
// run's surface and briefs ([Coordinator.ContinuedCapture], and
// internal/engine's probe of a first launch), of every member of the seat's
// chain, since which of them takes the launch is the launch's to pick; the
// launch asks it again of the record as it opened the job, of the runner of the
// member it picked; and the read surface asks it of each row of a seat's
// sessions (internal/api/queries' `continuable`) through [ReadContinuityOf] —
// of a live turn's row, by a relaunch, of the same two records read as a
// relaunch reads them; of an ended turn's row that is its conversation's head,
// by that conversation's next turn, [BaseContinuityOf] as that turn's first
// launch asks it — each of the member the node holding the seat would launch it
// on now ([RelaunchTarget]), with the half a person's answer takes beside the
// first, carrying a parked run on in the box it parked in where the kept
// capture is not continued, by the launch's own tests of a box it continues in
// ([PriorBox.ContinuesIn], [Coordinator.hostsPrior]). What only a launch can
// know is the launch's alone, and no reader states it: whether the box it got
// runs a harness that takes the session up ([SessionRestorer.CheckCarry]),
// whether that box is one of its own, and whether the seat's budget can carry
// the session's context beside the brief it hands the run (internal/engine's
// launch floor).
//
// # The job before's capture, still being kept
//
// The job's capture is taken by the read that collected it, and its upload and
// record — the keep — are made off that read ([Coordinator.keepCapture]), while
// a relaunch can follow at once: a lost job's round is carried on by the very
// frame that read its box. So the relaunch waits for the keep, but only for
// as long as an ordinary keep takes ([relaunchCaptureWait]): the frame that
// waits holds the seat and a slot of the node's admission, and a keep that has
// not landed by then is one a slow store or a stopped node is holding, which a
// relaunch does not wait out on that frame. A keep still under way is then no
// kept capture: the relaunch continues the newest capture that job kept
// before it, where it kept one ([followedCapture]), and logs the newer one it
// passed over ([Coordinator.passedOver]); and where it kept none, it goes as
// one whose job's keep was refused does
// ([Coordinator.continuedWithout]): it continues the base the turn's first
// launch froze, where the turn has kept nothing and its row still holds that
// base — a base holds everything a session of its own would, and the
// conversation besides — and otherwise starts a session of its own, saying
// so, or carries its run on in the session its job's box holds, where it
// relaunches in that box. Should the capture land after the relaunch read the
// row, the relaunch's first landed capture supersedes it, as any newer
// capture does: what that job did is lost to the relaunch whichever session
// it starts in.
//
// A SAVE UNDER WAY IS WAITED FOR THE SAME WAY ([PendingRun.Saving], "A save
// under way" above): a capture of the job taken while it still worked, which a
// final take of the job can overtake while it still uploads. Where that final
// take is refused, the save is the job's newest session anybody kept, so the
// relaunch waits for it as for the newest capture ([relaunchWaitsOn]), settles
// and asks it as the newest, and names it by its own place.
//
// ONLY FOR WHAT ITS DECISION DEPENDS ON ([relaunchDependsOn]): a capture or a
// save of the job it follows, and an earlier job's only while the turn has
// kept nothing, whose landing ends the base the relaunch would otherwise
// continue. An earlier job's keep beside a capture the turn kept changes
// nothing a relaunch decides, so it decides at once rather than hold the
// seat's frame for it; the turn's end, whose tombstone would refuse it, still
// waits for every keep in flight.
//
// WITHIN THE SAME WAIT, WHAT IS UNSETTLED IS SETTLED BEFORE THE DECISION: a
// capture whose taker is gone — its taker lease lapsed or held at another
// epoch ([Coordinator.settleGone], session.go) — is settled from the
// turn's row on the log, so a relaunch that follows its taker's crash or
// restart continues a capture whose record the log took before that read
// rather than starting fresh beside it; and a capture whose record was sent
// and never answered is asked again under its own operation, as the turn's end
// asks it ([Coordinator.settleUnresolved]): one that lands is continued. Each
// settle the wait decides is written in its own room past the wait, as this
// node's own write ([Coordinator.awaitCapture]); an ask that leaves the record
// unanswered writes nothing. And once the wait has run out, a capture of the
// job it follows still begun under a taker whose lease HOLDS, or that the
// store cannot say of, however long ago it began, is read off the turn's row
// once more, in a read held to as long again as the wait, and settled landed
// where the row names its place, never refused: a host whose store answers its
// leases and not its runs keeps its lease while no stamp of its own reaches
// the row, for as long as that silence lasts, and the relaunch that follows a
// job lost with that host continues the capture the log took rather than
// start fresh beside it. A record the log takes only after the settle's read —
// one its taker had handed on before its lease's window closed — lands beside
// a stamp that says refused (above, "WHAT THE FENCE CANNOT REACH"): the
// log keeps it, and the relaunch starts without it, told with the row in hand
// as a capture the log keeps, and its run named the turn's own as a session
// it may read ([namedThoughRefused], [ContinuedSession.KeptTurn]).
//
// SO A READER IS TOLD NEITHER AS DECIDED: while a capture or a save under way
// is still being kept ([relaunchWaitsOn]), or its record awaits the log's
// answer, which the relaunch asks again ([captureAwaitsAnswer]), what the
// relaunch continues cannot be said, and a reader is told that rather than a
// no ([ReadContinuityOf]).
//
// # Never a session that ran out of context
//
// Another launch of it fails the same way, so its collection records it on the
// run's record ([PendingRun.ExhaustedSessions]) and refuses its capture, and
// each way a later launch could take it up refuses it: [ContinuityOf],
// [BaseContinuityOf] and [Coordinator.continueSession]. The turn's end retires
// it for later turns.
//
// # Never a failed launch
//
// An object the store no longer holds or whose seal does not open
// (executordomain.ErrUnreadable), a log that does not answer, a box whose
// harness cannot take the session up: each starts the run on a session of its
// own, handed the brief written for one — or, where the launch reuses the box
// the job before ran in, the session that box holds — and is logged with its
// reason once what the launch continues instead is decided, naming that
// (`executor_session_not_continued`). The session is fetched SEALED onto this
// node's disk and opened whole once before a byte of it reaches the box
// (executordomain.SessionStore.Fetch), so the decision is made before the
// brief is, never part way through writing the archive into the box.
//
// # A stopped run's read of its box
//
// [Coordinator.readStopped] decides, by stop.go:
//
// NOTHING TO READ, and the stop finishes with nothing charged, where nothing
// can be read: a job that never started — a launch stopped before its command
// was recorded — spent nothing in a box; a box found GONE ([ErrBoxGone]) has
// nothing left to read, and a stop waiting on it would never finish; and a box
// on this node's own host that fails to be read is reclaimed next by this very
// ending, which a stop must not hold up while the job in it still works.
//
// NOT YET, where the box's host gave no account of it ([ErrBoxHostSilent]).
// That is no failure of the box — only its host reclaims it, and only after it
// is back — so the ending is kept and this node asks again on the hand-back's
// spacing ([Coordinator.oweEnding]), as a collection hands its claim back
// uncounted for the same answer. Until either bound below holds, judged as each
// such ask returns ([Coordinator.hostGivenUp]); past one, the host is taken for
// gone and the run's spend is unknowable, which the log says once the stop's
// record of that lands.
//
//   - THE HOST SILENT FOR THE BOX TIMEOUT ([Manager.BoxTimeout]), as the fleet's
//     waiter waits before it judges a silent launch node gone, measured on ONE
//     clock: this node's, from the start of the first ask under this ending
//     that reached the broker and came back with nothing
//     ([Coordinator.hostSilence]). Never from the stop's decision, which the
//     deciding node's store handle dated by that node's clock
//     ([RecordedEnding.At]): a seat's next holder whose clock ran ahead of the
//     decider's by more than the box timeout would give the host up on its
//     very first ask and write the run's spend off, and one behind would wait
//     past the bound.
//     A host that answered busy ([ErrBoxHostBusy]) is there, and its silence
//     starts again; an ask this node could not make ([ErrBoxNotAsked]) is no
//     evidence about the host, and neither starts nor advances it. The
//     silence is kept on the stop's retry series, so the seat's next holder,
//     which has a series of its own, counts again from its own first silent
//     ask, as the fleet's waiter counts a launch node's absence again from its
//     own first sight once its duty moves; a frame with no series — a seat's
//     retirement, or an ask whose series was dropped while it ran because the
//     seat moved — counts the silence of its own ask, which is what it
//     measured.
//   - THE STOP THE BOX TIMEOUT AND TWICE [objstore.AssumedClockSkew] OLD by this
//     node's clock, on any ask that brought no account — silent, busy or not
//     asked. The decision's instant is another node's clock, and this is the
//     one reading of it that holds on any two clocks within the bound the
//     engine assumes: it never fires before the stop is the box timeout old in
//     real time, and it ends the wait across every seat move, restart and
//     retirement tick, none of which carries the silence over. A retirement
//     needs it most: each of its ticks asks for at most its own budget and
//     ReleaseSeat drops every series, so silence alone would never end there.
//
// NOT YET, TOO, where the coordination store did not answer a step the read
// cannot go on without ([ErrStoreSilent]): the host's read of the run's row,
// which licenses the read. The box and its host are up, so the ending is kept
// and asked again on the same spacing: ended uncharged at once for it, the stop
// would write off a spend the box still held. For how long turns on whose
// silence it is, which rowRead says: whether this attempt's own read of the
// row, off this node's store, was answered ([Coordinator.finishEnding]).
//   - Answered, the silence is the host's alone (a partition that cuts the host
//     off from the store's leader and not this node), and only the host can
//     read the box: a host that cannot is as good as away, so the stop waits
//     for it as for a silent host, until either bound above holds, its silence
//     counted from this node's first such ask, and is then ended with nothing
//     read. Unbounded, it would hold the seat's mail behind it for as long as the
//     partition lasted, while every write of the ending's would land.
//   - Unanswered, the silence is this node's too: nothing the ending writes
//     would land, so the stop is kept whatever either bound says, and never
//     said ended for it, and such an ask neither starts nor advances the
//     host's silence. An attempt reaches the read in such a silence only
//     where it began after the retry's own read of the row and its decision,
//     which come first and keep the ending as they fail
//     ([Coordinator.retryOwed]).
//
// The result is the run's own, as a collection reads it — floored, held to the
// record's bound — and marked as a run that did not finish: never a success,
// never a question, whatever it had written ([stoppedResult]).
//
// See docs/concepts/code-sandbox.md.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/words"
)

// log is the package's own component, bound for the backend-NEUTRAL runtime:
// the coordinator, the waiter, the manager, launch, setup, mcprender and otel.
//
// Each backend binds its own (localLog, e2bLog) so a line names the box it
// came from. This var used to live in local.go bound to "sandbox.local", which
// stamped every backend-neutral event — and every remote-box event — as the
// local backend, so filtering logs by component hid remote runs exactly where
// an operator would look for them. A file added here logs as plain "sandbox"
// unless it is backend-specific, which is the safe default: an over-general
// component is findable, a wrong one is not.
var log = logging.Get("sandbox")

// DefaultHome is where a box that says nothing else keeps its artefacts.
//
// Every run artefact — the result, the done marker, the ask signal, the
// findings — lives under <home>/.crewlet. It is a per-SANDBOX property rather
// than a constant, which it was for as long as a remote VM was the only
// backend: many local boxes share one filesystem, and a shared home would have
// every run reading its neighbour's done marker.
//
// It is [hostbox.ContainerHome], declared there beside [hostbox.InHome],
// which judges a container box's paths against it — for both local boxes and
// for config validation, all of which already import that leaf package: the
// remote templates' one account has it as its home, and a container box
// mounts its own there so a path reads the same in both.
const DefaultHome = hostbox.ContainerHome

// ExecResult is one shell command's outcome inside a box.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Limits are the caps handed to a harness.
//
// Zero means UNSET for each field, and the runner falls back to the agent's own
// default. Note what is not here: a run deadline. The engine imposes no
// wall-clock limit on a launched job — it runs as long as it needs, and
// completion is detected by tracking the job rather than by a clock. There WAS
// a TimeoutSec here, read by nobody, contradicting the paragraph above it.
//
// MaxTurns comes from the resolved [Spec] — providers.sandbox.default_max_turns
// overlaid with role.sandbox.max_turns — and is zero, uncapped, unless the
// company or the seat sets a cap: a job is deliberately never stopped on a
// clock, so this is the one bound on its rounds, and it is the operator's to
// choose.
//
// NO DOLLAR CAP. The fleet's own token meter already post-charges a collected
// run against the seat's budget, and a second cap denominated in the
// operator's dollars would fight it: two ceilings on one spend, disagreeing,
// with the harness's the one that silently wins. A MaxBudgetUSD field stood
// here, set by nobody while the runner turned it into a flag, and went the way
// TimeoutSec did.
type Limits struct {
	MaxTurns int
}

// Spec is what it takes to mint and drive one box.
//
// AN EXTERNAL HOST'S REPOSITORY IS NOT HERE. Which repository on GitHub or
// GitLab to work in is task context the executor puts in the brief, and the
// harness clones it with the token the config injects — a spec field for it
// would make one repository per role a structural fact when it is a per-turn
// one. A repository on Gitlet, the engine's own git host, is here ([Spec.Repos]),
// because naming one is a GRANT: the run row carries it and the box's
// credential is judged by it, which a brief cannot be.
//
// There are deliberately NO CPU, MEMORY OR DISK FIELDS, and no TEMPLATE either.
// A remote box's resources are a property of its template, fixed when the
// template is built, and the create APIs accept no resource arguments; a local
// box is sized by providers.sandbox.local.run_args. Both are configured on the
// BACKEND, once, so a spec field for either would be a second answer to a
// question the catalogue has already settled — and the last one, Template, was
// set by nobody while looking exactly like a wired knob.
type Spec struct {
	// Placement is WHICH configured backend runs this box. Resolved from
	// the run_in of the providers.llm entry the launch picked, over
	// providers.sandbox.default_run_in, and carried here rather than chosen
	// by the caller so the launch, the pending row and the reconnect all
	// name the same cell.
	Placement Placement

	// Harness names the runner: "claude-code", "opencode", …
	Harness string

	// TurnID is the turn whose run the box is made for. A box on the
	// engine's host records it in its own directory, which is how that
	// host's waiter tells a box no run names any more from one a run is
	// using, and reclaims it ([HostBox]); a remote box has no use for it.
	// Set by [Coordinator.Launch], never by a caller.
	TurnID string

	// TimeoutSec is NOT a run deadline: it is the box's initial TTL, which
	// the launch refreshes while it provisions the box and the waiter every
	// tick once the job runs, so neither a long provisioning nor a running
	// job is ever killed by the clock. It is the ORPHAN-RECLAIM GRACE — how
	// long a box outlives an engine that has stopped heart-beating.
	TimeoutSec float64

	// PauseTTLSec bounds a PAUSED box: how long its snapshot is held before
	// the reaper reclaims it. 0 means never pause — always re-seed from the
	// pushed branch instead.
	PauseTTLSec float64

	// MaxTurns caps the agentic rounds a launch may take. 0 is
	// uncapped, and is the default.
	//
	// Resolved here rather than passed alongside, because it is settled the
	// same way TimeoutSec and PauseTTLSec are — a provider default with a
	// per-seat override — and a second resolution path for the same shape
	// of knob is how the two come to disagree. [Launch] turns it into the
	// runner's [Limits].
	MaxTurns int

	// Env is the run environment: the LLM entry's env and sign-in, the
	// generic agent identity (CREWLET_AGENT_*), the setup steps' env, and
	// role.sandbox.env — which is where an operator DECLARES an external
	// token. The engine never names a tool-specific variable itself, but for
	// Gitlet, whose access it gives every launch on a
	// node that serves it ([BoxGit], ADR-0093) — and that through the
	// launch's own environment, never this field. A launch hands the box a
	// COPY with what it minted for the job added ([LaunchRequest.RunEnv]):
	// what the box is made with and every setup step runs with, the run's
	// telemetry added at the job's start.
	Env map[string]string

	// CredentialFiles is a subscription-harness login: a path relative to the
	// box home mapped to an absolute path on the ENGINE HOST.
	//
	// Each provider decides what to do with it, and they decide differently
	// on purpose. A local box seeds the files in and writes one its run
	// refreshed back — only over the very login it was seeded from, so an
	// idle box or a stale refresh never reverts a newer one; a REMOTE one
	// ignores them, because they carry a refresh token
	// whose rotation is shared fleet state, and pushing that onto somebody
	// else's VM is a materially larger trust step than the scoped headless
	// token the run env already exports.
	CredentialFiles map[string]string

	// Repos are the repositories on Gitlet this run is
	// given, each resolved at launch ([RunRepo]), at most [MaxRunRepos]. Empty
	// for a run that clones nothing from Gitlet — a repository on
	// another host is still named in the brief and reached with the
	// credentials the seat's configuration declares. [Coordinator.Launch]
	// writes them on the run's row ([PendingRun.Repos]), which is what the
	// box's credential is judged against for as long as the job runs.
	Repos []RunRepo
}

// MaxRunRepos is the most repositories one executor is given: each costs a
// clone before the harness starts, and a run that needs more is a task
// that should be split.
//
// THE CLONES ADD UP. They run one after another, after the operator's steps
// and the box's git check, each to its own budget ([DefaultSetupTimeout]), so
// eight are up to eighty minutes on top of the rest of the box's provisioning
// — five times the box's TTL ([DefaultBoxTimeout]), which is why the launch
// keeps the box's timer refreshed for as long as it provisions it
// ([keepProvisioning]) rather than leaving the sum to the timer the box was
// made with. What a run waits for before its harness starts is that sum.
//
// ONE AFTER ANOTHER, never at once, which would cut that wait to the slowest
// clone's: a clone's index-pack keeps an entry for every commit and tree it
// receives while it writes their index — 1.37 million of them for a gigabyte
// of history (internal/gitlet/host's BenchmarkBoxClone) — and eight at once
// would share a box its template sized for one harness. In turn, the
// clones cost a run with many repositories time; at once, they would cost
// its box memory.
//
// NEVER PAST gitlet.MaxRunRepos, the most a read of a run's pushes names: a run
// given more would have every collected result's "what it pushed" refused,
// seen only as the sentence that its push record was unread. internal/engine's
// TestARunGivenItsMostRepositoriesHasItsPushesReadWhole holds the two to each
// other.
const MaxRunRepos = 8

// MaxConePaths is the most directories one repository's cone names
// ([RunRepo.Paths]). A cone is a few top-level directories of a monorepo:
// sixty-four is past any build's declared reach, and it is one of the two
// figures that keep the clone step one argument a shell can be given (see
// [MaxConePathBytes]).
const MaxConePaths = 64

// MaxConePathBytes is the longest directory a cone names: 255 bytes, the bound
// a ref has (gitref.MaxRefNameBytes) and a single file name has on every
// filesystem git runs on (NAME_MAX). A cone names a directory a few
// components below a repository's root, which a person or a model types out;
// none comes near it.
//
// THE BOUND KEEPS THE CLONE STEP WHOLE. A box runs each setup command as ONE
// argument of a shell (`sh -c`, `bash -l -c` through E2B's envd), and Linux
// refuses an argument past 128 KiB (MAX_ARG_STRLEN) before the shell starts,
// with a failure that names nothing the run asked for. [MaxConePaths] of
// these, quoted at their worst — a path of nothing but single quotes is four
// times its length once quoted ([ShellQuote]), and each takes its two quotes
// and a space — come to at most 64 × 1 023 = 65 472 bytes of the step, and the
// rest of it (the names and the commands) to a few KiB more;
// TestTheWorstCloneStepIsOneShellArgument builds that step and holds it under
// the limit. It bounds what a run's grants add to its row
// too, which every node's completion poll reads whole: at most [MaxRunRepos] ×
// 64 × 255 bytes of paths, about 128 KiB before the JSON on the row escapes
// them.
const MaxConePathBytes = 255

// RepoAccess is what a run may do with a repository it was given. A named
// string with Valid, so a newer build's value off the row is a value rather
// than a panic.
type RepoAccess string

const (
	// RepoRead is fetch and clone.
	RepoRead RepoAccess = "read"
	// RepoWrite is those and push — whatever the seat's own grant allows,
	// which every push is decided against again.
	RepoWrite RepoAccess = "write"
)

// RepoAccesses is every [RepoAccess] this build knows.
func RepoAccesses() []RepoAccess { return []RepoAccess{RepoRead, RepoWrite} }

// Valid reports whether a is one of [RepoAccesses].
func (a RepoAccess) Valid() bool { return slices.Contains(RepoAccesses(), a) }

// RunRepo is one repository a box is given, RESOLVED WHEN THE BOX IS: the
// repository's id beside the name it had then, so a rename meanwhile cannot
// move the grant; the commit the box starts from; the branch an executor
// works on; and the cone it checks out.
//
// The json tags are a WIRE FORMAT: an executor's row carries these
// ([PendingRun.Repos]), and a node of another build reads them. A check's
// repository rides no row: its box is made, run and closed by one node.
type RunRepo struct {
	// Repo is the repository's name at launch: the directory the box is
	// given it in, and the name its URL carries.
	Repo string `json:"repo"`
	// RepoID is the repository's id, canonical: what the grant names.
	RepoID string `json:"repo_id"`
	// Base is the whole id of the commit the box starts from: an
	// executor's base, "" for an empty repository, or the one commit a check
	// measures.
	Base string `json:"base"`
	// Branch is the branch an executor works on, its ref
	// refs/heads/<branch>; "" for a check, which works on no branch.
	Branch string `json:"branch"`
	// Access is what the box may do with the repository: a check's box
	// reads it.
	Access RepoAccess `json:"access"`
	// Paths are the directories checked out (a sparse cone), at most
	// [MaxConePaths]; empty is the whole tree.
	Paths []string `json:"paths,omitempty"`
	// Change is the change an executor works on — the live change whose
	// source is Branch, which it starts from the head of — and nil for a run
	// on no change (runchange.go). An executor's alone.
	Change *RunChange `json:"change,omitempty"`
	// Open is the change an executor asked to open from Branch once it
	// finishes — at its collection, once a launch ([OpenChange]) — and nil
	// for none. An executor's alone, and only one given the repository to
	// write: a run that may push nothing delivers nothing to open one from.
	Open *OpenChange `json:"open_change,omitempty"`
}

// ValidateFor holds a RunRepo to the grammar of the box it is given to for
// purpose. EVERY PURPOSE: a repository name of either form
// (gitref.ValidRepoName), a canonical repository id, a base that is "" or a
// whole commit id, a valid access, and at most [MaxConePaths] cone paths, each
// a clean directory inside the repository ([ConePathProblem] says what that
// holds a path to). Then by purpose:
//
//   - [CloneCoding]: a branch name git and Gitlet both take
//     (gitref.ValidBranch), since the run works on one; a change it works on
//     named by its id and its number; and a change it asks to open given
//     write, never beside a change it works on — whose branch heads that
//     change already — and proposing its branch into another
//     ([OpenChange.problem]);
//   - [CloneSingleSHA], a CHECK's: a base — the commit it measures — no
//     branch, since a check is given one commit and works on none, and read
//     access, since its box's credential fetches and never pushes.
//
// A value a purpose does not read is REFUSED rather than ignored: a branch on
// a check's repository, or a change on one, is a caller that meant another
// purpose, and a box made from it would be told one thing and do
// another.
//
// ONE GRAMMAR for every reader: the launch that resolves a run's repositories
// refuses a request with it, and the clone step is written only for a
// RunRepo it passes ([CloneRecipe]), so nothing a box is told to run carries a
// value no RunRepo can.
func (r RunRepo) ValidateFor(purpose ClonePurpose) error {
	switch {
	case !purpose.Valid():
		return fmt.Errorf("sandbox: repository %s: %q is not a clone purpose (one of %s)", r.Repo, purpose,
			words.Or(ClonePurposes()))
	case !gitref.ValidRepoName(r.Repo):
		return fmt.Errorf("sandbox: %q is not a repository name", r.Repo)
	case !canonicalID(r.RepoID):
		return fmt.Errorf("sandbox: repository %s: its id %q is not a UUID in canonical form", r.Repo, r.RepoID)
	case r.Base != "" && (!gitref.ValidOID(r.Base) || r.Base == gitref.ZeroOID):
		return fmt.Errorf("sandbox: repository %s: its base %q is not a whole commit id", r.Repo, r.Base)
	case !r.Access.Valid():
		return fmt.Errorf("sandbox: repository %s: access %q is neither %q nor %q", r.Repo, r.Access,
			RepoRead, RepoWrite)
	case len(r.Paths) > MaxConePaths:
		return fmt.Errorf("sandbox: repository %s: a cone names at most %d directories, not %d", r.Repo,
			MaxConePaths, len(r.Paths))
	}
	for i, p := range r.Paths {
		if why := ConePathProblem(p); why != "" {
			return fmt.Errorf("sandbox: repository %s: paths[%d] %q %s", r.Repo, i, p, why)
		}
	}
	if purpose == CloneCoding {
		switch {
		case !gitref.ValidBranch(r.Branch):
			return fmt.Errorf("sandbox: repository %s: %q is not a branch name git accepts", r.Repo, r.Branch)
		case r.Change != nil && r.Change.problem() != "":
			return fmt.Errorf("sandbox: repository %s: the change it works on %s", r.Repo, r.Change.problem())
		case r.Open != nil && r.Change != nil:
			return fmt.Errorf("sandbox: repository %s: a run on change !%d asks to open another, and its branch "+
				"%s heads that one already", r.Repo, r.Change.Number, r.Branch)
		case r.Open != nil && r.Access != RepoWrite:
			return fmt.Errorf("sandbox: repository %s: a run given it to %s asks to open a change, and delivers "+
				"nothing to open one from", r.Repo, r.Access)
		case r.Open != nil && r.Open.problem(r.Branch) != "":
			return fmt.Errorf("sandbox: repository %s: the change it asks to open %s", r.Repo,
				r.Open.problem(r.Branch))
		}
		return nil
	}
	switch {
	case r.Change != nil || r.Open != nil:
		return fmt.Errorf("sandbox: repository %s: a check is given one commit and works on no change", r.Repo)
	case r.Base == "":
		return fmt.Errorf("sandbox: repository %s: a check is given one commit, and names none", r.Repo)
	case r.Branch != "":
		return fmt.Errorf("sandbox: repository %s: a check is given one commit and works on no branch, "+
			"and names %q", r.Repo, r.Branch)
	case r.Access != RepoRead:
		return fmt.Errorf("sandbox: repository %s: a check's box reads its repository, its credential "+
			"fetching and never pushing, and is given %q", r.Repo, r.Access)
	}
	return nil
}

// ConePathProblem is why p cannot be a directory of a repository's cone — ""
// when it can be one. A cone path is relative to the repository's root and
// already clean: no leading or trailing slash and no empty, "." or ".."
// component, so one directory has one spelling and none reaches outside the
// repository; valid UTF-8, because it crosses JSON on the run's row, which
// replaces an invalid byte, so two paths would arrive as one; no control
// character, since NUL, a newline and a carriage return each break the
// line-based file git keeps a cone in and none of the others is ever meant;
// no space at either end, which git trims from a cone's directory before it
// writes one (sparse-checkout's strbuf_trim), so " src" would check out src;
// and at most [MaxConePathBytes].
//
// EXPORTED FOR THE ONE OTHER PLACE A CONE IS WRITTEN: a repository's
// .crewlet/checks.yaml names the cone its check's box checks out
// (internal/gitlet's ParseChecks), and that cone becomes a box's clone step as a
// run's does, so a file is held to the rule the step is written under rather
// than to a second reading of it — one the box would refuse at setup, after
// the check had taken its box.
func ConePathProblem(p string) string {
	switch {
	case p == "":
		return "is empty"
	case len(p) > MaxConePathBytes:
		return fmt.Sprintf("is %d bytes, past the %d a cone's directory may have", len(p), MaxConePathBytes)
	case !utf8.ValidString(p):
		return "is not valid UTF-8"
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "holds a control character"
	case strings.HasPrefix(p, " ") || strings.HasSuffix(p, " "):
		return "begins or ends with a space, which git trims from a cone's directory, so it would check out " +
			"another one"
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../"):
		return "is not a clean path inside the repository: no leading or trailing slash, and no empty, " +
			"\".\" or \"..\" component"
	}
	return ""
}

// canonicalID reports whether s is a UUID other than the nil one, spelled as
// uuid.UUID.String spells it — the one spelling a row compares byte for byte.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// RunHandle points at a detached job.
//
// PERSISTED, which is the whole point: the command id and pid go into the
// pending-run row so a later turn — or a fresh engine after a restart — can
// reconnect to a job that is still running and collect its result.
type RunHandle struct {
	CommandID string
	PID       int

	// SessionID is the harness session the job was started in, named by the
	// runner BEFORE the harness starts — the one the job continues
	// ([RunRequest.Resume]), or the id it minted for a new one — so a job
	// killed before it reported anything still names its session. The job
	// itself says which it ran ([Result.SessionID], [Result.Continued]): one
	// asked to continue a session its box neither held nor could restore
	// starts a new one instead.
	SessionID string

	// FreshSessionID is that new one: the session of its own a job asked to
	// continue SessionID starts in its place, named by the runner before the
	// harness starts as SessionID is; empty for a job that continues none.
	// With which of the two its box said it started ([LaunchRecord.SessionStart]),
	// what names the session the job ran ([PendingRun.SessionRan]).
	FreshSessionID string
}

// Result is one launch's outcome.
type Result struct {
	Text    string
	Success bool

	// InputTokens is the run's WHOLE prompt count, its cached share
	// included, and CacheReadTokens / CacheWriteTokens are the share of it
	// the provider's prompt cache served and stored — the same convention
	// every engine provider reports on its completion, so a launch's
	// tokens fold into the spend rollup beside the phases the engine ran
	// itself. A breakdown, never an addition: the total is input + output.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64

	// SessionID is the harness session the run worked in, as its harness reported it
	// or, where it reported nothing — or nothing shaped as a harness's session
	// id — as the run's script recorded it before the harness started.
	SessionID string

	// Continued says the run continued the session it was asked to
	// ([RunRequest.Resume]): its box held that session, or restored it from
	// the archive carried into it ([RunRequest.Carried]), and the harness took it
	// up. False for a run that started a session of its own — one asked to,
	// and one whose script found the session neither in its box nor
	// restorable, which starts a new one under another id and says why
	// ([Result.FellBack]) rather than let its harness create an empty session
	// under the old id and answer the brief as if it remembered.
	Continued bool

	// FellBack says why a run asked to continue a session started a new one
	// in its place, in the words its script said it in on its error stream —
	// the session was not in its box and none was carried in, the box's harness
	// is older than the release that continues a carried one, the archive
	// carried in could not be restored — so whoever reads what the run did
	// learns why it began again rather than finding a new session id alone.
	// "" for a run that continued its session, and for one asked to start
	// one. ONE SENTENCE, redacted and cut to [MaxFellBackBytes] where the
	// runner reads it and again as the launch's record is published
	// ([BoundFellBack]): the line it is read from is a file in the box.
	FellBack string

	// HarnessVersion is the release of the harness that ran, as its `--version` gave
	// it ("2.1.310"), read by the run's script in the box; "" where it
	// recorded none this build can read, one longer than the executor log
	// records a release at included.
	HarnessVersion string

	// Captured is the run's session as its box packed it when the harness
	// exited, to be carried into another box ([SessionArchive]); nil where
	// the box holds none — a run killed before its harness exited, a harness that
	// wrote no session, a box without the tools to pack one, which the run's
	// error stream says.
	Captured *SessionArchive

	// HarnessCutOff says the run's harness did not exit cleanly: its exit
	// status was other than 0 — a signal's, its memory limit's, a crash's —
	// or its box recorded none. The run's script packs the session after the
	// harness exits whatever its status (Captured), so a session it packed
	// after a harness that was killed may end mid-record, its last call with
	// no result, as one the engine packs of a job still working does
	// ([SessionCapture.Interrupted]).
	HarnessCutOff bool

	// MemoryKills is how many processes the kernel killed for memory in the
	// job's memory domain while its harness ran, and MemoryBytes the domain's
	// ceiling, both 0 where it killed none or kept no count. OutOfMemory is a
	// run whose harness was one of them: it ended at its box's memory
	// ceiling (types.ExecutorErrorOutOfMemory). Read off the box, so a run
	// that forges it costs only its own conversation, which a person releases.
	MemoryKills int64
	MemoryBytes int64
	OutOfMemory bool

	// JobEnd is how the job was ended on request, as its script recorded it
	// ([JobEnder], internal/sandbox/executor's Paths.JobEnded): a harness told
	// to stop, one killed past its grace, or a request that came before the
	// harness started; "" for a job nobody asked to end, and for a word this
	// build does not know. A run ended so did not finish, whatever it wrote:
	// it never reads as a success, and its failure says first that it was
	// ended.
	JobEnd JobEnd

	// Kept is what the read that collected this result kept of the job's
	// session ([Coordinator.takeCapture]), made on the node whose host the
	// box is on and carried back with the result to whichever node collects,
	// for the launch's own record; nil where nothing was taken. Set by the
	// coordinator, never by a runner.
	Kept *SessionKept

	// HarnessEnd is how the job's harness ended, as the read that collected
	// this result found it ([LaunchRecord.HarnessEnd]): recorded on the job's
	// record by that read, on the node whose host the box is on, and carried
	// back with the result for the claim it collects for, which was read
	// before the record was written ([Coordinator.collectClaimed]). Set by the
	// coordinator, never by a runner; empty from a host of an older build,
	// which is never read as an own run.
	HarnessEnd HarnessEnd

	// LastRequest is what the newest request of the session the job's box ran
	// carried, as the run's own account measured it once the job was over
	// (internal/sandbox/executor: Claude Code's event stream, OpenCode's
	// session message list asked as the job ends): recorded on the job's record
	// with HarnessEnd, in that read's one write ([LaunchRecord.Context]), for a
	// relaunch that continues the copy the box holds, whose floor is that
	// context with its brief. Set by a runner's collection, beside its result;
	// zero where it measured none, and from a host of an older build, whose
	// collection reads none, which leaves the record the measure carried along
	// the copy's lineage.
	LastRequest RequestMeasure

	// ReportedWindow is the least context window the run's harness reported
	// holding a model to, over every model the process used (Claude Code's
	// result lines, `contextWindow` of each model they name), 0 where it
	// reported none: a run that printed no result, a release that writes no
	// per-run figures, another harness. It only ever lowers the window a
	// continuation of the copy is judged under ([ExecutorWindow.SendsWhole]),
	// so a figure a box forged adds telling and removes none. Set by a
	// runner's collection, beside its result.
	ReportedWindow int64

	// ResumedCount is what Claude Code counts the copy of the session the
	// job's box holds at before its next request, as the read that collected
	// this result found it in one read of the job's main thread
	// ([Coordinator.readMainThread]), with ReportedWindow beside it: recorded
	// on the job's record with HarnessEnd, in that read's one write
	// ([LaunchRecord.ResumedCount]), and carried back for the claim it
	// collects for. Set by the coordinator, never by a runner; nil where the
	// read asked nothing (a stop's, a loss's, another harness's job), and from
	// a host of an older build, which reads as a copy that may compact.
	ResumedCount *ResumedCount

	// Turns is how many model turns the run's harness says its own loop took —
	// each one a provider call the engine neither made nor saw — and zero
	// where the harness reported none. It is what the launch's own record counts
	// as its calls (tokens.LaunchCalls): a run whose harness said nothing counts
	// as one, the one call the engine knows it asked for.
	//
	// AS REPORTED, never estimated: Claude Code states it on its result
	// line (`num_turns`); OpenCode's stream does not carry a count a
	// reader can trust — 2.0 prints a step's `step_start` for some steps
	// of a run and not others — so its runs report none.
	Turns int

	// DeliveredRefs are the branches and pull or merge requests the run
	// reported delivering, in its report's order: the `Delivered:` lines it
	// wrote, or the pull-request URLs its prose holds where it named none.
	// They are DISPLAY and a reminder — the launch's own record and the run's
	// report its reviewer reads carry them — and nothing judges a turn on
	// them: whether a turn reached anybody is the engine's own record of the
	// calls it made ([github.com/crewlet/crewlet/internal/agent/turn]).
	//
	// A runner hands them on AS THE ACCOUNT WROTE THEM, byte for byte: the
	// coordinator holds them to what the record can carry, for every runner
	// alike — redacted, deduplicated, valid UTF-8 and within
	// [MaxDeliveredRefBytes] — and it can only judge the bytes it is given.
	DeliveredRefs []string

	// DeliveredRefsElided is how many more refs the run reported than its
	// record lists: the refs past [MaxDeliveredRefBytes], and any that is not
	// valid UTF-8, which the record's JSON would turn into another ref. Set
	// by the coordinator.
	DeliveredRefsElided int

	// Pushed are the refs the run moved on Gitlet, read
	// from the push rows Gitlet wrote under its launch ([RunPushes]):
	// each repository the run was given to push to in turn, its refs in the
	// order the run first pushed them, held with the run's own refs to
	// [MaxDeliveredRefBytes] ([boundPushed]). PushedElided is how many more
	// the push record names than Pushed lists. PushedUnread is why the
	// record could not be read — set, the run's pushes are UNKNOWN, and an
	// empty Pushed beside it never means "none".
	//
	// The ENGINE'S account of the run's delivery there, which the run's own
	// `Delivered:` lines are not: a box writes those as it likes. Set by the
	// coordinator, never by a runner — whatever a runner put here is
	// replaced at the collection, which reads the record itself.
	Pushed       []PushedRef
	PushedElided int
	PushedUnread string

	// Opened is what became of each change the run asked to open
	// ([RunRepo.Open]), in the order it was given its repositories: opened
	// at this collection, one its branch already headed, or why none was
	// ([OpenedChange]). Set by the coordinator from the engine's account
	// ([RunPushes.Changes]), as Pushed is, and never by a runner.
	Opened []OpenedChange

	Error string

	// ContextExhausted says the run ended because its conversation no longer
	// fits the model's context, as its harness named the cause: Claude Code's
	// terminal reason `prompt_too_long`, OpenCode's ContextOverflowError. It
	// is what tells such a run from every other failed one on its record
	// (types.ExecutorErrorContextExhausted), and no later launch continues the
	// session it ran ([ranOutOfContext]). Read only on a run that did not
	// succeed. The box's word: a run that forges it costs only its own
	// conversation's session, whose transcript it could spoil anyway.
	ContextExhausted bool

	// RequestRefused says the run ended because its provider refused the
	// request the harness made as it was built (an invalid tool definition, a
	// message the session carries that the provider no longer takes): what the
	// same session sends again on every launch. It names such a run on its
	// record (types.ExecutorErrorRequestRefused), and no later launch continues
	// the session it ran ([requestRefused]). Read only on a run that did not
	// succeed, and the box's word, as ContextExhausted is.
	RequestRefused bool

	// RoundCapped says the run ended on the round cap its launch handed it
	// before it finished (types.ExecutorErrorMaxTurns): Claude Code's
	// `error_max_turns`. Read only on a run that did not succeed.
	RoundCapped bool

	// Event is how the harness said its run ended IN FIELDS, beside the words of
	// Text and Error: its own terminal event, verbatim — Claude Code's
	// result line, whose `api_error_status` names a spent plan however the
	// sentence beside it is worded. It is what an executor's ending is
	// classified by first, as a call's is (harness.Provider.RunEnded), and
	// nothing else reads it. Set on a run that did not succeed, by a harness that
	// prints such an event; empty otherwise, and wherever the line is past
	// [MaxRunEventBytes] ([boundBox]).
	Event string

	// Transcript is the agent's activity log — what it said, each tool it
	// called and the first line of what it ran — rebuilt from its own
	// output, or its stderr where the harness streams nothing better. It is the
	// observability surface for an agent that emits no telemetry of its
	// own. A runner returns it WHOLE and redacted; the coordinator holds it
	// to the record's bound ([Coordinator.fitResult]) and redacts it again
	// at publish.
	Transcript string

	// TranscriptElidedLines and TranscriptElidedBytes are how much of the
	// transcript's middle the record's bound left out: whole lines, and
	// every byte not kept. Zero for a transcript kept whole. Set by the
	// coordinator, never by a runner.
	TranscriptElidedLines int
	TranscriptElidedBytes int

	// Condensed is what condensing this result's report, failure or
	// question to the record's bounds cost the seat's auxiliary model
	// ([Coordinator.fitResult]) — a failed rewrite included, since it was
	// still paid for. Set by the coordinator, never by a runner; the segment
	// that resumes from this collection pays it ([ResumeRequest.Engine]).
	Condensed AuxTokens
}

// usageFloored is r with every count of what the run spent made non-negative,
// and whether any had to be.
//
// A RUN REPORTS ITS OWN USAGE, parsed out of the harness's last line of
// output — and that line is printed inside a box the run's own agent can run
// any command in. A negative count there is a REFUND: the charge would
// subtract it from the seat's budget counter, and the launch's record from
// every rollup that sums it, so a run could buy itself headroom by printing a
// number. Nothing a run spends is negative, so a negative count is read as
// nothing spent, and the caller says so. Floored where every collected result
// enters the coordinator, so every backend's parser is covered by one rule.
func (r Result) usageFloored() (Result, bool) {
	floored := false
	floor := func(n *int) {
		if *n < 0 {
			*n, floored = 0, true
		}
	}
	floor(&r.InputTokens)
	floor(&r.OutputTokens)
	floor(&r.CacheReadTokens)
	floor(&r.CacheWriteTokens)
	floor(&r.Turns)
	if r.CostUSD < 0 {
		r.CostUSD, floored = 0, true
	}
	return r, floored
}

// Sandbox is one live, isolated execution environment.
type Sandbox interface {
	// ID is the provider's handle for this box, and what a reconnect needs.
	ID() string

	// Home is the absolute path this run's artefacts live under. A property
	// rather than a constant — see [DefaultHome].
	Home() string

	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

	// StartBackground launches a detached process and returns its handle
	// immediately. The job keeps running after the caller's turn ends and
	// writes its result to a file a later collect reads.
	StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error)

	// Job reports what is known of the background job whose handle
	// StartBackground returned: still running, ended, or lost with the
	// engine process that last saw it running ([JobState]).
	//
	// ASKED OF THE BOX, because only the backend knows what its handle
	// names. Inside a remote VM or a container the handle is a pid in the
	// box's own process namespace, where nothing but the box's own work
	// can hold it. On the engine host it is a host pid, which the kernel
	// hands to any process once the job is gone: a probe that took the pid
	// on trust would read a stranger as the job and hold a dead run open
	// until the stranger exits. And only a box on the engine host can lose
	// its job with the engine: a remote box answers running or ended. An
	// error means the box could not be asked, which is not proof of any
	// answer.
	Job(ctx context.Context, commandID string) (JobState, error)

	// EndJob asks the job commandID names to END: SIGUSR1 to script, ONE
	// process of it — the run's script, by the pid it recorded in the box
	// (internal/sandbox/executor's Paths.JobScript). The script ends its
	// harness and packs its session as its own exit does ([JobEnder],
	// endjob.go). SIGUSR1 and not SIGTERM, which is a teardown's to every
	// process of the job at once, server and exporter with it, where an end
	// needs them alive to pack through.
	//
	// [ErrNotJobProcess] where script is no living process of the job: on the
	// engine host, no member of the job's recorded process group, held to it
	// engine-side ([procgroup.Leader.SignalMember]), since the pid is the job's
	// word and the job runs as the engine's own user; in a container or a VM,
	// no process of that pid in the box's own namespace. A pid below 2 is
	// refused unsent everywhere: 1 is the namespace's init, which in a
	// container forwards a signal to the container's main process. Any other
	// error is a box that could not be asked.
	EndJob(ctx context.Context, commandID string, script int) error

	// KillJob ends every process of the job commandID names and KEEPS the box
	// and its files for the read that follows: SIGTERM, [JobStopGrace] for
	// them to go, SIGKILL, [JobStopGrace] for the kernel to take them
	// ([JobKillWait]). On the engine host that is the job's recorded group,
	// continued first as Close continues it. In a container it is the job's
	// group in its container — recorded on the engine host as the job
	// started, or, for a job an older build started, which recorded none, read
	// off the job's first process where an exec's own session leads it — never
	// every process the box's user may signal, which would take the
	// container's main process and stop the container the read still execs
	// its pack in. In an E2B VM it is every process of the box's user: envd
	// starts a job in envd's own process group, which no signal may reach,
	// and runs as root, out of reach of the user's; and no job runs in the box
	// after it — a stop's and a loss's ending reclaims it right after, and a
	// collection past its turn's cap pauses it for a turn that starts no round
	// past that cap.
	//
	// NEVER A PART OF THE JOB FOR THE WHOLE: a box that can name no process
	// group of the job's own signals nothing of it and answers [ErrNoJobGroup].
	// The job's first process alone is no kill of the job — the run's script
	// and its harness are that process's children, and go on without it — and
	// it is the process a box answers [Sandbox.Job] by, so a kill of it alone
	// had a job still working read as one that had ended.
	//
	// A job that is already over is nothing to kill. Any other error is a box
	// that could not be asked; whether what it killed is over is the job's
	// poll to say.
	KillJob(ctx context.Context, commandID string) error

	// WriteFile puts content at path whole, by REPLACING the file there: the
	// content is staged under a fresh name beside the path and renamed over
	// it, so a process in the box reading the file meanwhile — a git
	// credential helper reading the token file the completion poll keeps
	// current under a running job — finds the old file or the new one,
	// never an empty or part-written one. A write that fails removes what it
	// staged, or says it could not, where a remote box stopped answering.
	// The new file has the mode the backend writes every file with,
	// whatever the replaced one had.
	//
	// A backend that writes the box from the engine host replaces only a
	// regular file where it lies, or puts one where nothing is, and refuses
	// anything else at the path with [ErrNotRegularFile], leaving it as the
	// box made it: the rename writes through no link, and a named pipe is
	// never opened to be waited on. A remote box is a machine of its own, so
	// what is at the path there is replaced, a link included.
	WriteFile(ctx context.Context, path string, content []byte) error

	// WriteStream puts what content yields at path whole, exactly as
	// [Sandbox.WriteFile] puts a byte slice there — staged beside the path
	// and renamed over it, the same refusals on the engine host — for content
	// the engine must not hold in memory: a harness's session archive,
	// carried into a box from the object store, which grows with the session
	// it packs.
	//
	// READ ONCE, A PIECE AT A TIME, and only while ctx lasts: a write whose
	// caller gave up stops at the next piece rather than reading its source
	// to the end. A source that fails part way fails the write, and the file
	// at the path is the one that was there before — the staged one is
	// removed, never moved over it — so an archive cut short in transit is
	// never a file the box reads as whole. The caller keeps content: the
	// write neither closes it nor reads it once it has returned.
	WriteStream(ctx context.Context, path string, content io.Reader) error

	// ReadFile reads a file the engine means to read WHOLE — a report, a
	// question, a marker, a result line — and REFUSES one past
	// [MaxFileBytes] ([ErrFileTooLarge], as a [*FileTooLargeError] naming the
	// cap it refused at) rather than returning its first part, because a
	// clipped report reads as a finished one. Empty on missing: a poll for a
	// marker that is not written yet is not an error.
	//
	// NEVER FOR A MACHINE STREAM. A harness's event log and its stderr
	// grow with the run and have no size a whole read could honestly refuse
	// at — the engine keeps a bounded share of them in the end — so they
	// are read with [Sandbox.OpenFile] or [Sandbox.ReadTail] instead.
	//
	// A path that names something other than a regular file is refused with
	// [ErrNotRegularFile] by a backend that reads the box from the engine
	// host, for all three reads, because opening one there could block.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// OpenFile streams a file front to back, for a MACHINE STREAM decoded
	// once in bounded memory: a harness's event log, read at
	// collection a line at a time. Nothing is refused for its size, since
	// the reader holds one piece at a time and decides itself what to keep.
	// A missing file is a reader that yields nothing, as ReadFile's empty
	// answer is; the caller closes it.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, error)

	// ReadTail reads at most n bytes from the END of a file, with the
	// file's whole size — for a question about a stream's end: what is the
	// job doing now, how far has its stream grown, what did it say last
	// before it failed. A whole read answers each of those at the cost of
	// the whole stream, every poll, for as long as the run lasts. Empty on
	// missing, as the others are.
	ReadTail(ctx context.Context, path string, n int) (FileTail, error)

	// OpenTail streams at most the last n bytes of a file — from where its
	// end less n stood when it was opened — with the file's whole size then,
	// for a stream's END kept whole rather than held: a failed check's log,
	// the output's last bytes put into the object store as they arrive. Its
	// cost is the n bytes, never the file before them: a remote box sends
	// only the range, a box on the engine host seeks. OpenFile with the
	// earlier bytes read and dropped cost a check that wrote gigabytes their
	// whole transfer from the box to keep its last tens of MiB. A missing
	// file is a reader that yields nothing and a size of 0, as the others
	// answer; the caller closes it.
	OpenTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error)

	// SetTimeout resets the box's wall-clock TTL to seconds from now.
	//
	// THE KEEPALIVE. A provider reclaims a box that many seconds after the
	// TTL was last set, and the waiter calls this every poll tick — so a
	// running box is bounded only by how long the engine can go WITHOUT a
	// heartbeat, never by a fixed run deadline — as the launch does while it
	// provisions the box, on another goroutine than the one running the
	// box's setup steps, so a backend answers it beside a command in flight.
	// A provider with no settable TTL no-ops.
	SetTimeout(ctx context.Context, seconds float64) error

	// Pause snapshots the box for a later resume, holding a run blocked on
	// a clarification with its working tree as it left it, uncommitted work
	// included, and its harness's own copy of the conversation. Where the
	// turn kept the asking job's session (session.go), the relaunch continues
	// that in this box or any other (restore.go), so the pause holds only
	// the working tree; where it kept none — keeping switched off, the
	// capture refused — the paused box is the one copy of the conversation,
	// and a relaunch after it is reclaimed starts a session of its own. A
	// provider without snapshots no-ops, and the relaunch re-seeds its
	// working tree from the pushed branch instead.
	Pause(ctx context.Context) error

	Close(ctx context.Context) error
}

// FileTail is the end of a file, read by [Sandbox.ReadTail].
type FileTail struct {
	// Data is the file's last bytes — all of it when the file is no longer
	// than what was asked for. It begins wherever the byte count put it, so
	// a reader of lines drops a partial first one ([FileTail.Lines]).
	Data []byte

	// Size is the whole file's size as the read found it, so a caller can
	// say how much came before Data rather than present the end as the
	// whole.
	Size int64
}

// Whole reports whether Data is the entire file.
func (t FileTail) Whole() bool { return int64(len(t.Data)) >= t.Size }

// Before is how many bytes of the file came before Data and were not read.
func (t FileTail) Before() int64 { return max(t.Size-int64(len(t.Data)), 0) }

// Lines is Data without a first line the window began inside, and how many
// bytes that partial line held. A tail that is the whole file begins at a
// line, so nothing is dropped from it; one that began mid-file begins
// wherever the byte count landed, and the bytes before its first line break
// are the end of a line nobody can read whole from here.
func (t FileTail) Lines() ([]byte, int) {
	if t.Whole() {
		return t.Data, 0
	}
	i := bytes.IndexByte(t.Data, '\n')
	if i < 0 {
		return nil, len(t.Data)
	}
	return t.Data[i+1:], i + 1
}

// tailOf is the end of a file held in memory, in the shape every backend's
// [Sandbox.ReadTail] answers.
func tailOf(content []byte, n int) FileTail {
	n = max(n, 0)
	if len(content) <= n {
		return FileTail{Data: content, Size: int64(len(content))}
	}
	return FileTail{Data: content[len(content)-n:], Size: int64(len(content))}
}

// ExecOptions are the per-command knobs both exec shapes take.
type ExecOptions struct {
	Env        map[string]string
	Cwd        string
	TimeoutSec float64
}

// JobState is what is known of a detached job ([Sandbox.Job], [Runner.Poll]).
//
// THREE ANSWERS, because "not running" is two facts that a run is carried
// forward from differently. A job that ENDED finished, or died while an engine
// that had seen it running was still there to see it go: what it left is the
// round's result, a crash included, and it is collected. A job that was LOST
// went with the engine process that last saw it running, before any engine
// could see it end: a `direct` box's process tree is the engine host's, and
// the restart that replaces the engine — its container restarted, its systemd
// unit stopped with every process in its control group, its host rebooted —
// kills the job in the same instant, mid-step, with nothing written to say so.
// What it left is not a result, and its round is carried on rather than
// collected ([PendingRun.Lost]).
type JobState string

const (
	// JobRunning is a job still working — or hung, which nothing tells apart
	// from working without a clock the engine deliberately does not impose.
	JobRunning JobState = "running"
	// JobEnded is a job that is over and was seen to end: it wrote its done
	// marker, or its process went while an engine that had watched it run was
	// still there.
	JobEnded JobState = "ended"
	// JobLost is a job that is over and that nothing alive saw end: it went
	// with the engine process that last saw it running, or after it, before
	// the engine that replaced it could look.
	JobLost JobState = "lost"
)

// JobStates are the three.
var JobStates = []JobState{JobRunning, JobEnded, JobLost}

// Valid reports whether this build knows s.
func (s JobState) Valid() bool { return slices.Contains(JobStates, s) }

// Over reports whether the job is no longer running, whichever way it went.
func (s JobState) Over() bool { return s == JobEnded || s == JobLost }

// ErrBoxGone is a [Provider.Connect] that found the box DEFINITIVELY not
// there — reclaimed by its provider, reaped, its directory removed — as
// against one that merely could not be reached. The difference decides
// whether asking again can help: a collection that cannot reach a box is
// retried, and one whose box is gone is settled at once.
var ErrBoxGone = errors.New("sandbox: the box is gone")

// boxGone is a backend's own sentence saying a box no longer exists, which is
// also [ErrBoxGone] — the sentence is what a person reads, the sentinel what
// a caller acts on.
type boxGone struct{ err error }

func (g boxGone) Error() string   { return g.err.Error() }
func (g boxGone) Unwrap() []error { return []error{g.err, ErrBoxGone} }

// ErrBoxPaused is a [Provider.Attach] that found the box PAUSED where its
// backend cannot read a paused box without waking it — an E2B snapshot, which
// has no envd to answer, or a paused container, which takes no exec. The box
// is there; reading it would mean resuming it, which is the one thing the
// caller asked not to do.
var ErrBoxPaused = errors.New("sandbox: the box is paused")

// boxPaused is a backend's own sentence saying a box is paused, which is also
// [ErrBoxPaused].
type boxPaused struct{ err error }

func (p boxPaused) Error() string   { return p.err.Error() }
func (p boxPaused) Unwrap() []error { return []error{p.err, ErrBoxPaused} }

// Provider mints sandboxes. Configured under providers.sandbox and swapped
// wholesale on an apply, mirroring the LLM providers beside it.
type Provider interface {
	// Kind names the backend, for logs and the operator surface.
	Kind() string

	Create(ctx context.Context, spec Spec) (Sandbox, error)

	// Connect reattaches to an existing box by id, live or paused.
	//
	// The detached lifecycle rests on this: the completion turn — possibly
	// in a fresh engine after a restart — reattaches to the box that ran the
	// job, collects its result and tears it down. A PAUSED box auto-resumes
	// on connect, which is why the reaper must not use it, and why neither
	// may a reader ([Provider.Attach]).
	Connect(ctx context.Context, sandboxID string) (Sandbox, error)

	// Attach reattaches to an existing box by id WITHOUT RESUMING IT, for a
	// caller that only reads: a peek at a running job's output, and the
	// completion poll.
	//
	// THE READ MUST CHANGE NOTHING, and through Connect it could not: a
	// reader that took a running record, then reached the box after the
	// collection had read it and paused it, woke the box back up — and on
	// E2B, where nothing renews a box once its record stops running, the
	// woken box was killed at its TTL and the parked run lost the snapshot
	// it had been paused to keep. A box this backend cannot read while it
	// is paused is refused with [ErrBoxPaused]; one whose files the engine
	// host reads directly is handed back as it is, paused or not. A box
	// that is gone is [ErrBoxGone], as for Connect. Nothing about the box's
	// timer or its state moves.
	Attach(ctx context.Context, sandboxID string) (Sandbox, error)

	// Kill terminates a box by id WITHOUT resuming it.
	//
	// The primitive the pause reaper needs. Connect auto-resumes, so
	// reclaiming a paused snapshot through it would boot the VM back up
	// purely to kill it — paying for a resume to pay for a shutdown.
	// Best-effort: a box that is already gone is not an error.
	Kill(ctx context.Context, sandboxID string) error
}

// Runner runs one harness inside a box against a brief.
//
// TWO SHAPES, ONE RUNNER: the inline Run (start, block, result — for tests and
// short jobs) and the detached Start/Poll/Collect triple the engine drives
// across turns. Run is Start followed by Collect on the same handle.
type Runner interface {
	Name() string

	// Install puts the agent in the box. Separate from Create because a
	// reused box already has it, and re-installing on every turn is the
	// slowest thing in a coding turn's critical path.
	Install(ctx context.Context, box Sandbox) error

	Start(ctx context.Context, box Sandbox, req RunRequest) (RunHandle, error)

	// Poll reports what is known of the background job: running, ended —
	// finished, or seen to die — or lost with the engine that last saw it
	// running ([JobState]).
	Poll(ctx context.Context, box Sandbox, handle RunHandle) (JobState, error)

	// Collect reads the finished job's result out of the box.
	Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error)

	// Follow begins a LIVE READING of what a job says about itself, for a
	// person watching the run ([LiveReading]): advanced a read at a time,
	// each read adding what the job has written since, settled and
	// redacted, without ending, pausing or otherwise touching the job. Safe
	// on a job in any state: one that has finished says so, and one that
	// has said nothing yet reads as nothing rather than as an error.
	Follow(handle RunHandle) LiveReading
}

// CallIdentifier is a [Runner] whose harness hands each tool call it makes over
// the bridge an id of its own, in the request's `_meta`: CallIDMeta is the key
// it sends the id under, the one the bridge reads ([BridgeCall.CallID],
// [OpenedJob.CallIDMeta]) — `claudecode/toolUseId` for Claude Code — and ""
// for a harness that sends none.
//
// OPTIONAL, as [SessionTaker] is: a runner that declares no key — a harness
// whose `_meta` carries no id of the call, OpenCode's, or a test's — has every
// call of its runs recorded with none, each read by its place as every call an
// older build recorded is. EXPORTED so a runner asserts it implements it, as
// internal/sandbox/executor's does: a method renamed out from under it would
// read as a harness that declares nothing, and every id would be dropped
// without a word.
type CallIdentifier interface{ CallIDMeta() string }

// RunRequest is one launch's inputs.
//
// NEVER PERSISTED: a run is started once, by the node that launched it, and
// everything a later collection or resume needs is on the run's row and in the
// box. So a field added here changes no record a peer of another build reads.
type RunRequest struct {
	// System is the run's standing instructions, which the runner hands its
	// harness on the channel that ADDS to the harness's own system prompt rather than
	// replacing it: the vendor's prompt is what teaches the harness this run
	// exists to use. A runner adds its own standing instruction to it — the
	// report the run writes before it stops, at the box's path for it.
	System string

	// Brief is the task: the prompt the harness runs, and nothing standing.
	Brief string

	// Resume is the harness's own session the run CONTINUES, as an earlier run in
	// the same box reported it ([Result.SessionID]): the conversation, its
	// tool results and its working files are where that run left them, and
	// Brief is its next message — Claude Code's `--resume <id>`, OpenCode's
	// `--session <id>`. A seat's job sets it on every relaunch of its turn that
	// continues the turn's session — the turn's kept capture, in whatever box
	// the job gets ([LaunchRequest.Session]), or the session the asking job's
	// box holds ([LaunchRequest.Continue]); the assistant's deep work, where
	// its run parked on a proposal and the approval has come. Empty starts a
	// new session.
	//
	// THE HARNESS'S SESSION AND NOT A SUMMARY OF IT, because the run that asked
	// knew what it had read, tried and decided, and a fresh conversation
	// told only the question and the answer starts that work again. It holds
	// in the box the session lives in, or in a box the session is carried
	// into ([RunRequest.Carried]). A runner whose harness cannot continue a
	// session refuses a run that asks it to, rather than start a new
	// conversation its caller believes is the old one; and a run whose box
	// turns out to hold the session neither way starts a new one and says so
	// ([Result.Continued]).
	Resume string

	// Carried is the session Resume names, packed in another box
	// ([SessionArchive]), for this run's box to restore before its harness takes
	// it up: written into the box as the run starts, after the previous
	// run's artefacts are cleared, and restored by the run's own script — or
	// not, in a box whose own copy of the session already holds this archive
	// (one the box packed, or restored before), which is never written over:
	// that copy is the archive's or newer — unless the archive is one its
	// harness was cut off in ([CarriedSession.Replace]), which replaces the
	// copy whatever it holds. A box holding a copy that does not hold the
	// archive has it replaced by the archive. Nil for a run that
	// continues a session its box already holds, or none. Its session must
	// be Resume's, and its harness and format the runner's, or the run is
	// refused.
	Carried *CarriedSession

	// FreshBrief is the brief a run asked to continue a session is started
	// with INSTEAD of Brief where its script finds the session neither held
	// by its box nor restorable from Carried — an archive that does not
	// unpack, an import its harness refused, a release that cannot continue
	// it — and starts a new session in its place ([Result.Continued]): Brief
	// is then a continuation written for a model that remembers, which a new
	// session would answer as if it did. So the run carries both, and its
	// script hands its harness the one its restore's outcome chose. Empty
	// where Resume is: a run that starts a session is handed Brief.
	FreshBrief string

	Env    map[string]string
	Limits Limits

	// Model is the model the run works under, written in the grammar of the
	// harness the runner drives: the seat's providers.llm entry's model exactly
	// as its runs on the engine host hand it to that same harness, which is the
	// only harness the engine lets reach this runner. Empty leaves the agent's
	// own choice.
	//
	// NO ENDPOINT AND NO KEY: an entry talks to its harness's own vendor, and its
	// sign-in rides the run env, so nothing secret is written into a file
	// inside the box.
	Model string

	// Effort is the entry's reasoning effort, in its harness's own levels, which
	// the runner passes its harness its own way. Empty: the harness's default.
	Effort string

	// MCPServers are the MCP servers the engine hands the run, keyed by
	// server name: its tool bridge, and the seat's servers that run in its
	// box. SERVER-LEVEL ONLY — there is no per-tool allowlist, because the
	// agent inside the box negotiates its own tool list with the server and
	// an allowlist the engine could not enforce would be a claim rather than
	// a control.
	MCPServers map[string]MCPServer

	// OnlyMCPServers holds the run to MCPServers alone: no server its
	// harness's own configuration, a repository's or a plugin's declares,
	// and no connector of the account it signs in with, starts beside them
	// (Claude Code's --strict-mcp-config).
	//
	// The assistant's deep work sets it: it acts for a person, confined on
	// the engine host, and reaches exactly the tools it is handed. A seat's
	// executor leaves it false, and its harness starts what it would start
	// for a person running it — the box's own configuration's servers, the
	// repository's, a plugin's, the signed-in account's connectors — beside
	// the ones the engine hands it: what those reach is the run's cell's to
	// bound, never the engine's to decide. A runner whose harness has no way
	// to be held to the servers it is handed refuses a run that sets it.
	OnlyMCPServers bool

	// Steer is how the run's harness reaches a person's notes to its turn while
	// it works — see [SteerWiring]. The zero value takes no note inside the
	// run: its notes wait for whatever reads the turn next, which is what a
	// run with no tool bridge — the assistant's deep work — has. NEVER
	// PERSISTED: it names the job's own endpoint, and a resumed turn's next
	// launch is handed its own.
	Steer SteerWiring

	// ReportedRepos are the repositories on Gitlet this
	// run may push to ([Pushable]), by the names it was given them under.
	// What it pushes there the engine reads from the push record and reports
	// for it ([Result.Pushed]), so a runner tells its agent to write a
	// `Delivered:` line only for what it delivered somewhere else: a line
	// for a push the record already names is the same delivery said twice.
	ReportedRepos []string

	// Skills are skills the run's harness is handed as its OWN — see [Skill]:
	// written where the harness reads skills of its own, listed to its model
	// beside the ones the box and the checkout hold, and loaded with the
	// harness's own skill tool. Nil hands none. A runner refuses a run handed
	// skills that its harness cannot take, or with nowhere to report a load
	// ([RunRequest.SkillLoads]).
	Skills []Skill

	// SkillLoads is where the run's harness reports loading one of Skills —
	// see [SkillWiring]. NEVER PERSISTED, for the reason Steer is not: it
	// names the job's own endpoint.
	SkillLoads SkillWiring

	// FreshWiring is how the run's harness reaches the seat's tools where its
	// script starts a session of its own in place of the one Resume names:
	// MCPServers, Steer and SkillLoads as they are but under the job's OTHER
	// bridge token, the one the bridge reads as the box having started a
	// session of its own (internal/api/mcpbridge). The run's script installs
	// exactly one of the two once its restore has come to whatever it came
	// to, and removes the other from the box before its harness starts, so the
	// token its harness dials IS the box's word on which session it started —
	// one the run's model can neither forge nor take back, holding only the
	// token of the session it is in. Nil where Resume is empty, which a
	// runner refuses a run that sets it for, and where the run has no bridge
	// to tell (the assistant's deep work). NEVER PERSISTED, for the reason
	// Steer is not.
	FreshWiring *Wiring

	// Attribution is the credit every commit the run authors as itself
	// carries — see [CommitAttribution]. Nil credits nothing: the run's git
	// is left exactly as the box and the repository configure it.
	Attribution *CommitAttribution
}

// SkillNamespace is what a run's harness lists every skill of
// [RunRequest.Skills] under: `crewlet:<name>` ([Skill.Listed]).
//
// ONE NAME ON EVERY HARNESS. Claude Code names a plugin's skill
// `<plugin>:<skill>`, and the plugin a runner hands it its skills in is named
// this; OpenCode names a skill by its directory, and a runner names each
// directory in full. So the engine can tell a run's model which skill to load
// without knowing which harness runs it, and a load the harness reports is
// read back by the name it listed. And NOT A NAME A REPOSITORY'S OR AN IMAGE'S
// OWN SKILL TAKES by accident, which, sharing one, the harness would run in
// the company's place or the company's in its.
const SkillNamespace = "crewlet"

// Skill is one skill a run's harness is handed as its own.
type Skill struct {
	// Name is the skill's own name ([ValidSkillName]), which the harness
	// lists it under in [SkillNamespace] ([Skill.Listed]).
	Name string

	// Description is what the harness lists beside the name, for its model
	// to choose by: the harness hands a model a skill's body only when the
	// model loads it.
	Description string

	// Body is the skill's text, which the harness hands its model once the
	// model loads the skill — AS THE HARNESS READS A SKILL OF ITS OWN, since
	// that is what it is: Claude Code reads a skill's file as a template, in
	// which a line's `` !`command` `` runs as the skill loads and
	// `$ARGUMENTS` is a placeholder, where OpenCode hands its model the text
	// as written.
	Body string

	// Required says the run's calls to the tools this skill covers are
	// refused until the harness reports loading it. A runner refuses a run
	// with a required skill on a harness release that cannot report a load:
	// it could never call those tools.
	Required bool
}

// Listed is the name the run's harness lists the skill under, and its model
// loads it by.
func (s Skill) Listed() string { return SkillNamespace + ":" + s.Name }

// maxSkillName is the longest [Skill.Name]: the Agent Skills specification's
// bound on a skill's name, which both harnesses read a skill's name by.
const maxSkillName = 64

// ValidSkillName reports whether name can be a [Skill.Name]: one to
// [maxSkillName] lowercase ASCII letters and digits, in runs joined by single
// hyphens — the Agent Skills specification's grammar. It is a directory's
// name in the box on either harness, and part of what the harness lists; a
// name outside it is one a harness may refuse to load, or one that names a
// path rather than a skill.
func ValidSkillName(name string) bool {
	if name == "" || len(name) > maxSkillName || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && name[i-1] != '-':
		default:
			return false
		}
	}
	return true
}

// SkillWiring is where a run's harness reports loading one of its skills
// ([RunRequest.Skills]): one tool of the MCP server the run's tool bridge is
// ([RunRequest.MCPServers]), which a hook of the harness's calls once its skill
// tool has answered (Claude Code), and the URL of the bridge's skill route,
// which a plugin of the harness's own posts each load to (OpenCode). The
// engine fills it from the same names its bridge serves, so the box and the
// bridge cannot disagree about them (internal/api/mcpbridge).
type SkillWiring struct {
	// Server is the name the bridge is listed under in the run's MCP
	// configuration, and Tool the tool a hook calls on it — never listed,
	// so no model is offered it, and no model can unlock a skill it never
	// read.
	Server string
	Tool   string

	// URL is the job's skill route: the bridge endpoint the box already
	// dials, with the route's suffix.
	URL string
}

// Set reports whether the wiring names anything a harness could reach.
func (w SkillWiring) Set() bool { return w.Server != "" && w.Tool != "" && w.URL != "" }

// CommitAttribution asks a run's runner to credit Crewlet on every commit the
// run authors as itself: `Co-authored-by: Crewlet <noreply@crewlet.ai>`, the
// one trailer GitHub renders as a participant in the commit; where Seat is
// set, the seat that made the commit and the work item its turn is charged
// to; and, where Run is set, the run that wrote it, which leads a reader of
// the history back to the run's page.
//
// GIT ADDS IT, NOT THE MODEL. The runner installs a hook in the box that git
// runs on every commit the run makes (internal/sandbox/executor's
// attribution.go), so the credit is there whether or not the agent remembered
// an instruction — and only on a commit whose author is the identity the run
// commits as, so a person's commit the run amends, cherry-picks or rebases is
// never credited to Crewlet.
//
// A CLAIM, NEVER EVIDENCE: the box can delete the hook or write any trailer
// it likes, so nothing in the engine reads a pushed commit's trailers — a box
// push's provenance is stamped from the run's own row at its admission. The
// trailers carry that provenance where the row does not reach: a mirror of
// the repository, and a commit older than Gitlet's push horizon.
type CommitAttribution struct {
	// Run is what the run's commits name it by, the identifier its page is
	// addressed by: a seat's job is its turn's, so every job of one turn —
	// a relaunch included — names the one run. Empty names no run — a run
	// with no page of its own — and the commit names none. One line of
	// printable text, since it becomes a trailer's value.
	Run string

	// Seat is the handle of the seat the run is the executor of, and
	// WorkItem the item its turn is charged to (nil for none, which the
	// commit says as "none" rather than leaving out, since a rewritten
	// commit's trailer is replaced, never kept). Empty for a run that is no
	// seat's — deep work for a person — which names neither; a WorkItem
	// without a Seat, or one with no backend, is the engine's own bug and is
	// refused.
	Seat     string
	WorkItem *types.WorkItem
}

// Wiring is how a run's harness reaches the seat's tools and the engine's: its
// MCP servers, the tool bridge among them, where it asks for its turn's notes,
// and where it reports a skill it loaded — the three parts of a run's request
// that name the job's bridge endpoint ([RunRequest.FreshWiring]).
type Wiring struct {
	MCPServers map[string]MCPServer
	Steer      SteerWiring
	SkillLoads SkillWiring
}

// SteerWiring is where a run's harness asks for its turn's notes: one tool of the
// MCP server the run's tool bridge is ([RunRequest.MCPServers]), which a hook
// of the harness's calls between its steps, and the URL of the bridge's notes
// route, which a harness that admits notes from a plugin of its own polls. The
// engine fills it from the same names its bridge serves, so the box and the
// bridge cannot disagree about them (internal/api/mcpbridge).
type SteerWiring struct {
	// Server is the name the bridge is listed under in the run's MCP
	// configuration, and Tool the tool a hook calls on it — never listed,
	// so no model is offered it.
	Server string
	Tool   string

	// URL is the job's notes route: the bridge endpoint the box already
	// dials, with the route's suffix.
	URL string
}

// Set reports whether the wiring names anything a harness could reach.
func (w SteerWiring) Set() bool { return w.Server != "" && w.Tool != "" && w.URL != "" }

// Run is the inline shape: start, wait, collect.
//
// A helper rather than an interface method, so a Runner implements three
// primitives instead of four and the two shapes cannot drift apart — the
// blocking one IS the detached one with a wait in the middle.
func Run(ctx context.Context, r Runner, box Sandbox, req RunRequest, wait WaitFunc) (Result, error) {
	handle, err := r.Start(ctx, box, req)
	if err != nil {
		return Result{}, err
	}
	if err := wait(ctx, func(ctx context.Context) (bool, error) {
		state, err := r.Poll(ctx, box, handle)
		return state.Over(), err
	}); err != nil {
		return Result{}, err
	}
	return r.Collect(ctx, box, handle)
}

// WaitFunc blocks until a predicate holds or the context ends. Injected so the
// inline shape's polling cadence is the caller's choice — a test wants none.
type WaitFunc func(ctx context.Context, done func(context.Context) (bool, error)) error

// probeByKill answers whether a job is running for a box whose handle is a pid
// in the box's own process namespace, by running `kill -0` inside it.
//
// The handle is parsed rather than interpolated: it is read back from the run's
// record in the coordination store, and a shell command is the wrong place to
// find out it was not a number.
func probeByKill(ctx context.Context, box Sandbox, commandID string) (bool, error) {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("sandbox: job handle %q is not a process id", commandID)
	}
	res, err := box.Exec(ctx, "kill -0 "+strconv.Itoa(pid)+" 2>/dev/null", ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// ErrNotJobProcess is an end request ([Sandbox.EndJob]) refused unsent: the pid
// it names is no living process of the job.
var ErrNotJobProcess = errors.New("sandbox: the pid is no living process of the job")

// ErrNoJobGroup is a kill ([Sandbox.KillJob]) that signalled nothing, because
// its box can name no process group that is the job's own: a container job
// whose start recorded none, and whose first process is gone or in a group no
// exec's session of its own leads. What is left of such a job runs on until
// its box is reclaimed, and whether it is over is its poll's to say.
var ErrNoJobGroup = errors.New("sandbox: the box names no process group of the job's own, " +
	"so nothing of it was signalled")

// The words [endScript] answers in, one for each of its two outcomes, so an
// exec that never ran the script — a container runtime that is down, a box
// whose agent dropped the stream — is told from a pid that is not there.
const (
	endSent = "crewlet-end-sent"
	endGone = "crewlet-end-gone"
)

// endScript is the shell line that sends pid the end request (SIGUSR1) in a box
// whose pids are its own namespace's, and says which way it went.
//
// THE `-SIG` SPELLING, `kill -USR1 <pid>`, which dash, bash and busybox ash
// all read the same way (end-job-scratch/exp-kill.sh).
func endScript(pid int) string {
	return "if kill -USR1 " + strconv.Itoa(pid) + " 2> /dev/null; then echo " + endSent +
		"; else echo " + endGone + "; fi"
}

// endByKill answers [Sandbox.EndJob] for a box whose pids are its own
// namespace's — a container and an E2B VM: script is signalled in the box,
// where nothing but the box's own processes can hold its pid, so a pid that
// names nothing there is [ErrNotJobProcess]. A pid below 2 is refused before
// the box is asked. Run from the box's home, a directory the job cannot take
// away as it can its checkout, which an exec's working directory must exist in.
func endByKill(ctx context.Context, box Sandbox, script int) error {
	if script < 2 {
		return fmt.Errorf("%w: pid %d is no process an end request is sent to", ErrNotJobProcess, script)
	}
	res, err := box.Exec(ctx, endScript(script), ExecOptions{Cwd: box.Home()})
	if err != nil {
		return fmt.Errorf("sandbox: asking pid %d in box %s to end its job: %w", script, box.ID(), err)
	}
	switch lastLine(res.Stdout) {
	case endSent:
		return nil
	case endGone:
		return fmt.Errorf("%w: box %s has no pid %d to signal", ErrNotJobProcess, box.ID(), script)
	}
	return fmt.Errorf("sandbox: box %s did not run its end request (exit %d): %s", box.ID(),
		res.ExitCode, strings.TrimSpace(res.Stderr))
}

// lastLine is the last non-empty line of out, trimmed.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// killScript is the shell script that ends every process target names — a
// pid, or `-<pgid>` for a process group — in the shape [Sandbox.KillJob]
// promises: SIGTERM, up to [JobStopGrace] for them to go, SIGKILL, and up to
// [JobStopGrace] more for the kernel to take them, each wait over as soon as
// `kill -0` finds nothing of target left. A target with nothing left to
// signal is done at once.
//
// THE `-SIG` SPELLING, which reads a negative operand as a pid in dash, bash
// and busybox ash alike; dash takes `kill -s TERM -<pgid>` for an option it
// does not know, and wants a `--` before the pid that busybox ash need not
// read the same way (end-job-scratch/exp-kill.sh).
func killScript(target string) string {
	return killAndAwait(target, "kill -0 "+target+" 2> /dev/null")
}

// killAndAwait is a kill in [killScript]'s shape whose waits last while the
// shell condition alive holds — what is left of target — and no longer than
// a grace each. Its output goes nowhere: a box's agent that stops listening
// when its own process ends would otherwise end the script on its next write.
func killAndAwait(target, alive string) string {
	await := fmt.Sprintf(`i=0; while %s && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); done`,
		alive, int(JobStopGrace/(100*time.Millisecond)))
	return fmt.Sprintf("exec > /dev/null 2>&1\nkill -TERM %s || exit 0\n%s\nkill -KILL %s\n%s\n",
		target, await, target, await)
}

// remoteJob answers [Sandbox.Job] for a box whose job runs in a machine of its
// own, which the engine's restart does not reach — an E2B VM, and the fake that
// stands in for one: running while `kill -0` there says so ([probeByKill]),
// and ended otherwise. Such a job is never lost with the engine: it outlives
// every engine process, and whichever one finds it gone found it gone.
func remoteJob(ctx context.Context, box Sandbox, commandID string) (JobState, error) {
	running, err := probeByKill(ctx, box, commandID)
	switch {
	case err != nil:
		return "", err
	case running:
		return JobRunning, nil
	}
	return JobEnded, nil
}
