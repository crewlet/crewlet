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
// AND A LAUNCH THAT FAILS ONCE ITS ROW IS WRITTEN ENDS ITS RUN, its log's
// opening included: a row left launching is polled and claimed by nothing and
// holds its seat until the seat moves. A write the store did not answer may
// have landed, so the launch ends the job it named and nothing else
// ([UnopenedLaunchError]); a row that names another job is left to its holder.
// See "Launching a run".
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
// AND A HOOK'S NOTE IS READ ONLY ONCE ITS JOB'S SESSION SHOWS IT. A Claude Code
// hook that hands a note to its harness only SERVES it ([PendingStore.ServeHook]):
// the harness may give the answer up, or a restart may drop it. So every read
// of a Claude Code job's box at its end, a collection's, a stop's and a loss's,
// asks the job's main thread, in its one walk (mainthread.go), which of the
// serves its copy holds ([askedTakes]) the session took, and settles them by
// that verdict ([Coordinator.settleHookReceipts], [PendingStore.SettleHook])
// BEFORE the take, so the capture's cursor counts what it confirmed. A thread
// whose harness the read cut off (a stop's or a loss's of a job still working)
// only confirms; one whose harness ended its own run, at a stop as at a
// collection, proves a take it does not show absent. Every such read hands the
// engine the notes a serve of the job handed over
// ([CoordinatorOptions.SteersConfirmed]), asked or not: a verdict that landed
// with its answer lost is published by the read made again, under the same ids.
//
// THE VERDICT IS A WHOLE-ROW WRITE, so it is given [hookVerdictWait] and the
// row's crossing, retried within that room ([Coordinator.rowContext]), and
// [BoxReadBudget] counts both, with the thread's read, for a read whose copy
// holds a serve ([PendingRun.asksReceipts]). A verdict the store does not
// answer in that room is the store's silence ([ErrStoreSilent]), returned
// before the take: going on would resume, or end, on a copy that lacks the
// verdict. A collection is handed back uncounted with its box marked answered
// ([boxAnsweredError], [BoxReadReply.Answered]), which ends its run of failed
// collections, and the hand-back's own write, the one that returns the row to
// running, records how the job's harness ended ([Release.HarnessEnd]), so no
// row reads running for a job whose harness is over with nothing saying so. A
// stop's and a loss's ending is kept and asked again (stop.go,
// [Coordinator.keepLostSession]); where only the host's store is silent, it is
// given up as a silent host is, and the loss ends with its notes unconfirmed.
// A loss's box is read for this whatever the company keeps
// ([Coordinator.capturesLoss]), and its ending reads the row again before it
// reports its notes.
//
// AND AN OPENCODE CONTINUATION'S FIRST REQUEST IS JUDGED IN ITS BOX. OpenCode
// compacts a session before a request, a continued run's first among them,
// where its count of the session with the brief reaches the point its box's
// configuration sets for the run's model: the model's limit less a buffer.
// Unlike Claude Code's window, that point is not the engine's to pin: it is
// read off the model catalog and the documents of the box the run is handed,
// which the relaunch's briefs are written before. So the runner builds both
// tellings where the session's count has a bound, the held one and the one
// told as a session that may be a summary, and the launch, once it has the
// box, asks the box for its point ([PointReader]) and hands the held telling
// only where the count's bound, the brief and what the box hands over before
// it sit strictly below that point ([OpenCodePoint.SendsWhole]); anywhere
// else, and wherever the point cannot be read, it hands the summarized one
// ([SessionContinuation.Summarized], [Continuation.Summarized]). What the box
// hands over before the brief is charged only in the box that holds the
// session's copy ([PointAsk.Sources]): there OpenCode tells the session every
// context source that changed since its last request in that box (its
// instruction files, the run's own instructions, the skill and MCP server
// listings, the date and the environment), each at most its whole render,
// where after an import into another box it starts afresh and hands none. A
// box document that adds a source the read cannot size is no point
// ([PointBoxSources]). A box whose release is not the writer's is no point
// either: no pair of differing releases has been measured. See
// opencodefirst.go.
//
// AND A SESSION THE RUN STARTS OF ITS OWN KEEPS ITS TASK. A fresh brief opens a
// session with the turn's task, and a later continuation of that session that
// may be a summary may lack it as surely as a cut-off copy lacks its brief. So
// every launch whose run may start a session on its fresh brief keeps that
// brief's task beside the run's record as a lead of its own kind
// ([LeadKindFresh]): a turn's first launch ([LaunchRequest.FreshOwn]), a
// relaunch handed its fresh brief ([Continuation.Fresh]), and one whose handed
// brief continues a session, whose box may start one of its own after a failed
// restore ([BriefLeads.Fresh]). That last is kept unmarked: the brief the job's
// run read did not tell it, so no continuation of a session that run did not
// start on it is told it as a lead its brief told, while a continuation that
// may be a summary is told every lead the run keeps (leads.go).
//
// AND THE HARNESS'S OWN ESTIMATE OF A SESSION'S NEXT REQUEST, read in the same
// read as its count, of a Claude Code job's main thread or an OpenCode job's
// export (asksMainThread): stamped on a capture
// ([SessionCapture.NextRequestTokens], [TakenSession.Next]) and written in the
// collection's one write ([EndRead.Next], [LaunchRecord.NextRequestTokens]),
// never carried and never on the executor log, and handed to the floor beside
// the last request's input of the same capture: a kept capture's own for its
// restore ([SessionCapture.NextRequest]), never a newer one's it was continued
// past, and its job's newest capture's for the copy in place
// ([ContinuedSession.CopyNextRequest]). An OpenCode count
// names the provider of the reply it is anchored on BESIDE it, never in it
// ([SessionCapture.CountProvider], [LaunchRecord.CountProvider],
// [ValidCountProvider]), written and cleared with the count
// (LaunchRecord.counted), so no record names one count's provider beside
// another's figure. internal/sandbox/executor's package doc says how each is
// read.
//
// The estimate is held AT BOTH RATES Claude Code counts text at
// ([NextRequest]'s Tokens and OlderTokens), since which model the next
// request goes to is the launch's to pick ([NextRequest.At]). Zero is no
// figure (a thread not read whole, a release whose rules were not read, a
// compaction whose kept stretch could not be found), and its reader then
// reads the last request's input. It is NEVER CARRIED along a session's
// lineage, as [ResumedCount] is not: a job that made no request of its own
// may have grown its copy by its brief, so a figure carried from its basis
// would under-state it. And never on the executor log: a stamp settled off
// the log carries the last request's input alone.
//
// See docs/concepts/code-sandbox.md.
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
// the take of a job still working or one cut off, zero, raised once its take
// is counted (savepoint.go) — and the last note read the record says it
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
// the save of its current launch, nothing of the job's end read, no refusal
// of its completion owed and no loss marked ([PendingRun.jobOver]), no ending
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
// cursor and what raises the save point all come from that one read, made
// before the pack, for "The place order is the content order".
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
// # A capture no read places is reserved at zero, and raised after its take
//
// Kept by savepoint.go. A save, the take of a job still working and a capture
// cut off may lack calls their harness had answered and never written, so no
// read of the log places them. Each is RESERVED AT ZERO
// ([SessionCapture.placeAtZero]): the ledger's compare-and-swap, then the
// stamp, then its instant last, then the reservation, with nothing that waits
// between the instant and the reservation; then the spool, the check against
// what the ledger counted, and the spool's whole read, which stamps the take's
// count and then RAISES the point ([SessionCapture.placeAtNewest]), pure
// computation on that count and the one row read; then the keep, whose outcome
// stamp, at the same instant, carries the raised point. A take of a job still
// working or cut off follows the same order. A standby's takeover reserves the
// next place after its own take, so at the point that take raised: the one
// reservation above zero. A log's settle that lands a begun capture lands it as
// the store holds it, at zero, which loses the raise and tells more, never
// less.
//
// THE RAISE: once the take's count names the session's newest compaction (its
// identity, [ResumedCount.Restart]), the point rises to the newest a held
// capture states whose own newest compaction is that one, both identities read
// and equal ([sameRestart], never [RestartedSince], which reads an unknown
// basis as no restart): the launch's own landed capture ([launchKept]), and the
// capture it continued ([LaunchRecord.Continued]), read only where its launch
// is the job before's, the box took that session up ([SessionStartContinued])
// and [LaunchRecord.BasisRestart], written from the same capture, is that
// compaction. Omitted becomes the places of the launch's
// [LaunchRecord.OmittedCalls] at or below the point, a read the brief left out
// past it being told as a call after the point anyway; BridgeSeq and BridgeOpen
// stay zero and nil, so an older build that reads them is told every call. A
// raise the stamp's bounds refuse is dropped, never the capture.
//
// WHY IT IS SOUND. The continued capture C1 states p1; the save's take names
// C1's newest compaction, so the job made none after C1. Where C1's own job
// made none after its brief, the session only grows between two captures with
// one newest compaction, so the save's archive holds C1's prefix through p1.
// Where it did, C1 holds some calls at or below p1 only in a summary, and the
// launcher that continued it told it as one, every call from the round's start
// (internal/agent/runner's keptRestarted), so the save holds those calls as
// that brief told them; only a build whose runner judges the harness writes
// [LaunchRecord.Continued]. Chains follow by induction: a raised save is a C1
// for the next launch. A relaunch then tells, and asks a reading of, only the
// calls past the point, so the identity check stands in for a reading of the
// calls at or below it.
//
// THE SESSION ONLY GROWS is the premise, so a raise is gated where it is read.
// An OpenCode count names its compaction only on a release whose count is read
// (internal/sandbox/executor's openCodeCountRead), and a Claude Code capture is
// raised only where its archive's release is one whose resume this build read
// ([TakenSession.ResumeRead], internal/sandbox/executor's claudeResumeRead):
// Claude Code rewrites its transcript in place to withdraw a message, and on
// 2.1.112, 2.1.310 and 2.1.322, read in their shipped code, a withdrawal
// reaches only the request attempt in flight, whose calls lie past the point,
// while a trim follows a compaction, which moves the identity. Outside the set
// the capture stays at zero.
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
// # A save as its node drains
//
// Kept by [Coordinator.SaveHosted]. A DRAINING NODE SAVES EACH DIRECT JOB IT
// LAUNCHED whose session moved since its last save here, since that job's box
// is on the node's host. Not a container's, which survives an engine's restart
// and dies only with its host, nor an E2B one, which outlives the engine and
// whose duty a peer takes up, saving it within an interval of first sight.
// It LISTS the runs rather than reading the waiter's offers, so a box whose
// poll is stuck at its bound is not missed, and saves THE MOST STALE FIRST
// under the same slots, waiting for one for at most [drainSaveWait]. A save
// of the turn under way counts; a drain save is never paced, only charged;
// and from the drain's first instant no offer starts a save. The drain's end
// bounds each save's box phase, and its take once the take's own context is
// made, never that context's parent, whose deadline would end the take before
// it began. A take cut before its place stamps nothing, one cut after it is
// stamped refused, and the run keeps its newest periodic save.
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
// where its harness ended its own run, and else zero raised after its take to
// a point its launch's session holds (savepoint.go) — which is what the
// continuation is told the calls after by: one an older build stamped states
// only the places it read the log at, which cannot say which calls came after
// it.
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
// # Resume routing
//
// ErrResumeUnavailable reports that this node cannot resume the run a
// completion refers to.
//
// NOT a failure of the run — a failure of ROUTING. The parked turn can only be
// resumed where the seat is, so the completion has to go back and reach that
// node instead of being settled here. Returning instead of raising would let
// the caller fall through to settling the run done and tearing the box down,
// discarding the agent's whole in-progress turn.
//
// The per-seat control topic makes this rare rather than routine — a completion
// reaches its seat's owner by construction — but "rare" is not "never": a lease
// can move between the publish and the receive.
//
// One another node or build can get past is [ErrResumeElsewhere]. A run with no
// parked state and a state that is no state are met again wherever the run
// goes, and a refusal that names nothing to wait on is decided by what refused
// it (internal/engine's controlOutcome).
//
// ErrResumeElsewhere is an [ErrResumeUnavailable] another node or build can get
// past: a seat this node does not hold, a parked state of a version this build
// does not read, a reply kind it does not know, a seat or a company that is not
// this node's. An error wrapping it matches both.
//
// NOT THE RUN'S FAILURE, AND NOT ITS MESSAGE'S. It is the node's on the seat's
// control topic (internal/engine's controlOutcome), so a delivery a roll spends
// waiting for a node that can resume the run is kept with the group's unhandled
// deliveries rather than with its poison. And it counts nothing toward the
// resume give-up WHILE A LIVE NODE MAY RESUME THE RUN
// ([CoordinatorOptions.ResumableElsewhere], [Coordinator.runsOwnFailure]):
// counted, a roll that kept an older node holding the seat for half an hour
// ended a run a newer node had parked, and the engine's issue report filed it
// as the engine's own defect. Once no live node may, it counts, so a rollback
// that leaves no node reading the version of the run's parked state holds the
// seat no longer than the give-up does. A reply kind is no part of that
// version, so an unknown one is not counted while a live node runs another
// build, and counted once every live node runs this one, which reads it
// nowhere.
//
// Resumer re-enters a turn parked on its executor's run.
//
// The one seam between the sandbox layer and the agent layer, and it is
// deliberately narrow: the coordinator hands back the run's OPAQUE state blob
// and the text to answer the pending call with, and knows nothing about the
// conversation inside. That is what keeps this package free of agent imports —
// the state's shape lives in the agent layer, which is the only side that
// understands it.
//
// It must return an error wrapping [ErrResumeAbandoned] when the turn broke in
// a way a retry must not repeat; see that sentinel for which ways those are.
//
// It must return an error wrapping [ErrResumeUnavailable] when this node cannot
// resume the run, so the caller reverts the claim instead of settling it, and
// wrapping [ErrResumeElsewhere] where another node or build can (no seat here
// to resume into, a state or a reply kind this build does not read): the
// control topic hands that back as the node's, and the give-up counts it only
// once no live node may ([CoordinatorOptions.ResumableElsewhere]).
//
// # Seat counts
//
// adjustLocked applies the deltas to handle's seat under c.mu, ONE STEP for
// both counts, and reports the seat's counts before and after for the caller to
// free the seat with once it has let go of the lock ([Coordinator.seatFreed]).
// One step is what [Coordinator.SeatRuns] needs: a park is a decrement and an
// increment, and a delivery screened between two separate writes would see a
// seat with no run of either kind. AND THE SEAT'S INBOX HOLD FOLLOWS IT,
// outside the lock, in what the caller runs next: a run entering [Holding]
// holds the seat's inbox and the last one leaving lifts it (see seathold.go),
// at the transition rather than at the next delivery.
//
// CLAMPED AT ZERO. A decrement for a run this node never counted is ordinary
// rather than a bug (a seat taken over mid-run, a start event that never
// arrived, a settle for a run recovery already reaped), and left negative the
// count would then swallow the next real increment, which is a seat that runs a
// turn while a job holds it.
//
// settleNoted is [Coordinator.settleFailed] over a whole announcement.
//
// OUT OF WHICHEVER SET THE RECORD WAS IN, as its delete lands: this path
// settles a claimed run ([StatusResumed]) and a launch that never suspended
// ([StatusLaunching]) alike, each counted by this node, and the delete's answer
// moves it out once ([ending.counted]). Counted out after the ending returned,
// a recount whose listing was read after the delete counted it out already, and
// the seat whose other run still worked read free beside it; and an ending kept
// for its retry, its record still on the row, was counted out all the same. A
// run that is no longer this node's to end, somebody else having ended it or a
// newer lease owning it, has no delete of this node's to move it, and the seat
// is recounted from the store instead, which reads the row as it stands, or
// none.
//
// # Recounting a seat
//
// syncSeat sets both of a seat's counts from the store's own answer.
//
// The store is the arbiter rather than an increment, so a redelivered start, a
// restart, and a seat takeover all converge on the same numbers instead of
// drifting apart. A store that cannot be read leaves them ALONE: the
// alternatives are parking a free seat forever or freeing a busy one into
// overlapping turns, and keeping what we already believed is the only answer
// that makes neither mistake on its own.
//
// BOTH FROM ONE LISTING. A run parked on a question does NOT hold the seat — a
// person can take days to answer, and the seat has to be able to receive that
// answer, which arrives on its inbox — so each row lands in exactly one of the
// two counts. Recounting them from two listings would let a run that moved
// between the reads be counted in both or in neither.
//
// AND NEVER OVER A TRANSITION IT DID NOT SEE. A listing taken before a launch
// wrote its row, written back after the launch counted it, would drop the run
// from the seat's count and lift the hold the launch just took — a seat freed
// beside a job that is starting. So a recount during which this node moved the
// seat is read again: a transition it counted ([Coordinator.adjustLocked]), a
// record an ending of its deleted ([Coordinator.moved]), or ANOTHER RECOUNT
// WRITTEN, whose listing may be newer than this one's. Two recounts of one seat
// run at once — a resumed turn's frame recounts as it returns while the ending
// it decided is finished, deleted and recounted by this node's retry on a
// goroutine of its own ([errAskAway]) — and a frame whose listing was read
// before that delete and written after the retry's recount counted the deleted
// run as holding again, and held the seat's inbox with nothing left to recount
// it. Whichever is written second now reads again first. One that cannot settle
// within [seatRecountRounds] reads leaves the counts alone, which is the same
// answer a listing that failed gets.
//
// AND NEVER A WRITE TWICE. A write of this node's that moves a run between the
// seat's sets moves the seat's count where the write put the run once that
// write's answer is taken, not as the write lands on the store: a claim it
// gives back (claimrelease.go), a park of its job's question, an answer's
// record, the claim that resumes it and a decline that lets it go, and the
// delete that ends a run it counts (countwrite.go). A release the store does
// not answer keeps the claim, which holds the seat
// ([Coordinator.unclaimOwing]), a park frees the seat only with its question on
// the row ([Coordinator.park]), and a delete that did not land leaves its run
// on the row. A recount whose listing was read after such a write landed, and
// written before its answer was taken, counted the run where the write put it,
// and the answer's move then counted it there again: an answer's release took
// the seat's holding count down twice, freeing a seat that held another run
// beside it; a park counted its one question twice, the seat offering its mail
// to an answer match with nothing left to answer; an answer's record took a
// seat holding a second question out of the question set altogether, that
// question's answer then run as an unrelated turn; and an ending's delete,
// which leaves the run out of the listing altogether, took it out a second
// time, freeing a seat whose other run still worked. So a run whose release is
// out, or with another such write out, is counted where the moves made so far
// leave it, whatever the listing found it in and whether or not the listing has
// its row ([Coordinator.countedLocked]), and the write's answer moves that
// count, once, under the lock that ends that counting.
//
// # Every write that moves a run between its seat's sets moves the count once
//
// A seat's two counts ([seatRuns]) are moved by the writes this node makes to
// the seat's runs, and set wholesale by a recount from the store's listing
// ([Coordinator.syncSeat]). A write that takes a run from one set to the other,
// or onto the store or off it, moves the count when its answer is taken, never
// as it lands: a write the store did not answer may not have landed, and one it
// refused moved nothing. Moved after the write's answer and outside the lock,
// as an answer's record, claim and declines moved it and an ending's delete
// still did, a recount whose listing was read after the write landed, and
// written before the move, counted the run where the write put it, and the move
// then counted it there a second time. The recount's guard reads again only
// where this node moved the seat while it listed, and a write that has landed
// with its move still to come is no move yet.
//
// So each such write is RECORDED BEFORE IT IS SENT, with the status it moves
// the run from and the one it moves it to ([Coordinator.writingCount]); a
// recount counts every run of the seat with a write out where the moves made so
// far leave it, whatever its listing found it in and WHETHER OR NOT ITS LISTING
// HAS ITS ROW ([Coordinator.countedLocked]): a delete that landed before the
// listing leaves the run out of it, its move still to come; and the write's
// answer makes its move once, under the lock, in the step that takes it
// ([Coordinator.wroteCountLocked]). The writes are the open of a run's row
// ([Coordinator.Launch], no row to launching, or the claim its resumed turn
// relaunches to launching), a park's question ([Coordinator.park], resumed to
// awaiting), an answer's record ([Coordinator.record], the question to
// answered), the claim that resumes the answer
// ([Coordinator.resumeOwedAdmitted], answered to resumed), either decline that
// lets it go ([Coordinator.dropUnopened], [Coordinator.declineAnswer], answered
// back to the question), the delete that ends a run this node counts, from its
// status to no row at all ([Coordinator.finishEnding], for the endings that
// count their run out: [Coordinator.settleNoted], [Coordinator.StopSeat] and
// [Coordinator.abandon], [ending.counted]), and a recovery's takeover of each
// run of the seat it acquires, from no set to the one it leaves the run in
// ([Coordinator.RecoverSeat], answered once the recovery is done with the run).
// A claim's release is one more such write, from the claim to the status it
// returns the run to, out for as long as the release is out or owed
// (claimrelease.go). A park whose own write did not land records its claim's
// release in the step that takes that write's answer, before taking it
// ([Coordinator.handingBackLocked]), so the run has a write out at every
// instant between the two: recorded in a step of its own, the release left a
// moment with none, in which a recount counted the run off its row, the
// question that can land though its answer is lost, while the release then
// recorded counted it as the claim. The seat counted that question open and the
// claim's hold off, and a write recorded behind the release later moved by the
// difference from the claim. Before the answer is taken, so the release follows
// the park's write and whatever was recorded after it, in the order they were
// recorded: an answer by turn's record of the question the lost write put on
// the row, its answer taken first, moves the run to its answer as the park's
// answer is taken, and a recount while the release is out counts the run there.
// Recorded after that answer, in an entry of its own from the claim, the
// release counted the answered run as the claim, the seat held by a run whose
// row holds nothing until the store answered the release.
//
// Each double count cost the seat something. Counted out of the question set
// twice, a seat holding a second question read as having none open, and that
// question's answer was run as an unrelated turn ([seatRuns.awaiting]); counted
// into it twice, the seat's mail was offered to an answer match with nothing
// more to answer; counted holding twice, a claim kept the seat's inbox held
// until the next recount; and counted out of the holding set twice by an
// ending's delete, a seat whose other run still worked read free beside it, and
// its next mail ran beside that run.
//
// A DELETE THAT DID NOT LAND MOVES NOTHING EITHER. The ending is kept for this
// node's retry ([Coordinator.oweEnding]), which holds the seat's mail behind
// it, and its run stays counted where a recount reads its row until a delete
// lands. Counted out as the ending was decided, whether or not the delete
// landed, the seat read a run still on its row as gone, and the next recount
// counted it back. An ending that turns out not to be this node's, somebody
// else having ended the run or a newer lease owning it, or a run that moved on
// before its stop, makes no delete here at all: its caller recounts the seat,
// which reads the row as it stands, or none. An ending whose run this node does
// not count, or recounts the seat after, moves nothing at its delete: a recount
// after a write's answer reads the store's own answer, which no move then
// counts a second time.
//
// # Two writes out on one run move it in the order they were recorded
//
// A run can have a second write out before the first one's answer is taken: an
// answer by turn recorded to a question the park has written and not yet heard
// back on ([Coordinator.AnswerByTurn]); and a claim or a decline of one answer
// beside another, the attempt a reply makes as it is recorded and the one the
// answer's series makes on its timer. Writes on one run that each need the
// status the one before left (the record needs the park's question) land in the
// order they were recorded, and of writes that need the same status (two claims
// of one answer) at most one lands. Their answers come back in whatever order
// their callers do. So a write whose answer is taken while one recorded before
// it is still out moves nothing yet: its move is made with that earlier one's,
// in the order the two were recorded, in the step that takes the earlier
// answer. Made at once, the record's move counted the run out of the question
// set before the park's had counted it in, which the clamp at zero
// ([Coordinator.adjustLocked]) swallowed on a seat with no other question, and
// the park's move then counted the answered run as a question.
//
// A RELEASE STILL OWED HOLDS THE MOVES RECORDED BEHIND IT. A claim's release
// the store did not answer stays out on its run until an attempt at it is
// answered, its series making one at most [answerRetryCeiling] apart, and none
// while a claim of the run is on its way ([Coordinator.oweRelease],
// [Coordinator.retryOwed]), unless a claim of its job that wins takes it over
// first or a later job's release takes its place (claimrelease.go). A write
// recorded behind it on the run, the park of a job its resumed turn relaunched
// before it failed and handed the claim back, say, has its answer taken and
// moves nothing until then, and the run stays counted where the moves made
// before the release left it, holding the seat as the claim does. That is the
// recorded order's own rule, and the wait it makes is transient and bounded: it
// lasts only while the release is out, which the first attempt at it the store
// answers ends at the latest. And the seat's mail is held behind the owed
// release throughout ([Coordinator.owe]): what its inbox has waits there, and a
// delivery that raced the hold, or found it refused, waits as well, since the
// inbox's screening runs nothing on a seat it reads held, and it reads this one
// held while the run is counted as the claim ([Coordinator.SeatRuns]). So a run
// counted as the claim though its job has parked on a question is never missed
// by an answer match. Moved ahead of the release instead, the later write's
// move would be made before the move it follows, which is what this section's
// rule refuses.
//
// # Where each write puts the run
//
// A write that lands moves its run to the set its own status names, whatever
// the writes before it did, and the seat's counts by the difference from where
// the moves made so far had left it. Not by the sets it was recorded from and
// to: a write before it on the run whose answer was lost may have landed all
// the same, and moved by its recorded sets, a later write took the run out of a
// set it was never counted in. A park's question can land with its answer lost,
// the park moving nothing and handing its claim back, its release owed, and a
// reply then answers that question: the record, moved from the question set to
// the answer while the run was still counted as the claim, took the seat's
// count of questions down for a question it never counted, and a seat holding
// another question read it as closed. The status a write is recorded from is
// where a recount counts a run with no other write out, until a move is made
// ([Coordinator.writingCount]).
//
// # A move for a seat this node no longer holds moves nothing
//
// A seat's release drops every write this node has out on its runs, in the step
// that drops the seat's counts ([Coordinator.ReleaseSeat]), as it drops the
// claims' releases it was making or owed, and a write's answer taken after it
// moves nothing. Moved all the same, an answer taken after the release counted
// its run for a seat this node no longer held, and once this node took the seat
// back its recovery counted the run where it found it: a park's question
// counted twice, the seat's mail offered to an answer match with nothing more
// to answer, and an answer's record taking the seat's other question out of the
// question set. Kept in its entry instead, the write held the recovery's
// takeover of its run behind it, the run counted where the write was recorded
// from until the write's answer came. A write recorded after the release, by a
// frame of the earlier holding still running here, moves a count nothing reads
// for a seat this node does not hold, and the recovery that takes the seat back
// starts its counts afresh ([Coordinator.takingOver]).
//
// # Every write is settled on every way out
//
// A write is answered on every way out of its store call, a panic included
// ([Coordinator.settleWrite], deferred where the write is recorded): the
// queue's handler recovers a panic and the process lives on, and a write left
// out held every later write's move on its run behind it, the run counted where
// the write was recorded from until the seat moved. A panic is no answer from
// the store, so such a write moves nothing. So is a claim's release, whose
// record refused every claim of its job, and kept its run counted as the claim,
// for as long as it stood ([Coordinator.unclaimOwing]), and the count of a
// job's claims on their way, which held an owed release of the job back while
// it was above nothing ([Coordinator.claimCompletion]).
//
// THE WRITES OF ONE RUN ARE KEYED BY ITS TURN, whatever job each names: a turn
// has one row, which the store holds once, and a recount that counted a run's
// row apart from a write out on another job of it would count the one row
// twice.
//
// takingOver records, before a recovery fences any of them, the takeover of
// every run its listing found on handle's seat ([Coordinator.RecoverSeat]),
// each from no set at all, and starts the seat's counts afresh from what this
// node's writes out on the seat leave them, once they are recorded. It returns
// the takeovers, one per run, for [Coordinator.takeoverAnswered] to answer.
//
// AFRESH, because whatever this node counted of the seat before its recovery is
// a recount's, off the control topic the recovery attaches first, and the
// recovery counts each run again where it leaves it: added to that recount's,
// every run it took over was counted twice, and a tail it reaped stayed
// counted. Nothing else of this node's moves the seat before its recovery: its
// mailbox is not open, and a write of an earlier holding moves nothing
// ([Coordinator.ReleaseSeat]). A recount whose listing was read before this
// step reads again, as after any move ([Coordinator.syncSeat]).
//
// # The seat's inbox hold
//
// The seat's inbox is held while a run holds the seat, or an answer is owed.
//
// # Why a busy seat is held rather than parked
//
// A seat a detached run holds takes no new turn. Were the inbox screening to
// PARK every delivery that reached it, requeuing it onto the seat's own inbox
// and acking the original, nothing would stop the consumer, so the copy is the
// next thing it fetches, immediately, and it is parked again: for the length of
// the run every waiting message would go round that loop at whatever rate the
// broker serves, a fetch, a screening, a store read, a replicated publish and
// an ack per message per lap, and a seat parked on a long run would be the
// busiest thing on its node, doing nothing.
//
// # What happens instead
//
// The coordinator holds the seat's inbox — a pause hold under ONE reason,
// [SeatHold] — for exactly as long as the seat has a run in [Holding] or owes
// one of its runs a recorded answer's resume or the finish of an ending this
// node keeps, and lifts it the moment it has neither. Held, the consumer
// fetches nothing: the mail waits on the broker with its order and its delivery
// count intact, costs nothing while it waits, and is delivered in order when
// the hold lifts. Both conditions are this node's own counts ([seatRuns], the
// owed set), moved by the coordinator's own transitions, so the hold follows
// them at the transition itself rather than at whichever delivery happens to
// arrive next: a launch, from the write that opens its row
// ([Coordinator.Launch]) — not from the started event a separate subscription
// processes later, which would leave a window for the seat's next mail to run
// beside the job. Then a park on a question, a run that settles — its record
// deleted, which for a resumed turn comes once the turn has RETURNED, and where
// the record owes the executor log the turn's end, once its ending's retry has
// recorded that end (never at a resumed turn's start: the resumed turn is the
// seat's work, and the hold lasts through it and through its end, see
// [Coordinator.resumeAndSettle]) — and a seat recovered mid-run, whose hold is
// taken in its preparation, before its mailbox is attached: for a run it finds
// running, and for an ending it finds decided and owing its turn's end, which
// this node then keeps ([Coordinator.endRun]).
//
// A delivery that reaches a held seat anyway — one that raced the hold, or a
// hold the queue refused — is DEFERRED by the screening, which asks for the
// hold again first ([Coordinator.HoldSeat]). A deferral keeps the delivery at
// the head of the inbox at the cost of one of its deliveries, once per hold
// rather than once per lap.
//
// # The rule: a seat busy coding takes no other work
//
// A HELD SEAT RECEIVES NOTHING, a person's answer to ANOTHER of its runs
// included, until the run holding it settles or parks on its own question. That
// is the rule rather than a price paid for the hold: an agent whose executor is
// in the middle of a job is busy with that job, and it takes up the next one —
// a new task, a colleague's message, the answer that resumes its other run —
// when this one is done, exactly as a person would. Resuming a second run
// beside the first would put two of its turns in flight at once, each acting
// for the seat with no view of the other.
//
// So a chat reply waits on the inbox with everything else and is offered when
// the job stops holding the seat. Nothing is lost: the reply keeps its place,
// the question keeps its anchor, and a box that outlives `pause_ttl_seconds`
// meanwhile re-seeds from its branch when the answer arrives. Offering the
// answer on a held seat would mean consuming the held seat's mail to look for
// it, and a consumed message that is not the answer has nowhere to go but back
// onto the inbox, in a loop.
//
// AN ANSWER BY TURN DOES NOT WAIT ON THE INBOX. It names its run, so nothing
// has to be read to find it, and it travels on the seat's control topic, which
// no hold stops ([AnswerDesk.Deliver]). On the inbox it would wait out every
// job that held the seat and then every older message, any of which could start
// the next job: on a busy seat it would never arrive. It is recorded on
// arrival, and the rule above still holds for its RESUME, which waits for the
// job holding the seat ([SeatCoding]) and is then the first thing the seat
// does, its inbox held behind it.
//
// WHO HAS TO AGREE ON IT: this node alone. The hold lives in this process's
// queue client, keyed on the subscription, and is derived from this node's
// counts of the seat's runs. A detach drops it with the attachment, and the
// seat's next holder takes its own from its recovery pass.
//
// # A hold is taken for the attachment the seat holds
//
// Each hold names the attachment of the seat's inbox it is for
// ([queue.Attachment]): the one the seat's recovery was handed as this node
// took the seat ([Coordinator.RecoverSeat]), read in the step that decides the
// hold. A node's release detaches the inbox before it tells the coordinator, so
// a decision made while the seat was held here can reach the queue after the
// detach, after the coordinator has forgotten the seat, and after this node has
// taken the seat back. Named by the subscription alone, that hold was put back
// on the inbox and carried onto the next attachment, with nothing on this node
// recording it: a seat retaken with nothing to recover attached held, and
// nothing lifted it, so it consumed no mail here until it moved. Named by its
// attachment, the queue refuses it ([queue.ErrAttachmentGone]), and a lift made
// for an ended attachment lifts nothing, never the hold the next attachment's
// recovery took under the same reason. So nothing compensates for a late call:
// no lift follows a hold the seat was released under, which would itself race
// that recovery's hold.
//
// SeatHold takes and lifts the hold on a seat's inbox — see the top of this
// file for when.
//
// DECLARED HERE AND IMPLEMENTED BY THE ENGINE, which owns the seat's inbox and
// the name the hold is taken under. The coordinator calls Hold when a seat
// starts to need it and Release when it stops, once each: holds are keyed by
// reason, so a second Hold would be the same hold and a Release the last of it.
//
// CALLED WITH THE SEAT'S HANDLE, which is what this node's own counts are kept
// under, and IMPLEMENTED ON ITS ID: the inbox a hold sits on is named by the
// seat's id, a pure function of its immutable handle (ADR-0013), so the
// implementation derives the id and holds the mailbox it names, as every other
// hold on a seat's mailbox is keyed.
//
// AND WITH THE ATTACHMENT the hold is for, which the queue holds it to: Hold
// refuses one that has ended with [queue.ErrAttachmentGone], and Release under
// one lifts nothing.
//
// The seat's INBOX HOLD while a run holds it or an answer is owed (`SeatHold`,
// seathold.go) is TAKEN FOR THE ATTACHMENT of the inbox the seat's recovery was
// handed (`RecoverSeat`), read in the step that decides it and forgotten with
// the seat (`forgetHold`): a node's release detaches the inbox before the
// coordinator hears of it, so a hold decided while the seat was held can reach
// the queue after the detach, which refuses it (`queue.ErrAttachmentGone`)
// rather than carry it onto the inbox the node attaches if it takes the seat
// back, and no compensating lift follows it, which would race that recovery's
// own hold (`TestAHoldDecidedBeforeTheReleaseDoesNotGateTheNextAttachment`).
//
// # Routing a control-topic delivery
//
// OnEvent routes a seat's control-topic delivery.
//
// A REFUSAL FOR A SEAT THIS NODE NO LONGER HOLDS NAMES [SeatLost], so the
// caller settling the delivery hands it on to the seat's holder by the
// condition, as it hands on a draining node's, rather than retrying it here
// until the broker dead-letters it.
//
// WHILE THIS NODE KEEPS A CLOCK ON THE STORE OF ITS BOX'S HOST, OR THE RUN'S
// ROW SAYS WHAT SUCH A CLOCK WOULD ([Coordinator.unannouncedJob]), A
// COMPLETION'S DELIVERY IS ACKNOWLEDGED ONLY WHERE ITS RUN SETTLED, which drops
// the clock and moves the row on: every other answer, a refusal's and one that
// settled nothing included, hands it back marked [ErrUnannounced], and it is
// RENEWED here, where the delivery in hand is, whatever the hand-back:
// announced afresh before that delivery is the last its budget allows, and
// acknowledged once the fresh announcement is out (renew.go). A DELIVERY THAT
// IS THE WAITER'S ANNOUNCEMENT OF THE JOB, MADE FROM A READ ASKED SINCE, ends
// the clock, and a claim of it clears the record ([announcedAfter]): that
// waiter read the row through the store they say is silent, and announces the
// job again from there.
//
// AND A REFUSAL THE CONTROL TOPIC ACKNOWLEDGES IS RECORDED ON THE JOB'S RECORD
// FIRST ([Coordinator.oweRefused]): the obligation the acknowledgement leaves
// with this node outlives it, and a record the store does not take hands the
// delivery back as this node's failure, never acknowledged.
//
// # Launching a run
//
// Launch provisions a box, starts the harness detached, and persists the row
// that outlives the turn — and HOLDS THE SEAT from the moment that row is
// written.
//
// THE ORDER IS THE CONTRACT. The row is created and the box attached to it
// BEFORE the job starts: a crash in that window leaves a row naming a box
// nobody is using, which recovery can kill, while the reverse ordering leaves a
// box that nothing names and nothing can ever reclaim.
//
// On any failure after the box exists, the box is reclaimed before the error
// propagates — an unreferenced box is billed for until its TTL and collected by
// nobody.
//
// # The seat is held from the row, not from the announcement
//
// A run is counted into the seat's [Holding] set, and the seat's inbox hold
// taken ([SeatHold]), in the step that takes the answer to the write that opens
// its row, before the box is provisioned, and so before the launching turn can
// return and its seat's consumer go on to the next thing it has. THROUGH THE
// ONE MECHANISM EVERY WRITE THAT MOVES A RUN'S COUNT TAKES (countwrite.go): the
// open is recorded before it is sent, so a recount between the row's write and
// its answer counts the run where it was, and the answer counts it in once.
// Counted after the write and outside the lock, a recount whose listing was
// read after the row was written counted the run holding, and the launch then
// counted it a second time, the seat held until a recount after the run had
// settled. WHERE IT WAS is no row at all for a turn's launch, and the claim its
// resumed turn holds for a relaunch that turn makes ([Coordinator.framing]),
// which holds the seat already: counted in a second time, the claim and the job
// it relaunched held the seat twice until the turn's frame recounted it. THERE
// IS NO THIRD: a launch outside a resumed frame of its turn is the turn's
// first, and finds no row, since a turn id names one execution
// ([TurnRef.TurnID], ADR-0017): a kick-off delivered again runs under an id of
// its own, and its launch opens a row of its own. Counted when its
// [types.ExecutorLaunched] is processed off the seat's control topic, a
// separate subscription the launching turn does not wait for, mail already
// drained beside the launching delivery and mail fetched after the turn
// returned would both reach a seat the screening called free, and run a turn
// beside the job. The announcement still RECOUNTS the seat from the store
// wherever it is processed — that is what carries a held seat across a restart
// or a handoff — but nothing waits for it.
//
// A launch that fails after the row is written gives the count back as the
// delete that abandons the row lands ([Coordinator.abandon]); one that fails to
// suspend into the row is settled by [Coordinator.FailRun], whose ending gives
// it back the same way.
//
// m is the manager the request's [Spec] was built with ([Manager.BuildSpec]),
// passed rather than read off the coordinator here: an apply landing between
// the two swaps the coordinator's, and a spec resolved against one catalogue is
// launched on that catalogue.
//
// Every live status and every job: this launch is abandoned whatever the row
// reached. TWO BOXES CAN BE OWED A RECLAIM, and each is reclaimed by the one
// party that can name it. The launch's own is killed above, because the launch
// may never have attached it to the row. The box the ROW names is reclaimed by
// the ending, after its decision: a relaunch that never got a box of its own
// leaves the row naming the previous job's paused one, and the ending deletes
// the only record of it — so a box left to the turn's own settle was named by
// nothing once the turn came back, and was killed only off that settle's stale
// snapshot, which a settle may no longer act on ([Coordinator.finish]). Where
// the two are one box — the attach landed — it is killed twice, which every
// provider answers as the reclaim it wanted: a kill of a box already gone
// succeeds.
//
// AND THE SEAT GOES BACK AS THE DELETE LANDS, once ([ending.counted]): the
// launch counted the run holding as it opened the row, and the ending's delete
// moves it out as its answer is taken. A record that survives the delete is a
// launching row whose ending this node keeps and retries, holding the seat's
// mail behind it, and it stays counted where a recount reads it until its own
// delete lands. Given back here whether or not the record was deleted, the seat
// read that row as gone, and a recount whose listing was read after a delete
// that did land had counted it out already, so the seat whose other run still
// worked read free beside it. A row no longer this node's to end, ended by
// somebody else or owned by a newer lease, has no delete of this node's to move
// it, and the seat is recounted from the store instead. So is the seat of an
// open the store did not answer ([UnopenedLaunchError]): the open counted
// nothing, a row it wrote all the same is counted only where a recount reads
// it, and the ending of the job it named moves no count of its own.
//
// resetLaunch is [CoordStore.BeginLaunch]'s half for a row that was already
// there, a relaunch its turn's resumed frame makes, reporting the row as it
// reset it and whether it did; a row that is gone is neither. A kick-off
// delivered again never reaches it: it runs under a turn id of its own
// ([TurnRef.TurnID], ADR-0017), and its launch creates a row of its own.
//
// Only the LAUNCH-SCOPED state is reset: the identity fields stay the existing
// row's, and so do the turn's read of its conversation ([PendingRun.Base]),
// which the create alone writes, and the box reference — its id, its cell, its
// harness and its host ([PendingRun.LaunchNode]), as one — until the launch
// attaches a box of its own ([CoordStore.AttachSandbox]). The reset row hands
// that box to the launch as [PendingRun.Prior], which decides whether the new
// job continues in it or reclaims it through its own cell first; the row naming
// it meanwhile is what lets recovery reclaim it if the launching node dies
// before deciding, and it is reclaimed where it is: a box on another node's
// host, by that host.
//
// WHAT WAS THE PREVIOUS JOB'S AND NOT ITS BOX'S GOES: its command and its
// session. The row names the new job from here, and a reader that found the
// previous job's command on it — a person's stop, before this launch attaches
// one of its own — read that finished job back off the box and charged it a
// second time, as this job's.
//
// A RESET THE ROW REFUSES IS NEVER A LAUNCH THAT WENT AHEAD: a newer lease owns
// the run — an error wrapping [ErrSeatNotHeld] — or its ending is decided — one
// wrapping [ErrRunEnding] ([CoordStore.mutateFenced] says which wins). Answered
// as a launch, either would start a job in a box the row did not record —
// killed under it by the ending, or billed and named by nothing.
//
// # Keys a later build adds
//
// A run's record keeps what this build cannot read, AT EVERY LEVEL THAT GROWS.
//
// The record is read, modified and written whole by every node that touches it
// ([PendingRun.Extra]), so a key this build does not know survives a write of
// this build's only where the decode kept it. A key at the top of the record is
// kept in [PendingRun.Extra]. A key INSIDE a section is kept only by the
// section's own Extra: decoded into its struct alone, every key the struct does
// not declare is dropped, and the next status flip, lease, bridged call or note
// writes the section back without it, so a field a later build adds is gone
// from the record within seconds on a fleet running both builds, and nothing
// says so.
//
// So each section that a later build may add fields to carries an Extra of its
// own, filled by its decode and written back by its encode under the rule the
// record's top follows: [RunSteer], [RunNote], [NoteRead], a hook's serve of a
// note ([HookServed]) and the call it answered ([NoteCall]), [BridgeCall],
// [ElidedCall], [SessionCapture], [HeldCall], [ConversationBase] with its
// [BaseSession] and each [BaseSibling], and [LaunchRecord] — the job's own
// record, which grows a fact about the job with nearly every piece that touches
// launching, and which every write but a relaunch's hands back; and the measure
// inside it, [SessionMeasure], whose word on where a figure came from a later
// build may add to. A section that does not is extended the other way, with a
// key of its own at the top of the record (as [PendingRun.Stop] is, beside
// [RecordedEnding]). And the log kept beside the record, [BridgeLog], is a
// value of its own with an Extra of its own at its top, under the same rule. So
// is the handed part kept beside it ([handedSet]), and the set it seals
// ([HandedSecrets]): the attach of each job writes its own whole, but every
// token the completion poll renews in the job's box is a read of the part, an
// open of the set, a change and a write back of both ([CoordStore.AddHanded],
// [Coordinator.addHanded]), on whichever node polls the box, every few minutes
// of a long run. And the leads kept beside the record ([leadsPart]), with each
// [KeptLead] it keeps: every take of a job's leads rewrites the part whole, so
// each carries an Extra of its own, under the same rule.
//
// # A run's byte budget
//
// A run's values in the coordination store are held to a byte budget, and every
// field they carry has a share of it.
//
// # Why there is a budget at all
//
// A run is kept as values in the coordination store ([coord.SandboxRuns]): its
// row ([PendingRun]), read and written whole by every mutation (coordstore.go),
// and beside it the parts a job can make large: the bridged call log
// ([BridgeLog]), read and written whole by every append, the secrets the job's
// box was handed, and the turn's leads ([PendingStore.Leads]). One value is
// held to what one key of the store holds, [coord.MaxValueBytes] (the largest
// whose read a node without `data` takes back across its leaf link,
// base64-encoded in its stream leader's reply), and past it every write of the
// value is refused ([coord.ErrValueTooLarge]): for the row, a lease stamped on
// the run, a status flip, the hold a turn takes on its next scratch repository;
// for the log, every call's append. The bridged call log grew one call at a
// time with nothing measuring it (two hundred builtin answers of 64 KiB are
// 12.8 MiB), so a long run's log stopped taking writes, and its calls were the
// first thing to go: the resumed turn's ledger, its delivery check and its
// reviewer's evidence missed what the run did.
//
// # The shares, in bytes as each value holds them
//
// Every figure here is bytes AS THE STORE'S ENCODER WRITES THEM (json.Marshal,
// HTML escaping on), escaping counted: a cut is measured by internal/kept,
// never estimated from what was sent, so no six-fold multiplier is spent on
// text that does not escape. Each value of a run may weigh [RowBytes]. The
// row's is split two ways, and the split closes:
//
//	the parked turn's state ......... ExecuteStateBytes   2,097,152  (2 MiB)
//	everything else, and the room
//	  a later build adds fields in .. RestBytes           4,193,390  (4 MiB less 914 bytes)
//	                                                      ---------
//	                                  RowBytes            6,290,542
//
// and the log's three ways, beside it, a split that closes as its figures are
// stated (TestACallInFlightIsHeldToItsShare):
//
//	its calls and their tallies ..... BridgeLogBytes      3,473,408  (3.3125 MiB)
//	its calls in flight ............. InFlightBytes       1,048,576  (1 MiB)
//	the turn's earlier rounds' tool
//	  names, the log's own fields,
//	  and the room a later build
//	  adds keys to the log in ....... LogRestBytes        1,768,558  (1.69 MiB)
//	                                                      ---------
//	                                  RowBytes            6,290,542
//
// The handed set beside them ([PendingStore.Handed]) is the third value, at
// most [MaxHandedBytes] before it is sealed: a fraction of [RowBytes], which
// TestTheWholeRowFitsItsBudget holds it to as well. And the turn's leads beside
// them ([PendingStore.Leads]) are the fourth, held to [MaxKeptLeadBytes] and
// [MaxKeptLeads], their own fields at their worst within [RowBytes] beside them
// (leads.go's table, measured by TestTheWholeLeadsPartFitsItsBudget).
//
// THE LOG'S CALLS IN FLIGHT are held by construction too: [MaxCallsInFlight]
// entries, each its call's identity and the id its harness gave it, held to
// [MaxCallInFlightBytes] ([keptEntry]) — measured, every one at its widest,
// inside their share by TestTheWholeLogFitsItsBudget, with the log's calls at
// theirs beside them.
//
// THE LOG'S CALLS are held by construction: [MaxBridgeCalls] calls, one of them
// (the newest successful submission, which the collection replays) at most
// [MaxSubmissionCallBytes] and every other at most [MaxBridgeCallBytes]; the
// identities of the calls the log does not list ([MaxElidedIdentityBytes]) and
// of the calls the run's call log took in from a bridged call's own work
// ([MaxAbsorbedIdentityBytes]). 199 × 16,384 + 73,728 + 2 × 65,536 + 300 for
// the keys, the commas and the count is 3,465,516, inside the share. The rest
// of the log is the turn's earlier rounds' tool names ([MaxPriorCalls]) and the
// log's own fields — its identifiers (its job, its round, the answer it was cut
// for), every kind of change its round's calls committed, each once
// ([BridgeLog.Committed]), and how many of the calls it dropped no save point
// holds ([BridgeLog.LateElided]) — which weigh tens of KiB together at their
// worst (TestTheWholeLogFitsItsBudget): the room left above them is a later
// build's, for keys it adds to the log or to an identity ([BridgeLog.Extra],
// [ElidedCall.Extra]); a key added to a call comes out of the call's own share.
//
// THE PARKED STATE is held by the park, which is the one writer of it: past
// [ExecuteStateBytes] the closed rounds' payloads are condensed, and a state
// still past it is refused ([ErrStateTooLarge]) — see
// internal/agent/execstate's EncodeWithin. A field the turn's frozen context
// gains is added inside this share, which the closed rounds give way to, as how
// the turn's conversation's earlier turns ended was (execstate's
// Frozen.Endings, up to 24,000 bytes as the state holds them, never condensed).
//
// THE REST is every other field of the row, each at the bound its own writer
// holds it to (TestTheWholeRowFitsItsBudget builds every one of them at that
// bound, through the code that writes it, escaped at its worst, and
// TestTheWorstRowSetsEveryField holds that model to every field the row
// carries), and the shares stated here:
//
//	a recorded answer ............... MaxAnswerBytes            524,288  (512 KiB)
//	the copies one owes back ........ MaxAnswerBytes            524,288  (512 KiB)
//	a held call's sealed arguments .. MaxHeldArgsBytes          262,144  (256 KiB)
//	the turn's read of its
//	  conversation .................. ConversationBaseMaxBytes   24,576  (24 KiB)
//	what a later build adds ......... ExtraRowBytes             262,144  (256 KiB)
//	every other field, measured ..... measuredOtherFields     2,309,462
//	to spare, measured .............. measuredSpare             286,446
//
// Every other field is the identity, status and time fields, the run its turn's
// leads are kept for and how many leads that part has taken
// ([PendingRun.LeadsRun], [PendingRun.LeadsAppended]); the title a launch keeps
// ([MaxRunTitleBytes], twice) and a colleague's question; a refused reply's
// refusal beside its answer ([PendingRun.AnswerRefusal], a hundred delivery ids
// at most, two figures and where it was posted, [MaxRefusalAddressBytes]) and
// the round's refusals kept for its review ([maxRoundRefusals]); the ask and
// its decision, and the seats it is put to ([MaxAudienceHandles]); a held
// call's sealed result and the call a person approved, on the held call and
// owed to the log ([OwedCall]), and the decision the call carries out, whose
// question, for one asked in words, is the body of the comment that asked it,
// and the delegates it was put to first ([HeldCall.Acting],
// [HeldCall.Delegation]); the branch and the notes ([MaxNoteSealedBytes]); who
// the turn works for and the answers that woke it ([PendingRun.WorkingFor],
// [PendingRun.Answered]); the job's own record, the reads its briefs left out
// among it ([LaunchRecord.BriefOmitted], at most [MaxBridgeCalls] places each)
// and which of the turn's leads its brief told, by their numbers in the run's
// leads part ([LaunchRecord.LeadMarks], at most [MaxKeptLeads]), what the
// session its box holds carried at its newest request the engine measured
// ([LaunchRecord.Context]), its harness's count of that copy, the count's
// provider and writer and its next request (ResumedCount, CountProvider,
// CountRelease, NextRequestTokens on [LaunchRecord]) and the restart its
// launch's session held ([LaunchRecord.BasisRestart]), and a refused
// completion its seat's holder owes ([LaunchRecord.RefusalOwed], its condition
// at most [MaxOwedConditionBytes]); the owed end and the captures (newest,
// kept and [PendingRun.Saving]), each with the reads its session holds only as
// a count ([SessionCapture.Omitted]) and the same three figures; and what the
// job may do on Gitlet: the holds of its turn ([MaxCodeHolds]), its
// repositories ([MaxRunRepos], each held to its grammar, its cone and its
// change's title, [MaxChangeTitleBytes], among them), its floors and whom its
// pushes and its change are made for ([PendingRun.CodeWorkingFor]). Every one
// of them is bounded by THIS ENGINE: a person's reply, a tool's arguments and
// a box's account of its session are each held to their share by the write
// that puts them on the row, which refuses one past it by name
// ([ErrAnswerTooLarge], [ErrHeldCallTooLarge], [ErrCaptureUnbounded]) rather
// than leave it to the store; and the turn's read of its conversation is held
// to its line by construction, made of a read the executor log bounds field by
// field ([NewConversationBase]), the largest of which a test holds under it.
//
// The two measured lines are the model's, and pinned ([measuredOtherFields],
// [measuredSpare]): what every other field weighs at its worst, a held call's
// arguments among them as none (their key, and the two quotes their share
// counts as well), and what is left once every line above it is counted and the
// keys a recorded answer, its copies and the turn's read are written under,
// each with its comma, those two quotes counted once.
//
// [ExtraRowBytes] is the room for the keys a LATER BUILD ADDS to the row — at
// its top and in its sections (the job's own record, [LaunchRecord], first
// among them) — which is where the next such field goes. A field that lands is
// moved out of it into a line of its own above, as the turn's read of its
// conversation was ([PendingRun.Base]), so the room stays whole for the field
// after it.
//
// # Collecting a box
//
// collect reconnects, reads the result, and PAUSES the box rather than tearing
// it down.
//
// THE BOX OUTLIVES ITS JOB for three readers. The resumed turn relaunches its
// executor whenever its reviewer sends the work back, and the relaunch
// continues in this box — the same checkout, the harness's own session — rather
// than re-provisioning and throwing the working tree away
// ([Coordinator.Launch]). A run that parks waits in it for its answer. And a
// resume that fails and is retried collects again, from a box still there to
// read. Paused rather than left running between them, because nothing runs in
// it until one of those comes, and a paused box is the one the pause reaper
// bounds ([PendingRun.HeldSince]); whichever ending the turn reaches reclaims
// it.
//
// ENDED reports a run its turn's wall-clock cap ended ([PendingRun.Deadline]):
// the poll announced its completion while its job was still working, so the
// read ENDS THE JOB first, as a stop's read does ([Coordinator.readBox]) — its
// script asked to stop its harness and pack the session as its own exit does —
// and what is collected is what the job did up to then, and the result says it
// was cut off and why, so neither the reviewer nor the record takes a run the
// clock stopped for one that finished.
//
// AND ITS TOOL-BRIDGE SESSION IS CLOSED FIRST ([CoordinatorOptions.Ended]): a
// job its turn's cap ended speaks for the seat no more, and while the read ends
// it — the harness's grace, the script's own pack, up to [JobEndWait] — it
// would otherwise go on posting and writing as the seat after its turn had
// stopped waiting for it. A job that finished before the cap makes no call to
// refuse, and a turn past its cap starts no round that would open the session
// again.
//
// THE CLAIM IS ANSWERED BACK beside the result, marked the moment the box
// answered ([PendingRun.BoxAnswered]), so a step of this read that fails after
// the box's own (what the run pushed, read once the box answered) hands the
// claim back with the run of failed collections behind it ended, as every step
// after the read does ([Coordinator.collectFailed]).
//
// awaitCalls waits until run's log holds no entry of its round in flight
// ([BridgeLog.InFlight]), or until each it still holds is one no collection
// waits for any longer — its own bound passed, the turn's cap passed, or its
// runner gone — and answers those, with how many entries it saw in flight at
// any point. See "What ends the wait" in drain.go.
//
// THE WATCH IS ITS OWN, started and stopped here: a watch that stops hearing is
// watched again, after a jittered pause, and the store's watch reads the log
// again as it begins ([PendingStore.WatchLog]), so nothing written between the
// two is missed. A watch that cannot be made is an error: a store that cannot
// be read could not have answered the read the collection makes next either. It
// is the store's silence ([ErrStoreSilent]), the first watch's and one made
// again alike, and never the box's.
//
// # Box host failures
//
// BoxReader asks the node whose host a run's box is on to read it back — the
// part of a collection that touches the box ([Coordinator.readBox]) — for a
// node that holds the run's seat and not its box.
//
// What comes back is the run's own account, floored and with its transcript and
// refs bounded ([boundBox]), and whether its turn's cap ended it; the asker
// fits the rest ([Coordinator.fitText]).
//
// FOUR KINDS OF FAILURE, each told apart because each is handled apart: the
// host said the box is gone (wrapping [ErrBoxGone]); the host gave no account
// of the box ([ErrBoxHostSilent]) — it did not answer, or stopped as it read,
// which is the same fact, carried apart from the host's own words
// ([BoxReadReply.Withdrawn]), or answered that it was busy with other reads of
// its boxes ([ErrBoxHostBusy]), or this node could not put the read to it at
// all ([ErrBoxNotAsked]), each of which says nothing about the box and is
// handled as a host that did not answer; the host's coordination store did not
// answer its read of the run's row, which licenses the read ([ErrStoreSilent]),
// carried apart the same way ([BoxReadReply.Unsettled]); and anything else the
// host said, which is the box's own failure to be read, counted like one read
// here.
//
// ErrBoxHostSilent is a box read the node whose host the box is on gave no
// account of the box: it is stopped, restarting, or gone, or it stopped as it
// read ([BoxReadReply.Withdrawn]) — or it answered that it was busy
// ([ErrBoxHostBusy]), or this node could not ask it ([ErrBoxNotAsked]), which
// wrap this.
//
// NOT A FAILURE OF THE BOX, which is why a collection handed it gives its claim
// back UNCOUNTED and acknowledges the completion: the host's own waiter
// announces the completion again on its schedule once the host is back
// (reannounce.go), and a host that never comes back is found gone by the
// fleet's waiter, which has the run's round carried on in a box of its own
// ([Waiter.goneAfter], [LossLaunchNodeGone]). Counted, a host that restarted
// for a minute would have its runs given up as unreadable. ACKNOWLEDGED ONLY
// WHERE NO CLOCK RUNS ON THAT HOST'S STORE, AND THE RUN'S ROW SAYS NOTHING OF
// ONE: a host whose last answer was that its store did not answer it reads the
// run's row through that store once it is back, and announces nothing, so its
// collection's completion is handed back for the queue's backoff and renewed
// instead ([Coordinator.hostClocked], [Coordinator.unannouncedJob], renew.go).
//
// ErrStoreSilent is a step a collection cannot go on without that the
// coordination store did not answer: the host's read of the run's row, which
// licenses a read of its box ([Coordinator.ReadHostedBox]); the watch of the
// run's log a collection waits on its calls in flight by
// ([Coordinator.awaitCalls]); the record of the sessions its job ran out of
// context in or whose request its provider refused
// ([PendingStore.RetireSessions]); the record of its job lost with the host its
// box is on, where only that host has kept the box unread for the box timeout
// ([PendingStore.MarkClaimLost], [Coordinator.loseWithHost]); the record of the
// question its job asked, which parks the run ([PendingStore.MarkAwaiting],
// [Coordinator.park]); and the claim the collection is made under, where the
// store did not answer it or this node refused it while the store has yet to
// take the release of the last one ([Coordinator.claimCompletion]). Each wraps
// the store's error, or that refusal, in it ([storeSilent]). A completion
// handed back for it is this node's failure, never the message's: the engine's
// control handler hands it back as the node's own, so a delivery whose budget a
// store outage spends is not kept with its group's poison (internal/engine's
// controlOutcome).
//
// NOT A FAILURE OF THE BOX, which answered every read, nor of its host, which
// is up: a collection handed it gives its claim back UNCOUNTED and has its
// completion retried ([Coordinator.collectFailed]), and renewed where the
// silence is its box's host's alone (renew.go), a lost job's round is not
// carried on without its read for it ([Coordinator.collectGivenUp]), and a
// stop's read of it is asked again. Each lasts for as long as a silent host is
// waited for where the silence is the host's alone, a collection's by the clock
// on its host ([hostUnsettled]), past which its job is lost with that host
// ([LossLaunchNodeStoreSilent]), and a stop's by this node's own clock
// ([Coordinator.readStopped]), and for as long as the silence lasts where this
// node's own store shares it. Counted, a store silent for the give-up window
// settled a run whose box held its result as one whose result was lost, and
// carried a lost job's round on uncharged while its box held what the job had
// spent.
//
// storeSilent is err, the store's error at a step a collection cannot go on
// without, wrapped in [ErrStoreSilent]: the store did not answer it.
//
// UNLESS ctx HAS ENDED, which is then the cause and is returned as it is. ctx
// is the context of the node that makes the step, which at the host's read of
// the run's row ([Coordinator.ReadHostedBox]) is the HOST'S: its answerer's own
// lifetime, held to its share of the asker's time. On the node that collects,
// the collection judges it as it judges every read its context ended
// ([Coordinator.collectFailed]): a cancellation of this node's own is handed
// back uncounted whatever it cut, and a deadline the delivery ran out of is
// counted, or a collection that always outlasts its delivery would be asked
// again for ever, its last step the store's. On the host, the reply says which
// ([AnswerBoxRead]): a cancellation is the host stopping as it read
// ([BoxReadReply.Withdrawn]), which its asker hands back uncounted, and a
// deadline is the read running out of the asker's time, which it counts.
//
// the host reads only what the row licenses, `Coordinator.ReadHostedBox`, and a
// read its own withdrawal cut, draining or restarting, comes back flagged,
// `BoxReadReply.Withdrawn`, and is read as a host that did not answer,
// `ErrBoxHostSilent`, never as the box's failure), leaves the box's reclaim to
// its host and tells it (`Nudge`), and relaunches in a fresh box of its own;
//
// # Box read budget
//
// BoxReadBudget is how long a node waits for a box's host to make a read of
// run's box ([BoxReader]) — by what the read it asks may do there — before
// taking it for a host that did not answer.
//
// [pollBound] — sixty seconds, the control plane's whole-request budget — for
// the reason the poll is bounded by it: what the host does is the poll's own
// work (a reconnect, a probe) and the run's own files read back and its box
// paused, all of it on its own host. And the TAKE of the job's harness session
// beside it ([SessionTakeBudget]), and the room its reservation of the
// capture's place is given beside it ([Coordinator.reserveRoom]), with the room
// the take leaves after it: a grace for its own record (the stamp of what came
// of it, or the digest it leaves in the box), another for how a collection's
// job's harness ended ([Coordinator.markHarnessEnd],
// [Coordinator.takeRecordsRoom]), and a third for the pause, its record and the
// answer ([Coordinator.pauseRoom]). They are counted because the read is where
// the session is taken out of the box, on the host and before the box is
// paused: the host reads only for its share of the wait the request carries,
// counted from the moment the request reaches it, on its own clock
// ([BoxReadRequest.AnswerBudget]), so all of it is counted at that share, and a
// take at its bound fits whole a read the host makes as the request arrives
// rather than being cut by the host's own deadline. A read that first waited
// for one of the reads a host makes at once is handed only what is left of the
// share, so one that waited long may be cut by that deadline and answer that it
// could not read the box: reading on would take the session out of the box for
// an asker that had gone. Only the take: the upload and the record are made off
// the read, from the moment the session is spooled ([Coordinator.keepCapture]),
// so no asker waits on an upload.
//
// # Each change of the run's row the host's read makes, at the slowest crossing
//
// The reservation, the stamp of what came of the take, how the job's harness
// ended and the pause's record each read the run's row whole and write it whole
// ([boxReadChanges]), and the host may reach the store across a link the engine
// allows to be as slow as thirty seconds a mebibyte, where a change of a row at
// its bound takes thirteen minutes ([RowBudget]). The host gives each its
// crossing on its own connection; the asker cannot know which connection that
// is, so it counts the slowest node's, coord.MaxValueCrossing, for each, as
// every wait one node keeps on another node's change of a run's row does
// ([standbyBudget]). Held to their flat graces, each was cut while its bytes
// were still crossing: the place went unreserved, so no capture was made, or
// the stamp of what came of the take, or how the harness ended, went unrecorded
// (crewlet/crewlet#1151).
//
// AND A READ THAT MAY END THE JOB HOLDS THE JOB'S END BEFORE ITS TAKE
// ([JobEndBudget]): a stop's and a loss's end it first ([Coordinator.endJob]),
// and so does a collection's where the turn's cap has passed with the job still
// working ([Coordinator.readBox]), at most the end's wait and the kill past it,
// and that is counted at the host's share as the take is — and for a
// collection, the record that the cap ended a job it saw working, one more
// change of the row, made on its own context before the end's wait begins,
// within a teardown's grace and the row's crossing
// ([Coordinator.markEndedAtDeadline]), so it is counted at both, as every other
// change is at its reason and its crossing. The host's share of such a read's
// budget is then the end's 145 seconds and that record's grace, the take's 240
// and its reservation's room, its three graces, the crossings of the changes,
// and 45 — the poll's bound at the share — for the read's own steps, so a take
// after an end at its bound and its record at theirs is handed its whole budget
// all the same ([Coordinator.takeContext]).
//
// EVERY COLLECTION OF A TURN WITH A CAP holds the end, whether or not the cap
// has passed on the asker's clock: the host decides whether to end the job on
// its own clock, as it reads, and nothing in a completion tells the asker
// whether its job finished or the host's waiter announced it at the cap. A
// collection of a turn with none never ends its job, and waits for the take
// alone.
//
// AND A READ THAT ASKS ITS JOB'S MAIN THREAD ([asksMainThread]) HOLDS THAT READ,
// [MainThreadReadWait], at the host's share: a Claude Code collection, and a
// stop's or loss's whose copy holds a hook's serve ([PendingRun.asksReceipts]),
// which holds too the verdict on it, [hookVerdictWait] and one more change of
// the row at the slowest crossing. Every other read waits as it did.
//
// What that costs is the wait for a host that has truly stopped (about
// seventy-eight minutes for a collection of a turn with no cap, ninety-six
// with a served note; eighty-one and a half for a stop's or a loss's read,
// ninety-nine and a half with one; ninety-nine and a half for a collection of
// a turn with a cap, a hundred and seventeen with one), which a host that is
// only slow, ending a job, packing a session or changing a run's row at its
// bound across a link at the slowest pace allowed, must not be taken for.
// Ordinary reads answer in seconds. A read that has not finished in that is
// a host that is not answering, and is answered as one ([ErrBoxHostSilent]);
// the host's own waiter announces the completion again on its schedule once
// the host is back (reannounce.go). Only a node that asks ANOTHER node's host
// waits on it: a box this node hosts it reads itself.
//
// A cross-node box read waits by what it may do (`BoxReadBudget(run, read)`, at
// the host's share `BoxReadAnswerShare` plus the poll's bound: a collection of
// a turn with no cap about 4,700 s, some 78 minutes, for the take, its
// reservation's room, a teardown's grace for each of the take's two records and
// one for the pause (`Coordinator.takeRecordsRoom`, `Coordinator.pauseRoom`),
// and the crossing of each of the four changes of the run's row its host makes,
// at the slowest node's pace (`boxReadChanges`, `coord.MaxValueCrossing`); a
// stop's and a loss's about 4,893 s, some 81 and a half minutes, with the job's
// end on top (`JobEndBudget`); and every collection of a turn with a cap about
// 5,974 s, some 99 and a half minutes, with the end and the record that the cap
// ended a job it saw working (`recordsTheCap`) at its grace and crossing on
// top, since the host decides on its own clock whether the cap has passed;
//
// # Collection failures
//
// collectFailed answers a collection that could not read its box back: AGAIN,
// within a bound, and then the run is given up.
//
// IT IS SAFE TO ASK AGAIN, which the claim protocol is what proves. Nothing the
// tail writes happens before a collection succeeds (the charge, the launch's
// record, the park and the resume all follow it), so a failed collection has
// written nothing a retry does not find as it left it: the claim, handed back
// ([Coordinator.unclaim]); the facts its read records as it reads (the cap's
// sighting, the session it takes, how the job's harness ended, the box's
// pause), each of which the collection made again finds as it was left or makes
// again to the same effect; and, where its drain began or its turn's cap had
// passed, the job's session on the bridge ended ([CoordinatorOptions.Ended]),
// which a retry ends again to no effect. So the run is left as the completion
// found it: running, its box kept alive by the poll, its turn suspended. The
// completion is then NAK'd and comes back on the queue's backoff, here or on
// the seat's next owner, and the waiter announces the job again on its
// schedule, a fresh message at most [reannounceCeilingPolls] poll intervals
// after its last (reannounce.go); whichever claims first collects. Settling on
// the first failure would destroy the turn — and its charge and its record —
// over one transport blip, or over the drain whose cancellation was the
// failure.
//
// THE BOUND IS THE WAITER'S OWN, so the engine has one answer to "how long may
// a box be unreachable": at least [MinConnectFailures] CONSECUTIVE attempts
// spanning [ConnectGiveUp] since the last collection that read the box, counted
// on the job's record so a retry on another node, or after a restart, does not
// start a fresh allowance. Past it the run is settled as unreachable. A
// collection that read the box ends the run of failures behind it, as the
// waiter's reconnect that succeeds ends its streak: a box collected and then
// handed back because its resume failed is a box that answered, and its next
// failure starts a run of its own. So is a box whose read answered before a
// later step of its collection, one the coordination store did not answer or
// one this node cut short, handed the claim back uncounted
// ([Release.BoxAnswered]): left open, the run of failures from before a store's
// silence was measured across it, and the first failure after the silence gave
// up as lost a box that had answered every read. A later step its delivery's
// deadline cut is counted onto the run instead, as below.
//
// A BOX THAT IS GONE ([ErrBoxGone]) is settled at once: no attempt can read a
// box its provider reclaimed. It went between the poll that found its job over
// and this collection — a poll that finds a RUNNING job's box gone has already
// waited out that same window, and records the job lost so that its round is
// carried on instead ([LossBoxGone]).
//
// A COLLECTION THIS NODE ITSELF CANCELLED is handed back WITHOUT being counted.
// A drain, a restart and a seat moving to another node all end the delivery's
// context mid-read, and that says nothing about the box: counted, it opened the
// failure window at the moment of the drain, so the first real failure after a
// restart minutes later was already past the window and gave a reachable run up
// after one attempt. Only a cancellation is spared — a deadline the delivery
// ran out of is counted, because a box whose collection always outlasts it
// would otherwise be retried for ever.
//
// A STEP THE COORDINATION STORE DID NOT ANSWER ([ErrStoreSilent]) is handed
// back WITHOUT being counted as well: the host's read of the run's row, the
// drain's watch of its log, the verdict on its job's served notes and the
// record of the sessions its job ran out of context in or whose request its
// provider refused are the store's, and the box answered every read, so
// counted, a store silent for the window settled a run whose box still held
// its result as one whose result was lost. Its completion
// is NAK'd for the queue's backoff, as a counted retry's is, and not
// acknowledged as a silent host's is: that host announces it again once it is
// back, and here the host and the box are up. Where the store is this node's
// own, it has NO GIVE-UP OF ITS OWN: it is retried as a claim the store does
// not take is ([Coordinator.completeAdmitted]), each announcement bounded by
// the broker's deliveries of it and the waiter's schedule announcing the job
// again, which loses nothing whatever the deliveries come to
// ([Coordinator.collectFailedAt] says why), and a store silent that long holds
// every run where it is, the claim first, while its own alarms name it. Going
// on without the step would read the run without what the step stands for (a
// call still in flight, a session that ran out or was refused), and giving the
// run up would throw away a result its box holds. But where the silence is the
// box's host's alone, its store not answering the host's read of the run's row
// while this node's answers the claim, that premise fails: every write of this
// node's lands, and the collection was asked again for as long as the partition
// lasted. So such a host is given up after the box timeout, as a silent host is
// ([hostUnsettled], [Coordinator.collectGivenUp]): NOT HERE, by the collection
// that read the box, which records a finished job lost with that host and
// carries its round on ([Coordinator.loseWithHost]), and carries a lost job's
// round on without the read ([Coordinator.carryOnLost]). Until then no waiter
// announces the job again, since only that host's drives its box and it reads
// the run's row through the silent store, so while this node keeps its clock on
// that host's store, the completion of every hand-back is RENEWED before the
// delivery in hand is its last, whatever the hand-back (renew.go): NAK'd and
// nothing more, it was dead-lettered before the bound, which the broker's
// budget falls short of. A store's silence that reaches this hand-back is
// handed back, whatever the bound, and never ends the run as a box that could
// not be read. A step its delivery's deadline cut is the deadline's, and
// counted as above ([storeSilent]).
//
// EVERY HAND-BACK HERE THAT THE STORE DOES NOT ANSWER IS KEPT by this node and
// made again once the store answers, never turned into an ending for its
// silence ([Coordinator.unclaimAt], claimrelease.go): the store that does not
// answer a collection's step is very often the one that does not answer its
// release, and a run ended there was announced claim_unreverted, "its result is
// lost", while its box held the result.
//
// ONE VERDICT PER FAILURE, at one reading of the clock ([collectVerdict]):
// whether a store's silence is handed back and whether the collection is given
// up are both read off it, and a caller that judged the failure already hands
// its own verdict on ([Coordinator.collectFailedAt]).
//
// THE BOX'S HOST DID NOT ANSWER, which says nothing about the box
// ([ErrBoxHostSilent]): handed back uncounted, and the completion acknowledged
// rather than retried on the queue's backoff — the host's own waiter announces
// it again on its schedule once the host is back (reannounce.go), and one that
// never comes back is found gone by the fleet.
//
// UNLESS THIS NODE KEEPS A CLOCK ON THAT HOST'S STORE
// ([Coordinator.hostClocked]): the host last answered that its store did not
// answer its read of the run's row, and a host that did not answer since says
// nothing of that store, so its waiter, back or not, reads the row through a
// store that does not answer it and announces nothing. The delivery is then all
// that asks again: the completion is handed back for the queue's backoff, never
// acknowledged, and renewed before its last delivery as every hand-back is
// while that clock runs (renew.go).
//
// THE COORDINATION STORE DID NOT ANSWER a step the collection cannot go on
// without ([ErrStoreSilent]), which says nothing about the box: handed back
// uncounted, and the completion NAK'd for the queue's backoff. A host whose
// store alone has been silent for the box timeout is given up by the collection
// that read the box, never here: its job is lost with that host
// ([collectVerdict.lostWithHost]).
//
// WHERE THE STORE IS THIS NODE'S OWN, THE NAK IS ALL IT TAKES. Once the claim
// is back (now, or when a release this node owes lands, claimrelease.go), the
// run's row is running and unheld, and the waiter that drives its box announces
// such a job's completion on its schedule for as long as the row stays so,
// never stopping (reannounce.go): each announcement a fresh message with a
// fresh budget, due at most [reannounceCeilingPolls] poll intervals after the
// last. So a delivery that dead-letters meanwhile loses nothing. And at
// production's cadence that ceiling, 300 s, is inside the broker's delivery
// budget, about 601 s from a message's first delivery to its dead letter and
// never under about 481 s with its jitter (internal/queue/jetstream's
// LeastTimeToDeadLetter): while the waiter's own store lists the row, a fresh
// announcement is out before the last can dead-letter, so the completion always
// has a delivery in flight, and no dead letter the silence leaves is its last
// copy (TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter). A waiter
// whose store is silent too announces nothing while it is, and its first pass
// that lists the row again announces the job at once, its wait long past.
//
// WHERE THE SILENCE IS THE BOX'S HOST'S ALONE, NO WAITER ANNOUNCES THE JOB
// AGAIN: the host's own drives its box, and reads the row through the silent
// store. The broker's redelivery is all that asks again, and its budget falls
// short of the box timeout, so while this node keeps its clock on that host's
// store, by the host's answer to this read or to one before it (a record of the
// job's loss with that host this node's own store did not take,
// [Coordinator.loseWithHost], among them), the completion is renewed before the
// delivery in hand is its last: announced afresh, and the delivery acknowledged
// once that is out, as every hand-back is while that clock runs (renew.go). The
// wait goes on until the host's store answers or the bound records the job lost
// with that host.
//
// BoxAnswered is whether the claim's collection READ THE JOB'S BOX BACK and was
// handed back UNCOUNTED by a step after the read: one the coordination store
// did not answer ([ErrStoreSilent]), or one this node cut short by stopping. It
// ENDS the job's run of failed collections as Collected does (the release
// clears [LaunchRecord.CollectFailures] and [LaunchRecord.CollectFailingSince])
// and records nothing more: the job is not collected
// ([LaunchRecord.Collected]), so it stays stoppable, and the collection made
// again charges and records it.
//
// The bound is on failures OF THE BOX, consecutive, and a box that answered
// while the store was silent is a box that answered. Left open behind it, the
// run of failures from before the silence was measured across it, and the first
// failure after it gave up as lost a box that had answered every read in
// between. A release that counts a failed collection cannot say it too, and one
// that does is refused: a counted failure continues the run of failures it is
// counted onto, or a collection that always outlasted its delivery after
// reading the box would start a run of its own each time and never be given up.
//
// reannounceCeilingPolls is the most poll intervals a job its holder has not
// resumed waits between two announcements: twenty, which at
// [DefaultPollInterval] is five minutes.
//
// IN POLLS, NOT A DURATION, for the reason [ConnectGiveUp] gives the other way
// round: the schedule is the poll's, and a waiter driven at a faster cadence
// (the e2e suite's 100 ms) keeps the same shape at its own scale, where a fixed
// five minutes would leave a job its condition released unannounced for longer
// than any of its tests run. At the default cadence a condition that clears
// with nothing to signal it resumes its job within five minutes, and a job held
// for a day is announced a few hundred times rather than on every poll. The
// resume give-up ([ResumeGiveUp]) still sees several attempts inside its half
// hour.
//
// AND INSIDE THE BROKER'S DELIVERY BUDGET, which nothing else keeps it. A
// completion whose claim the holder's own store does not answer is handed back
// NAK'd and nothing renews it ([Coordinator.collectFailedAt]), so its next
// announcement has to be out before its last can be dead-lettered: at the
// default cadence the ceiling, the poll that makes the announcement and that
// poll's own bound come to under seven minutes, and the broker carries a
// failing message at least about eight (internal/queue/jetstream's
// LeastTimeToDeadLetter).
// TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter holds the two apart,
// and a ceiling raised past it would leave a completion whose only copy is its
// dead letter while a store is silent.
//
// # The clock on a box host's store
//
// hostUnsettled is how long the host of a job's box has answered this node's
// collections of the job that its own coordination store did not answer
// ([BoxReadReply.Unsettled]): since the first of them, one after the other.
//
// A HOST WHOSE STORE ALONE IS SILENT IS BOUNDED AS A SILENT HOST IS. Only the
// host can read a box on its host, and a host that answers its store did not
// answer the read of the run's row, which licenses the read, never reads the
// box. Where this node's own store answers (its claim of the job, the write
// every collection is made under, landed), the silence is the host's alone: a
// partition that cuts the host off from the store's leader and not this node.
// Handed back uncounted with no give-up, as every step the store does not
// answer is, such a collection was asked again for as long as the partition
// lasted, its seat held behind it, while a host that did not answer at all is
// taken for gone after the box timeout ([Waiter.goneAfter]). So it is given up
// after that same bound, measured here ([Coordinator.collectGivenUp]), as that
// waiter gives a gone node's jobs up: recorded lost with the host, and their
// rounds carried on ([Coordinator.loseWithHost], [LossLaunchNodeStoreSilent]).
// And the bound is REACHED: no waiter announces such a job again, since the
// host's own reads the run's row through its silent store, so while the clock
// runs the collecting node renews the completion of every hand-back, whatever
// its kind, before the broker's budget for its delivery runs out, which is
// shorter than the box timeout (renew.go).
//
// IN THIS NODE'S MEMORY, as the waiter's clock on an absent node is: a restart
// or the seat's next holder starts it again, which can only make the verdict
// later. What it says of the host is not left to memory, though: every claim's
// release writes it onto the job's record ([LaunchRecord.HostStoreSilentSince],
// [Coordinator.hostStoreSeen]), so a restart or the seat's next holder hands
// back and renews the job's completion as the clock would before its own first
// collection starts one (renew.go). A silence this node's own store shares
// gives nothing up: no claim of the job lands while it lasts, so no collection
// is made to judge it.
//
// IT IS KEPT UNTIL THE RUN SETTLES, and goes where the run's collection lands
// its outcome, whatever that comes to: the job collected, its host answering a
// read its store answered ([Coordinator.noteHostStore]); the run ended, its
// collection given up or any ending of it decided ([Coordinator.collectFailed],
// [Coordinator.endRun]); or the job lost and its round carried on, the turn
// launching its next job here ([Coordinator.Launch]) or its end decided; and it
// goes when the seat is let go ([Coordinator.ReleaseSeat]). It goes too at any
// read its host answers with its store answering, whatever the read came to,
// the box's own failure to be read included ([Coordinator.noteHostStore]): the
// silence it times is over, and that host's waiter announces the job again from
// there. And it goes at that waiter's announcement of the job made from a read
// of the run's row through that store asked after the clock started
// ([Coordinator.hearAnnouncement], [rowAskedField]): kept past it, while a
// refusal that settles nothing lasted, every announcement the waiter made from
// there was handed back and renewed beside the one already in flight, one more
// chain for each. A FAILED ATTEMPT IS OTHERWISE NO SETTLE, and keeps it: a
// hand-back that has the job collected again, and a loss recorded with the host
// ([Coordinator.loseWithHost]) whose carry or resume then fails, whose next
// collection finds the bound passed at once and whose completion is renewed
// meanwhile (renew.go), since the host's waiter still announces nothing.
// Dropped at the recorded loss, a carry that failed for longer than one
// delivery's budget left the run with nothing to drive it; and kept until the
// seat was let go, a clock outlived every run that ended, or went on to its
// next job, with no remote collection after it: a stop's, a deadline's, a lost
// job's round carried on without its read.
//
// hostClocked reports whether this node keeps a clock on the store of the host
// of the box of turnID's job launch ([hostUnsettled]): the host last answered a
// collection of it that its store did not answer, and has answered nothing else
// since. While it does, the host's waiter, which reads the run's row through
// that store, announces nothing, and every hand-back of the job's completion is
// renewed (renew.go).
//
// NO RUN ANOTHER PARTY ENDS OUTLIVES IT WHILE THIS NODE HOLDS THE SEAT, so it
// goes only where this node settles the run ([hostUnsettled]) or lets the seat
// go, and never has to hear of an ending decided elsewhere. Every ending is
// decided by a coordinator's own [Coordinator.endRun], the one caller of
// [PendingStore.DecideEnding], which drops the clock on the node that decides
// it ([Coordinator.runSettled]); the decision's license is fenced to the
// deciding node's seat lease ([outranked]); and the collection that started the
// clock claimed the job under this node's lease, which stamps that lease's
// epoch on the row ([PendingStore.ClaimForResume]). So the seat's earlier
// holder is outranked, or refuses the write up front once it knows it lost the
// seat ([CoordinatorOptions.Lease]), and a later holder exists only once this
// node's lease is gone, whose release ([Coordinator.ReleaseSeat]) drops the
// clock with the seat, renewals made before this node notices reaching the new
// holder. A job its host's waiter records lost ([PendingStore.MarkLost]) is no
// ending, and the clock is kept for it on purpose ([hostUnsettled]).
// TestNoOtherPartyEndsARunWhoseHostThisNodeClocks holds the fence this rests
// on.
//
// # Unannounced jobs
//
// HostStoreSilentSince, where set, says the host of this job's box last
// answered a collection of it that its own coordination store did not answer
// its read of the run's row ([BoxReadReply.Unsettled]), and has answered
// nothing since that says its store answers, and is when the collecting node's
// clock on that store started, by that node's clock: the DURABLE TWIN of that
// clock ([hostUnsettled]), written by the release of every claim whose
// collection found it so ([Release.HostStoreSilentSince]), the latest such
// clock's start replacing an earlier one, and cleared by the release of one
// that learned that store answers ([Release.HostStoreAnswered]). A release that
// learned nothing of that store, a host that did not answer at all among them,
// leaves it as it stands, and the job's next launch starts without it.
//
// AN INSTANT, NOT A FLAG, because a waiter's announcement of the job made from
// a read of its row asked after it ends it ([announcedAfter]), and one whose
// read was asked before it does not: that waiter read the row through the store
// this records as silent, and says when it asked ([rowAskedField]), so a read
// asked later says the store answered since, while one asked earlier says
// nothing of it, however late its announcement was made or delivered.
//
// ON THE JOB'S RECORD because the clock is in one node's memory: while that
// host's store is silent its waiter announces nothing, so a completion's
// delivery is the one copy of it, and a node that restarted, or the seat's next
// holder, refused that delivery on a condition and acknowledged it before any
// collection of its own had started a clock, which left the job announced by
// nothing until that store answered. Every node reads the row before it
// acknowledges a delivery no clock of its own covers, and hands back and renews
// one whose job this says is unannounced (renew.go).
//
// # A completion is renewed while the clock on its box's host runs
//
// A collection whose box is on another node's host asks that host to read it,
// and a host whose coordination store does not answer its read of the run's row
// reads nothing, while this node's own store answers the claim
// ([hostUnsettled]). This node then keeps a clock on that host's store
// ([Coordinator.hostClocked]): the claim is handed back uncounted and the
// completion NAK'd, and asked again until the host's store answers or the box
// timeout passes, when the job is recorded lost with that host and its round
// carried on ([Coordinator.loseWithHost]). And the claim's release writes what
// the host said onto the job's record ([LaunchRecord.HostStoreSilentSince]),
// the clock's durable twin, which the release of a collection that reads the
// box clears.
//
// WHILE THAT CLOCK RUNS, THE BROKER'S REDELIVERY IS ALL THAT ASKS AGAIN. Every
// other hand-back of a running, unheld job has the waiter that drives its box
// behind it: its next announcement is a fresh message with a fresh budget
// (reannounce.go). But a box on a node's host is driven by that node's own
// waiter alone, and that waiter reads the run's row through the very store that
// is silent, so it announces nothing for as long as the silence lasts. The
// broker carries a failing message for about ten minutes before its dead
// letter, never under eight (internal/queue/jetstream's LeastTimeToDeadLetter),
// and the box timeout is fifteen by default: NAK'd and nothing more, the
// completion was dead-lettered before the bound was reached, the job was never
// recorded lost, and the seat was held behind a run nothing drove again until
// the host's store answered.
//
// SO NO DELIVERY OF THE COMPLETION IS ACKNOWLEDGED WHILE THE CLOCK RUNS, OR
// WHILE THE RUN'S ROW SAYS NO WAITER ANNOUNCES ITS JOB ([unannouncedOnRow]),
// unless the run settled, which drops the clock ([hostUnsettled]) and moves the
// row on: every other answer hands it back, marked [ErrUnannounced], and renews
// it, WHATEVER ITS KIND ([Coordinator.renewing]). The host's own answer that
// its store did not answer; a claim this node's own store does not answer, or
// one this node refuses while a release it owes is out ([errReleaseOwed]); a
// host that does not answer the read at all ([ErrBoxHostSilent]), which is
// acknowledged only where no clock runs and the run's row says nothing of one;
// a record of the job's loss this node's store did not take
// ([Coordinator.loseWithHost]), or a carry of its round that fails after it; a
// refusal of the node's gate, or a completion this node holds off itself
// ([Coordinator.keepRefused]), which is kept for its condition's signal
// besides; and an answer that settled nothing, a claim another attempt holds or
// a resume the run's row holds for the seat's budget ([HeldError]), whose
// host's waiter would announce the job once that hold lifts; and a claim the
// seat's recovery gate refused while a recovery pass of the seat ran here
// ([ErrSeatRecovering], recoverygate.go), which the broker brings back past the
// pass. This node's memory of the job, a completion it keeps for a condition
// included, outlives neither its restart nor its letting go of the seat, so
// while the clock runs the delivery is the one copy of the completion that
// does. KEYED ON THE CLOCK, NEVER ON THE HAND-BACK'S KIND: renewed for the
// host's answer alone, a claim or a release the store did not answer meanwhile
// was NAK'd until its budget was spent, and a host that did not answer for a
// minute, a refusal whose condition nothing signals clearing, and a resume held
// on its row each had their completion acknowledged, and each left the job with
// nothing to drive it.
//
// Before the delivery in hand is the last its budget allows ([renewReserve],
// [queue.DeliveriesLeftFor]), the completion is announced afresh: a new event
// of the same completion ([events.New]: a fresh id, which the stream's
// duplicate window would otherwise drop, and with it a fresh budget), to the
// seat's control topic, PUBLISHED BEFORE THE DELIVERY IN HAND IS ACKNOWLEDGED,
// which it then is, so the fresh one carries the completion on and each renewal
// leaves one announcement in flight, never a growing number. A renewal the
// broker does not take leaves the delivery in hand NAK'd, with deliveries still
// left to make it again on. A duplicate is harmless either way: the claim is
// made once.
//
// AND ON THE ROW, NEVER ON ONE NODE'S MEMORY ALONE
// ([Coordinator.unannouncedJob]). The clock goes with a restart, and the seat's
// next holder starts with none, while the host's waiter still announces
// nothing. So a node that keeps no clock reads the run's row before it lets a
// delivery go, and keys the same hand-back on what the row says durably: the
// job recorded lost with that host for its store's silence
// ([LossLaunchNodeStoreSilent]), until its round is carried on, which a carry's
// failure after a restart has no collection to start the clock again for, since
// such a loss's carry reads no box; or the job's record saying that host last
// found its store silent, which a refusal on a restarted node or the seat's
// next holder meets before any collection of its own has started a clock. Kept
// by memory alone, a carry that failed after a restart was NAK'd until its
// budget was spent, and such a refusal was acknowledged, each leaving the job
// with nothing to announce it until that host's store answered.
//
// A WAITER'S ANNOUNCEMENT FROM A READ SINCE ENDS BOTH. That waiter's
// announcement says when it asked its store for the run's row it was made from
// ([rowAskedField]): it announces the job only having read the row through the
// store the clock times, so one whose read was asked after the clock started,
// or after the instant the job's record holds, says that store answered since
// ([announcedAfter]). It ends this node's clock as it is delivered
// ([Coordinator.hearAnnouncement]) and the record at the release a claim of it
// makes ([Coordinator.hostStoreSeen]), and a refusal of it is acknowledged as
// any is where a waiter stands behind it (ADR-0063). One whose read was asked
// before ends nothing, however late it was made or delivered: the waiter polls
// the box between its read and its announcement, which can take a minute
// ([pollBound]) after a slot's wait, so a silence that began in between and was
// found meanwhile was, by the announcement's stamp, read as over while it
// lasted. Nor does a renewal, which reads no row and so says no read
// ([Coordinator.renew]). Ended by nothing, the clock and its record stood
// through every announcement that waiter made once its store answered again:
// while a refusal that settles nothing lasted, each was handed back and renewed
// beside the chain already in flight, one more for every announcement, about
// twelve an hour. The chain in flight is renewed on its own budget until a
// release clears the record, one chain, as before. A fleet mixed with a build
// from before renewals existed is unchanged by the read's instant, which rides
// the envelope's bag every backend carries verbatim and every republish keeps:
// that build's host never answers that its store did not answer its read
// ([BoxReadReply.Unsettled] is this build's), so no node keeps a clock or a
// record on a job whose box it hosts, and its waiter's announcements, which say
// no read, end nothing; and holding the seat, that build neither renews nor
// reads the instant, and acknowledges a refusal and NAKs a failure as it always
// did.
//
// AND A REFUSAL ACKNOWLEDGED BEFORE ANY NODE HEARD OF THE SILENCE IS OWED ON
// THE ROW. A refusal of a job no collection has read since its host's store
// went silent finds no clock and nothing on the row to say so, since no node
// has heard that host say it, so it is acknowledged as any is where a waiter
// stands behind it, and kept for its condition's signal, whose re-fire's
// collection starts the clock and renews the completion at once. That keep is
// this node's memory, and a silence of the store's runs alone has no bound: the
// host's lease and presence are another stream, so no watchdog ends that host
// and no peer finds it gone. So the refusal is written on the job's record
// before its delivery is acknowledged ([LaunchRecord.RefusalOwed],
// [Coordinator.oweRefused]), as every acknowledged refusal is, and the seat's
// recovery, this node's after a restart or the seat's next holder's, re-arms it
// and asks its own gate at once ([Coordinator.rearmOwed]); a record the store
// does not take is no acknowledgement, and the delivery is handed back as this
// node's failure. Kept in memory alone, a restart or a letting go of the seat
// before that signal left the job with nothing to announce it for as long as
// that host's store was silent. And WITH NO RESTART AND NO SEAT MOVE, the keep
// re-fires itself at its refusal's instant, no later than [answerWaitBackstop]
// unless this node holds it off itself until that instant (whose backstop
// before it asks only whether its turn's deadline has passed, and ends the run
// if so, [Coordinator.recheckHeld]), and at the backstop where it names none
// ([Coordinator.keepLocked]), as a recovery's re-arm and a re-fire that settled
// nothing do ([Coordinator.keepAgain]): a condition that lifts on the clock
// with no edge (a lease merely stale, a provider's limit at its instant) left
// the kept completion waiting on this node, for the signal that never came and
// the announcement that host's waiter never made, for as long as that store's
// silence lasted. So is a completion whose resume its run's row holds for the
// seat's budget, whose delivery the hold spends where no clock runs: kept
// unheld, re-fired at the hold's instant and never sooner, since no claim wins
// before it, and on its condition's signal once its own lift of the hold has
// landed, a signal during the resume the budget held included
// ([Coordinator.keepHeldOnRow]), every lift of a hold the signal makes
// re-firing the job besides unless this node carries it already, kept for
// anything but that hold or a re-fire of it on its way, whose end re-fires it
// where it left it kept for that hold ([Coordinator.refireLifted],
// [Coordinator.carriedLocked], [Coordinator.refired]), kept through a delivery
// of it or a re-fire that meets the hold ([Coordinator.forgetAdmitted],
// [Coordinator.keepHeldClaim]), and kept again by the seat's recovery from the
// row ([Coordinator.keepHeldRows]), or, once a lift has cleared the hold,
// re-armed by it from the refusal the release that wrote the hold owed on the
// job's record ([Release.Hold], [Coordinator.rearmOwed]); nothing else
// announced the job once its hold had lifted while that host's store was
// silent. Its re-fire's collection starts the clock where the silence still
// stands.
//
// WHAT IT COSTS: a hand-back that settles nothing for as long as it lasts is
// NAK'd again at the queue's backoff, at most every thirty seconds, each
// delivery one claim attempt or one admission and, where this node keeps no
// clock and the delivery read no box, one read of the run's row; and renewed
// about every seven and a half minutes, since the delivery with [renewReserve]
// left is the twentieth of the broker's twenty-five, after nineteen returns
// spaced 1, 2, 4, 8 and 16 seconds and fourteen times thirty (451 s, about 361
// s at the floor of the backoff's jitter; internal/queue/jetstream's
// LeastTimeToDeadLetter). The longest such is a carry whose resume its row
// holds for the seat's budget while the clock runs, for as long as that hold
// lasts. A duplicate the claim makes once, and nothing dead-letters.
//
// A RE-FIRE HAS NO DELIVERY IN HAND. A completion this node kept for a
// condition and re-fires once it clears ([Coordinator.refire]) was kept from a
// delivery that was acknowledged where no clock ran then, and handed back where
// one did. So a re-fire is renewed at once only where its own attempt started
// the clock, its claim having won and its read of the box found the host's
// store silent, and that clock still runs once the attempt is done: nothing
// else carries it then. One whose own attempt then settled the run, its
// hand-back refused and the run ended, announces nothing, the run's end having
// dropped the clock with it. A re-fire whose claim lost to a delivery's, or
// that found the clock that delivery's collection started, announces nothing:
// that delivery, handed back, carries it. Where the clock ran before it, the
// delivery refused while the clock ran is in flight still, handed back and
// renewed on its own budget, and a re-fire that announced it afresh beside it
// would leave two announcements in flight, and more with every refusal and
// re-fire after; and so does one the run's row handed back, which it carries
// the same way. Nor does any delivery bring back a re-fire whose renewal the
// broker does not take, so this node makes that renewal again on its own timer
// until one lands or this node settles the run ([refirePublish]), past the
// seat's release and a lease it lost, and its own line says so, never that a
// delivery was handed back to make the renewal again: WHAT A REFUSED RENEWAL
// LEAVES IS ITS CALLER'S TO SAY ([Coordinator.renewing], [Coordinator.refire]).
// A re-fire the seat's recovery gate refused is renewed by nothing and kept for
// no backstop: the pass that refused it re-fires it when it is done
// ([Coordinator.carryPastRecovery]).
//
// NOTHING IS RENEWED WHILE NEITHER THE CLOCK NOR THE ROW SAYS SO: a step this
// node's own store did not answer is NAK'd and nothing more, since the waiter
// that drives the box, whose store does answer or answers once the silence is
// over, announces the job again on its schedule
// ([Coordinator.collectFailedAt]); and a delivery whose count the transport did
// not state is NAK'd as before this existed, since a missing number is no
// evidence of a spent budget.
//
// renewReserve is how many further deliveries a completion's delivery has when
// a hand-back while the clock on its box's host runs is announced afresh
// ([Coordinator.renewing]): the renewal is made on the delivery that has this
// many left, or on any after it that comes back.
//
// FIVE, AS [AnswerDeliveryReserve] IS, and for its reasons: what can spend a
// delivery between the renewal and the dead letter is the renewal's own publish
// failing (one each try) and a seat handoff while the delivery is in flight (a
// deferral, one or two). Five span about two and a half minutes at the broker's
// 30 s ceiling, which outlasts a stream's leader election many times over and a
// seat lease's turnover. Renewed sooner, every delivery would be a fresh
// message the broker hands over at once, a box read made again at the pace of
// the host's answer rather than the backoff's. Not derived from the broker's
// budget, for the reason [AnswerDeliveryReserve] gives.
//
// renewing is what a delivery of ev's completion leaves with the broker, err
// being what the coordinator made of it ([Coordinator.OnCompleted]): err as it
// is, unless this node keeps a clock on the store of the host of the job's box
// ([Coordinator.hostClocked]) once that answer is in, or, keeping none, the
// run's row says nothing else announces the job ([Coordinator.unannouncedJob]).
// Then the run has not settled, which drops the clock and moves the row on, so
// the delivery is handed back marked [ErrUnannounced], err or no err; and where
// trigger, the delivery in hand, has at most [renewReserve] deliveries left,
// the completion is announced afresh ([Coordinator.renew]) and nil is answered
// instead, so the delivery is acknowledged and the fresh one carries the
// completion on. A renewal the broker does not take answers the hand-back
// beside why, so the delivery is NAK'd and comes back to make it again, which
// its line says (sandbox_completion_renew_failed).
//
// unannouncedJob reports whether nothing but a delivery of ev's completion
// announces it again: this node keeps a clock on the store of the host of the
// job's box ([Coordinator.hostClocked]), or, where it keeps none, the run's row
// says so ([unannouncedOnRow]), read here; trigger is the delivery in hand, nil
// for a re-fire, which has none.
//
// THE ROW, NEVER THIS NODE'S MEMORY ALONE: the clock does not outlive a
// restart, and the seat's next holder starts with none, while the host's waiter
// reads the row through its silent store and announces nothing either way. A
// row this node's own store does not answer says nothing, and the delivery is
// left as it came: the waiter, whose store answers once this node's does,
// announces the job again then.
//
// AND ONLY WHERE THE DELIVERY'S OWN COLLECTION READ NO BOX ([boxWasRead]): the
// box's host reads the run's row through its store before it reads the box, so
// a read of it says that host's waiter announces the job again from there, and
// the release of a claim that read the box clears the record anyway
// ([Coordinator.hostStoreSeen]). Read after every delivery, a successful one
// cost a read of the coordination store for an answer that could not change
// what it left. What is left reads it: a refusal of the node's gate, which
// would otherwise be acknowledged while no waiter can announce the job, a
// hand-back made before the box answered, an answer that settled nothing, and
// the carry of a job lost with its host, which reads no box.
//
// rowAskedField is the envelope bag key under which a waiter's announcement of
// a job's completion says when that waiter asked its coordination store for the
// run's row it announced the job from ([askedRow]): an instant in RFC 3339, in
// UTC, which every backend carries verbatim and every republish keeps, as
// [queue.PriorityField] is.
//
// THE READ'S INSTANT, NEVER THE ANNOUNCEMENT'S. What the node holding the job's
// seat reads off it is that the store of the host of the job's box answered
// that waiter after an instant this node can compare with its own clock on that
// store ([announcedAfter]). The announcement's own stamp
// ([events.Event.Timestamp]) is taken once the waiter has polled the box, which
// can take a minute ([pollBound]) after the wait for a slot: compared instead,
// an announcement whose read that store answered before a silence that began
// during the poll, made after a collection had found the silence, was read as
// the silence's end while it lasted, its clock dropped and its record cleared
// by a claim that learned nothing more.
//
// announcedAfter reports whether trigger, a delivery of a job's completion, is
// a waiter's announcement of it made from a read of the run's row asked after
// since, the start of a clock on the store of the host of its box or its record
// of it: the read it says it was made from ([rowAskedField]) asked after that
// instant, which a renewal, reading no row, never says. That waiter read the
// row through the store the clock times, so the store answered since.
//
// STRICTLY AFTER, BY THE READ'S OWN INSTANT, never the announcement's stamp
// ([events.Event.Timestamp]): an announcement whose read was asked before the
// clock started says nothing of the silence the clock found, however late it
// was made or delivered, its first delivery among them, and read as that
// store's answer it dropped the clock while the store was still silent, and the
// delivery that was the job's one copy was acknowledged. One that says no read,
// a build's that stamps none, says nothing either. The instant is the waiter's
// node's clock and since the collecting node's, as a hold's instant is written
// by one node and read by another's ([PendingRun.Held]): an announcement read
// wrongly takes a skew between the two larger than the time from the waiter's
// read to the collecting node's receipt of the host's answer that its store did
// not answer, which that host's own read, its bound and the answer's carry are
// part of.
//
// renew announces ev's completion afresh on its seat's control topic, for a
// hand-back while the clock on its box's host runs, and answers why it could
// not where the broker did not take it. What that leaves is the caller's to
// say: a delivery in hand comes back to make it again ([Coordinator.renewing]),
// and a re-fire, which has none, is made again on this node's timer
// ([Coordinator.refire], [refirePublish]).
//
// TRIGGER'S TRACE AND SOURCE, so the resume it leads to nests under the turn
// that launched the job, as the waiter's own announcement does; trigger may be
// nil, which carries neither. NEVER THE READ TRIGGER SAYS ([rowAskedField]): a
// renewal is made because no waiter can read the run's row, and reads none
// itself, so it says no read and no node takes it for that waiter's
// announcement made since a silence began ([announcedAfter]).
//
// refirePublish is a re-fire's announcement afresh that the broker did not take
// ([Coordinator.refire]), which this node makes again on its own timer.
//
// A RE-FIRE HAS NO DELIVERY TO COME BACK ON. It renews only where its own
// collection started the clock on its box's host, so the delivery it was kept
// from was acknowledged, no clock running then, and neither that host's waiter
// nor anything else carries the completion: a renewal the broker refused, left
// so, held the job until that host's store answered. So it is published again
// after the hand-back's spacing ([answerRetryDelay]: a second, doubling to
// thirty, the queue's own failure backoff), each refusal arming the next
// attempt, and KEPT UNTIL ONE LANDS OR THIS NODE SETTLES THE RUN
// ([Coordinator.runSettled]): from there whatever settled the run has announced
// what became of it. NOT DROPPED WHEN THE SEAT IS LET GO, NOR AT A LEASE THIS
// NODE LOST ([Coordinator.ReleaseSeat]): the announcement goes to the seat's
// control topic, which takes a publish under no lease and which whoever holds
// the seat next reads, and nothing else would tell that holder of the job,
// since the delivery the re-fire was kept from was acknowledged and the host's
// waiter announces nothing; dropped there, the completion waited for that
// host's store. Outside the clock, should the host's store answer again
// meanwhile, that host's waiter's schedule stands behind it as well; inside,
// nothing but this does. [Coordinator.Stop] cancels its armed attempt, and it
// is IN THIS NODE'S MEMORY, as the kept completion it came from is: a restart
// forgets both.
//
// # Lost jobs
//
// Lost is the verdict that this job was lost with the host it ran on
// ([JobLoss]): a RUNNING job, before anything could see it end, killed with its
// engine by a restart, its box gone from its host, or its launch node gone from
// the fleet for good; or a job, finished or not, whose box only its node's host
// can read, on a node that could not read the run's row off the coordination
// store for the box timeout ([LossLaunchNodeStoreSilent]). Nil for every other
// run.
//
// A VERDICT, RECORDED, because the one who reaches it is not always the one who
// acts on it: the waiter that drives the job's box finds the first three, and
// the seat's holder carries the job's round on from the completion the waiter
// announces for it ([Coordinator.carryOnLost]) — which waits on the seat's
// control topic for the seat's next holder where no node holds it, and is
// announced again on the waiter's schedule until a claim takes it
// (reannounce.go). The fourth the seat's holder finds itself, and records on
// the claim its collection holds before it carries the round on
// ([Coordinator.loseWithHost]), so a claim of the job that comes after carries
// it on rather than collecting it. On the row rather than in that announcement,
// so a completion anybody could put on the broker cannot have a run's round
// started again. Cleared by a relaunch: the verdict was about the job before
// it.
//
// MarkClaimLost records on a job CLAIMED for its collection the verdict that it
// was lost with the host its box is on ([PendingRun.Lost]): the claim's own
// collection's, which found that host unable to read the job's row for the box
// timeout ([LossLaunchNodeStoreSilent]), so that the claim carries the job's
// round on and the relaunch that does counts the loss
// ([PendingRun.RoundLosses]). Reports whether the row carries a verdict for the
// job now, written by this call or by an earlier one, which stands.
//
// PINNED TO THE CLAIM the caller holds: the job (launch), the claim
// ([StatusResumed]) under fence, which a newer lease outranks, and, for a box
// on one node's host, that node (loss.Node). A job still running is the
// waiter's to judge ([PendingStore.MarkLost]), and a claim of another job,
// given back, outranked, or whose ending is decided is a claim no collection
// holds: false with no error, as is a row that is gone. A zero loss.At is the
// store's clock.
//
// A run whose job was LOST with the host it ran on, and the round carried on.
//
// A JOB CAN GO WITHOUT ENDING. A `direct` box's job dies with its engine's
// restart ([JobState]), a box root that did not outlive its node takes the job
// with it, a VM is reclaimed while the engine is away past the box timeout, and
// a node that leaves the fleet for good takes every local box it held
// ([Waiter.goneAfter]). None of those is a run that finished, or one that
// failed: the job was cut off mid-step by something outside it, with nothing
// written to say how far it got. Collected, its partial output would be judged
// as the round's result — a run that "did not succeed" for reasons it never had
// — and where its box was gone the run would end the turn, which the park had
// already acknowledged, so nothing would ever start it again. And a job whose
// box only its node's host can read, on a node cut off from the coordination
// store for the box timeout, is as lost to the turn as one whose node left,
// whether or not it finished ([Coordinator.loseWithHost]).
//
// SO A LOST JOB'S ROUND IS CARRIED ON, as a parked run's is once its question
// is answered ([ResumeRequest.Lost], [ResumeRequest.Asked]): the turn resumes
// and relaunches its executor for the same round, its calls so far kept on the
// row ([PendingRun.Continues]), in the box the lost job used where that box is
// still there and this node may continue in it — its working tree as the job
// left it — and in a fresh one from whatever the job pushed otherwise. What the
// job spent up to then is read off its box where the box is left, charged and
// published as its own record, exactly once, as a finished job's is.
//
// NOT FOREVER. A round whose jobs keep being lost is a run that takes its own
// host down — it exhausts the memory its container is held to, say, which on a
// Kubernetes node with cgroup v2 kills every process in the container, the
// engine included — and every relaunch takes the node's other seats down with
// it again. So a round is carried on after at most [MaxRoundLosses] losses, and
// the next ends its executor phase, and so its turn, saying why.
//
// carryOnLost is a completion's claim of a lost job carried on: what the job
// did up to then read off its box where its box is left, charged and published
// once, and the turn resumed to carry the job's round on ([LostRun]) — or,
// where the job had asked a person something before it went, parked on that
// question as a finished job is.
//
// THE READ IS ASKED AGAIN like any collection ([Coordinator.collectFailed]): a
// box that did not answer for a moment hands the claim back for the
// completion's retry, so the job's spend is not lost to one transport blip. One
// that is gone by now, or has not answered for the give-up window, is no reason
// to hold the round any longer: it is carried on without the read, and the line
// says the job's spend went with its box. A coordination store that did not
// answer one of the read's steps is no box that did not answer: the claim is
// handed back for as long as the store is silent, and the round is not carried
// on without the read for it, unless the silence is the box's host's alone and
// has lasted the box timeout, as a silent host's would
// ([Coordinator.collectGivenUp]). A read that failed is judged ONCE, at one
// reading of the clock, and the hand-back acts on that verdict
// ([collectVerdict]).
//
// IT RESUMES NOTHING ITSELF: it returns the carry for its caller, the frame
// that claimed the completion under the node's admission
// ([Coordinator.completeAdmitted]), to resume the turn with — the one frame
// that may, so this road to a turn passes the admission as every other does
// (ADR-0063). A nil carry with a nil error is a claim this settled already:
// handed back for the completion's retry, or parked on the job's question.
//
// collectVerdict is what one collection attempt decided about a failure of its
// read: whether it is past asking again ([Coordinator.collectGivenUp]), and,
// where the failure is the box's host answering that its coordination store did
// not answer it, for how long that host has ([hostUnsettled]).
//
// DECIDED ONCE PER ATTEMPT, at one reading of the clock, by the step that
// judges the failure first, and handed to every step after it that acts on the
// same failure. A lost job's read is judged by its carry
// ([Coordinator.carryOnLost]), which asks again where the read is not past
// asking again, through the collection's hand-back
// ([Coordinator.collectFailed]); each judged the failure at its own reading of
// the clock, so a bound crossed between the two had the hand-back give up, and
// end as unreachable, the run of a lost job whose carry had just found its read
// too young to carry the round on without it.
//
// collectGivenUp is the verdict on a collection that failed with cause, at now
// ([collectVerdict]): whether it is past asking again, which it is where its
// box is gone, or where it has failed for [MinConnectFailures] attempts
// spanning [ConnectGiveUp] since the last collection that read the box, the
// waiter's own bound on an unreachable box. See [Coordinator.collectFailed].
//
// NEVER A STEP THE COORDINATION STORE DID NOT ANSWER ([ErrStoreSilent]),
// whatever the window counted before it: the bound is on a box that does not
// answer, and the store's silence says nothing about the box. The host's read
// of the run's row comes before the box is asked at all, and every other such
// step comes after the box's read, where the job's box is read at all (a lost
// job whose box went with its host has none); a read the box answered ends the
// run of failures behind it as the claim goes back ([Release.BoxAnswered]).
// Given up, a lost job's round was carried on without its read
// ([Coordinator.carryOnLost]), uncharged for what its box still held.
//
// BUT A HOST WHOSE STORE ALONE IS SILENT IS GIVEN UP AS A SILENT HOST IS: one
// that has answered this node's collections of the job, for the box timeout,
// that its store did not answer its read of the run's row, while this node's
// own store answered the claim each collection was made under
// ([hostUnsettled]). The box is up, but only that host can read it, and a host
// that cannot for that long is as good as away, as the fleet takes a launch
// node absent for the box timeout to be ([Waiter.goneAfter]): the job is LOST
// with it, its round carried on, and never ended as a box that could not be
// read ([collectVerdict.lostWithHost]). Never given up, the collection was
// handed back for as long as the partition lasted, its seat held behind it.
// Every other step the store did not answer is this node's own store's, and no
// bound of a host's ends it: nothing this node writes would land.
//
// loseWithHost records a job LOST with the host its box is on, on the claim its
// collection holds ([LossLaunchNodeStoreSilent]), and answers the claim as that
// left it, for the frame that took it to carry the job's round on as it carries
// on every job the row holds lost ([Coordinator.carryOnLost]): or, at the
// turn's deadline, to end it there as it ends one
// ([Coordinator.completeAtDeadline]).
//
// ITS COLLECTION IS PAST ASKING AGAIN: the box's host has answered every
// collection of it, for the box timeout, that its coordination store did not
// answer its read of the run's row, while this node's store answered the claim
// each was made under ([collectVerdict.lostWithHost]). Only that host can read
// the box, and a host that cannot for that long is as good as away, as the
// fleet takes a launch node absent for the box timeout to be: the fleet's
// waiter records such a node's running jobs lost with it, a finished job whose
// completion nobody claimed yet among them, and the seat's holder carries each
// one's round on in a box of its own ([Waiter.goneAfter]). Ended instead as a
// box that could not be read (collect_unreachable), a job that finished had its
// turn thrown away, where the round's calls, its submission among them where it
// made one, and its pushed branch are all still there to carry on from.
//
// RECORDED ON THE CLAIM BEFORE ANYTHING IS CARRIED ON
// ([PendingStore.MarkClaimLost]), as the waiter records a loss before it
// announces one: the relaunch that carries the round on counts the loss toward
// the round's ([MaxRoundLosses]), and a claim this node does not get to resume
// is carried on, never collected, by whichever claim of the job comes next. A
// verdict the store does not take is a step this node's own store did not
// answer, handed back uncounted with the clock on the host kept, so the next
// collection records it at once; and one the claim no longer holds (a newer
// lease took the run, or its ending was decided) is that party's to settle, and
// answers the claim unlost with no error.
//
// THE CLOCK ON THE HOST IS KEPT PAST A VERDICT THAT LANDS, until the run
// settles ([hostUnsettled]): the round is still to be carried on, by a carry or
// a resume that can fail and be handed back, and the host's waiter, which reads
// the run's row through the silent store, announces the job no more than
// before. While the clock runs, every such hand-back's completion is renewed
// (renew.go); dropped here, a carry that failed for longer than one delivery's
// budget left the run, and its seat, with nothing to drive it.
//
// goneAfter is the fleet's waiter finding the launch nodes that left for good,
// and ending what their local runs were waiting on.
//
// A LOCAL BOX GOES WITH ITS NODE. A `direct` or `container` box is driven by
// the node that launched it alone ([Drives]), so a node that leaves the fleet
// for good takes with it every poll, keepalive and reclaim its runs would ever
// get: a running job is never announced, and its seat stays held behind it for
// ever. So the fleet's waiter watches the presence of every node a local run
// names ([NodeRoster]), and once one has been without it for the box timeout
// ([Manager.BoxTimeout]) — the bound a box in any cell gets without its
// engine's keepalive, which on E2B is when the provider reclaims it — its runs
// are ended for it:
//
//   - A RUNNING JOB has the verdict recorded on its row as lost
//     ([PendingStore.MarkLost], [LossLaunchNodeGone]) and its completion
//     announced, which the seat's holder claims and carries the job's round
//     on from in a box of its own ([Coordinator.carryOnLost]) — announced
//     again on its schedule until a claim takes it, as a finished job's is
//     (reannounce.go).
//   - A RUN PARKED ON A QUESTION, or answered and waiting on its resume, has
//     its box released as an expired pause's is ([PendingStore.ExpirePause]),
//     with nothing to kill: the run waits for its answer without a box, and
//     its resume starts from the pushed branch.
//
// NEVER WHILE THE NODE IS MERELY RESTARTING. Presence is what the fleet knows a
// node by, and a node that drains gives it up at the drain's first step, so its
// absence at one instant is no verdict at all: the bound is the box timeout,
// measured from when this waiter first found the node without it, and a node
// that is back within it — its own waiter picking its runs up again — is never
// found gone. A roster that cannot answer is no evidence either way, and
// changes nothing.
//
// The clock is this waiter's own and starts again where the duty moves — every
// absence is forgotten when the duty is lost, or cannot be confirmed
// ([Waiter.forgetAbsences]) — which can only make a verdict later.
//
// The waiter that drives the box records the loss (`engine_restarted`,
// `box_gone` for a box its backend says is gone after the give-up window,
// `launch_node_gone`) on the row before announcing the completion, and the
// seat's holder records `launch_node_store_silent` on its own claim (below),
// and the seat's holder reads what the job spent where its box is left (asked
// again as a collection is, left behind past the give-up window) charges and
// publishes it once, noted cut off, and resumes the turn with
// `ResumeRequest.Lost` to relaunch the same round (`PendingRun.Continues`), at
// most `MaxRoundLosses` (2) times a round (`PendingRun.RoundLosses`, counted by
// the relaunch that carries a lost job's round on);
//
// # Parking a question
//
// FREED AND OPENED AS ONE MOVE, not two: a person can take days, the answer
// arrives on the seat's own inbox, and between two separate writes the seat
// would read as idle with nothing open, the state in which that answer is run
// as an unrelated turn. The claim took the row through [StatusResumed], so it
// is the holding side it leaves.
//
// AND IN THE STEP THAT ENDS THE WRITE'S COUNTING AS THE CLAIM: made after it, a
// recount that read the parked row in between counted the question already, and
// the move counted it a second time.
//
// A WRITE THAT DID NOT LAND HAS ITS CLAIM'S HAND-BACK RECORDED IN THIS STEP
// TOO, out on the run before the park's write leaves it: the question may be on
// the row though its answer was lost, and recorded in a step of its own, the
// release left a moment with no write out on the run, in which a recount
// counted the run off its row, the question, while the release then recorded
// counted it as the claim (countwrite.go). AND BEFORE THE WRITE'S ANSWER IS
// TAKEN, so the release is recorded behind it in the run's entry, and behind
// whatever was recorded after it: an answer by turn's record of the question
// the lost write put on the row, its answer taken first, moves the run to its
// answer as this answer is taken, and the release then counts the run there.
// Recorded after it, in an entry of its own from the claim, the release counted
// the answered run as the claim, holding the seat.
//
// The hand-back records what the claim learned of the store of the host of the
// job's box, as every hand-back does ([Coordinator.unclaimAt]).
//
// # Releasing a claim
//
// ReleaseClaim hands a claimed run back to the status it was claimed from,
// reporting whether THIS call did.
//
// Only while the claim still stands: the run is resumed, holds the release's
// launch, and no newer lease outranks the release's fence. Anything else means
// the run has moved on from the claim (see [Release]), and a retry of the
// signal has nothing left to take.
//
// The claim's charge is recorded IN THE SAME WRITE, and never cleared by one: a
// run is reopened to a retry with its record or not at all (see
// [PendingRun.Charged]).
//
// AND A RECORDED ANSWER THE CLAIM'S TURN TOOK IS THE RUN'S AGAIN
// ([RecordedAnswer.TakenAt] cleared): a resume whose turn gives its claim back
// has reported that nothing it did reached anybody, which is what makes the
// retry safe, and the retry hands the same answer to a turn of its own. A claim
// whose answer an ending has already let go of is not released to
// [StatusAnswered] at all: an answered run carries its answer, and one that did
// not would be owed a resume nobody could make.
//
// A RELEASE THAT HOLDS THE RESUME ([Release.Hold]) RECORDS THE COMPLETION AS
// OWED in the same write, the hold's condition and the store's instant, where
// the job's record owes none yet ([LaunchRecord.RefusalOwed]).
//
// FALSE IS NOT AN ERROR: it is a run that moved on, or a row that is gone. A
// release to a status outside [Claimable] is an error, because no claim ever
// takes a run out of one, and so is a hold whose condition is past
// [MaxOwedConditionBytes].
//
// # Handing a claim back
//
// unclaim hands a claimed tail back, so the signal's retry can win the flip
// again, and leaves the seat's busy count saying what the run holds after it.
//
// To the EXACT status the claim took it from, which the claim snapshotted: a
// run answered out of a clarification goes back to waiting for its answer, and
// one collected from a running job goes back to running for its completion. A
// tail left in resumed is refused by every retry and looked at by nothing but
// the seat's next owner.
//
// ONLY THE CLAIM THIS CALL TOOK (see [Release]). A run that has moved on from
// it, relaunched by the resumed turn or reaped by the seat's next owner, is
// left as it stands, and what holds the seat is then the store's answer rather
// than this claim's.
//
// THE RUN IS COUNTED BACK INTO THE SET ITS REVERTED STATUS BELONGS TO, which is
// not always the seat. counted is whether this node's count still holds the run
// where the hand-back puts it: true where the caller left the claim's count as
// it was, false where it gave it back first, as a resume does, and the release
// counts the run again. A run back in running holds the seat again; one back
// waiting on a person does not — it is an OPEN QUESTION instead. Re-marking the
// seat held for it parked every later delivery on a run no poll completes, the
// person's own answer included; counting it into neither set is the opposite
// mistake, and leaves that answer to be run as an unrelated turn (see
// [seatRuns]).
//
// A CONTEXT OF ITS OWN, like [Coordinator.teardown], because this is a rollback
// and the failure it undoes is often the cancellation itself: a drain cancels
// the delivery's context, the resume breaks on it, and a release that inherited
// it wrote nothing. The run then stayed resumed, and the seat's next owner, the
// node the drain was handing it to, reaped it as abandoned instead of resuming
// it.
//
// # A HAND-BACK THAT CANNOT BE WRITTEN IS NOT A RETRY, so it ENDS the run
//
// Giving the claim back is the whole of what makes every failure after it a
// retry, and a row left in [StatusResumed] is picked up by nothing: the
// completion poll reads only running rows, a redelivered completion is refused
// by the very claim it would retake, no answer matches a row that is not
// awaiting, and the pause reaper expires only one that is. So what looks like
// "leave it for next time" is a turn destroyed in silence, its box paused and
// billed until the seat happens to change hands. It is settled instead — record
// deleted, box reclaimed, loss announced, stop reported — which is what every
// other destroyed turn gets, and detail is the sentence that reaches an
// operator's board. A person's reply the claim holds goes back to the seat as
// it ends, EVEN ONE ITS TURN TOOK: a turn that gave its claim back has said
// nothing it did reached anybody, and the retry that would have handed the
// reply to a turn of its own will never come ([RecordedEnding.Unused]).
//
// BUT A RELEASE THAT REPORTED A FAILURE MAY HAVE LANDED, and then the run is
// not stranded at all: it is back where the claim found it, owed its retry, and
// for a recorded answer carrying the person's reply. So it is ended ONLY WHILE
// IT IS STILL THE CLAIM ([Coordinator.endClaim]), which the store decides: one
// the release did reach is declined and left to its retry, exactly as if the
// release had answered. Ended on every status instead, the run was killed under
// its own retry — box reclaimed, record deleted — and, when the ending then
// refused to let the reply go from a run that no longer read as the claim, the
// reply was deleted with it.
//
// AND A RELEASE THE STORE DID NOT ANSWER IS NOT ENDED AT ALL, on any hand-back:
// no answer is no refusal, and the store that did not answer it is very often
// the silence the claim was handed back for. This node keeps that claim and
// gives it back once the store answers ([Coordinator.oweRelease],
// claimrelease.go); what is ended here is a release the store refused.
//
// # NO STOP IS REPORTED ON THE SUCCESSFUL PATH, on either route
//
// Which is a decision rather than an omission — see
// [CoordinatorOptions.Stopped] for what a stop takes down. A completion's
// hand-back puts the run back to [StatusRunning], where the box IS still
// working and the indicator the suspension kept alive must stay up. An answer's
// puts it back to [StatusAwaiting], where nothing is working — but the
// indicator over that wait was already taken down twice over: once at the park,
// which reports its own stop, and again by the resume frame itself, which
// raises a fresh indicator off the answer's trigger and clears it on exactly
// this failure. A stop reported from here would have to know which of the two
// it was in, which is the same distinction the reverted status already makes —
// and getting it wrong on the completion route takes down an indicator over
// work that is still running. The SETTLE is the exception, and it makes its own
// report through the ending it performs.
//
// Reports whether the run may still be in the hands of whatever will retry it:
// nil where the claim went back (or had already moved on, or its release is
// owed, which this node makes again until it lands), or where the ending could
// not be finished and is kept — whether the hand-back landed is then still
// open, and a signal that comes back finds the outcome — and the release's own
// error where the run was ended in its place. The caller's own error is not
// this answer — a retry keeps it, so the delivery comes back, and an ending
// drops it, because there is nothing left to come back to.
//
// THE SEAT'S COUNT MOVES WITH THE RELEASE, and with nothing before it: every
// caller hands back a claim whose run the seat counts as the claim it is (a
// collection that failed, a park whose question could not be announced, a
// resume that failed or was held; a park whose question could not be recorded
// records its hand-back in the step that takes that write's answer instead,
// [Coordinator.park]), and a release that lands moves that count to the set it
// returns the run to, once, in the step that takes its answer
// ([Coordinator.unclaimOwing]), which a recount reads past while the release is
// out ([Coordinator.syncSeat]). A release the store did not answer keeps the
// claim, which still holds the seat on the store's row.
//
// handingBackLocked records the release that hands run's claim back, as
// [Coordinator.unclaimAt] takes its arguments, among the releases out, and
// returns it for [Coordinator.unclaimOwing] to make. Under mu; the caller runs
// what it returns once it has let go of the lock, which makes the moves the
// release it takes the place of lets go.
//
// AND ITS WRITE IN THE RUN'S ENTRY, in the same step (claimrelease.go): a
// recount counts the run as the claim it still is for as long as the release is
// out, and a write recorded behind it moves after it. A caller whose own write
// on the run did not land records the release in the step that takes that
// write's answer, before taking it ([Coordinator.park]), so no recount finds
// the run with no write out between the two, and the release moves after that
// write and whatever was recorded behind it (countwrite.go).
//
// THE RELEASE IT TAKES THE PLACE OF IS ANSWERED FIRST, as one that did not
// land: an earlier job's, still owed, which can no longer land (see "A later
// job's release takes an earlier one's place", below). Left out in
// the entry, it held every later move on the run behind it for as long as this
// node held the seat.
//
// # Owed claim releases
//
// A CLAIM'S HAND-BACK THE COORDINATION STORE DID NOT ANSWER IS KEPT OWED.
//
// A claim whose work could not finish is handed back for its signal's retry
// ([Coordinator.unclaimAt]): a collection that could not read its box or finish
// after it ([Coordinator.collectFailed]), a park whose question could not be
// announced or recorded ([Coordinator.park]), and a resume that failed or that
// its seat's conditions held ([Coordinator.resumeAndSettle]). A hand-back that
// cannot be written ends the run ([Coordinator.unclaim]): a row left in the
// claim is picked up by nothing. But a release the store did not answer is no
// refusal. Its request found no store to answer it, or its write was sent and
// the answer lost, so it may have landed ([coord.ErrUnavailable]); and a store
// that does not answer the release is very often the same silence the claim was
// handed back for ([ErrStoreSilent], a park's record, a resume's read of its
// log): a leader election, a member restarting, a quorum lost for seconds.
// Ended there, the run would be announced claim_unreverted, "its result is
// lost", while its box held the result, its question or its turn, and the store
// answered again a moment later.
//
// So this node KEEPS IT ([Coordinator.oweRelease]). The claim stays its own
// until the store answers, the release is asked again no further apart than the
// hand-back's spacing ([answerRetryCeiling]), sooner where an answer's own
// backoff arms its series sooner ([Coordinator.scheduleOwedLocked]), with the
// seat's mail held behind it, as a revival or a give-back the store did not
// confirm is ([Coordinator.oweRevival], [Coordinator.giveBackClaim]), and this
// node never turns it into an ending for the store's silence. UNCOUNTED: the
// release's own failure says nothing about the box, and a release that counts a
// failed collection or resume counts it once, when it lands. A release the
// store REFUSED (a definite answer no retry changes: a row this build cannot
// decode or encode, a value past what its key holds) still ends the run as
// [Coordinator.unclaim] says, and a seat that moves before the release lands
// has the claim reaped by its next holder, as a claim whose node stopped is
// ([Coordinator.reapTail]).
//
// THE SEAT COUNTS THE RUN AS THE CLAIM IT STILL IS until a release lands, and
// the landing moves that count to the set the release returns the run to, ONCE,
// in the step that takes the release's answer and drops it from the releases
// out ([Coordinator.unclaimOwing]), or from the claim that won on the run it
// put back ([Coordinator.claimCompletion]): a run owed its release is in the
// claim on the store's row, which holds the seat. Given back before the release
// instead, a release owed for minutes left the seat counted free beside a claim
// that would come back to it, and a recount meanwhile counted the run again, so
// the landing counted it twice.
//
// THE RELEASE IS ONE MORE WRITE IN ITS RUN'S ENTRY (countwrite.go), recorded
// with the release, before it is sent, from the claim to the status it returns
// the run to ([claimRelease.write]), and out for as long as the release is out
// or owed: a park's release where the park's write did not land is recorded in
// the step that takes that write's answer, before taking it
// ([Coordinator.handingBackLocked]), so no moment falls between the two with no
// write out on the run, and the release follows whatever was recorded on the
// run after the park's write. So a recount counts the run where the moves made
// so far leave it, the claim, whatever the store's row says by then
// ([Coordinator.syncSeat]): one that listed the row after the release landed
// and was written before its answer was taken counted the run where the release
// put it, and the answer's move counted it there again, an answer's release
// taking the seat's holding count down twice. And a write recorded behind it on
// the same run moves after it, in the order the two were recorded: a park's
// question can land with its answer lost, the park moving nothing and handing
// its claim back, and a reply then answers that question while the release is
// owed. Read apart from the run's other writes, the release counted the run as
// the claim whatever was out behind it, and the record of that reply took the
// run out of a question set it was never counted in: a seat holding another
// question read it as closed, and that question's answer ran as an unrelated
// turn.
//
// # No claim of the job is taken on this node while its release is out
//
// A release names its claim by the job, the status and the lease, and a claim
// this node takes of the same job again carries all three. A release that
// landed though its answer was lost puts the run back where the claim found it,
// where the claim's retry takes it again; the release asked again then would
// give back that claim while its work is still going on, and the claim after it
// would collect and resume the job a second time beside the first. So the
// release is RECORDED AS OUT BEFORE IT IS SENT, and kept recorded while it is
// owed ([Coordinator.releases]), and no claim of the job is taken here while it
// is. One whose answer is never taken, its store call panicking, is dropped on
// the way out ([Coordinator.dropRelease]): the queue's handler recovers the
// panic, and a record left standing refused every claim of the job for as long
// as this node held the seat.
//
//   - A COMPLETION'S CLAIM is refused ([Coordinator.claimCompletion],
//     [errReleaseOwed]): the completion comes back on the queue's backoff, as
//     one whose claim the store does not take does, both as the store's
//     silence ([ErrStoreSilent]), and the waiter announces the job again once
//     the release has put the run back in running, or, where it cannot, the
//     completion is renewed (renew.go).
//   - AN ANSWER'S CLAIM is left to the series that owes the release
//     ([Coordinator.leaveToRelease]), which makes the release again before
//     anything else and goes on to the answer only once it is no longer out
//     ([Coordinator.retryOwed]). The release is the answer's own where its
//     resume failed, and a park's where the store recorded the question though
//     it did not answer the write: a reply then answers the question while the
//     park's release is still owed, and an attempt the reply makes where it is
//     recorded ([Coordinator.attemptOwed]) that claimed the answer at once
//     would take the claim the release names, so the release made again would
//     give it back to running under the turn it had resumed, the answer with
//     it. And settled once its release landed, as the series of a completion's
//     release still is, the series left the answer on its run with nothing on
//     this node to resume it. The answer's run is owed in the step that finds
//     the release out and records the series, so a seat's release falls before
//     both, the release then no longer out, or after both, dropping them.
//
// A CLAIM ALREADY ON ITS WAY when the release went out is not refused, and
// cannot be. The node's admission orders two claims of a job only where both
// are admitted turns, one at a time on the seat, and a claim made at the turn's
// deadline asks for none ([Coordinator.completeAtDeadline]), beside the control
// topic's deliveries and the completions re-fired off their own goroutines
// ([Coordinator.Readmit]): such a claim can pass the refusal before the release
// is recorded and reach the store after it has landed. It wins only then, so
// ITS WIN IS PROOF THE RELEASE LANDED, whatever the release's own answer said:
// the release is settled there and never made again, nor owed where the win
// falls between the step that found the release out with its answer lost and
// the one that keeps it ([Coordinator.oweRelease], which asks again), and a
// release owed is not made again while a claim of its job is still on its way
// ([Coordinator.claiming]), which decides first, and which counts each claim
// off on every way out of it, a panic included ([Coordinator.claimDoneLocked]):
// a count left up held the owed release back for good.
//
// # A later job's release takes an earlier one's place
//
// A CLAIM OF ANOTHER JOB OF THE RUN IS NOT REFUSED, and its release can go out
// while an earlier job's is still owed. A resumed turn can relaunch its
// executor and then fail: the claim its frame gives back names the job the
// relaunch moved the run off, and a release the store does not answer is owed.
// The relaunched job's completion is claimed all the same, the refusal naming
// only the owed release's own job, and where that job's resume fails as well,
// its release is recorded in the earlier one's place ([Coordinator.unclaimAt]).
// THE EARLIER ONE IS ANSWERED THERE, as a write that did not land, which it is.
// Had it landed, it put its own job back, and the run moves on from there only
// through a claim of that job, which is refused while its release is out or
// takes the release over as it wins ([Coordinator.claimCompletion]); and on a
// run that has moved on to a later job, an earlier job's release can no longer
// land. Left out in the run's entry, it held every later move on the run behind
// it, the later release's and the run's last among them, for as long as this
// node held the seat: the seat counted the run as the claim after it was over,
// and took no mail.
//
// oweRelease keeps a claim's release the store did not answer for this node's
// retry ([Coordinator.retryOwed]) on the hand-back's spacing, with the seat's
// mail held behind it; if the seat moves first, its next holder's recovery pass
// reaps the claim. See above.
//
// ONLY WHILE IT IS STILL OUT, asked in the one step that records the release on
// its series, owes the seat's run and arms the series.
// [Coordinator.unclaimOwing] found it out in a step of its own, and a claim of
// its job on its way when it went out can win between the two, taking it over
// and finding no owed series to settle ([Coordinator.claimCompletion]); and a
// seat's release drops it with the seat ([Coordinator.releaseOwed]). Recorded
// then all the same, as it was in a step after the one that asked, the release
// was owed though no longer out: the seat's mail held behind it until its
// retry, a hand-back's spacing later, found it gone, and the node said it kept
// a claim that had been given back.
//
// leaveToRelease reports whether an answer's attempt at run must leave its
// claim to the series that owes the release of the job's last claim, because
// this node is still giving that release back or owes it; and keeps that series
// armed where it is, so it makes the release first and then comes back to the
// answer. See above.
//
// ARMED ON THE HAND-BACK'S SPACING, and only where nothing arms it yet: an owed
// release's series is armed already ([Coordinator.oweRelease]), and one still
// on its way is armed again once it is owed. One that lands or is refused
// instead is followed by this series' read of the row, which finds what became
// of the answer. Armed at once, a series that came back here while the release
// was still on its way would have read the row and come back again, as fast as
// the store answered.
//
// OWED IN THE STEP THAT FINDS THE RELEASE OUT AND RECORDS THE SERIES, as
// [Coordinator.oweRelease] owes one: a seat's release
// ([Coordinator.releaseOwed]) drops the series, its attempt and the seat's owed
// runs, and owed in a step of its own after that, the run was owed again with
// no series and nothing armed to drive it, holding the seat's mail on this node
// until a settle of that run or the seat's next release. In the one step, the
// seat's release falls before it, and the release is found no longer out, or
// after it, and drops all of it.
//
// claimCompletion claims ev's job for this node's collection of it, as
// [PendingStore.ClaimForResume] does, unless this node is giving back or owes
// the release of the job's last claim: refused then, with [errReleaseOwed], and
// the completion comes back for its retry. See above.
//
// THROUGH THE RECOVERY GATE FIRST ([Coordinator.claimGateLocked]), in the same
// step: refused with [ErrSeatRecovering] while a recovery pass of the seat runs
// on this node, which the caller's road carries past the pass
// (recoverygate.go); and otherwise counted as this node's live attempt on the
// run until the caller calls done, which it does once its attempt has returned,
// whatever the claim answered. done is nil only beside an error the gate or the
// release refusal returned.
//
// EITHER FAILURE IS THE STORE'S SILENCE ([ErrStoreSilent]), as every step a
// collection cannot go on without is: a claim the store did not answer, and a
// claim refused while the store has yet to take the release of the last one,
// say nothing of the completion, so its delivery is handed back as this node's
// failure and never as the message's (internal/engine's controlOutcome).
//
// THE REFUSAL AND THE COUNT OF CLAIMS ON THEIR WAY ARE ONE STEP, so a claim
// either finds the release recorded, or is counted before the release can be
// made again ([Coordinator.retryOwed]). A claim that WINS while the release is
// still recorded is proof the release landed: the release is settled, never
// made again, and the seat's mail it held is let go.
//
// # Ending a claim
//
// endClaim ends a run WHILE IT IS STILL THE CLAIM THIS CALL TOOK — its status,
// its job and its lease — for an ending decided WITHOUT knowing what the row
// holds: a settle whose read of the row failed ([Coordinator.settleClaimed]),
// and a claim whose release the store refused ([Coordinator.unclaim]). Reports
// whether it ended the run, and the ending's error where it was kept for its
// retry ([errEndingOwed]).
//
// THE CONDITION MOVES TO THE STORE, which is what makes acting without the read
// safe. An ending licensed for this claim alone is not decided on a run that
// moved off it: a relaunch the resumed turn made takes the row through
// [StatusLaunching] and a new job, and the claim that job's own completion
// takes is the same status on another job — which a license on the status alone
// ended, box and all. A release that LANDED although it reported a failure puts
// the run back where the claim found it, and an ending licensed for every
// status then ended a run that was fine, with the person's reply on it.
// Declined, either is left to whatever moved it.
//
// DECIDED FIRST AND RECLAIMED SECOND, as every ending is
// ([Coordinator.finish]), and here the reason is sharpest: the decision is what
// establishes the row was still this call's to end, so reclaiming first would
// destroy the box before it — the box of a relaunch, or of a run handed back
// for its retry. The reclaim rides the recorded ending
// ([RecordedEnding.Reclaim]), so whichever attempt finishes the ending — this
// node's retry, or the seat's next holder — reclaims the box before it deletes
// the record, rather than leaving the box billed behind a record that is gone.
//
// # Finishing an ending
//
// finishEnding does what a decided ending ([PendingRun.Ending]) still has to
// do, IN THIS ORDER, and deletes the record last:
//
//  1. a person's reply the run still holds and owes the seat is LET GO
//     ([Coordinator.letGoAnswer]), its copies recorded on the row as owed;
//  2. every copy the row owes ([PendingRun.HandBack]) is published and cleared;
//  3. for a STOP, the run's tool-bridge session is closed, and the read of the
//     job's box ENDS THE JOB, reads what the run spent off it, charged and
//     recorded, and takes its harness session ([Coordinator.collectStopped],
//     [Coordinator.readBox], [Coordinator.endJob]) — before the box goes,
//     since nothing can read it after;
//  4. for a LOSS whose box may still be read, the run's tool-bridge session is
//     closed, the job ended, its served notes settled by its session and its
//     session taken ([Coordinator.keepLostSession]), and the row read again;
//     and the box is reclaimed, where the ending reclaims it after its decision;
//  5. the announcement is published, under the ending's own identity;
//  6. for a loss, the run is handed to the engine, which answers the
//     colleague whose ask its turn took up and says how the turn ended
//     ([CoordinatorOptions.TurnLost]), and that end is recorded on the row
//     as owed ([PendingStore.OweEnd]);
//  7. for a stop, the turn is closed on the record as stopped where it was
//     parked on the run, its colleague answered and its end owed the same
//     way ([CoordinatorOptions.CloseStopped]);
//  8. every note a person sent the turn that no step reported reading is
//     published as the record read after the box knows it: delivered, told
//     again by the brief of a later job that started; unconfirmed, handed to a
//     job's harness that never confirmed it; missed, read by a take whose
//     answer was lost and told by no later launch whose job started; expired,
//     still waiting or read first by the brief of a job that never started
//     ([CoordinatorOptions.SteersEnded]);
//  9. the end the row owes — this ending's, or the one the turn's resumed
//     frame decided — is recorded on the executor log once every capture of
//     the turn has settled ([Coordinator.awaitCapture],
//     [Coordinator.publishEnd]), and cleared from the row;
//  10. the record is deleted ([PendingStore.Finish]), every call of the round
//     its log still holds in flight named as it goes
//     ([Coordinator.nameDeletedInFlight]); for an ending whose run this node
//     counts ([ending.counted]), the delete is recorded before it is sent and
//     its answer moves the run out of the seat's counts, once (countwrite.go).
//
// # Off any frame somebody waits on
//
// Steps 3, 4 and 9 wait on what is not the frame's own: another node's host, a
// job's end, a box's pack and the scrub of its session, the executor log and a
// capture being kept; and so does the wait for the run's calls still in flight
// before steps 6 and 7 decide the turn's end on its log
// ([Coordinator.drainEnding]), which a frame makes only where the log holds
// none still waited for. Only where mayAsk says this is the ending's own retry,
// on the coordinator's lifetime, are they made; anywhere else — the pause
// watch, a seat's acquisition, a completion's handler — the ending stops there,
// decided and with its session closed, and is handed to that retry at once
// ([errAskAway]), the seat's mail held behind it until it lands. Every stop of
// a job that started in a box is read there, whatever
// `conversation_session.enabled` says: the switch governs what is kept of a
// session, not how a stop ends a job, and ending a job is waiting for it.
//
// # Across a crash at each step
//
// Nothing reads a deleted row again, so everything the ending owes is done
// before the delete, and every step is one a repeat makes harmless: the let-go
// finds nothing left to let go of, the copies go out again under the same
// derived ids (the inbox's same-id dedupe and the completion ledger collapse
// them), a box already reclaimed is a kill of nothing, the announcement is the
// same event again, the turn's end was decided ONCE and is found owed on the
// row rather than decided — and its colleague answered — again, and an end the
// log took already is refused as one, which is the answer. Stopped anywhere
// before the delete, the row still carries the ending, and whoever reads it
// next — this node's retry, or the seat's next holder
// ([Coordinator.RecoverSeat]) — finishes it from the step it stopped at.
//
// # A delete the store refuses
//
// The store will not delete a row that still owes the seat copies, holds a
// reply the ending must hand back, or owes the executor log its turn's end
// ([ErrHandBackOwed], [ErrAnswerOwed], [ErrEndOwed]), and hands back the row as
// it stands; the steps run again against it. A step that does not land is
// returned, and the caller keeps the ending.
//
// # Resuming and settling
//
// resumeAndSettle re-enters the parked turn, then settles the box. seen is what
// was signalled before the turn's admission was asked
// ([Coordinator.signalsSeen]), which a resume its seat's budget held is kept
// against ([Coordinator.keepHeldOnRow]).
//
// After the resumed turn returns: if it relaunched its executor the row is back
// in running and a new job owns the paused box, so it is left for the next
// completion. Otherwise the phase is done with the box, so the run's ending is
// decided and the box torn down, and the record goes with the ending's last
// step: here, or — where it owes the turn's end, as every record of a
// coordinator that records on an executor log does — by this node's retry of
// the ending ([Coordinator.finishEnding]), armed at once on a goroutine of its
// own, which records the end once every capture of the turn has settled,
// deletes the record and recounts the seat. That retry runs beside the rest of
// this frame, its own recount included, and can finish before this returns; the
// two recounts are ordered by the seat's moves, so whichever is written second
// reads the store again first ([Coordinator.syncSeat]).
//
// IT REPORTS WHAT IT LEFT THE DELIVERY, not merely whether it failed, because
// "the resume did not happen" is three different facts and only one of them is
// a retry: the run can be back where the claim found it (the delivery is still
// owed to it), or terminally gone (nothing is owed it any more), or the turn
// can have run and concluded something (the delivery is spent). Its callers
// arrive by different routes and read the pair differently — a completion NAKs
// on the error and lets the broker's own budget bound the retry (the message
// names its run, so where it comes back does not matter); and a RECORDED answer
// — a chat reply or an answer by turn — is retried by this coordinator on
// [AnswerDeferred], bounded by [MaxAnswerAttempts] ([Coordinator.attemptOwed]).
//
// # A recorded answer the run ends without
//
// A run claimed out of [StatusAnswered] carries the person's reply on its row,
// and a resume can end the run before any turn took that reply: no parked state
// to re-enter, a claim that cannot be given back, a resume that broke before
// its turn began. ON EVERY ROUTE the reply then goes back to the seat through
// the run's row: the store will not delete a row still holding a reply no turn
// took ([ErrAnswerOwed]), so the ending lets it go first, recording its copies
// as owed before the record is deleted ([Coordinator.letGoAnswer]), and a crash
// anywhere in between leaves them for whoever reads the row next. Handing on
// the delivery the inline attempt still holds instead would need the store to
// delete a reply on the caller's word, and an ending that could not land would
// leave the reply both on the row and handed on.
//
// A REPLY A TURN TOOK IS SPENT, whatever becomes of the turn, and the line
// between the two is drawn by the turn's own start ([ResumeRequest.Begin]) —
// which is where its delivery is recorded as worked, too.
//
// # What bounds a collected job's turn
//
// A job that finished is collected and its turn resumed to review it. Until
// that resume runs, the run's row holds the seat: its other mail waits behind
// it. A resume that fails is handed back, and one that meets a condition that
// ends by waiting (a model's usage limit, a spent budget, a paused seat) is
// held until that condition clears. Either way the waiter announces the job
// again on its schedule (reannounce.go), so the job comes round until its
// resume runs.
//
// Two bounds keep that from going on for ever, and both END THE RUN, announced
// on the board under a reason of its own, with the seat freed:
//
//   - THE TURN'S DEADLINE ([PendingRun.Deadline]), a scheduled fire's
//     wall-clock cap. The poll and the collection after it end a job still
//     working at it, and the turn loop refuses a round that starts past it,
//     but neither covered the time between: a finished job whose resume kept
//     failing or waiting was handed back and announced on every poll. On a
//     live company a health check with a ten-minute cap finished its job in
//     six minutes, met a network outage and then the model's weekly limit in
//     its review, and held its seat for twenty-seven hours, its next three
//     fires queued behind it. Past the
//     deadline a completion's resume gets the attempt it can have at once,
//     and anything that would make it wait or retry ends it instead
//     ([Coordinator.completeAtDeadline], [Coordinator.endAtDeadline]). A
//     resume its seat's budget held on the run's row, which the waiter passes
//     over and the claim refuses until the hold lifts, is held no later than
//     the deadline ([Coordinator.unclaimHeld]): held until the budget's window
//     turned, a day, a week or a month on, nothing reached it to end it.
//   - THE RESUME GIVE-UP ([ResumeGiveUp], [MinResumeFailures]), for every
//     turn, capped or not. A resume that FAILS for a reason of the run's own
//     (not one that waits, not one the node or its infrastructure failed, and
//     not one another node or build can get past while a live node may) is
//     counted onto the job's record and, once it has failed that often for
//     that long, the run is settled ([Coordinator.resumeGivenUp]).
//     Uncounted, each poll's announcement was a fresh delivery with a fresh
//     budget, so a resume that always failed was retried every poll for as
//     long as the engine ran.
//
// A WAIT ON A CONDITION is not bounded here for a turn with no deadline: the
// condition names when it lifts, or the event that lifts it, and the seat could
// not run other work through it meanwhile. An answer's resume is bounded by its
// own series ([MaxAnswerAttempts]) and the run's pause TTL.
//
// NOR IS A CONDITION ONLY AN OPERATOR CLEARS. A resume that fails on this
// node's disk (full, mounted read-only, or failing to read or write), on its
// limit of open files, or on the fleet's nodes holding different keys or
// running different builds is the node's or its infrastructure's
// ([CoordinatorOptions.NodesFailure]), so it counts nothing toward the give-up,
// and no wait clears it: the run and its seat wait for the operator, the job's
// completion coming round on each announcement, and a turn with a deadline
// still ends at it. Ending the run would throw away the review of a job that
// finished, for a condition the next resume after the operator's fix gets past.
// What the operator sees, condition by condition:
//
//   - every one: the `executor_resume_blocked` alarm, raised by every data
//     node from the status record of the node holding the seat, a satellite's
//     included, while one of its runs waits so ([Coordinator.ResumesBlocked],
//     [CoordinatorOptions.OperatorsCondition]), naming the seat waiting
//     longest, the node holding it, the condition's kind and for how long,
//     and cleared once a resume gets past it;
//   - every one: each attempt's `sandbox_resume_failed` line, at error, its
//     `error` naming the condition (its `attempt` does not rise for these:
//     it numbers only the run's own failures): the system's or the store
//     driver's own words for a disk or the open files (such as "no space
//     left on device", "database is full", "read-only file system",
//     "input/output error", "too many open files"),
//     the estate's "finish the upgrade" for builds that differ, and a note or
//     a run's record this node could not verify for keys that differ;
//   - a full disk that is the volume holding this node's databases: the
//     `volume_low` alarm besides, raised while the volume has less free
//     space than its next restore, vacuum or snapshot needs.
//
// completeAtDeadline ends the run of a completion its node would not resume
// now, because its turn's deadline has passed: claimed, collected, charged and
// recorded as every completion is ([Coordinator.collectClaimed]), and then
// settled instead of resumed ([Coordinator.endAtDeadline]).
//
// THE CONDITION THAT REFUSED IT IS NOT WAITED FOR. Before the deadline a
// completion the gate refuses, or one this node holds off after its resume met
// a limit, waits for the condition to clear, because the turn still has time to
// review its job. After it, the wait is time the turn does not have, spent
// holding the seat. Asked by an announcement of the job, or, for one this node
// holds off itself where no waiter announces it, by that hold's own backstop
// ([Coordinator.recheckHeld]), so a deadline that passes while nothing
// announces the job still ends the run within the backstop.
//
// No turn starts here, which is why no admission is asked: the claim, the
// collection and the ending are the run's, and none of them is a turn. But the
// claim is a claim like any other, and meets the seat's recovery gate: a pass
// of the seat under way on this node refuses it, and its road carries it past
// the pass (recoverygate.go). Asking no admission, it was the one claim the
// acquisition's own refusal of a seat still establishing never kept out of that
// seat's recovery.
//
// resumeGivenUp settles a claimed run whose turn's resume has now failed
// [MinResumeFailures] times across [ResumeGiveUp], counting the failure being
// handled, and reports whether it did. The run's other failures are on its
// job's record ([LaunchRecord.ResumeFailures]), so the count holds across nodes
// and restarts.
//
// ONLY THE RUN'S OWN FAILURES COUNT ([Coordinator.runsOwnFailure]), and its
// caller asks before it asks this. A resume the node or its infrastructure
// failed (a coordination store that did not answer the read of where the turn's
// writes are minted, a full disk, a stop that cut it short) is handed back as
// any failure is, and comes round again when the poll announces the job, but it
// is no evidence that the turn cannot be re-entered: counted, an outage of half
// an hour ended every run whose job finished during it, the work it reviewed
// thrown out with the run, though the next attempt after the outage would have
// resumed it.
//
// NOR DOES A RESUME ANOTHER NODE OR BUILD CAN MAKE ([ErrResumeElsewhere]),
// while a live node may make it: a parked state a newer build wrote, met by an
// older node still holding the seat through a roll, is a failure of where the
// completion went, and the run is resumed once a node that reads it holds the
// seat. Once no live node reads the state's version, as after a rollback, it is
// counted like the run's own, so the seat is not held for ever by a state
// nobody running can read; and a reply kind this build does not read, which is
// no part of that version, once no live node runs another build.
//
// # Resumes blocked on an operator
//
// blockedResume is a completion whose last resume on this node failed on a
// condition only an operator clears ([CoordinatorOptions.OperatorsCondition]):
// the seat it holds, the condition's kind, and when the first such failure in a
// row was met.
//
// COUNTED ONCE THE ATTEMPT'S HAND-BACK LANDS, never as the failure is met:
// handBack is the release that gives the claim back while it is out (or owed,
// which this node makes again until it lands), and counted whether a release of
// it landed. A run waits for the operator only once it is back where its claim
// found it, the completion coming round again on it; one whose hand-back did
// not land was ended in its place, or had moved on, its ending decided by
// another path while the resume ran (a person's stop of the run the resumed
// turn relaunched, say), and counted as the failure was met, after that ending
// had taken it out ([Coordinator.runSettled]), it held the alarm up until its
// seat moved. Recorded with the release, in the step that records it
// ([Coordinator.handingBackLocked]), and decided in the step that takes its
// answer ([Coordinator.blockedAnsweredLocked]), so an ending that lands between
// the release's landing on the store and that step takes the entry out before
// it is counted. An entry counted by an earlier attempt stays counted while the
// next one's release is out.
//
// AND ONLY WHILE THIS NODE HOLDS THE SEAT UNDER fence, the lease the attempt's
// claim was taken with ([fenceOf]). The seat's release takes the run out
// ([Coordinator.ReleaseSeat]), but nothing cuts a resume's frame short there
// (the node's admission ends a turn at a drain's cut alone), and a frame still
// running here met its failure after the release, its hand-back landing on a
// row no newer holder had fenced yet: counted, a run of a seat this node no
// longer held stood in its status record, raised fleet-wide whatever the seat's
// next holder made of it, until the node restarted or the run came back to it.
// So [Coordinator.ResumesBlocked] counts a run only where the lease seam
// answers that this node holds the seat under fence
// ([CoordinatorOptions.Lease]), and lets go of one it no longer does; and an
// entry an earlier holding left is no start for a later one's
// ([Coordinator.blockedHandBackLocked]).
//
// ResumesBlocked reports the runs of seats this node holds whose job finished
// and whose last resume here failed on a condition only an operator clears.
//
// THE GIVE-UP NEVER ENDS SUCH A RUN ([Coordinator.resumeGivenUp]): the failure
// is the node's, so the run and its seat wait for the operator, the job's
// completion coming round on each announcement and failing again until the
// condition is cleared. Each attempt's `sandbox_resume_failed` line says why,
// but a line repeated on every announcement is no signal anyone is paged by, so
// the wait is raised as the executor_resume_blocked alarm while it lasts: the
// node says this in its status record, and every data node raises it from there
// (internal/engine's executorReading), since the node holding the seat may be a
// satellite, which evaluates no alarm table of its own. FROM THIS NODE'S OWN
// MEMORY of its attempts, which is exact for the seats it holds: counted once
// the failed attempt's hand-back lands, and only while this node holds the seat
// under the lease that attempt's claim was taken with ([blockedResume]); a
// resume of the run that gets past the condition, or fails for anything else,
// takes it out, and so does a hand-back that does not land, the run's settling
// ([Coordinator.runSettled]) and the seat's release, whose next holder says it
// again from its own attempts. A restart forgets it until the run's next
// attempt fails again.
//
// # Holding a refused resume
//
// THE CLAIM'S COUNT MOVES WITH THE HAND-BACK, as on the revert below: the claim
// kept the seat counted through the resumed turn, and the release that lands
// moves the count to the set it returns the run to, once
// ([Coordinator.unclaimOwing]), never before it, since one the store does not
// answer keeps the claim. WHAT WAS SIGNALLED BEFORE THE TURN WAS ADMITTED, for
// the keep below ([Coordinator.keepHeldOnRow]), never what was by the time the
// resume returned: the budget refused the resume inside it, and a signal that
// landed after that, while the row was still the claim, found no hold to lift,
// so read here it was counted as seen and the job waited for the hold's
// instant, until the budget's window turned. Read before the gate, it re-fires
// the job at once, at the cost of one review re-run for a signal the budget
// still refuses.
//
// UN-CLAIM so a retry can win the flip again. Without this the NAK'd completion
// redelivers, the claim refuses, and the suspended conversation is permanently
// lost with the row stranded in resumed. The claim's count moves to the set the
// hand-back returns the run to once its release lands, and stays the claim's
// while it is owed.
//
// A resume the resumer REFUSED on a condition it names (its models all at their
// limit before it wrote anything, or an answer's resume its seat's budget held)
// is a wait rather than a failure, and is said as one; its caller waits on it.
//
// HANDED BACK UNHELD, A COMPLETION'S TOO: what refused it is this node's
// models, which only this node knows are at their limit, and the run's row is
// read by every node. The completion is held here instead, until the instant
// the refusal names, beside being kept for its signal
// ([Coordinator.OnCompleted], [Coordinator.keepRefused]): the gate can admit a
// resume whose relaunch then meets the limit (a review on a ready member of the
// chain, sending back to runners every one of which sits out), and unheld,
// every announcement of the job collected its box and ran the review again,
// into the same refusal, for as long as the limit lasted. Written onto the row,
// the hold silenced every waiter until the instant, whoever held the seat next:
// a restarted node, or the peer the seat moved to, whose own models may know of
// no limit at all, left the job unannounced for up to the hour a vendor's
// window is benched for. An answer's is its own series, which waits in this
// node's memory already.
//
// unclaimHeld hands back a claim whose resume the seat's conditions refused,
// holding the run's resume until refusal lifts ([Release.Hold]), and counting
// onto the job's record the window its relaunch was refused below its floor in,
// where the refusal names one ([Release.RelaunchRefusedIn]): the relaunch the
// hold's lifting lets the turn try again reads it.
//
// A REFUSAL ONLY AN EVENT LIFTS is held until [answerWaitBackstop], the bound a
// recorded answer's series waits for the same signal: the signal lifts it
// sooner ([Coordinator.Readmit]), and a signal never wired to say so costs the
// turn that much and no more.
//
// A TURN WITH A DEADLINE IS HELD NO LATER THAN IT ([PendingRun.Deadline]): past
// the deadline nothing waits, and the resume that meets anything then ends the
// run ([Coordinator.endAtDeadline]). But nothing reaches a held row to end it:
// the waiter passes it over, the claim refuses it, so an ending at the
// deadline's claim is refused too ([Coordinator.completeAtDeadline]), and the
// backstop that ends a completion held past its deadline is a hold this node
// keeps in memory ([Coordinator.recheckHeld]), never one on the row. Held until
// the budget's window turned, a day, an ISO week or a calendar month on, a
// capped turn whose review met the budget minutes before its cap held its seat
// that long. Held until the deadline, the hold lapses there, and the keep made
// for it ([Coordinator.keepHeldOnRow]) or the waiter's next announcement
// re-fires the job then, its resume past the deadline ending the run.
//
// It returns the hold it asked the release to write, its instant set, which the
// completion is kept here for ([Coordinator.keepHeldOnRow]).
//
// Hold, where set, holds the run's resume back until the refusal lifts
// ([PendingRun.HeldFor]): the claim is handed back because the seat's
// conditions refused the resumed turn, not because it failed, and the next
// attempt waits for them. Nil hands back a claim to be retried as the job's
// completion comes round, and clears any hold an earlier release put on it.
//
// A HOLD ALSO OWES THE COMPLETION ON THE JOB'S RECORD
// ([LaunchRecord.RefusalOwed]), its condition, where the record owes none yet.
// The hold spent the completion's delivery, and the row is all a restart or the
// seat's next holder reads: while the hold stands its recovery keeps the job
// for the hold's instant, but a lift clears the hold, and a lift whose re-fire
// had not yet claimed the job (one the store did not answer, kept again in
// memory alone) left that recovery nothing to keep or re-arm, the job announced
// by nothing while the box's host's store was silent. Owed, it is re-armed and
// offered to the recovering holder's gate at once ([Coordinator.rearmOwed]).
//
// # Kept completions
//
// keepLocked keeps kept for its condition, in place of anything kept for its
// run before, and arms its own re-fire ([Coordinator.keptRefireIn]): at the
// instant its refusal named for the clock to lift it, no later than
// [answerWaitBackstop] unless this node holds the completion off itself until
// that instant, and at that backstop where it named none or one already past;
// and at the instant the hold on its run's row lifts where its resume was held
// there, never sooner. A completion held here until an instant still has its
// timer at the backstop, which before that instant re-fires nothing but asks
// what an announcement would ([Coordinator.recheckHeld]). The caller holds c.mu
// and has checked the coordinator is not closed.
//
// # Why a kept completion has a timer of its own
//
// Its condition's signal re-fires it ([Coordinator.Readmit]), and the waiter
// that drives its box announces it again on its schedule. But a condition can
// lift on the clock with no edge anything signals (a seat's lease merely stale
// from a late renew, a provider's limit whose instant came with no park to
// release), and while the coordination store does not answer the host of the
// job's box that waiter announces nothing. Kept for the signal and the waiter
// alone, such a completion waited on this node, with no restart and no seat
// move to re-arm it from its record, for as long as that store's silence
// lasted: never lost, but with no bound. So every keep arms a re-fire of its
// own, as an answer's wait does ([Coordinator.waitOwed]), on the same rule: the
// refusal's instant where it names one, and no wait longer than the backstop,
// which bounds a signal that never came at the cost of one ask of this node's
// gate per kept completion per five minutes; save a completion this node holds
// off itself until its instant, whose re-fire waits for that instant, its
// backstop before it asking only whether its turn's deadline has passed
// ([Coordinator.keptRefireIn], [Coordinator.recheckHeld]). Re-fired, it is
// offered to [Coordinator.OnCompleted] as a signal's re-fire is: admitted, it
// is carried; refused, it is kept again with a timer of its own.
//
// NOT COUNTED AS WORK IN FLIGHT ([Coordinator.inflight]): an armed re-fire is
// no work yet, and [Coordinator.Stop] cancels it rather than wait out a
// backstop. Its callback re-fires under c.mu only while the coordinator is
// open, and that re-fire is counted from there ([Coordinator.refireLocked]).
//
// recheckHeld is the backstop of a completion this node holds off itself until
// an instant ([Coordinator.keptRefireIn]), before that instant: it does what an
// announcement of the job does here and nothing more
// ([Coordinator.OnCompleted]): still held, it asks whether the job's turn is
// past its deadline ([Coordinator.pastDeadline]), and ends the run if so
// ([Coordinator.completeAtDeadline]); and it keeps the hold and arms the next
// backstop, or re-fires the completion where its instant has come meanwhile.
// armed is the re-fire this backstop was, so a keep that went or was replaced
// meanwhile is the keep's that replaced it.
//
// # Why a hold has a backstop at all
//
// Its re-fire waits for its instant, since a re-fire drops the hold and resumes
// the job into the limit that held it. But the turn's deadline is not that
// instant: past it, nothing waits ([Coordinator.completeAtDeadline]), and an
// announcement made then ends the run. Where no waiter announces the job (its
// box's host's store silent) nothing asked, and a turn whose deadline passed
// while its job was held here waited for the hold's instant, up to the hour a
// vendor's window is benched for, holding its seat. So the backstop asks what
// an announcement would, at most [answerWaitBackstop] apart: the run ends
// within a backstop of its deadline, at the cost of one read of the run's row
// per held completion per five minutes, and never a claim or a resume before
// the instant.
//
// AN ENDING THAT DID NOT SETTLE, its claim's collection not answered, say,
// keeps the completion held, its next backstop armed, unless something else
// keeps it since: nothing announces the job where no waiter can, and the next
// backstop asks again.
//
// ITS LIFETIME IS THE COORDINATOR'S, for the reason [Coordinator.refire] gives.
//
// keptRefireIn is how long kept waits for its own re-fire
// ([Coordinator.keepLocked]): until the instant its refusal named for the clock
// to lift it, no longer than [answerWaitBackstop], and that backstop where it
// named none or one already past; and, for a completion whose resume is held on
// its run's row, until the instant that hold lifts, uncapped and never sooner
// ([Coordinator.keepHeldOnRow]): before it the store refuses the claim a
// re-fire makes, and a re-fire that claims nothing settles nothing and leaves
// nothing kept. An instant already past is a re-fire at once, since the claim
// no longer refuses the row then.
//
// # A completion held here is re-fired at its instant, never the backstop
//
// One whose resume was refused after its claim, on a condition the gate could
// not see, is held here until the instant its refusal names
// ([Coordinator.stillRefused]): the gate admitted it and would again, so a
// re-fire before that instant claimed it, woke its box to collect it and ran
// its review into the same refusal. A re-fire drops what is kept, the hold with
// it, so one made at the backstop resumed the job into the limit every five
// minutes for as long as the limit lasted, up to the hour a vendor's window is
// benched for, where the seat's park did not refuse it first. Its re-fire comes
// at the instant instead: the signal that ends the wait sooner re-fires it as
// it re-fires any kept completion, and this node refuses every announcement of
// the job until then.
//
// BUT ITS TIMER STILL FIRES AT THE BACKSTOP, where the instant is further off,
// and before the instant what fires is no re-fire: it asks only what an
// announcement of the job asks here, whether its turn's deadline has passed,
// which ends the run, and keeps the hold, its timer armed again
// ([Coordinator.recheckHeld]). Armed for the instant alone, a turn whose
// deadline passed while no waiter announced the job (its box's host's store
// silent) waited for the hold's instant, up to that hour, rather than ending.
//
// keepRefused keeps a completion refused on refusal's condition, for
// [Coordinator.Readmit] to re-fire the moment that condition clears, and, where
// held, holds its announcements off on this node until refusal.Until.
//
// # Why it is kept, when the waiter announces it again anyway
//
// The waiter that drives the run's box (the fleet's, or the box host's own,
// boxhost.go) re-announces a finished run for as long as its row is running, on
// its schedule (reannounce.go), and that is what makes acknowledging a refused
// completion safe: nothing is lost with the delivery. But the next announcement
// is up to [reannounceCeilingPolls] poll intervals away, and the end of a
// condition is an event the engine already observes (a seat's turn ending, a
// renew proving ownership again, a provider's limit lifting). So the clearing
// re-fires the completion itself, and the poll is the backstop for a clearing
// nothing signals, beside the keep's own re-fire at the refusal's instant or
// [answerWaitBackstop], which bounds the wait where no waiter announces the job
// ([Coordinator.keepLocked]). Any of them may arrive together: the claim is
// at-most-once, so the run resumes once.
//
// # Unless no waiter can announce it
//
// THE WAITER IS THE BACKSTOP WHILE THE STORE ANSWERS THE BOX'S HOST, AND THE
// BROKER IS WHILE IT DOES NOT. While this node keeps a clock on the store of
// the host of the job's box ([Coordinator.hostClocked]), or the run's row says
// what such a clock would ([Coordinator.unannouncedJob]), that host's waiter
// reads the run's row through a store that does not answer it and announces
// nothing, and what is kept here is this node's memory alone: a restart, a
// letting go of the seat, or a clearing nothing signals would leave the job
// with nothing to announce it. So the refusal is still kept for the signalled
// re-fire, and its delivery is handed back as well, never acknowledged, and
// renewed before its last ([ErrUnannounced], renew.go). And where neither says
// so yet, the refusal its delivery's acknowledgement leaves this node owing is
// written on the job's record first ([Coordinator.oweRefused]), so a restart or
// the seat's next holder owes it as this node did ([Coordinator.rearmOwed]). A
// duplicate is harmless, since the claim is made once, and the re-fire
// announces nothing afresh while that delivery is in flight
// ([Coordinator.refire]).
//
// A RESUME REFUSED AFTER ITS CLAIM is kept the same way: one whose models were
// all at their limit before its turn wrote anything is reverted, so the run is
// back to running as the gate would have left it, and its refusal names the
// condition ([Coordinator.OnCompleted]).
//
// # Why one refused after its claim is also held here
//
// The gate admitted it, and would admit its next announcement too: a
// completion's gate asks no limit its models know, since its job is collected
// first and its resume refused once it is (the engine's), and a review on a
// ready member of the seat's reviewer chain that sent the run back to runners
// every one of which sits out met the limit a round in, at its relaunch. Asked
// again, the gate says the same, and every announcement of the job collected
// its box and ran the review again into the same refusal, for as long as the
// limit lasted. So such a completion is kept held (held, and the refusal's
// Until is the instant): its announcements are refused HERE, before the gate
// and the claim ([Coordinator.stillRefused]), until that instant, and the
// signal that ends the wait re-fires it as it does any kept completion; its own
// re-fire comes at that instant, never at the backstop, which would drop the
// hold before it ([Coordinator.keptRefireIn]), and its backstop before the
// instant only asks, as an announcement does, whether its turn's deadline has
// passed ([Coordinator.recheckHeld]). A refusal that names no instant holds
// nothing: the gate is asked again on every announcement, as for a completion
// it refused itself.
//
// THE HOLD IS IN THIS NODE'S MEMORY, NEVER ON THE RUN'S ROW. (What the row
// keeps is that a refusal was acknowledged and is owed its re-fire
// ([Coordinator.oweRefused]): an obligation the seat's next holder honours by
// asking its own gate, never a hold anybody reads.) The refusal is this node's
// models', whose limits are its own: the sit-out is not shared with the fleet,
// and a restart, or the seat moving to a peer, asks the models again on the
// first delivery (ADR-0064). The row is read by every node, and a hold written
// there silenced every waiter until its instant, whoever held the seat next: a
// restarted node, or the seat's next holder, whose models may know of no limit
// at all, left the job unannounced for up to the hour a vendor's window is
// benched for. Held here, it goes with the node: its restart forgets it, and so
// does its letting go of the seat ([Coordinator.ReleaseSeat]), so the job's
// next announcement asks whoever holds the seat then. The waiter announces the
// job on its schedule meanwhile, as it does for any refused completion; each
// announcement costs this node a lookup, not a collection and a review, and the
// job's box, which the collection paused, is left asleep, since the job was
// read back already ([Waiter.reach]). A seat's budget, which every node reads
// alike, holds on the row instead ([HeldError]), and the completion is kept
// here for that hold's instant and its signal ([Coordinator.keepHeldOnRow]).
//
// A SIGNAL THAT LANDS WHILE THE GATE IS REFUSING IS NOT LOST. Readmit re-fires
// only what is kept when it runs, so a condition that cleared between the
// gate's answer and the keep would leave the completion kept for a signal that
// has already been and gone, until the waiter's next announcement of it. So the
// caller reads what was signalled BEFORE it asks the gate (seen), and the keep,
// under the lock Readmit takes, re-fires at once instead of keeping when the
// condition has been signalled for the seat since: the same rule
// [Coordinator.waitOwed] applies to an answer.
//
// NOR IS A PERSON'S STOP. A stop ends a collected job's wait by re-firing what
// is kept for it ([Coordinator.endCollectedWait]), and it leaves a run whose
// resume is out to that resume's turn. A resume refused after its claim returns
// minutes on, its review's call refused as the stop landed: kept then, its hold
// outlived the stop, and the stopped seat's work stayed open until the limit's
// instant, up to the hour an entry is benched for. So a stop is signalled for
// its seat too ([stopLanded]), and a keep that finds the seat stopped since it
// was asked re-fires at once, into a gate that admits a stopped seat's
// completion and a turn its fence closes as stopped.
//
// A refusal that names no condition is not kept: nothing would signal its end.
// Nor is one for a seat this node lets go of ([Coordinator.ReleaseSeat]): the
// waiter that drives its box announces it to the seat's next holder, or, while
// no waiter can, the delivery handed back for it does; and an acknowledged one
// is on the job's record, which that holder's recovery re-arms
// ([Coordinator.rearmOwed]). A drain's ([Draining]) is kept, re-fired by
// nothing but its own backstop, which the draining gate refuses again, until
// that release drops it: nothing signals a drain's end.
//
// NOR ONE THAT LANDS AFTER THAT RELEASE. The release waits for no resume, and a
// resume refused after its claim returns once its review has run, minutes on:
// kept then, its hold outlived the seat here, and the seat, back on this node
// inside the limit's window, had the job's announcements refused until the
// instant, by a hold the release was to have forgotten. The seat's lease is
// asked once the keep is made, outside the lock as every lease read here is:
// the seat host lets go of a seat before the release drops what is kept for it,
// so a keep that finds the seat held is one the release drops, and one that
// finds it gone is dropped now.
//
// keepHeldOnRow keeps the completion of run's job, whose resume its seat's
// budget refused once it ran and whose claim was handed back holding the run's
// row until hold.Until ([HeldError], [Coordinator.unclaimHeld]): kept UNHELD
// here, for a re-fire of its own at that instant and never sooner
// ([Coordinator.keptRefireIn]), and for hold's condition's signal, which lifts
// the row's hold and then re-fires it ([Coordinator.liftAndRefire]). trigger is
// the completion's event, or nil for one rebuilt from the row
// ([announcementOf]); seen is what was signalled before the resume that met the
// hold was admitted ([Coordinator.resumeAndSettle]), or before a claim the hold
// refused was admitted ([Coordinator.keepHeldClaim]), or before a recovery read
// the held row ([Coordinator.keepHeldRows]); and recovered is whether the
// caller is that recovery, which keeps nothing and re-fires nothing for a job
// this node already carries ([Coordinator.carriedLocked]), decided under the
// lock the keep is made under. Reports whether the completion was kept for the
// hold's instant: not where it was re-fired at once, was carried already, the
// coordinator is closed or this node no longer holds the seat.
//
// # Why a hold on the row is kept in memory too
//
// The completion's delivery is acknowledged, spent by the hold, because the
// waiter that drives the job's box announces the job again once the hold has
// lifted (or, where no waiter can, the delivery is handed back, renew.go). But
// that waiter reads the run's row through its node's coordination store, and a
// silence of that store's runs alone has no bound. Before the instant nothing
// else carried the job: the claim refuses a held row, the waiter passes one
// over, a signal only lifted the hold ([Coordinator.liftHoldsLocked]), and a
// recovery re-armed only a refusal owed on the job's record. So with that
// host's store silent, and no restart and no seat move, the job waited after
// its hold had lifted for as long as the silence lasted. Kept here, it is
// re-fired by this node as a refused completion is ([Coordinator.keepLocked]).
//
// NOT HELD HERE, and never re-fired before the instant: the row holds it, for
// every node alike, and a re-fire before the hold lifts claims nothing, which
// settles nothing and drops the keep.
//
// NOR LOST TO A CLAIM THE HOLD REFUSES: a delivery of the completion admitted
// leaves the keep standing ([Coordinator.forgetAdmitted]), and a claim the hold
// refuses where nothing is kept for the job, what was kept in the keep's place
// re-fired into the hold, keeps it again from the row
// ([Coordinator.keepHeldClaim]).
//
// A SIGNAL THAT LANDED BEFORE THE KEEP IS NOT LOST, as for a refused completion
// ([Coordinator.keepRefused]): one whose listing of the runs ran before the
// hold was written, or was not answered, lifted nothing, so a condition or a
// person's stop signalled for the seat since seen lifts the hold here and
// re-fires at once. Seen is read before the resume's admission, never once the
// resume has returned: a signal during the resume, after its budget refused it,
// found the row the claim with no hold to lift, and read after it the job
// waited for the hold's instant, until the budget's window turned. The cost is
// one review re-run at once for a signal the budget still refuses.
//
// keepAgain keeps kept once more, its own re-fire armed afresh
// ([Coordinator.keepLocked]), after a re-fire of it that neither settled it nor
// was kept again by a refusal: unless something is kept for its run already (a
// refusal's keep, or a recovery's re-arm still standing), which owns the
// re-fire, and unless this node no longer holds the seat, whose next holder
// owes the completion from the job's record.
//
// KEPT UNHELD, FOR THE BACKSTOP ALONE. What re-fired it ended whatever its
// refusal held it to (the condition's signal, a person's stop, the instant
// itself), and what it met since is a step that settled nothing, which names no
// instant of its own. Kept again as it was, a completion held here until a
// limit's instant ([Coordinator.stillRefused]), re-fired by the signal that
// ended the limit and then not answered by the store, was held again: the
// waiter's announcements of the job refused here, and its re-fire armed for the
// instant the signal had ended, up to the hour a vendor's window is benched
// for. Nor is a hold on the run's row it was kept for: a re-fire is made once
// that hold is lifted or past ([Coordinator.liftAndRefire]).
//
// forgetAdmitted drops what is kept for ev's run once a delivery of ev's
// completion has been admitted, for the claim the delivery goes on to make,
// which settles the job whichever announcement carried it
// ([Coordinator.forgetRefused]).
//
// SAVE A KEEP FOR THE HOLD ON THE RUN'S ROW OF EV'S OWN JOB, short of the
// hold's instant ([Coordinator.keepHeldOnRow]): the store refuses that claim
// while the hold stands, so it settles nothing, and the keep is this node's one
// carrier of the job while the waiter that drives the job's box reads a silent
// store. Dropped here, it went with a delivery of the completion queued behind
// the resume that held the job, which the claim refuses, and the job waited,
// after its hold had lifted, for as long as that store's silence lasted. A
// claim that wins drops it ([Coordinator.completeAdmitted]).
//
// keepHeldClaim keeps ev's completion where a claim of it under fence the store
// refused finds nothing kept here for ev's job and the run's row still holding
// the job running, under no newer lease: for the hold on the row where it holds
// the job's resume ([PendingRun.Held]), as the resume that wrote the hold keeps
// it ([Coordinator.keepHeldOnRow]), against seen, what was signalled before the
// delivery's admission was asked; and for the backstop where it no longer does
// ([Coordinator.keepAgain]). trigger is the completion's event.
//
// # Why a refused claim reads the row
//
// The keep made where the hold was written is this node's one carrier of the
// job while the waiter that drives the job's box reads a silent store, and a
// run has one keep: a refusal of the node's gate is kept in its place
// ([Coordinator.keepRefused]), and a re-fire that settled nothing, the signal's
// re-fire of a hold whose release this node still owed among them, is kept
// again unheld, for the backstop ([Coordinator.keepAgain]). Each one's re-fire
// met the hold with its claim, settled nothing and left nothing kept, so the
// job waited, after its hold had lifted, for as long as that store's silence
// lasted. So a claim refused with nothing kept for its job reads the run's row,
// one read, and a hold on it keeps the completion for the hold's instant and
// its signal again.
//
// A ROW STILL RUNNING THE JOB AND HELD NO LONGER keeps it for the backstop: the
// claim met a hold since lifted, its condition's lift of every hold on it
// landing between the claim and the read, with nothing kept for that signal to
// re-fire; or a row that moved under the claim. Either way the claim settled
// nothing and the row still owes the job its resume. So does A ROW THE STORE
// DOES NOT ANSWER: nothing says it does not hold the job. A row that holds
// another job, this one claimed or settled, or a newer lease's, is no longer
// this delivery's: the claim refuses a row a newer lease owns, whose holder
// recovers it.
//
// liftAndRefire is a signalled re-fire of a completion whose resume is held on
// its run's row: the hold for kept's condition lifted first
// ([PendingStore.LiftHold]), and the completion re-fired once that write has
// landed, or found the row held for it no longer ([Coordinator.refire]).
//
// THE LIFT FIRST, because the claim refuses a held row: re-fired beside the
// lift ([Coordinator.liftHoldsLocked] lists and lifts every hold on the
// condition in the background), the re-fire's claim met the hold still
// standing, claimed nothing, and left nothing kept. A lift the store does not
// answer leaves the completion kept as it was, for its re-fire at the hold's
// instant ([Coordinator.keepUnlessKept]), when no claim is refused for it.
//
// ITS LIFETIME IS THE COORDINATOR'S, for the reason [Coordinator.refire] gives.
//
// refireLifted re-fires the completion of run's job once a signal's lift of the
// hold on its row for cond has landed ([Coordinator.liftHoldsLocked]), on a
// goroutine of the coordinator's own lifetime, as the lift of a hold this node
// keeps the completion for re-fires it ([Coordinator.liftAndRefire]).
//
// # Why every lift re-fires
//
// The completion's delivery was spent by the hold, and the waiter that drives
// the job's box, which announces the job once its hold has lifted, reads the
// run's row through its host's coordination store, whose silence has no bound.
// A lift that found nothing kept here for the job left it to that waiter alone:
// a signal that landed while the seat's recovery ran, its lift landing before
// the recovery's fence read the row ([Coordinator.RecoverSeat]), so the row the
// recovery took over was held no longer and nothing was kept for it; or a keep
// replaced by a refusal's ([Coordinator.keepRefused]) and dropped since.
//
// BUT NOT A JOB THIS NODE ALREADY CARRIES ([Coordinator.carriedLocked]): one a
// re-fire of is on its way, the keep this signal released among them, which the
// signal lifts and re-fires itself ([Coordinator.liftAndRefire]); and one it
// keeps a completion of for anything but this hold, whose own re-fire, signal
// and backstop carry it. Re-fired beside either, the job was re-fired twice at
// once: a seat's recovery that read the row held before this lift landed kept
// the job and, the signal landing since, lifted and re-fired it, while this
// lift re-fired it too; and a keep's own lift raced this one, each re-firing
// the job. A keep replaced by a refusal's re-fires within that refusal's
// backstop at the latest, and asks the gate that refused it, which this re-fire
// would ask too.
//
// # A keep for the hold this lift cleared is re-fired in its place
//
// The signal re-fired every keep for its condition the moment it landed
// ([Coordinator.refireRefusedLocked]). So a keep for this hold that the lift
// finds was made since: by a seat's recovery, or after a claim the hold
// refused, that read the row held before this lift landed and read what was
// signalled after the signal, so it found no signal since
// ([Coordinator.keepHeldOnRow]). It waits for the hold's instant, the budget's
// window turning, and skipped as a job the node carries, the job waited that
// long after its hold was gone while the box's host's store was silent. So it
// is re-fired here, in the keep's place ([Coordinator.refireHeldKeepLocked]). A
// re-fire on its way whose claim met the hold, and whose read of the row came
// before this lift, makes the same keep, so the lift is left to that re-fire's
// end, which re-fires the job where it left it kept for this hold
// ([Coordinator.refired]), unless a claim of the job has won since
// ([Coordinator.forgetRefused]). Either way one re-fire is on its way at a
// time.
//
// carriedLocked reports whether this node already carries turnID's job: it
// keeps a completion of it ([Coordinator.refused]), whose own re-fire, signal
// and backstop carry it, or a re-fire of it is on its way
// ([Coordinator.refiring]). A frame that would start a re-fire of the job from
// outside both, a signal's lift of the job's hold ([Coordinator.refireLifted])
// or a recovery's keep or re-arm of it from its row
// ([Coordinator.keepHeldRows], [Coordinator.rearmOwed]), starts none where it
// does; save that the lift re-fires a keep for the very hold it cleared in its
// place, and hands itself to the end of a re-fire on its way, since such a keep
// waits for an instant the lift ended ([Coordinator.refireHeldKeepLocked]). The
// caller holds c.mu.
//
// # Why a lift or a recovery starts no second re-fire
//
// Two re-fires of one job race each other through the claim, and the claim is
// at-most-once, so only one resumes it; but the other is not nothing. It asks
// this node's gate, claims, and, refused, reads the run's row for a hold to
// keep the job for ([Coordinator.keepHeldClaim]), all for a job the first
// carries already; and it meets whatever the first left on the row at that
// moment, a claim, or a hold the first's resume met again and wrote, which it
// keeps the job for a second time. A signal's lift of a hold this node kept the
// job for raced the keep's own lift and re-fire of it, and a seat's recovery
// that read the row held before the signal's lift landed raced that lift's
// re-fire, each pair re-firing the one job twice. So the claim's at-most-once
// decides which re-fire resumes the job, and this decides that there is one to
// decide.
//
// refire offers a kept completion to [Coordinator.OnCompleted] again, under the
// coordinator's own lifetime, as the waiter's next announcement would. A
// refusal keeps it again. ANY OTHER FAILURE KEEPS IT TOO, for its own re-fire
// at the backstop ([Coordinator.keepAgain]): the store not answering its claim,
// or this node's gate failing to ask the seat's lease, settles nothing, and
// left to the waiter's next announcement the completion waited with no bound
// wherever that waiter's store was silent and nothing had found so.
//
// BUT NO WAITER ANNOUNCES A JOB WHOSE BOX'S HOST FINDS ITS STORE SILENT, and a
// re-fire has no delivery in hand for the broker to bring back. Where its own
// attempt started this node's clock on that host's store
// ([Coordinator.hostClocked]), its claim won and its read of the box found that
// store silent first ([startingClock]), the delivery that carried the
// completion was acknowledged when it was kept, no clock running then, so the
// re-fire's completion is announced afresh at once, whatever the hand-back
// (renew.go), and a renewal the broker does not take is said here, as what it
// leaves: no delivery to make it again on, so this node makes it again on its
// own timer until one lands or this node settles the run, whoever holds the
// seat by then ([refirePublish]). WHERE THE CLOCK RAN BEFORE IT, OR THE RUN'S
// ROW SAID WHAT IT WOULD, NOTHING IS ANNOUNCED: the delivery refused then was
// handed back, never acknowledged, and is in flight still, renewed on its own
// budget, so an announcement made here beside it would leave two in flight, and
// one more with every refusal and re-fire after it. NOR WHERE ANOTHER ATTEMPT
// STARTED THE CLOCK while this one ran, its claim losing to that attempt's, or
// refused while that attempt's release was out, or winning after it to find the
// clock running: that attempt was a delivery's, handed back and in flight,
// which carries the completion. Renewed wherever the clock ran once the attempt
// was done, such a re-fire put a second announcement beside it. NOR WHERE ITS
// OWN ATTEMPT SETTLED THE RUN after starting the clock, its hand-back refused
// and the run ended, which drops the clock ([Coordinator.runSettled]): the
// clock it started must still run once the attempt is done. Renewed on the
// start alone, such a re-fire announced the completion of a run already ended,
// and a refusal of that announcement was made again on this node's timer with
// no settle left to drop it.
//
// ITS LIFETIME IS THE COORDINATOR'S (c.life, ended by [Coordinator.Stop]),
// never a caller's: a re-fire runs on a goroutine of its own after the
// delivery, the signal or the seat's release that started it has returned, so
// no caller's context reaches it.
//
// answerWaitBackstop is the longest a retry the seat's conditions refused waits
// for the signal that clears them before it re-checks on its own.
//
// A BACKSTOP AND NOT A SCHEDULE. Every condition a refusal can name changes on
// an event the engine already observes and passes on ([Coordinator.Readmit]) —
// a person resuming the seat, an apply bringing a model or a ceiling, the
// posture admitting work again, the seat established or its ownership proven
// again — or on a clock the refusal itself names ([Refusal.Until]). What this
// bounds is a signal that never came: a path that moves one of those conditions
// and was never wired to say so. Five minutes is one admission check (this
// node's own reads and, at most, one read of the token counters) per refused
// answer per five minutes while nothing has changed, and it keeps a missed
// signal's cost — the person's answer resumed late — well inside the 30-minute
// default `pause_ttl_seconds`, so a missed signal does not also cost the run
// its paused box. Re-checking every 30 seconds would poll conditions that
// change only on events.
//
// AND A KEPT COMPLETION'S, on the same reasoning ([Coordinator.keepLocked]):
// one ask of this node's gate per kept completion per five minutes, the ceiling
// the waiter's own repeat reaches at the default cadence
// ([reannounceCeilingPolls]), so a completion no waiter announces, its host's
// store silent, is re-fired no later than one the waiter does. Save one this
// node holds off itself until an instant, which every announcement before it
// would meet refused here, and one whose resume its run's row holds until an
// instant, whose claim the store refuses before it: each one's re-fire waits
// for that instant ([Coordinator.keptRefireIn]). The first still has its
// backstop before the instant, which re-fires nothing and asks only whether its
// turn's deadline has passed, as an announcement would
// ([Coordinator.recheckHeld]).
//
// Readmit re-checks at once every attempt waiting on cond, for the named seats
// or — named none — every seat: the signal that the condition may have cleared.
// An attempt waiting on anything else is left waiting, so a signal costs
// nothing but the attempts it can actually release, and one that turns out not
// to have released them waits again on whatever refuses it now.
//
// The attempts are an owed answer's retry, a completion this node refused on
// cond, at its gate or held here after its claim ([Coordinator.keepRefused]),
// which is re-fired at once rather than at the waiter's next announcement of
// it, and a resume held on cond on its run's row after its run was collected
// ([PendingRun.HeldFor]), whose hold is lifted and its job re-fired once the
// lift has landed ([Coordinator.liftHoldsLocked]), and whose completion, kept
// here for that hold ([Coordinator.keepHeldOnRow]), is re-fired once its own
// lift has landed besides ([Coordinator.liftAndRefire]), so a listing of the
// holds the store does not answer still releases it.
//
// Safe from any goroutine, and cheap enough to call on every occurrence of the
// event: with nothing waiting it is one pass over this node's owed answers and
// refused completions, and one listing of the runs, in the background, for the
// holds.
//
// liftHoldsLocked lifts, in the background, every hold on cond this node's
// seats' runs are under ([PendingRun.HeldFor]) — the seats named, or every seat
// — and re-fires the completion of each job whose hold it lifted and this node
// does not carry already ([Coordinator.refireLifted]), which the waiter would
// announce again on its schedule only while its host's store answers it. The
// caller holds c.mu.
//
// THE ROWS ARE THE RECORD, not a list in memory: a hold is written on the run
// and survives the node, so a node that took the seat over since lifts what its
// predecessor held. Only the seats this node holds, because a peer's signal is
// its own to act on, and a lift is a write under the lease.
//
// IN THE BACKGROUND because Readmit is a signal sent from the engine's own
// event paths and must stay cheap; the listing is one read of the store.
//
// NOT FOR [SeatCoding], which no hold names. A hold is a resumed turn refused
// by its seat's conditions as the engine words them ([HeldError],
// [TurnRefused]), while SeatCoding is the coordinator's own, asked of an owed
// answer before its turn is admitted at all. Its signal comes every time a
// seat's last holding run lets go of it ([Coordinator.seatFreed]), and listing
// every run the fleet holds on each of those, to find none, cost a read of the
// whole store per freed seat.
//
// # Owed refusals
//
// completionUnsettled reports whether the job the row holds has a completion
// nothing has settled yet, which an acknowledged refusal of it leaves owed
// ([PendingStore.OweRefusal]): the job running, or claimed for its completion
// ([CompletionTail]). The caller matches the job, and writes live, which
// refuses a run whose ending is decided ([ErrRunEnding]).
//
// A CLAIM SETTLES NOTHING BY ITSELF. A kept completion is re-fired the moment
// its condition is signalled, so a re-fire's claim can land between the gate's
// refusal of a delivery and that refusal's record, and a claim handed back, its
// attempt cut short before its collection heard from the box's host, puts the
// completion back as it was: owed only where the job was unclaimed, the refusal
// acknowledged meanwhile was owed by nothing once it was. A RECORDED ANSWER'S
// CLAIM is not one: the job it holds parked on its question, which settled its
// completion, and its answer carries the run on.
//
// OweRefusal records on job launch's record that the seat's holder acknowledged
// a delivery of its completion it refused, and owes the completion its re-fire
// ([LaunchRecord.RefusalOwed]), reporting whether THIS call wrote it: ONE
// WRITE, made only where the row still holds that job with its completion
// unsettled, running or claimed for its completion, with no ending decided,
// since a claim handed back puts the completion back as it was, and its record
// owes no refusal yet, so a re-announcement refused again writes nothing and
// the record keeps the first refusal's condition and instant. Under the
// holder's fence: a row a newer lease owns refuses it with [ErrSeatNotHeld]. A
// refusal that names no condition, or one past [MaxOwedConditionBytes], or no
// instant, is an error and writes nothing. FALSE IS NOT AN ERROR: the job moved
// on, its ending was decided, or the row is gone, and nothing is owed for it.
//
// oweRefused is answer, what a delivery of ev's completion leaves with the
// control topic, once a refusal the topic acknowledges is recorded on the job's
// record as this holder's obligation ([PendingStore.OweRefusal],
// [LaunchRecord.RefusalOwed]); every other answer is returned as it is.
//
// # Why an acknowledged refusal is written down
//
// Acknowledging a refusal is safe while the waiter that drives the job's box
// announces the job again, and the completion is kept here for the refusal's
// signal besides ([Coordinator.keepRefused]). But that waiter reads the run's
// row through its node's coordination store, and a silence of the store's runs
// alone has no bound: its node's lease and presence are another stream, so no
// watchdog ends it and no peer finds it gone. A kept completion is this node's
// memory, which its restart, or its letting go of the seat, forgets; one
// acknowledged before any collection of the job had found that silence left
// nothing anywhere that owed the job a re-fire, for as long as the silence
// lasted. So the obligation is durable state, on the row every holder of the
// seat reads at its recovery ([Coordinator.RecoverSeat]), written by ONE write
// before the delivery is let go, and only where the record owes none yet, so a
// re-announcement refused again writes nothing: each acknowledged refusal costs
// one read of the row, and the first of a job's a write besides.
//
// # Which refusals
//
// EVERY ONE THE TOPIC ACKNOWLEDGES, wherever the job's box is hosted: a refusal
// that names a condition (internal/engine's controlOutcome), the gate's own, a
// completion this node holds off itself ([Coordinator.stillRefused]) and a
// resume reverted on a wait after its claim alike. Not one handed back marked
// [ErrUnannounced], which is renewed and so carried by its delivery; not a
// store's silence; and neither of the two conditions the caller hands on to the
// seat's next holder rather than acknowledging, [SeatLost] and [Draining],
// since a delivery handed on leaves nothing owed here. A drain's refusal
// recorded cost a write per job its deferred delivery never needed, and a
// record the store did not take made it the store's silence, handed back to the
// node that is leaving rather than on.
//
// # Where the record does not land
//
// The refusal is NOT acknowledged. The answer is then the store's silence alone
// ([ErrStoreSilent], the node's failure, as every step the store does not
// answer is), which the caller hands back, so the delivery comes back to be
// recorded; or, where this node's own context ended, that cause
// ([storeSilent]), which the caller hands back as the node's as well. Never the
// refusal: nothing it hands on reaches the write, so every refusal here is one
// the caller would acknowledge. The completion is still kept here for its
// signal either way. A row a newer lease owns refuses the write
// ([ErrSeatNotHeld]), which the caller names [SeatLost] where this node no
// longer holds the seat ([Coordinator.lost]), so the delivery goes on to the
// holder, and hands back while this node's own lease still says it holds the
// seat, for the heartbeat that settles which is right ([SeatLost]); and a seat
// this node let go of before the write is deferred the same way, never
// acknowledged by a node that owes nothing any more.
//
// Draining is a resume refused because this node is draining and starts no
// turn: the engine's gate names it ([TurnGate]). Nothing signals its end,
// because nothing undoes a drain: it ends when the drain hands the seat back,
// which lets go of what this node keeps for it ([Coordinator.ReleaseSeat]), or
// when the node stops. So the caller that settles a completion's delivery hands
// it on to the seat's next holder rather than acknowledging it
// (internal/engine's controlOutcome), as it hands on [SeatLost], and an answer
// waiting on it waits for that release.
//
// THIS PACKAGE NAMES IT, though only the engine's gate refuses on it, because
// what it decides is this package's: whether the refusal leaves an
// acknowledgement owed on the job's record ([Coordinator.oweRefused]). A
// delivery handed on is never acknowledged, so it owes nothing. Named in the
// engine alone, a drain's refusal was written on the job's record once per job,
// a write its deferred delivery never needed, and one the store did not take
// came back as the store's silence, which the caller handed back to the node
// that is leaving rather than on.
//
// # Re-arming owed refusals and holds
//
// rearmOwed owes again, on this node, the completion of each of runs' jobs,
// whose record says a holder of the seat acknowledged a refused delivery of it
// and owes it its re-fire ([LaunchRecord.RefusalOwed]): kept as
// [Coordinator.keepRefused] keeps a refusal, for the recorded condition's
// signal, and offered to this node's gate at once.
//
// WHY THE ROW. The holder that acknowledged the refusal kept the completion in
// its memory for the condition's signal, and its restart, or the seat moving to
// this node, took that with it; the waiter that drives the job's box would
// announce the job again, but reads the run's row through its node's
// coordination store, and a silence of that store's runs alone has no bound.
// Read here, the obligation is this holder's as it was that one's, and nothing
// waits on that store.
//
// AND AT ONCE, NEVER ON THE RECORDED CONDITION ALONE. The condition is the
// holder's that refused, and its signal an edge on that node: a provider park's
// release, a pause watch's change, a seat turn's end, an apply. A holder whose
// models know no limit, or a seat no turn holds, never sees it, and re-armed on
// it alone the job waited for a signal that never came. So the completion is
// offered to this holder's own gate at once, as the first delivery after a
// restart or a seat move is (ADR-0064): admitted, it is carried; refused, it is
// kept for this holder's own condition, which replaces the re-arm
// ([Coordinator.keepRefused]), and whose signal carries it, the seat's
// admission above all, which every acquisition signals once its recovery is
// done. A re-fire that fails otherwise, a store that did not answer it, leaves
// the completion kept all the same: the re-arm stands where the gate could not
// ask, and where the gate admitted the re-fire, which drops the re-arm, and the
// claim went unanswered, the re-fire keeps it again ([Coordinator.keepAgain]).
// AND EITHER KEEP HAS A RE-FIRE OF ITS OWN at the backstop
// ([Coordinator.keepLocked]): kept on the recorded condition alone, whose
// signal this holder may never see, the completion waited for the waiter's next
// announcement, which a silent store keeps from coming, and for nothing else
// while this holder kept the seat.
//
// A job this node already carries is left as it is, and nothing is re-fired for
// it ([Coordinator.carriedLocked]): a completion it keeps for the job, a
// delivery's refusal since the seat was taken, and a re-fire of the job on its
// way, a signal's that lifted the job's hold while the pass ran among them
// ([Coordinator.refireLifted]).
//
// keepHeldRows keeps on this node the completion of each of runs' jobs, whose
// resume a holder of the seat handed back held on the run's row
// ([PendingRun.HeldFor], [HeldError]), as that holder kept it
// ([Coordinator.keepHeldOnRow]): unheld here, for its own re-fire at the hold's
// instant, at once for one already past, and for the hold's condition's signal,
// which lifts the hold and re-fires it.
//
// WHY THE ROW. The holder that met the hold kept the completion in its memory
// for that instant, and its restart, or the seat moving to this node, took that
// with it; the waiter that drives the job's box announces the job once the hold
// has lifted, but reads the run's row through its node's coordination store,
// and a silence of that store's runs alone has no bound. NOT AT ONCE, as an
// owed refusal is ([Coordinator.rearmOwed]): the claim refuses a held row, so a
// re-fire before the instant claims nothing.
//
// A SIGNAL THAT LANDS WHILE THE PASS RUNS IS NOT LOST: seen is what was
// signalled before the pass read the seat's runs, and a condition or a stop
// signalled for the seat since lifts the hold here and re-fires the job at
// once, as for a hold a resume here wrote ([Coordinator.keepHeldOnRow]). The
// signal lifts every hold on its condition itself and re-fires each job it
// released ([Coordinator.liftHoldsLocked]), but from a listing of the runs,
// which the store may leave unanswered where it answers one row, and it found
// nothing kept here yet: kept for the instant regardless, the job waited for
// the hold's instant after the signal that lifts it.
//
// A JOB THIS NODE ALREADY CARRIES IS LEFT AS IT IS
// ([Coordinator.carriedLocked]): a completion it keeps for the job, and a
// re-fire of the job on its way. The signal's lift that landed on the row after
// the pass's fence read it held re-fires the job ([Coordinator.refireLifted]),
// and kept here too, with that signal since seen, the job was lifted and
// re-fired a second time beside it. Asked under the lock the keep is made under
// ([Coordinator.keepHeldOnRow]), so that lift's re-fire cannot start between
// the question and the keep; and a lift that lands once the keep is made, its
// signal read before the pass read the seat's runs, re-fires the keep in its
// place ([Coordinator.refireLifted]).
//
// RefusalOwed, where set, says the seat's holder ACKNOWLEDGED a delivery of
// this job's completion that it refused on this condition, and owes the
// completion its re-fire; RefusalOwedSince is when it did, by that holder's
// clock. Both or neither. Written by the one write [PendingStore.OweRefusal]
// makes before the delivery is acknowledged, and by the release that holds the
// job's resume on the row ([Release.Hold]), whose hold spends its delivery as
// an acknowledgement does, with the hold's condition and the store's instant;
// each only where the record owes none yet, so the first refusal's condition
// and instant stand through every re-announcement refused after it; cleared by
// the write that settles the completion: the park on the job's question
// ([PendingStore.MarkAwaiting]), the turn's next launch, which starts the job's
// record afresh, or the run's end, which deletes the row. A claim leaves it: a
// claim handed back puts the completion back, still owed, and one whose resume
// settles it reaches one of those three writes. And a refusal acknowledged
// while another attempt holds the job's claim is written on the claimed row,
// for that reason.
//
// ON THE JOB'S RECORD, NEVER IN ONE NODE'S MEMORY ALONE. An acknowledged
// refusal is safe because the waiter that drives the job's box announces the
// job again; but that waiter reads the row through its node's coordination
// store, and a silence of the store's runs alone has no bound (the node's lease
// and presence are another stream, so no watchdog ends it and no peer finds it
// gone), while the completion the holder kept for the refusal's signal went
// with its restart, or with the seat moving to its next holder, and nothing
// announced the job again. So the seat's recovery reads this and owes the
// completion as the holder did ([Coordinator.RecoverSeat]): re-armed for the
// condition's signal, and offered to the recovering holder's own gate at once.
//
// AN OBLIGATION, NEVER A HOLD: nothing reads it as a reason to refuse or to
// hold off the job's announcements, and the condition is the holder's that
// refused; the recovering holder asks its own gate (ADR-0064). A build that
// predates it keeps both keys in [LaunchRecord.Extra] and writes them back as
// they are, and neither writes, reads nor clears them.
//
// # Owed answers
//
// An answer that is RECORDED and still OWED a resume.
//
// # Why the answer is recorded before anything is done with it
//
// Held by its inbox delivery until a resume succeeded, a person's reply is lost
// to order: a resume that failed hands the MESSAGE back to the queue with a
// Nak, and on the only broker this engine ships a Nak'd message comes back
// after its redelivery backoff, BEHIND the conversation's newer mail. So:
//
//  1. a run parks with a question in a thread;
//  2. the reply R1 arrives, the resume fails on something transient, and R1
//     is handed back;
//  3. the person's next message R2 arrives during R1's backoff, reaches the
//     still-waiting run first, and is spliced in as its answer;
//  4. R1 comes round afterwards and is either spliced into the run as the
//     answer to whatever it asked NEXT, or run as a turn with no context.
//
// Nothing compared a reply with the question it was supposed to answer — the
// match was positional, "the next inbound on the conversation" — and the
// in-memory twin replayed a Nak at the head, so every test certified the order
// production never gave.
//
// So receiving an answer and resuming with it are now two steps with two
// owners. The FIRST QUALIFYING reply — posted at or after the question
// ([PendingRun.AskedAt]) — is recorded on the row by a compare-and-set
// ([PendingStore.RecordAnswer]), and its delivery is acknowledged: the answer
// is durable from that moment and nothing about it depends on the inbox again.
// Resuming is then the coordinator's: one attempt inline, and every retry on
// this package's own backoff ([answerRetryDelay]) under the same two bounds a
// handed-back delivery had ([MaxAnswerAttempts] and [answerWindow]). A later
// reply finds the question answered and is the ordinary message it looks like.
//
// # Why the seat's inbox is held while an answer is owed
//
// The resume no longer runs inside the delivery that carried the answer, so
// nothing about the inbox's own serial dispatch keeps the seat's later mail
// behind it. The coordinator holds the seat's inbox ([SeatHold], the hold
// seathold.go keeps for a busy seat too) for as long as any of the seat's
// answers is owed, and lifts it once the resume has RETURNED and, where the
// resume ended the run, its ending is finished: the turn's end recorded on the
// executor log, once every capture of the turn has settled, and the run's
// record deleted — by this node's retry, off the resume's frame, the answer's
// series kept until it is ([Coordinator.settleOwed]) — so R2 is worked after
// R1's resume and after its end, with the run's own replies already in the
// thread it reads, rather than racing either. A hold rather than a park,
// because the held mail waits on the broker with its place and its delivery
// count intact, where a park would republish it in a loop for the whole
// backoff.
//
// AN OWED RUN HOLDS THE SEAT ONLY WHILE SOMETHING DRIVES IT. Every step that
// owes a seat's run for a series' retry records that series and arms its
// attempt in the same step of the lock ([Coordinator.keepSeriesLocked],
// [Coordinator.oweRelease], [Coordinator.leaveToRelease]), since a seat's
// release drops all three together ([Coordinator.releaseOwed]). Owed in a step
// after the series, a run whose seat was released between the two was owed
// again with nothing to drive it, and held the seat's mail on this node until a
// settle of that run or the seat's next release. Only the inline attempt owes
// outside such a step ([Coordinator.resumeOwed]): it drives the run itself, and
// reads the seat's lease before it acts. A step that keeps or arms a series
// again without owing anew (a failed attempt's next, a refusal's wait) owes
// nothing, and what a seat's release leaves of that series is dropped by the
// attempt it arms, which finds the seat not held.
//
// # What ends an answer that cannot be resumed
//
// The attempt bounds. A run nothing on this node can resume (no runner, a
// conversation this build cannot decode, a seat the epoch does not hold) gives
// its answer up after them ([Coordinator.declineAnswer]): the run goes back to
// waiting on its question, the reply is handed to the seat's ordinary route as
// the message it looks like, and that reply is declined for this question so it
// is never recorded against it again. That is exactly where the handed-back
// delivery used to end up, with the difference that it now gets there EXACTLY
// ONCE, in order, rather than circling the inbox: the decision and the copies
// it owes are one write, and the copies are published off the row afterwards
// under ids derived from the originals, so a crash at any step is finished by
// the next reader of the row without losing the reply or delivering it twice
// (see [Coordinator.declineAnswer]). The run itself stays resumable: the seat's
// next holder, or this node after a restart, is offered the next qualifying
// reply.
//
// A RUN THAT ENDS BEFORE ANY TURN TOOK THE ANSWER lets it go the same way,
// whoever ends it and on whichever route the answer arrived: the store will not
// delete a row that still holds a reply no turn took ([ErrAnswerOwed]), so the
// ending takes the answer off the row and records its copies as owed in one
// write, and hands them back before it deletes the row
// ([Coordinator.letGoAnswer], [Coordinator.endRecord]). That covers a retry or
// an inline attempt whose run turns out to be over, a resume that broke before
// its turn began, and the seat's next holder reaping a claim whose node stopped
// before its turn TOOK the answer, once the answer's revivals are spent
// ([RecordedAnswer.TakenAt], [Coordinator.reapTail]) — the one fact that tells
// a reply nobody acted on from one a turn has used.
//
// # Spent when it leaves
//
// A reply that leaves its run — taken by a turn, on the inline attempt as on a
// retry, or let go of — has the delivery that carried it recorded as worked in
// the completion ledger at that moment ([CoordinatorOptions.Spent]): at the
// take, and before the let-go's write. A node that recorded the answer, or took
// it inline, and stopped before acknowledging the delivery leaves it to come
// round again, and by then nothing on the run recognises it as the answer it
// was.
//
// # A reply too large to be the answer is recorded as its refusal
//
// A reply past what the row carries as an answer ([MaxAnswerBytes]) cannot be
// recorded, and was once left to go its way as the ordinary message it looked
// like: the seat answered it knowing nothing of the run, the run kept waiting,
// and the person's next short message ("any luck?") was the first reply the
// question could take. So its REFUSAL is the question's outcome instead
// (ADR-0114): recorded in the answer's place by the same compare-and-set
// ([PendingStore.RecordRefusal]) — its deliveries, when it was posted and the
// figures, sealed, none of its words — and resumed as an answer is, through
// every step above: owed with the seat's inbox held behind it, retried, taken
// once, revived, or let go of. The resumed run is told the reply did not reach
// it ([AskedRun.Refused]), its working indicator raised where the reply was
// posted ([AnswerRefusal.Address]); while the refusal is the run's the question
// takes nothing written after the record, and a question the run asks again is
// anchored on its own asking. What the row cannot carry it does not need to:
// the delivery is kept on the seat's inbox, unspent, until the refusal leaves
// the run ([AnswerRefused]) — spent once a turn that took it keeps it
// ([Coordinator.spendRefused]), never at the take, which a turn that broke
// before it reached anybody gives back, and back to the seat as the ordinary
// message it is if the refusal is let go of, with nothing to publish from the
// row. The delivery in hand on the attempt whose turn kept the refusal is spent
// on that attempt's own account ([Coordinator.attemptOwed]), never on the
// completion ledger's, which fails open: read there, a miss handed the seat, as
// a new message, a reply its run had just been told never reached it. A refusal
// let go of — after its attempts, or a seal that does not open — leaves the run
// waiting on its question again under the anchor it was asked at, as an answer
// let go of does: the next qualifying reply answers it, and the let-go is
// announced ([Coordinator.announceRefusalLetGo]).
//
// # A retry the seat's own conditions refuse
//
// WAITS FOR WHAT REFUSED IT, rather than re-checking on a timer: every
// condition a retry is admitted under changes on an event the engine observes
// or at an instant the refusal names, so the refusal says which ([Refusal]),
// and the engine re-checks the attempts waiting on it when it happens
// ([Coordinator.Readmit]). A backstop ([answerWaitBackstop]) re-checks what a
// missed signal would strand. See [Coordinator.waitOwed].
//
// A CLAIM'S RELEASE THE STORE DID NOT ANSWER is made again first, with no read
// of the row before it: the release decides on the row as it stands. Owed again
// while the store is still silent, and once it answers, the attempt goes on
// against the row as the release left it (claimrelease.go): an answer's claim
// given back is the answered run this series exists to resume, or to let go of
// once its attempts are spent, and a completion's is back in running, which
// owes the series nothing and settles it. Settled at once instead, the answer
// was left on its run with nothing on this node to resume it.
//
// BUT NOT ONCE A CLAIM OF ITS JOB HAS TAKEN IT OVER, by winning on the run it
// put back, and NOT WHILE ONE IS ON ITS WAY: made then, it could land after
// that claim and give it back. The claim decides first, and a claim that starts
// from here on is refused while the release stays recorded
// ([Coordinator.claimCompletion]).
//
// queueOwed holds the seat's inbox behind what a run is owed and arms its first
// attempt at once, through the admission every retry passes
// ([Coordinator.retryOwed]) rather than inline.
//
// Two callers, for one reason: neither runs inside a mailbox delivery the
// seat's own screening admitted, so neither may start the resumed turn without
// asking. The seat's preparation takes over what a previous holder left owed on
// a run (an answer it recorded and did not resume with, or copies a decline it
// wrote did not publish), and an answer by turn arrives on the seat's control
// topic, at any moment of the seat's day: while another of its runs holds it,
// in the middle of one of its turns, or while a person has it paused
// ([Coordinator.AnswerByTurn]).
//
// KEPT, OWED AND ARMED IN ONE STEP ([Coordinator.keepSeriesLocked]).
//
// keepSeriesLocked owes the seat's run ([Coordinator.oweLocked]) and arms the
// next attempt of run's series after delay ([Coordinator.scheduleOwedLocked]),
// in the step of the lock that recorded the series and what it keeps; the
// caller brings the seat's inbox hold into line ([Coordinator.reconcileHold])
// once it lets go of c.mu.
//
// IN THE ONE STEP, because a seat's release ([Coordinator.releaseOwed]) drops
// the seat's series, their attempts and its owed runs together. A run owed in a
// step after the one that recorded its series, with the seat released between
// them, was owed again with no series and nothing armed to drive it, and
// nothing cleared it but a settle of that run or the seat's next release: the
// seat's mail stayed held on this node, behind a run of a seat it no longer
// held (a recovery of the seat resets nothing it owes). In the one step, the
// seat's release falls before it, and the attempt it arms finds the seat not
// held and drops the series ([Coordinator.retryOwed]), or after it, and drops
// all of it.
//
// # A turn's notes
//
// A person's notes to a turn whose executor is out, on the run's own record.
//
// # Who has to agree on them: the whole company, now
//
// From a run's launch to its turn's end, the notes a person sends the turn wait
// here, on the run's record in the coordination store — the same record that
// carries its bridged calls, and for the same reason. The node a person's
// request reaches, the node serving the run's tool bridge (the seat's holder),
// the node that resumes the turn and the node that finishes the run's ending
// can be four different processes across a restart and a seat moving, and each
// of them already reads this record. A note held in one node's memory would be
// lost with that node, or reachable from it alone.
//
// # What this package decides, and what it does not
//
// The record's shape, and the conditions every change to it is taken on: a note
// filed while the run's ending is undecided, taken only by the job the taker
// names and only while the run is in a status the taker names, every read
// recorded by the write that hands the note over — and a take whose answer was
// lost settled by its identity, its reads answered to a reader that can still
// hand them over or marked unanswered where its reader never had them
// ([PendingStore.SettleTake]).
//
// A HOOK'S SERVE IS NO READ. A harness answered its notes by a hook is SERVED
// them, in a write before its answer that keeps the call it answered
// ([PendingStore.ServeHook]), and a note so served is read only once the job's
// own session shows the hook took it: a verdict settled by the serve's take,
// beside the serves its harness cancelled or answered nothing
// ([PendingStore.SettleHook]). The first serve of a note fixes its place among
// the turn's reads ([RunNote.Place]), and every later read of it is recorded
// there. And an ITEM FOR ONE JOB ([RunNote.For]) is handed over only to a
// reader inside that job, and removed by the write of the launch that opens the
// next.
//
// What a note SAYS it never reads: a note is sealed by the engine under the
// fleet keyring before it is filed ([RunNote.Sealed]), and opened by the engine
// before anything of it reaches a model (internal/engine, runsteer.go, and
// ADR-0067). The store keeps the seal as the bytes it was handed, as it keeps
// an answer's ([RecordedAnswer.Sealed]). A FILING IS A PERSON'S NOTE ALONE
// ([PendingStore.FileNote]): an item for one job is the store's to file, in its
// own writes of the record, never a filer's, so a filing that names a job is
// refused ([ErrNoteKind]) and no filer can hand one job words a person's note
// would not reach it with.
//
// # Bounds are the caller's
//
// How many notes may wait, and how many a turn takes in all, are
// internal/agent/steer's ([NoteFiling]): this package must not import the agent
// layer, and a second copy of the two numbers here would be one that drifts.
//
// Served is the job the run's bridge last served the note to while it waited:
// by a poll ([PendingStore.ServeNotes]), since a harness that polls for its
// notes (OpenCode's plugin) is answered one only once this records it, and the
// note is recorded read only once the harness confirms delivering it; or by a
// hook's serve ([PendingStore.ServeHook]), the note read only once the job's
// own session shows the hook took it ([PendingStore.SettleHook]). So a note
// served and not read may have been delivered all the same, its confirmation
// lost or its harness cut off before it landed, and a launch that continues a
// session tells it as a note the session may already show
// (internal/agent/runner). CLEARED where a settle leaves the note no live hook
// serve to that job: every one lost, or removed as answered nothing. Kept once
// the note is read; the latest job served it, where more than one was.
//
// For is the one job the item is for, where the store made it that job's alone.
// FILED BY THE STORE ALONE, in its own writes of the record, never handed in by
// a filer: [PendingStore.FileNote] files a person's note and refuses one that
// names a job ([ErrNoteKind]), since an item tells a job something on the
// store's account rather than a person's. This build's store files none, and a
// record holds one only where a store's write put it there: wherever it does,
// it is counted apart from a person's notes ([MaxJobItems]), handed over by
// every write only to a reader inside that job ([RunNote.IsFor]), passed over
// by every reader of a person's notes (the ending, the carry, the review, a
// launch's brief, the capture's cursor), and removed by the write of the launch
// that opens the next job ([openNotes]). Empty for a person's note, an item of
// the whole turn.
//
// keptCallOf is call as a serve keeps it: whole where its session is of the id
// charset (letters, digits, `-` and `_`, [ValidCallID]'s) and its id is a JSON
// integer's text or a JSON string of that charset, each at most
// [MaxNoteCallBytes], a string's quotes counted; and NONE otherwise, as where
// it names no session or no id.
//
// ONE RULE, THE STORE'S. The call is the box's word, which the bridge hands on
// as it read it, so the write that serves holds every writer to the rule,
// whatever bridge, or build, handed the call, as the log holds the id a bridged
// call keeps ([keptCallID]).
//
// THE CHARSET REFUSES NOTHING REAL, AND HOLDS THE ROW. Claude Code numbers its
// JSON-RPC calls with integers, and the session is the bridge's own, which
// go-sdk mints as rand.Text, in base32. A kept call is then written as itself,
// each half costing the row its length and a string id's two quotes a byte more
// each, where the store's encoder writes a byte it escapes as six (`\u003c`):
// so the row's worst serve carries a call at its length
// (TestTheWholeRowFitsItsBudget).
//
// NEVER CUT: a cut call could match another call cut alike, and lose a serve
// its harness kept. A serve kept with no call is settled by its job's session
// alone.
//
// EndedNotes is every note on the run's record that no step reported reading,
// each as what the record knows became of it: what the run's ending reports
// ([CoordinatorOptions.SteersEnded]), decided in this order of precedence.
//
//   - DELIVERED: read by a launch's brief that told it again ([NoteRead.Retold])
//     after a take that recorded it read was marked unanswered, where that
//     job started. Its harness was handed the note in its brief; the step that
//     took it first never handed it over, and nothing reported the telling.
//   - UNCONFIRMED: SERVED to a job's harness ([RunNote.Served]) and read by no
//     step that handed it over: still waiting, or read only by a take marked
//     unanswered or by the brief of a job that never started. The session it
//     was handed into may hold it, and nothing says it does; a note that may
//     have reached the run is never said to have reached nothing. At the hook's
//     serve that was live before those reads, where a hook was served it
//     ([EndedNote.Serve]), the step it was handed over at; else at the job a
//     poll served it to. And a note whose EVERY LOST SERVE ITS SESSION MAY
//     HOLD ([HookServed.MayHold]), its harness having given the call up, at
//     the last of them, ahead of a marked take's read as a live serve is.
//   - MISSED: read by a take marked UNANSWERED ([NoteRead.Unanswered]), and
//     told again by no launch whose job started: the take's write landed, its
//     answer never reached its reader, and no later write records the note
//     handed to a harness. A launch that tells it again records the telling
//     and clears the mark ([PendingRun.RetoldNotes]), keeping the marked
//     take's read beside it ([NoteRead.Retold]): so a note still marked is one
//     no launch recorded telling again, and one whose telling's job never
//     started is missed all the same, at that take, since the brief handed
//     nothing over. And SERVED TO A JOB'S HOOKS, EVERY SERVE LOST, ONE ABSENT
//     ([ServeAbsent]), read by nothing since: at the last serve ([EndedNote.Serve]).
//   - EXPIRED: still WAITING, or read by the BRIEF OF THE RECORD'S CURRENT JOB
//     where that job never started and that brief was the first to tell it:
//     the job's record never took its command ([LaunchRecord.Started]),
//     because the launch failed, or was ended, between the write that opened
//     the job and the job's start.
//
// A note any other step read is none of them: that step published it delivered
// as it read it, and the ending reports nothing of it. NOR IS AN ITEM FOR ONE
// JOB ([RunNote.For]): the ending reports what became of a person's notes, and
// a job's item is the store's, filed in its own writes of the record, so what
// becomes of it is the store's to say.
//
// NEVER THE ROW'S COMMAND ([PendingRun.CommandID]): that is the box's, and goes
// with the box — a park's teardown and an expired pause clear it from a job
// that ran — so a turn ending after either would report expired the notes its
// job's brief carried, which its runner had already recorded delivered.
//
// THE RECORD IS THE ONE WITNESS. A launch records its brief's notes read in the
// write that opens its job ([PendingStore.BeginLaunch]), before it knows
// whether the job will run, and every way it can then fail ends the record
// ([Coordinator.abandon]) — or meets an ending another party decided, which
// finishes in its place. So the ending is the one party that sees both what the
// brief took and that no job ran with it, and it reports those notes under the
// same ids as the waiting ones: a launch that reported them itself could not
// tell a note its own brief took from one another node's ending had already
// reported, and published one twice. And the same holds of a take whose answer
// was lost, which reports nothing as it is lost; of a note a brief told again,
// whose job is known to have started only once the launch has returned; and of
// a note a harness was handed and never confirmed, which nothing else sees
// unconfirmed.
//
// openNotes is the write a launch makes of the turn's notes on the record it
// opens, under the job it opens ([PendingStore.BeginLaunch]), in this order:
//
//   - every ITEM FOR ONE JOB ([RunNote.For]) is removed first: the job this
//     write opens is new, so every item on the record was the job before it's
//     alone, and nothing in this one will read it. An item for the job this
//     write opens is the store's to file after it, held to [MaxJobItems];
//     the steps below are a person's notes';
//   - the brief's notes ([PendingRun.BriefNotes]) are recorded READ by the job:
//     a note the record already holds, still waiting, is recorded read; one it
//     holds read by the take the brief names as lost ([RunNote.LostTake]) is
//     recorded read by the job in that take's place, the brief being the first
//     to tell it, and at that take's own place among the reads, which this
//     write leaves as it was; one it holds read otherwise is left as it was
//     read; and any other — a turn's first segment's own, which the record has
//     never held — is added already read, numbered as if filed now. Every
//     other read this write records takes its place among the record's reads
//     ([RunSteer.Reads]), but a waiting note a hook's serve placed, which is
//     read at its own place ([RunNote.Place]), ahead of every note a person
//     filed after it;
//   - the notes the brief tells again ([PendingRun.RetoldNotes]) are recorded
//     read by the job, told again: each one the record holds read by a take
//     marked unanswered, in that take's place and at that take's own place
//     among the reads, which this write leaves as it was, the take's read kept
//     beside the telling ([NoteRead.Retold]). A note the record holds
//     otherwise is left as it is, and one it does not hold is never added:
//     there is nothing to tell again;
//   - then the notes the first segment's box still held ([PendingRun.HandedNotes])
//     are filed WAITING, numbered after them — so they are on the record from
//     the write that creates it, ahead of every note a person files there once
//     it exists. A note the record already holds is left as it is.
//
// The handed notes are filed WHATEVER THE TURN'S BOUNDS SAY: the box answered
// each one accepted under the same two bounds ([NoteFiling]), so each is a note
// the person was told the turn holds, and a hand-over that refused one would
// turn an accepted note into an expiry. They count against the bounds for every
// filing after them.
//
// MaxNoteServes bounds the serves of one note, or one job's item, to one job's
// hooks ([RunNote.Serves]), held by [PendingStore.ServeHook]: its first serve
// and two after losses. THREE, A JUDGMENT VALUE no run of losses has measured.
// A serve is lost when its harness gives its hook's call up, which takes an
// answer later than the hook's ten seconds (internal/sandbox/executor's
// ClaudeHookTimeout): a relay frame retried on top of a slow take, or a store's
// leader election across a boundary. One serve again delivers a note whose
// answer was lost once, and a second rides out a stall that spans two
// boundaries; past two losses in a row the path holds answers persistently, and
// each serve more would cost the job one more ten-second stall. A serve its
// hook was answered nothing for is removed rather than lost
// ([HookServe.Unanswered]), so a slow store spends none of it. Past it the note
// is passed over by the job's hooks and waits, served or lost, for the review
// or the next launch's brief, which read it at its place.
//
// notesHeld is the last note read ([RunSteer.Reads]) run's record says a
// session packed from its job holds ([SessionCapture.NoteCursor]): every one
// the record says was read, where its harness ended its own run, up to the
// first read it marks unanswered or restarted, or the place of a note a hook
// was served and nothing read; and NONE where it was cut off.
//
// NEVER PAST A READ MARKED UNANSWERED ([NoteRead.Unanswered]): its take's write
// landed and its answer never reached the harness, so the session lacks that
// note however cleanly it ended — while it holds the answered reads after it.
// One number cannot say both, so the cursor stops before the unanswered read,
// and a launch that continues the session tells every note past it, as notes it
// may already show (internal/agent/runner's keptSession). WHICHEVER JOB'S TAKE
// it was: a later launch whose brief told the note again records it read by its
// job, unmarked ([NoteRead.Retold]), and records only a note its brief told as
// marked. A mark made off the take's frame can land after a brief was built,
// held past the next launch by a store's bad moment, and that brief did not
// tell it as marked; so a read still marked unanswered stops the cursor of
// every capture after it, told again rather than never. An older build, which
// tells a continuation every note past the cursor with no header, tells the
// unanswered note too.
//
// NEVER PAST A NOTE A HOOK WAS SERVED AND NO STEP HAS READ ([RunNote.Place]):
// served and never confirmed, or its serve lost to a cancel or to its session's
// own record, it sits at its first serve's place among the reads, and the
// session may lack it while it holds the reads after it. So the cursor stops
// before that place, and a launch that continues the session tells it and every
// note past it as notes it may already show, as for an unanswered read. NOR
// PAST A READ MARKED RESTARTED ([NoteRead.Restarted]): the session shows the
// hook took the note only before its last restart, and a resume rebuilds its
// conversation from that restart on.
//
// AN ITEM FOR ONE JOB ([RunNote.For]) stops nothing: whatever its read, a
// continuation is told the turn's notes, and an item is no person's note.
//
// NONE FOR A SESSION CUT OFF: it is not known to hold its job's brief, the
// message that told the notes the session it continued lacked, an earlier job's
// among them. No probe has measured whether Claude Code 2.1.310 writes the
// message a continued session is handed before it answers a call, and no record
// says which notes that brief told, so any cursor but none says the session
// holds notes that may never have reached it. A launch that continues a session
// cut off tells it every note the turn has read, as notes it may already show,
// whatever its cursor (internal/agent/runner's keptSession); an older build,
// which tells it every note past its cursor, tells it every one.
//
// # Filing and serving notes
//
// FileNote files a person's note on a run's record ([RunSteer]), waiting, and
// answers what became of it with the record as it was written or — for any
// other answer — as the filing found it.
//
// IN EVERY LIVE STATUS, while the record exists and its ending is not decided:
// a note to a turn whose run is working, parked on a question or being
// collected is held for whatever reads the turn next. A note whose id the
// record holds is [NoteDuplicate], and nothing is added; a turn that holds
// [NoteFiling.MaxNotes] notes is [NoteSpent], and one with
// [NoteFiling.MaxWaiting] waiting [NoteFull]; a decided ending, or no record at
// all, is [NoteClosed]. NONE OF THOSE IS AN ERROR.
//
// THE TWO BOUNDS ARE A PERSON'S: they count a person's notes alone, and toward
// the waiting bound a note a hook's live serve handed to the record's job
// ([RunNote.LiveServe]) no longer waits, its hook's answer having handed it to
// the model at the step it answered; a note a poll served still waits until its
// harness confirms it. An item for one job the record holds ([RunNote.For])
// counts toward neither.
//
// A PERSON'S NOTE ALONE: a filing that names a job ([RunNote.For]) is refused
// with [ErrNoteKind] before anything is read or written, whatever the record
// holds. An item for one job is the store's to file, in its own writes of the
// record, never a filer's.
//
// NO FENCE, as [PendingStore.AppendBridgeCall] takes none: a note is an append
// to the turn, never an ownership decision. The note must carry its id and its
// seal, a seal within [MaxNoteSealedBytes] ([ErrNoteTooLarge]); the store opens
// nothing, and files it waiting and served to nobody, whatever reads or serves
// the filing hands in.
//
// ServeNotes records the notes a serve names SERVED to its job
// ([RunNote.Served]) — each still waiting, the record still on that job, in a
// status the serve names, its ending undecided ([NoteServe]) — and answers
// every one of them that waits served to that job, marked by this call or an
// earlier one, in the record's order. A serve that fails its pin, a note no
// longer waiting, an item for another job ([RunNote.IsFor]), and a record that
// is gone or whose ending is decided are each nothing served, never an error; a
// serve that names no job or no status is an error.
//
// THE MARK BEFORE THE HANDOVER: a caller hands a harness a note to deliver
// itself only once this has answered it served, so a note the harness may have
// delivered is never one the record says nothing of — a harness gone before it
// confirmed the delivery ([PendingStore.TakeNotes]) leaves the note waiting,
// and served.
//
// ServeHook records the notes a hook's serve names SERVED to its job's hook
// ([HookServed]), and answers the ones THIS CALL served, each with its serves.
// Pinned to the serve's job, its statuses and an undecided ending
// ([HookServe]); a serve that fails its pin, and a record that is gone, are
// each nothing served, never an error; one that names no job, no status or no
// take is an error. In ONE write, on the record as it stands:
//
//   - a waiting note's live serve to the job ([RunNote.LiveServe]) that
//     answered a call the serve names cancelled ([HookServe.Cancelled]) is
//     lost to the cancel ([ServeCancelled]);
//   - a waiting note's serve to the job of a take the serve names
//     unanswered ([HookServe.Unanswered]) is removed, its hook having been
//     answered nothing, so it spends nothing of the bound;
//   - then every note named that waits, is for the job ([RunNote.IsFor]),
//     holds no live serve to it and fewer than [MaxNoteServes] serves to it
//     (a list of another job's counting none, and replaced) is served: the
//     serve appended at the writer's clock, its call kept whole within
//     [MaxNoteCallBytes] and the id charset or as none ([NoteCall]),
//     [RunNote.Served] naming the job, and a note no hook was served before
//     placed at the write's own place among the reads ([RunNote.Place]),
//     the record's count of them moved once.
//
// [RunNote.Served] is cleared wherever the first two steps leave no live serve
// to the job, and the record is written where any step changed it.
//
// A SERVE, NEVER A READ: a caller answers a hook a note only once this has
// answered it served, and nothing here records it read, the job's own session
// being what says whether the hook took it ([PendingStore.SettleHook]). So a
// note a harness may hold is never one the record says nothing of, nor one it
// says was read while the session lacks it.
//
// SettleHook settles the serves of a waiting note to the job the settle names
// ([HookSettle]), and acts on no other note, in this order:
//
//   - the serve of a take named UNANSWERED is removed, its hook having
//     been answered nothing, so no session holds the note from it;
//   - a note one of whose serves is of a take named HELD is READ, at its
//     place among the reads ([RunNote.Place]), off the EARLIEST such serve:
//     that serve's take, step and instant, [RunNote.Served] naming the job
//     and its serves kept as the read's history. One with no serve of a
//     take held and one of a take named RESTARTED is read so, marked
//     ([NoteRead.Restarted]). The session outranks a cancel and an
//     absence: a note read so is read whatever its serves lost;
//   - on a note not read so, a live serve that answered a call named
//     CANCELLED is lost to the cancel, and a serve, live or lost to a
//     cancel, of a take named ABSENT is lost absent ([ServeAbsent]).
//
// [RunNote.Served] is cleared wherever no live serve to the job is left. It
// answers every note it changed and, where it names a take held or restarted,
// every note on the record read off a serve to the job, an earlier settle's
// read included: so a settle made again, finding its verdict landed, answers
// what it read.
//
// WRITTEN ONLY WHERE IT CHANGED SOMETHING, but for a take named unanswered that
// no serve on the record names: that take's write may still be in flight, so
// the record is written all the same, and the serve, conditioned on a version
// before this one, never lands after it. NO PIN AND NO FENCE: what a hook's
// serve handed over is a fact about the job, true whoever holds the record and
// whatever its status or its ending. A record that is gone, before the settle
// or under it, answers nothing, never an error; a settle that names no job is
// an error.
//
// # Stopping a seat's running work
//
// A pause with `stop_running` is a person saying the seat's work in flight must
// not go on. The turn the seat is on hears it at its next round, through the
// turn's own fence; a run that turn launched has no rounds the engine sees — it
// is its harness's own process in a box, minutes or hours long — so the fence
// alone let it run to its end, and only the turn that came back to collect it
// was stopped. [Coordinator.StopSeat] ends it instead, at once.
//
// # An ending like every other
//
// A stop is decided on the run's row through the one protocol every ending
// takes ([Coordinator.endRecord]): DECIDED first, under the seat's lease and
// only while the run is still launching or running the job it was read on, and
// then finished from the row by whoever reads it next — this node, or the
// seat's next holder after a crash. What makes it a stop rather than a loss is
// recorded in the deciding write ([PendingRun.Stop]): who stopped it, which is
// what its announcement ([types.ExecutorStopped]) and the stopped turn's record
// name.
//
// Finishing it is the ordinary ending's steps with two added for the stop (see
// [Coordinator.finishEnding]): before the box goes, the run's tool-bridge
// session is closed, its job ENDED — its script asked to stop its harness and
// pack its session as its own exit does, and what is left of the job killed
// past the wait ([Coordinator.endJob]) — and what the run had spent read off
// its box, charged to the token counters and published as its launch's record
// ([Coordinator.collectStopped]) — a run stopped halfway still spent what it
// spent, and nothing can read its box once it is reclaimed; and after the
// announcement, the turn parked on the run is closed on the record as stopped
// ([CoordinatorOptions.CloseStopped]).
//
// # Off the frame that decided it
//
// Ending a job is waiting for it — seconds for a harness to go and its session
// to be packed, at most [JobEndBudget] — so the frame that decides a stop — a
// pause watch, a seat's acquisition — closes the run's session and goes on, and
// the ending's own retry, armed at once, makes the read: for every job that
// started in a box, on this node's host or another's, and whatever
// `conversation_session.enabled` says, which governs what is kept of a session
// and not how a stop ends a job.
//
// # A box on another node's host
//
// A `direct` or `container` box is read on its own host ([BoxReader]), and that
// node may be draining or restarting while its seat has moved here. This node's
// retry of the kept ending asks the host, at once and then on the hand-back's
// spacing while the host is silent, and the host ends the job and reads it
// ([Coordinator.ReadHostedBox]), licensed by the stop the run's row records and
// nothing else; the rest is finished here ([Coordinator.endRun]). A silent host
// is no verdict about the box, and its spend is not written off for it: only a
// box found gone, a host silent for as long as the fleet waits before it takes
// a launch node for gone — the box timeout, by the retrying node's own clock,
// since it first asked the host — or a stop that old and two hours more by that
// clock, is ended with nothing read ([Coordinator.readStopped]). A host whose
// coordination store did not answer its read of the run's row
// ([ErrStoreSilent]) is waited for the same way where this node's own store
// answers, since only that host can read the box; where this node's store is
// silent too, nothing the ending writes would land, and the stop is kept for as
// long as that lasts.
//
// # Not the seat fence
//
// The seat fence is about OWNERSHIP, and it deliberately leaves a running run
// alone: a seat that moves to another node on an ordinary rebalance must not
// kill an hour of an executor's work (see seat.Host.Fence). A stop is a
// person's decision about the work, not about where it runs, so it is its own
// call, made by the node that holds the seat when the pause reaches it.
//
// endCollectedWait ends whatever the resume of run's collected job waits on,
// for a person's stop: a hold on its row ([PendingRun.HeldFor]) is lifted, so
// the waiter announces the job again on its schedule, and a completion of it
// this node kept for a condition, held here or refused at the gate
// ([Coordinator.keepRefused]) or kept for that hold
// ([Coordinator.keepHeldOnRow]), is re-fired at once. Reports whether this node
// was holding the job itself.
//
// WHY A STOP ENDS THE WAIT. The stop leaves a collected job to its turn, whose
// fence closes it as stopped at its first round, before any model call or
// launch. But that round comes only when the resume does, and a resume held for
// a model's limit waits up to the hour the entry is sat out, one held for the
// budget until its window turns over: the turn stayed open, its job neither
// resumed nor ended, for as long as the condition lasted after a person had
// asked for the work to stop. A resume of a seat a person stopped is refused
// nothing by those conditions (the engine's gate admits it), since the turn it
// re-enters ends before it could meet either.
//
// A KEEP FOR ANOTHER JOB of the same turn is dropped, as an announcement drops
// it ([Coordinator.stillRefused]): the stop decides this job.
//
// A HOLD ON THE ROW THE STORE DID NOT LIFT keeps the completion kept for it
// ([Coordinator.keepHeldOnRow]) as it was: a re-fire now claims nothing while
// the hold stands, and drops the keep, which left nothing on this node to
// re-fire the job at the hold's instant, where the waiter that would announce
// it reads a silent store.
//
// collectStopped is the stop's own step of its ending: what the run had spent
// read off its box, charged and published as its launch's record, and all of it
// recorded on the row in one write ([PendingStore.CollectStop]). Returns the
// row as that step left it, or why the box could not be read YET — which keeps
// the ending for this node's retry.
//
// THE SESSION IS CLOSED BEFORE IT, by the ending ([Coordinator.finishEnding]).
// The run's harness is still working until the read ends it, and every tool
// call it makes through the bridge acts as the seat: closed first, the stop
// takes effect on the seat's surface at once — no call the box makes after it
// is run, so no post, write or submission is asked for after it — rather than
// after a read of the box that ends the job and can take as long as the box's
// host takes to answer. A CALL ADMITTED BEFORE IT RUNS ON to its own end: the
// close answers it that the run has ended and the call is still running, and
// waits for none (internal/api/mcpbridge, "An opening ends without waiting for
// its calls"), so what such a call does lands after this read, and is in no
// collection of the stop's. Closing a closed session is nothing, which is what
// the reclaim that follows does again.
//
// ONCE PER ENDING, for the reason a release records its charge: the collection
// is recorded with the charge and the publish it made, so an attempt after a
// crash finds it done.
//
// A STOP ENDED WITH NOTHING READ IS SAID SO ONCE THAT WRITE LANDS
// (`sandbox_stop_unread`), and never before it. The write is a step of this
// node's store, which can fall silent after the attempt's own read of the run's
// row answered, and the stop is then kept for this node's retry, which reads
// the box again and charges whatever that read finds. Said before the write,
// the log had a stop still kept ended with its spend written off, whatever its
// read had come to, and a stop kept on a silence both nodes share said ended
// all the same. A write whose answer was lost though it landed is found
// recorded by the next attempt, which goes on past it and says nothing of it:
// no attempt can tell its own landed write from another's.
//
// rowRead is whether this attempt at the ending read the run's row off this
// node's own store ([Coordinator.finishEnding]), which the read judges a silent
// store by ([Coordinator.readStopped]).
//
// readStopped reads what a stopped run's job had done by now off its box — the
// job ended first, as the read's own step ([Coordinator.readBox]) — reporting
// whether there was anything to read, or why it cannot be read yet. Where the
// box was asked and nothing could be read off it, the [stopUnread] says why:
// the stop ends with nothing charged, which [Coordinator.collectStopped] says
// once its record of that lands, and never here, before the write that can
// fail.
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
//     decider's by more than the box timeout gave the host up on its very first
//     ask and wrote the run's spend off, and one behind waited past the bound.
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
// which licenses the read, or the verdict on its served notes. The box and its
// host are up, so the ending is kept and asked again on the same spacing: ended
// uncharged at once for it, the stop wrote off a spend the box still held. For
// how long turns on whose silence it is, which rowRead says: whether this
// attempt's own read of the row, off this node's store, was answered
// ([Coordinator.finishEnding]).
//
//   - Answered, the silence is the host's alone (a partition that cuts the host
//     off from the store's leader and not this node), and only the host can
//     read the box: a host that cannot is as good as away, so the stop waits
//     for it as for a silent host, by the same two bounds, such an ask counted
//     as a silent one ([Coordinator.hostSilence]), and is then ended with
//     nothing read. Unbounded, it held the seat's mail behind it for as long as
//     the partition lasted, while every write of the ending's would land.
//   - Unanswered, the silence is this node's too: nothing the ending writes
//     would land, so the stop is kept whatever the bounds say, and never said
//     ended for it; nor is such an ask counted toward the host's silence, since
//     it says nothing of the host. An attempt reaches the read in such a
//     silence only where it began after the retry's own read of the row and
//     its decision, which come first and keep the ending as they fail
//     ([Coordinator.retryOwed]).
//
// The result is the run's own, as a collection reads it — floored, held to the
// record's bound — and marked as a run that did not finish: never a success,
// never a question, whatever it had written ([stoppedResult]).
//
// hostSilence is since when this node has heard nothing from the host of a
// stopped run's box, by this node's own clock, given that the ask it started at
// asked came back with err, which gave no account of the box: zero where
// nothing it has seen counts as silence ([Coordinator.readStopped]).
//
// KEPT ON THE STOP'S RETRY SERIES ([answerRetry.silent]), under the ending it
// was measured for, and so only for as long as this node drives the stop:
//
//   - a host that answered BUSY is there: the silence is cleared, and none is
//     answered;
//   - an ask this node could NOT MAKE says nothing of the host: whatever silence
//     the series holds stands, neither started nor advanced;
//   - a host that did not answer an ask that reached the broker, or answered
//     that its coordination store did not answer its read of the run's row
//     ([ErrStoreSilent]) where this node's own store did, starts the silence at
//     that ask's start, where the series holds none for this ending, and the
//     series' own is answered otherwise: a host that cannot read the box is as
//     good as away;
//   - with no series — a seat's retirement, which runs on its own frame, or an
//     ask whose series the seat's move dropped while it ran — the silence is
//     that ask's alone, the only one this frame measured.
//
// # Recovering a seat
//
// RecoverSeat re-attaches to a seat's still-active runs as this node claims it.
//
// inbox is the attachment of the seat's inbox this node is about to make
// ([queue.Attachment]): every hold of the seat's inbox from here, this
// recovery's own included, is taken for it, so one decided while this node held
// the seat before and asked after that holding's release is refused by the
// queue rather than carried onto this one (seathold.go).
//
// attach, where not nil, attaches the seat's control topic: called once the
// seat's recovery gate is closed and before the seat's runs are read, so a
// completion published while nothing consumed the topic is held by the
// subscription rather than dropped, and delivered into a gate that refuses its
// claim until the pass is done (recoverygate.go). An attach that fails fails
// the recovery.
//
// # The pass is exclusive with every claim of the seat's runs here
//
// The pass reads a claim it finds as one whose node stopped, and reaps it or
// revives its answer, which is right only while no attempt of this node's own
// holds it. So from the moment the pass begins until it is done, every claim of
// the seat's runs on this node is refused ([ErrSeatRecovering]) and carried
// past it, and a claim this node's attempt took before the pass began is left
// to that attempt ([Coordinator.reapTail]): see recoverygate.go for why the
// node's admission alone did not keep them apart, and who carries what the gate
// refuses.
//
// PER-SEAT, and inside the acquire hook, because a fleet-wide boot scan is
// wrong twice over: every node would re-mark and reap runs belonging to seats
// its peers own, and a node that claims a seat LATER — a takeover, not a boot —
// would never recover it at all.
//
// Running jobs re-mark this seat held; the waiter then drives them to
// completion. Clarification and reseed runs are left for their answer — the run
// untouched, but COUNTED, because the seat's new owner is the one that will be
// handed that answer and has to recognise it as one. Each is fenced to this
// node's lease and taken over as the fence found it, not as listed
// ([Coordinator.fenceRun]): an ending the seat's last holder decided between
// the listing and the fence is this node's to finish.
//
// A RESUMED row means the engine that owned this seat died between claiming a
// run's tail and settling it. Nothing will ever pick it up — the at-most-once
// claim already flipped, so a redelivered completion is refused — and its box
// sits paused. A LAUNCHING row is the same fact one step earlier: that engine
// died between starting the job and writing the conversation a resume would
// re-enter, so there is nothing to resume into and never will be. Both are
// tails nobody drives, and both are reaped ([Coordinator.reapTail]) — except a
// claim of a person's answer that no turn took yet, which is not unresumable at
// all: it is REVIVED, the answer given back to the run and the run resumed with
// it, and reaped, its reply handed back to the seat, only past the revival's
// bounds ([Coordinator.revivable]).
//
// Reaping is safe HERE and only here: taking the seat's lease is what proves no
// live process holds the row — and for a launching row that proof is the whole
// argument, because a launching row on a seat this node already owns is a
// launch happening right now, microseconds from becoming resumable. A boot-time
// scan proves nothing of the sort, because a peer could be mid-resume on a seat
// this node never owned.
//
// # Each run it takes over is counted once
//
// The seat's control topic is attached before the recovery reads the seat's
// runs, so a recount off it can land before the recovery's count or between a
// run's fence and its count. So the recovery COUNTS THROUGH THE ONE MECHANISM
// every write that moves a run's count takes (countwrite.go): before any fence
// it records the takeover of every run its listing found, and starts the seat's
// counts afresh from what this node's writes out on the seat leave them,
// setting aside whatever a recount counted before it
// ([Coordinator.takingOver]); a recount meanwhile counts each run where the
// recovery has left it so far, nothing until its takeover is answered; and each
// takeover is answered once the recovery has done with its run, counting it in
// the set the recovery leaves it in, or in none
// ([Coordinator.takeoverAnswered]). Counted after the fence and outside the
// lock, on top of whatever a recount had counted already, a running job held
// the seat twice, a question was counted twice, the seat's mail offered to an
// answer match with nothing more to answer, and a tail a recount had counted
// kept the seat held after its reap.
//
// recoverRun takes over one run of a seat this node has just acquired, and
// reports the status the seat counts it in from here
// ([Coordinator.takeoverAnswered]): a running job's, an answered run's or a
// question's, and none for a run it ended, reaped, left to a newer lease or
// found gone, nor for a dead claim whose revival it keeps owed, which holds the
// seat's mail behind it.
//
// again is whether a run that turns out to have moved under this recovery's
// fence is taken over once more as what it moved to: the fence reads the row
// again ([Coordinator.fenceClaim], [Coordinator.fenceRun]), and a process that
// lost the seat without noticing can still have written before it: the release
// that hands its claim back, a run it parked, an ending it decided. Once,
// because the fence is in place by then and nothing else can move the row; an
// ending found decided is finished whichever pass finds it.
//
// # The seat's recovery gate
//
// The pass ([Coordinator.RecoverSeat]) fences each run of the seat to this
// node's lease, reads it again and takes it over as it found it: a claim it
// finds, a resumed tail, is one whose node stopped, and it is reaped as
// abandoned ([Coordinator.reapTail]), or its answer revived. That reading is
// right only where no process of this node's holds the claim. The acquisition
// made it so by the node's own admission: a seat still establishing admits no
// turn ([seat.Host.MayStart]), so no completion or answer of the seat's was
// claimed while its recovery ran. But not every claim asks that admission, and
// not every pass runs inside the acquisition:
//
//   - a completion past its turn's deadline is claimed without it
//     ([Coordinator.completeAtDeadline]), and is claimed precisely because the
//     admission refused it;
//   - an apply that brings the code-work runtime up on a seat already held
//     runs the seat's recovery with the seat established, so a delivery of a
//     completion, a re-fire of one on a signal, or an owed answer's attempt is
//     admitted and claims while the pass runs.
//
// Each such claim the pass then fenced, read again as resumed, and reaped as
// abandoned while this node's own attempt was collecting or resuming it: an
// ending decided under a live attempt, which that attempt, finishing, kept
// retrying, and a resumed turn ended under the review that was still running.
//
// # The rule, at one place
//
// So every claim of a seat's run on this node passes ONE gate before the store
// is asked ([Coordinator.claimGateLocked]): a completion's claim
// ([Coordinator.claimCompletion]), for its resume or its ending at the
// deadline, whichever announcement carried it (a delivery, a signal's re-fire,
// a re-fire armed by a recovery's re-arm or keep, a held completion's
// backstop), and an owed answer's claim ([Coordinator.resumeOwedAdmitted]).
// While a recovery pass of the seat runs on this node the gate refuses the
// claim ([ErrSeatRecovering]): the acquisition's MayStart rule, carried to
// every claim. And the gate counts every claim it lets through as this node's
// live attempt on the run until the attempt returns, so the pass, which marks
// the seat in the same lock, never reaps a claim this node's own attempt holds
// ([Coordinator.liveHereLocked]): a claim let through before the mark is left
// to the attempt that holds it, and no claim is let through after it.
//
// THE MARK COMES BEFORE THE CONTROL TOPIC, so the pass has no window before it.
// The seat's control topic is attached inside the pass, once the seat is marked
// ([Coordinator.RecoverSeat]'s attach): attached before the pass, a delivery
// could be admitted and claim between the two, live when the pass read the row.
//
// # A claim the gate refuses is never lost
//
// Each road states who carries it:
//
//   - A DELIVERY is handed back to the queue as this node's failure
//     (internal/engine's controlOutcome NAKs [ErrSeatRecovering] as the node's),
//     and the broker brings it back on its backoff, past the pass.
//   - A RE-FIRE (a signal's, a lift's, a recovery's re-arm or keep, a kept
//     completion's own timer) is handed to the pass ([Coordinator.carryPastRecovery])
//     and re-fired at its end, before the pass re-arms and keeps from the rows
//     it read, which start no second re-fire of a job one is on its way for
//     ([Coordinator.carriedLocked]).
//   - A HELD COMPLETION'S BACKSTOP that found its turn past the deadline is
//     re-fired at the pass's end the same way, and the re-fire, still held here
//     and past the deadline, ends the run ([Coordinator.recheckHeld]).
//   - AN OWED ANSWER'S ATTEMPT is handed to the pass, and its series is armed
//     at once at the pass's end, the seat's mail held behind it meanwhile
//     ([Coordinator.resumeOwedAdmitted]).
//
// Handed over in the lock the pass ends in, so a refusal that lands as the pass
// ends is carried at once rather than appended to a pass that is over.
//
// A SEAT'S RECOVERY PASS IS EXCLUSIVE WITH EVERY CLAIM OF THE SEAT'S RUNS ON
// THE NODE (recoverygate.go): one gate in the claim path (`claimGateLocked`,
// asked by `claimCompletion` for a resume and for an ending at the deadline,
// and by `resumeOwedAdmitted`) refuses a claim while the seat's pass runs
// (`ErrSeatRecovering`), closed before the seat's control topic is attached
// (`RecoverSeat`'s attach), and counts every claim it lets through as the
// node's live attempt until it returns, which the reap refuses to touch
// (`errReapLive`), because the acquisition's admission alone kept neither the
// deadline's claim, which asks none, nor an apply's pass on a seat already
// established from claiming inside the pass, which reaped the node's own live
// claim; what it refuses is carried, a delivery NAK'd as the node's and a
// re-fire, a held completion's backstop and an owed answer's attempt by the
// pass at its end (`carryPastRecovery`).
//
// # Store silence, owed refusals, kept completions and renewals, in brief
//
// A HOOK'S SERVE IS NO READ (notes.go): `PendingStore.ServeHook` records a note
// served to a job's hook before the hook is answered (`RunNote.Serves`, at most
// `MaxNoteServes` to one job, each keeping the call it answered within
// `MaxNoteCallBytes` and the id charset, or none), the first serve fixing the
// note's place among the turn's reads (`RunNote.Place`), where every later read
// of it is recorded; and `SettleHook` settles them by take: removed where the
// hook was answered nothing, lost where the harness cancelled the call or the
// job's session proves it never took it, and READ only where the session shows
// the hook took it, marked `Restarted` where it shows that only before its last
// restart, which no capture counts held (`notesHeld` stops before that read,
// and before an unread note's place). An ITEM FOR ONE JOB (`RunNote.For`) is
// the store's alone to file, in its own writes of the record: `FileNote` files
// a person's note alone and refuses one that names a job (`ErrNoteKind`), so no
// filer hands a job words on the store's account. One a record holds is counted
// apart from a person's two bounds (`MaxJobItems`), handed by every write only
// to a reader inside the job, passed over by the ending and the cursor, and
// removed by the next launch's write. A STEP A COLLECTION CANNOT GO ON WITHOUT
// THAT THE COORDINATION STORE DID NOT ANSWER IS NO FAILURE OF THE BOX
// (`ErrStoreSilent`, boxhost.go): the host's read of the run's row that
// licenses a read of its box (`ReadHostedBox`, carried back by the reply's own
// flag, `BoxReadReply.Unsettled`, since its `Error` is only the host's words),
// the drain's watch of the run's log, the first and one made again
// (`awaitCalls`, `watchAgain`), the record of the sessions a job ran out of
// context in or whose request its provider refused (`RetireSessions`), and the
// record of a job lost with its box's host (`MarkClaimLost`, below), each wrap
// the store's error in it where their context is still live (`storeSilent`: a
// step a deadline cut is the deadline's, and counted); a collection handed it
// gives its claim back UNCOUNTED and NAK'd (`collectFailed`,
// `sandbox_collect_unsettled`), and a lost job's round is not carried on
// without its read for it (`collectGivenUp`, judged ONCE per attempt at one
// reading of the clock, the verdict the carry reached handed to the hand-back,
// `collectVerdict`, `collectFailedAt`, since a bound crossed between two
// readings had the hand-back end a lost job's run its carry had just decided to
// ask again): for as long as a silent host is where the silence is the host's
// alone (`hostUnsettled`, this node's own clock from the first collection the
// host answered so, started again by any other answer it gives and left running
// by a host that did not answer, `noteHostStore`, and KEPT UNTIL THE RUN
// SETTLES, dropped where the run's collection lands its outcome whatever it
// comes to (the box read; the run ended, given up or its end decided in
// `endRun`, not at the record's delete; the job lost and its round carried on,
// the turn's next launch in `Launch` or its end decided; the seat let go),
// never only at `ReleaseSeat`, while kept through every attempt that fails, a
// hand-back that collects the job again and a recorded loss whose carry or
// resume then fails, never dropped at the loss; past it the job is LOST with
// that host, recorded on the claim that collects it,
// `PendingStore.MarkClaimLost`, `LossLaunchNodeStoreSilent`, `loseWithHost`,
// and its round carried on in that frame, a finished job's included, never
// ended `collect_unreachable`, as the waiter has a gone launch node's jobs; a
// record of that loss this node's store does not answer is handed back
// uncounted with the clock kept; while that clock runs, or the run's row says
// what it would (`unannouncedJob`, read by a node that keeps no clock before it
// lets a delivery go whose own collection read no box, `boxWasRead`, since the
// box's host read the row through its store to license that read, so a
// successful collection reads nothing more,
// `TestASuccessfulDeliveryReadsNoRowForItsHostsStore`: the job's record of its
// host's silence, `LaunchRecord.HostStoreSilentSince`, the clock's start, which
// every claim's release writes from the clock and a claim that learns that
// store answers clears, or the job lost with that host,
// `LossLaunchNodeStoreSilent`, until its round is carried on, so a restart or
// the seat's next holder renews it before its own first collection,
// `TestAJobNoWaiterAnnouncesIsRenewedByItsRow`; that host's waiter's own
// announcement says when it asked its store for the row it was made from
// (`rowAskedField`, on the envelope's bag), which a renewal, reading no row,
// never says, so one made from a read asked after the clock or the record began
// (`announcedAfter`) ends the clock as it is delivered (`hearAnnouncement`) and
// the record at the release a claim of it makes (`hostStoreSeen`), and a
// refusal of it is acknowledged: unmarked, every announcement that waiter made
// once its store answered again was renewed beside the chain in flight, one
// more chain each, `TestAWaiterAnnouncementMadeSinceTheClockEndsIt`; and by the
// read, never the announcement's own stamp, since the waiter polls the box
// between the two, up to a minute, and a silence that began in between and was
// found meanwhile was read as over while it lasted,
// `TestAnAnnouncementFromAReadBeforeTheClockEndsNothing`,
// `TestAnAnnouncementSaysWhenItsRowWasRead`, and so does a nudge's
// announcement, by its read, and a gone launch node's, by its record of the
// loss, `TestEveryRoadsAnnouncementSaysWhenItsStoreWasAsked`; and EVERY REFUSAL
// THE CONTROL TOPIC ACKNOWLEDGES IS OWED ON THE JOB'S RECORD
// (`LaunchRecord.RefusalOwed`, `PendingStore.OweRefusal`, `oweRefused`): one
// CAS write before the delivery is acknowledged, its condition and instant,
// only where the record owes none, so a re-announcement refused again writes
// nothing, the gate's refusal and a hold this node keeps after its claim alike,
// wherever the box is hosted, and a record the store does not take hands the
// delivery back as the store's silence, never acknowledged, while a drain's
// refusal is recorded nowhere (`Draining`, named here beside `SeatLost` as the
// two conditions the control topic hands on rather than acknowledges, so
// neither leaves anything owed: named in the engine alone, a drain's cost a
// write per job and, where the store did not take it, was handed back to the
// node that is leaving); the park on the job's question clears it in its own
// write, the next launch starts the record without it and the run's end deletes
// it, and a claim leaves it, since a claim handed back puts the completion back
// still owed, so a refusal acknowledged while another attempt holds the job's
// claim is written on the claimed row; the seat's recovery, a restart's or the
// next holder's, re-arms it for its condition's signal and asks its own gate at
// once (`rearmOwed`, ADR-0064: a recorded condition's signal is an edge on the
// node that refused), since a silence of the store's runs alone has no bound,
// its node's lease and presence another stream, and a refusal kept in memory
// alone went with a restart or a seat move,
// `TestAnAcknowledgedRefusalOutlivesItsHolder`,
// `TestAnAcknowledgedRefusalIsRecordedOnItsJob`,
// `TestASettledCompletionOwesNoRefusal`,
// `TestARefusalAcknowledgedUnderAnotherAttemptsClaimIsOwed`,
// `TestADrainsRefusalIsHandedOnOwingNothing`, and every `Condition` constant
// the module declares held to `MaxOwedConditionBytes` where it is written,
// since one past it is refused at its first record and every delivery refused
// on it handed back unrecorded,
// `TestEveryConditionFitsWhatAnOwedRefusalRecords`; and EVERY KEPT COMPLETION
// HAS A RE-FIRE OF ITS OWN, since a condition that lifts on the clock with no
// edge (a lease merely stale, a provider's limit at its instant) left a kept
// completion on the node that kept the seat waiting for an announcement a
// silent store never made (`TestAKeptCompletionIsReFiredWithoutASignal`,
// `TestARefireTheStoreDidNotAnswerIsKeptForItsBackstop`,
// `TestAKeptCompletionsRefireGoesWithIt`): at its refusal's instant and never
// later than `answerWaitBackstop`, save one the node holds off itself until a
// limit's instant after its claim, whose re-fire waits for that instant, since
// a re-fire drops the hold and one at the backstop claimed the job and ran its
// review into the same limit (`keptRefireIn`,
// `TestACompletionHeldHereIsReFiredAtItsInstantAndNeverSooner`), its backstop
// before the instant doing only what an announcement does, the run ended past
// its turn's deadline and the hold otherwise kept and the backstop armed again,
// so a deadline passing while no waiter announces the job ends the run within
// the backstop (`recheckHeld`,
// `TestAHeldCompletionPastItsDeadlineEndsWithinTheBackstop`), cancelled when
// the keep goes (`keepLocked`, `dropKeptLocked`), and a recovery's re-arm and a
// re-fire that settled nothing kept the same way, the latter unheld and for the
// backstop alone, since what re-fired it ended any hold (`keepAgain`,
// `TestASignalledRefireTheStoreDidNotAnswerIsHeldNoLonger`), and a completion
// whose resume its seat's budget held on the run's row kept unheld, re-fired at
// the hold's instant and never sooner, since no claim wins before it, that hold
// written no later than the turn's deadline so it lapses there and the run is
// ended at it (`unclaimHeld`,
// `TestABudgetHoldOnTheRowLapsesAtItsTurnsDeadline`), and the completion owed
// on the job's record by the release that writes the hold, so a restart after a
// lift of it re-arms the job (`ReleaseClaim`, `refusal_owed`), and on its
// condition's signal only once its own lift of the hold has landed, a signal
// since what was read before the resume's admission, so one landing during the
// resume counts (`resumeAndSettle`), every lift of a hold the signal makes
// re-firing the job besides unless the node carries it already, kept for
// anything but that hold or a re-fire of it on its way, since a lift racing the
// seat's recovery found nothing kept, a keep for that hold re-fired in its
// place and a re-fire on its way followed by one where it left the job kept for
// that hold, since that keep waited for the hold's instant once the hold was
// gone (`refireLifted`, `refired`), and the seat's recovery keeping no row
// whose job the node carries, so neither starts a second re-fire beside one
// (`carriedLocked`), a lift the store did not answer (the signal's or a stop's)
// leaving it kept for the instant, a delivery of it that the hold refuses
// leaving it kept, a claim the hold refuses with nothing kept for the job (what
// was kept in its place re-fired into the hold) keeping it again from the row,
// or for the backstop where the row no longer holds it or is not read, and a
// recovery keeping every held row it finds the same way, a signal since it read
// the seat's runs re-firing it at once (`keepHeldOnRow`, `liftAndRefire`,
// `forgetAdmitted`, `keepHeldClaim`, `keepHeldRows`,
// `TestACompletionTheBudgetHeldOnItsRowIsKeptForTheHold`)), EVERY hand-back's
// completion is RENEWED, KEYED ON THE CLOCK (`hostClocked`) AND NEVER ON THE
// HAND-BACK'S KIND: the host's answer, a claim this node's store does not
// answer or this node refuses for a release it owes (`errReleaseOwed`), a host
// that does not answer the read (`ErrBoxHostSilent`, acknowledged only where no
// clock runs), a loss unrecorded or a carry of a recorded one that fails, a
// refusal of the node's gate or a completion this node holds off itself (kept
// for its condition's signal besides, `keepRefused`) and an answer that settled
// nothing (a claim another attempt holds, a resume its row holds for the
// budget) alike, NO DELIVERY ACKNOWLEDGED WHILE THE CLOCK RUNS UNLESS THE RUN
// SETTLED, each handed back marked `ErrUnannounced`, which the engine's
// `controlOutcome` NAKs as the node's own failure (`queue.NakNode`, as it does
// every store's silence, `ErrStoreSilent`, the claim's, a claim refused for an
// owed release and a park's record included, every failure `queue.NodeFault`
// tells as the node's, a publish the broker did not take and a handler a stop,
// drain or seat move cancelled, and a condition of the infrastructure a resumed
// turn failed on, by the one answer the seat's mailbox NAKs by, the engine's
// `nodesFailure`, so a delivery a store outage spends is kept with the
// unhandled deliveries and never with the poison, and by which the coordinator
// counts toward the resume give-up that ends a run only the run's own failures,
// `CoordinatorOptions.NodesFailure`, `runsOwnFailure`, so half an hour of an
// outage ends no run whose job finished during it, and one only an operator
// clears (a disk full, read-only or failing, open files at their limit, nodes
// holding different keys or running different builds,
// `CoordinatorOptions.OperatorsCondition`, internal/agent/builtin's
// `OperatorKind`) leaves its run waiting for the operator, counted from this
// node's own memory of its attempts while it does, once the attempt's hand-back
// lands and only while this node holds the seat under the lease that attempt's
// claim took (`Coordinator.ResumesBlocked`, taken out by a resume that gets
// past it or fails for anything else, a hand-back that does not land, the run's
// settling and the seat's release), which the node says in its status record
// and every data node raises as `executor_resume_blocked` (internal/engine's
// `executorReading`), since the node holding the seat may be a satellite with
// no alarm table, and a resume another node or build can make
// (`ErrResumeElsewhere`: a seat this node does not hold, a parked state or
// reply kind of a newer build or a retired state an older node still live
// reads, a seat or company not this node's) only once no live node reads its
// parked state's version (`CoordinatorOptions.ResumableElsewhere`,
// internal/engine's `fleetReadsParkedState`), so a roll's older holder ends no
// run a newer node parked and a rollback holds no seat for ever on a parked
// state nobody running reads (a reply kind is no part of that version, so an
// unknown one is bounded by the live nodes' builds instead: not counted while a
// live node runs another build, counted once every live node runs this one),
// while the control topic hands it back as the node's; that answer puts each of
// this package's own failure classes, `Why`, on a side by whether a mail can
// cause it, every class the node's today, a provider refusing the company's
// credentials among them, internal/agent/builtin's `sandboxClassIsTheNodes`)
// whatever condition a refusal names but for a drain's and a lost seat's, which
// defer it, since the host's own waiter reads the row through the silent store
// and announces nothing, this node's memory of a kept completion outlives
// neither its restart nor the seat's release, and the broker's budget is
// shorter than the bound: announced afresh by this node (`events.New`, a fresh
// id and budget) once the delivery in hand has `renewReserve` (five) or fewer
// deliveries left, that delivery acknowledged only once the fresh one is out,
// so one announcement is in flight, a renewal the broker refuses NAK'd to be
// made again on the next, and a re-fire's at once where its own collection
// started the clock (`startingClock`) and it still runs once that attempt is
// done, none where its own attempt ended the run, none where the clock ran
// before it or another delivery's collection started it meanwhile, the delivery
// refused meanwhile carrying it, a re-fire's the broker refuses made again on
// this node's timer on the hand-back's spacing until one lands or this node
// settles the run, past the seat's release and a lost lease, since its publish
// to the seat's control topic needs no lease and reaches the next holder
// (`refirePublish`, `runSettled`,
// `TestARefiresRefusedAnnouncementIsPublishedAgain`) (`renew.go`, `renewing`,
// `TestACompletionWhoseHostAloneFindsTheStoreSilentIsRenewed`,
// `TestEveryHandBackIsRenewedWhileTheClockOnItsHostRuns`,
// `TestARefusalWhileTheClockOnItsHostRunsIsHandedBack`); a cause a build does
// not know reads as unknown, leaves no box and is said in general terms), and
// for as long as the silence lasts where the store is this node's own, under
// which no claim of the job lands, the completion NAK'd and nothing more, since
// the waiter announces a running, unheld job again on its doubling schedule
// (`reannounce.go`), a fresh message at most `reannounceCeilingPolls` poll
// intervals after the last, a ceiling held under the broker's least time to a
// dead letter at production's cadence
// (`TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter`), so no dead
// letter the silence leaves is the completion's last copy; and a stop's read is
// kept and asked again (`readStopped`, `sandbox_stop_read_waits` with
// `waits_for` the store), for as long as a silent host is where this attempt's
// own read of the row answered (`finishEnding`'s `rowRead`: the silence is the
// host's alone, and a host that cannot read the box is as good as away), and
// whatever that bound says where it did not (the silence is shared, so nothing
// the ending writes would land, and the stop is never logged as ended for it);
// a stop ended with nothing read is logged so (`sandbox_stop_unread`) only once
// its record of that lands (`collectStopped`), never before a write that can
// fail. A CLAIM'S HAND-BACK THE STORE DID NOT ANSWER IS KEPT OWED, never ended
// (claimrelease.go), a collection's, a park's and a resume's alike: a release
// that fails `coord.ErrUnavailable` is made again by this node no more than the
// hand-back's spacing apart with the seat's mail held behind it (`unclaimAt`,
// `oweRelease`, which keeps it only where it is still out, asked in the one
// step that records it on the owed series' `release`, owes the seat's run and
// arms the series, since a claim on its way can take it over in between,
// `scheduleOwedLocked` arming it no later than that spacing whatever the series
// waits for), the seat counting the run as the claim it still is until the
// release lands, which moves the count where the release put the run ONCE, in
// the step that takes the release's answer under the lock that drops it from
// the releases out (`unclaimOwing`), the release being one more write in its
// run's entry (`claimRelease.write`, recorded with the release in
// `handingBackLocked`, for a park whose own write did not land in the step that
// takes that write's answer and before taking it, so the run never stands with
// no write out between the two and the release moves after whatever was
// recorded behind the park, answered by `wroteCountLocked`), so a recount
// counts a run whose release is out as the claim whatever the store's row says
// by then (`syncSeat`, `countedLocked`), since one that read the landed row and
// was written before the answer counted the run there and the answer's move
// counted it again, freeing a seat that held another run or counting a question
// twice, and a write recorded behind the release on the run moves after it, the
// record of a reply to the question a park put on the row while that park's
// release is owed taking the run from the claim to its answer once the release
// is found moved on, where read apart it took a question the seat never counted
// out of the question set, and no claim of its job is made here from the moment
// the release goes out: a completion's is refused (`releases`,
// `claimCompletion`, `errReleaseOwed`), since a release that landed with its
// answer lost names the very claim the next collection takes, and an answer's
// is left to the series that owes the release, which makes the release first
// and goes on to the answer once it lands (`leaveToRelease`, `retryOwed`; the
// answer's run owed in the step that records that series, as every step that
// owes a run for a series' retry records and arms that series,
// `keepSeriesLocked`, since a seat's release drops all three and a run owed
// after it was held behind with nothing to drive it), a park's release
// included, whose question the store recorded though it did not answer the
// write, since a reply answers that question while the release is out and the
// release names the very claim the answer would take; once a release lands its
// series reads the row for what it still owes, and a read the store does not
// answer is made again on the same spacing, never counted against a run the
// release put back in running, which owes it no answer (`answerRetry.unread`);
// a claim of the job already on its way when the release went out wins only
// once it has landed, so its win settles the release, never made again, nor
// while such a claim is on its way (`claiming`); a release the store refused
// still ends the run (`claim_unreverted`), and a seat that moves first has the
// claim reaped by its next holder. EVERY OTHER WRITE THAT MOVES A RUN BETWEEN
// ITS SEAT'S SETS MOVES THE COUNT ONCE too (countwrite.go): the open of a run's
// row (`Launch`: from no row, or from the claim a resumed turn relaunches,
// which `framing` tells), a recovery's takeover of each run it lists
// (`takingOver`, before any fence, which starts the seat's counts afresh from
// the writes out on it, a recount off the control topic attached before it set
// aside, and `takeoverAnswered` once the recovery has done with the run), a
// park's question, an answer's record, the claim that resumes it, either
// decline that lets it go, and the delete that ends a run this node counts (a
// run settled as lost, a stop, an abandoned launch: `ending.counted`, kept with
// the ending for its retry) are each recorded before they are sent
// (`writingCount`, `writing`, by the run's turn, since a turn's row is one row
// whatever job a write names), a recount counts every run of the seat with one
// out where the moves made so far leave it, whether or not its listing has the
// run's row (`countedLocked`), and the write's answer makes its move under the
// lock that takes it (`wroteCountLocked`), each landed write putting the run in
// the set its own status names, never moving it by the sets it was recorded
// from and to (an earlier write whose answer was lost may have landed), a write
// that did not land moving nothing, nor one whose seat this node let go of
// while it was out (`ReleaseSeat` drops the seat's writes in the step that
// drops its counts, and a write whose entry is gone moves nothing,
// `countWrite.entry`, so a node that takes the seat back counts the run once,
// where its recovery finds it), every write settled on every way out of its
// store call, a panic the queue's handler recovers included, as one that did
// not land (`settleWrite`, deferred where the write is recorded), as are a
// release's record (`dropRelease`), a release an earlier job of the run still
// owed when a later job's release takes its place (`handingBackLocked`: a claim
// of another job is not refused, and the earlier one can no longer land), and
// the count of a job's claims on their way (`claimDoneLocked`), since a write
// left out held every later move on its run, a release left recorded refused
// every claim of its job and a count left up held an owed release back for
// good, two writes out on one run moving it in the order they were recorded,
// whatever order their answers come back in; moved after the write and outside
// the lock, a recount between the two counted the run twice, the record's
// taking a seat that held a second question out of the question set, so that
// question's answer ran as an unrelated turn, and an ending's delete, read out
// of a listing taken after it, freeing a seat whose other run still worked,
// while an ending kept for its retry counted out a run still on its row. A
// COLLECTION WHOSE READ THE BOX ANSWERED ENDS THE RUN OF FAILURES BEHIND IT,
// whatever later step hands its claim back uncounted (a step the store did not
// answer, a drain this node stopped): the read marks the claim
// (`PendingRun.BoxAnswered`, on a collection and on a lost job's carry) and the
// release clears the run without collecting the job (`Release.BoxAnswered`),
// while a later step its delivery's deadline cut is counted onto the run as a
// counted failure always is, and the store refuses a release that says both.
package sandbox
