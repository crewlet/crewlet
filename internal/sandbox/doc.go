// Package sandbox is the executor runtime: every agent seat's executor is its
// harness's own run, inside a box placed in one of the catalogue's cells. See
// docs/concepts/code-sandbox.md.
//
// A run is DETACHED: the launch starts it, the turn suspends, and the engine
// resumes the same turn with the run's report when it completes, possibly
// after a restart or on another node. Nothing parks a goroutine on a job: a
// goroutine does not survive a restart, and a harness outlives a deploy.
//
// A run that asks a person something parks, and the seat goes free. The answer
// is matched by the CONVERSATION it was asked in ([ConversationRef.Answers]),
// never the inbox batch, which a reply often lands in a finer partition of: the
// first reply posted after [PendingRun.AskedAt]. It is RECORDED on the row
// before anything is done with it ("Owed answers"), and reaches the seat once:
// TAKEN by a resumed turn ([RecordedAnswer.TakenAt]) or LET GO back through the
// row's outbox ([PendingRun.HandBack]). An ending is DECIDED ONCE, on the row,
// before any of it is done ([RecordedEnding]), and announced once, before the
// delete ([Coordinator.announceEnding]). Every claim stamps its claimant's lease
// on the row, and a take under an older lease or none is refused
// ([PendingStore.ClaimForResume]). A reply's delivery is recorded worked as the
// reply leaves the run ([CoordinatorOptions.Spent]). The answer is sealed
// (ADR-0031). A reply too large to be the answer is the question's refusal
// (ADR-0114). A tool call that needs approval parks its run as an ask does
// (ADR-0077, heldcall.go).
//
// Every subject a run is reached on is named by its row's seat ID (ADR-0013);
// the handle is this node's bookkeeping key, never an address. A box is
// touched only on its launching node's host (ADR-0065, boxhost.go). What a run
// pushed to Gitlet is the push record's account ([RunPushes]); an unreadable
// record is said unknown, never left out, since silence reads as "nothing
// pushed".
//
// AND A HOOK'S NOTE IS READ ONLY ONCE ITS JOB'S SESSION SHOWS IT: a hook only
// SERVES it ([PendingStore.ServeHook]). Every read of a job's box at its end
// asks the job's main thread which serves it took and settles them BEFORE the
// take ([PendingStore.SettleHook]), so a capture's cursor counts only what was
// confirmed; a verdict the store does not answer is [ErrStoreSilent].
//
// An OpenCode, Gemini CLI or Qwen Code continuation's first request is judged
// in its box, whose own settings can move the point: the held telling only
// where the box's point is read and the count sits below it
// ([BoxPoint.SendsWhole]), else the summarized one. A session a run starts of
// its own keeps its task as a [LeadKindFresh] lead. The harness's
// estimate of a session's next request ([NextRequest]) is never carried along
// a lineage nor put on the executor log: a copy grown by its brief would be
// understated.
//
// # A launch's harness session is kept, and a turn's end recorded
//
// By session.go (ADR-0073). A session is taken wherever its box can still be
// read, on the box's host: a collection, a stop (which ENDS the job first,
// [Coordinator.endJob]), a loss whose box is left; so a capture is final
// wherever its job ended. The TAKE (scrub, seal, spool) runs on the read's
// frame within [SessionTakeBudget]; the KEEP (upload, record) runs off it
// within [SessionKeepBudget], so no waiter holds for an upload. Each capture's
// place is reserved on the run's row before a byte is spooled
// ([PendingStore.StampCapture]) with its save point and note cursor, and its
// outcome stamped: landed, refused, or unresolved (asked again).
//
// # Two reads of one job: one holds the place, the other stands by
//
// The store gives the place to one read; the other takes the session anyway
// and STANDS BY ([PendingStore.StandBy]), keeping its own only where the first
// does not land ([PendingStore.TakeOver]): one upload, and nothing lost.
//
// # A capture whose taker stopped is settled from the log, not waited out
//
// Every stamp names its process's TAKER LEASE ([coord.ClassTaker], taker.go). A
// taker is gone only where that lease lapsed or moved epoch, never on a store
// that cannot answer, and a wait settles a gone taker's capture from the turn's
// row on the executor log: LANDED where the row names its place, else REFUSED.
// A capture the log took is landed whatever its taker's lease. Not presence: a
// draining node gives presence up while its keeps still upload. A keep cuts
// every record attempt at its lease's exclusive window.
//
// WHAT THE FENCE CANNOT REACH is a record handed on before that window closed
// and taken by the log after a peer settled it refused. No refusal goes to the
// log, which would lose a session for a stamp's sake: the log keeps it, and
// only the relaunch that followed the settle starts without it, saying so
// ([namedThoughRefused]). A stamp's instant is its taker's clock, so every age
// bound adds twice objstore.AssumedClockSkew ([SessionCaptureBudget]).
//
// The turn's end is owed on the row ([PendingStore.OweEnd], [ErrEndOwed]) and
// recorded on the executor log once every capture of the turn has settled
// ([Coordinator.publishEnd]), off any frame somebody waits on; its first launch
// records its begin ([PendingRun.Base], internal/executordomain).
//
// # The session switch
//
// Off (internal/config's ConversationSession), nothing is captured or prepared,
// and a stop still ends its job.
//
// # A save under way
//
// A SAVE is a capture of a job still working (saving.go). A final take can
// overtake it while it uploads, so it keeps a second place on the row
// ([PendingRun.Saving]) until it can no longer become the turn's kept capture,
// read only through [PendingRun.savingTake], since a build without the field
// keeps it stale in Extra.
//
// # A save's license
//
// A save takes its place only while its job runs undecided ([licensesSave]),
// in one read-decide-write; every final read writes its decision first, so a
// final take never loses its place to a save. Place, save point and cursor come
// from ONE read before the pack: the place order is the content order.
//
// # A save taken as its job works
//
// save.go, saveledger.go: steps cheapest first, so a refused save costs its box
// nothing. The seat's allowance is charged by compare-and-swap before the
// stamp, amended at the keep to what it uploads, so a failure only over-counts.
//
// # A capture no read places is reserved at zero, and raised after its take
//
// savepoint.go. A save, a take of a job still working and a cut-off capture may
// lack calls the log holds, so each is reserved at zero and raised after its
// take to the point of a held capture with the same newest compaction
// ([sameRestart]): a session only grows between two such captures. Only for a
// release whose resume this build read; a raise its bounds refuse is dropped.
//
// # A save is offered by the poll, from memory
//
// The poll reaches every running box; another trigger is a second way in.
// The offer decides from memory, never blocking the keepalive;
// [maxConcurrentSaves] slots bound packs and uploads.
//
// # A save as its node drains
//
// A draining node saves each direct job it launched ([Coordinator.SaveHosted]),
// most stale first; container boxes outlive it. Every pack in a box
// sharing the node's memory takes the node's one slot ([Coordinator.sharedPack]),
// and an OpenCode save checks its room first ([ErrSaveNoRoom]); a final pack
// does not, since refusing it loses the job's newest state.
//
// # Each process's own spool directory
//
// A spool's age is no proof its process is gone, and a container restart keeps
// /tmp, so each process spools into a directory of its own, held by an flock
// the kernel drops however the process ends (spooldir.go). A sweep removes only
// a directory whose lock it takes; made under a staging name and renamed once
// locked, a live one is never found unlocked.
//
// # Every spool on a node in one room
//
// The disk holds what the spools hold, so one room bounds them all
// (spoolroom.go): saves take room at the sealed bound before their pack slot
// and never wait, leaving a final take its room; a take waits a minute for
// room at the bound before its pack slot and its scrub's slots, a save's
// order, then stamps its capture refused; no seal (a media spool's too)
// writes past its room.
//
// # A turn's session is continued in whichever box its next launch gets
//
// restore.go. A relaunch continues the job before's newest kept capture, the
// only one holding all the turn's jobs were told, else a session of its own;
// never over a copy in its box that went past it ([copyWentPast]). While the
// turn has kept nothing, it continues the base the first launch froze, in a box
// of its own. One decision each: [ContinuityOf], [BaseContinuityOf]; each
// holds a session to its entry ([EntryContinues]).
//
// # The job before's capture, still being kept
//
// A relaunch waits only for what its decision depends on ([relaunchDependsOn]),
// as long as a capture of its size may take ([Coordinator.relaunchWaitOf]); a
// reader is told the answer is undecided rather than no ([ReadContinuityOf]).
//
// # Never a session that ran out of context
//
// Recorded on [PendingRun.SpentSessions] with how it was spent (out of
// context, or refused by its provider); every path refuses to take it up and
// says which.
//
// # Never a failed launch
//
// An unreadable object, a silent log or a box that cannot take the session up
// starts one of its own, its loss said (sessionloss.go). The session is
// read whole before a byte reaches the box, then streamed in held to that
// read, never spooled ([Coordinator.fetchSession]); one that differs the second
// time fails the run's start. A cut-off session's calls are read by id (Claude
// Code) or by content (OpenCode): a held class is never a false hold
// ([HoldingsByContent]).
//
// # Resume routing
//
// [ErrResumeUnavailable] is a failure of routing, not of the run: settling it
// would discard the turn. [ErrResumeElsewhere] counts nothing toward the resume
// give-up while a live node may resume it ([CoordinatorOptions.ResumableElsewhere]).
// [Resumer] hands back an opaque state blob, so this package imports no agent
// code.
//
// # Seat counts
//
// Both counts move in one step under the lock (a park is a decrement and an
// increment), clamped at zero, and the inbox hold follows at the transition.
// A seat holds up to its cap of runs in [Holding], one per conversation
// (ADR-0169): a launch or an answer's resume reserves its slot with its write,
// under the lock, or waits ([SeatFull], [ConversationBusy]). Mail waiting on a
// run waits aside, readmitted as the run leaves ([Coordinator.readmitFreed]).
//
// # Recounting a seat
//
// From one listing, the store the arbiter; a store that cannot be read leaves
// the counts alone. A recount during which this node moved the seat reads again.
//
// # Every write that moves a run between its seat's sets moves the count once
//
// Recorded before it is sent ([Coordinator.writingCount]), keyed by the run's
// turn; a recount counts a run with a write out where the moves so far leave
// it ([Coordinator.countedLocked]); the answer moves it once, under the lock,
// in recorded order; a write that did not land, a panic included, or one for a
// seat this node let go, moves nothing (countwrite.go). It puts the run in the
// set its own status names, never by its recorded from and to: an earlier
// write whose answer was lost may have landed.
//
// # The seat's inbox hold
//
// At the cap, with its aside mail full, or with an answer or ending owed
// ([SeatHold]); parked mail would loop. An answer by turn travels on the
// control topic and is recorded on arrival. This node's alone, taken for one
// [queue.Attachment], so a late one is refused.
//
// # Routing a control-topic delivery
//
// A refusal for a seat not held names [SeatLost]. While a clock runs on the
// store of its box's host, a completion is acknowledged only where its run
// settled ([ErrUnannounced]). A wait's wake carries its run on from the row,
// never as an answer, at one cost in a mixed fleet (waitover.go).
//
// # Launching a run
//
// The row is written and the box attached BEFORE the job starts: a crash then
// leaves a box its row or its own record names, never an unnamed one. A launch
// failing once its row may be written ends that job ([UnopenedLaunchError]): a
// row left launching is polled and claimed by nothing.
//
// # The seat is held from the row, not from the announcement
//
// Counted when the row's write is answered: counted off ExecutorLaunched, the
// seat's next mail could outrun the count. A reset the row refuses is never a
// launch that went ahead.
//
// # Keys a later build adds
//
// Every section a later build may grow carries its own Extra; a section-only
// decode drops unknown keys on the next write.
//
// # A run's byte budget
//
// Each value is held to [RowBytes] as the encoder writes it: the row in
// [ExecuteStateBytes] and [RestBytes], the log in [BridgeLogBytes],
// [InFlightBytes] and [LogRestBytes]; [ExtraRowBytes] is a later build's room.
//
// # Collecting a box
//
// A collection PAUSES the box, for a relaunch, a park or a retry to reuse. A job
// its turn's cap ended is ended first, its bridge session closed before.
//
// # A node's collections
//
// A read of a run's box made on this node holds one of its turns for the read
// alone ([Coordinator.collectionSlot]), since the gate does not admit them all:
// a completion ended at its deadline and an ending start no turn.
//
// # Box host failures
//
// Four kinds, handled apart: the box gone ([ErrBoxGone]); the host silent
// ([ErrBoxHostSilent], uncounted); its store silent ([ErrStoreSilent]); the
// box's own failure, counted.
//
// # Box read budget
//
// [BoxReadBudget] allows for all the read may do on the host: a slow host must
// not be taken for a stopped one.
//
// # Collection failures
//
// A retry is safe: nothing is written before a collection succeeds. Given up
// after [MinConnectFailures] attempts across [ConnectGiveUp], counted on the
// job's record; a box gone, at once. A cancellation of this node's own and a
// store's silence are uncounted. Each failure gets one verdict
// ([collectVerdict]). [reannounceCeilingPolls] stays inside the broker's
// delivery budget.
//
// # Instants another node stamped
//
// A wait timed by an instant on the row (a run of failed collections or
// resumes, an answer's lost claims, an in-flight call's bound, a held call's
// deadline) is judged by whichever node handles the run next. The instant names
// the node that stamped it: that node judges it on its own clock against the
// bound, any other against the bound plus [RowClockAllowance], never before the
// bound in real time. A node recording the wait again restarts its window on its
// own clock ([RowWindow]). A kept box's age is read on its host's clock.
//
// # The clock on a box host's store
//
// [hostUnsettled]: a host whose own store alone is silent is given up after the
// host grace, as a silent host is, its job lost with it. Kept until the run
// settles.
//
// # Unannounced jobs
//
// While that clock runs, or the row says so, no waiter announces the job, so
// every hand-back is renewed ([renewReserve], five deliveries left): announced
// afresh, then acknowledged. Only a waiter's read since ends it ([rowAskedField]).
//
// # Lost jobs
//
// A job cut off by something outside it (its engine's restart, its box or node
// gone) is no result: collected, its partial output would be judged as the
// round's. So the waiter records the verdict on the row ([PendingRun.Lost]),
// where no forged completion can restart a round, and the seat's holder
// carries the round on ([Coordinator.carryOnLost]), at most [MaxRoundLosses]
// times, since a job that keeps being lost takes its host down with it.
//
// # Parking a question
//
// Freed and opened as one move: between two writes the seat reads idle and the
// answer runs as an unrelated turn.
//
// # A box left on its host
//
// Each box has one reclaimer, by its record, which a kill deletes only with the
// box ([Provider.Kill]), so a failed kill is retried. One recording its run
// is its host's sweep's ([Waiter.sweep]), off its row, so a parked box lives
// to its pause TTL; a check's, its check runner's ([CheckBox]); any other,
// the orphan reaper's ([Local.reapOrphans]) once it is idle.
//
// # Releasing a claim
//
// To the status the claim took, the charge in the same write; a taken answer
// is the run's again.
//
// # Handing a claim back
//
// A hand-back that cannot be written ends the run only while it is still the
// claim; one the store did not answer is kept owed. No stop is reported.
//
// # Owed claim releases
//
// Kept and retried at most [answerRetryCeiling] apart, the seat's mail held; no
// claim of its job is taken here meanwhile, and a claim that wins proves it
// landed. A later job's release takes an earlier one's place.
//
// # Ending a claim
//
// Only while the run is still the claim this call took, decided before reclaim.
//
// # Finishing an ending
//
// Each step a repeat makes harmless, the record deleted last; the store refuses
// a delete still owing ([ErrHandBackOwed], [ErrAnswerOwed], [ErrEndOwed]).
//
// # Resuming and settling
//
// It reports what it left the delivery: owed, gone, or spent.
//
// # What bounds a collected job's turn
//
// The turn's deadline and the resume give-up ([ResumeGiveUp]) end the run; only
// the run's own failures count.
//
// # Resumes blocked on an operator
//
// A node's failure no wait clears is raised as `executor_resume_blocked`.
//
// # Holding a refused resume
//
// A refusal of this node's models is held in memory, never on the row
// (ADR-0064); a seat's budget holds on the row, no later than the deadline.
//
// # Kept completions
//
// Each keep re-fires at its refusal's instant or [answerWaitBackstop]
// (ADR-0063).
//
// # Owed refusals
//
// Every refusal the control topic acknowledges is owed on the job's record
// ([LaunchRecord.RefusalOwed]) first. [Draining] and [SeatLost] are handed on.
//
// # Where the record does not land
//
// The refusal is not acknowledged; it is the store's silence.
//
// # Re-arming owed refusals and holds
//
// The seat's recovery owes each again and asks its own gate at once.
//
// # Owed answers
//
// A reply handed back to the broker returns behind the person's next message,
// which then takes the question. So the answer is recorded by compare-and-set
// and its delivery acknowledged, and this package retries its resume
// ([answerRetryDelay], [MaxAnswerAttempts]) with the seat's inbox held.
//
// # A retry the seat's own conditions refuse
//
// Waits for the signal ([Coordinator.Readmit]), [answerWaitBackstop] at most.
//
// # A turn's notes
//
// They wait sealed on the run's record (ADR-0067); bounds are
// internal/agent/steer's ([NoteFiling]).
//
// # Filing and serving notes
//
// A filing is a person's note alone ([ErrNoteKind]); a serve is never a read.
//
// # A late call is handed to the job working as it lands
//
// A call whose outcome the working job's session lacks is filed as that job's
// item ([RunNote.LateCall]), only by the write that finds it late, unsealed
// since it is the log's account (ADR-0067, ADR-0073). A call outliving its
// round's collection is a late call of the round after, with no place.
//
// # A wake joins a running run
//
// The engine files somebody's message about a job's work as that job's wake
// item ([PendingStore.FileWake], ADR-0167), sealed in a domain of its own.
// Wakes between two steps join one unserved item; a hook serves an item once.
// A read item is the turn's and outlives its job; an unread one goes with it,
// its delivery left to a turn of its own.
//
// # Stopping a seat's running work
//
// A pause with `stop_running` must stop work in flight, which a run's turn
// hears only when it collects. [Coordinator.StopSeat] decides the stop on the
// row as every ending is decided; the ending ends the job, charges what it
// spent off its box before the box goes, and closes the turn as stopped.
//
// # Not the seat fence
//
// The fence is about ownership and leaves a running run alone.
//
// # Recovering a seat
//
// Per seat, on acquisition, never a fleet-wide boot scan, which would reap
// runs of seats peers own. Taking the seat's lease is what proves no live
// process holds a resumed or launching tail, so reaping is safe here only; a
// claim of an answer no turn took is revived instead ([Coordinator.revivable]).
//
// # The seat's recovery gate
//
// Every claim of the seat's runs here passes one gate before the store is
// asked ([Coordinator.claimGateLocked]) and waits out its pass
// ([ErrSeatRecovering]); a claim it refuses is carried past it, never lost.
package sandbox
