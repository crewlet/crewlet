# ADR-0065: A local box is driven by the node that launched it

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:** `internal/sandbox.TestEachWaiterDrivesOnlyTheBoxesItCanReach`, `internal/sandbox.TestAPauseIsReapedOnlyOnItsBoxsHost`, `internal/sandbox.TestACollectionOfABoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAHostThatDoesNotAnswerHandsTheClaimBackUncounted`, `internal/sandbox.TestAnEndingOnAnotherNodeLeavesTheBoxToItsHost`, `internal/sandbox.TestARelaunchAwayFromTheBoxsHostStartsFresh`, `internal/sandbox.TestARelaunchOnTheBoxsHostContinuesInIt`, `internal/sandbox.TestAStopOfARelaunchBeforeItsBoxReadsNoJobAndLeavesTheBoxToItsHost`, `internal/sandbox.TestAStopOfARunWhoseBoxIsOnAnotherHostIsReadThere`, `internal/sandbox.TestAHostReclaimsTheBoxesNoRunNamesAnyMore`, `internal/sandbox.TestALaunchNodeGoneForTheBoxTimeoutHasItsJobLost`, `internal/sandbox.TestALaunchNodeBackWithinTheBoxTimeoutKeepsItsRuns`, `internal/sandbox.TestAJobKilledWithItsEngineIsLost`, `internal/sandbox.TestAJobFoundRunningAfterARestartIsWatchedByTheNewEngine`, `internal/sandbox.TestAJobItsEngineRestartKilledIsRecordedLostAndAnnounced`, `internal/sandbox.TestALostJobsRoundIsCarriedOnNotCollected`, `internal/sandbox.TestACompletionAfterTheHostWasFoundGoneCarriesTheRoundOn`, `internal/agent/runner.TestALostJobsRoundIsCarriedOnInAFreshSession`, `internal/agent/runner.TestARoundLostTooOftenIsNotStartedAgain`, `internal/sandbox.TestALocalBoxsTailIsReadOnTheNodeThatLaunchedIt`, `internal/engine.TestABoxOnAnotherHostIsReadBackByThatHost`, `internal/engine.TestABoxReadTheFleetDidNotSignReadsNothing`, `internal/engine.TestBothWaitersKeepARunsTokenFileCurrent`, `internal/engine.TestAHostBehindItsAskersClockReadsOnlyItsShareOfTheAskersWait`, `internal/engine.TestAHostAheadOfItsAskersClockStillReadsWithinTheAllowance`, `internal/engine.TestABoxReadAnotherBuildCannotReadWholeReadsNothing`, `internal/engine.TestAHostTakesEveryBoxReadAsItArrives`, `internal/engine.TestABoxReadWhoseShareIsSpentIsRefusedAndFreesItsSlot`, `internal/engine.TestAnAskThatNeverLeftThisNodeIsOneItCouldNotMake`
- **Cost-when-tried:** the fleet's one waiter polled every running job from whichever node held its duty. A `direct` or `container` box on another node's host is not there to reconnect to, so the duty's node failed every tick, gave the box up after `ConnectGiveUp` (60 s), and settled a job that was still working as lost; its keepalive never reached the box either. An ending or a relaunch on a node the seat had moved to reached for the box the same way, and found nothing.
- **Tag-status:** unreleased

## The decision

**A box in a `direct` or `container` cell is touched only on the host of the
node that launched it**, which the run's row names (`launch_node`). Its poll,
keepalive, pause reaper and reclaim are that node's own waiter, on every node
and whatever the fleet's waiter duty; the duty drives only the boxes every node
reaches (`e2b`). So is the refresh of what the running box holds that expires
under it, a seat's GitHub App token file
([ADR-0058](0058-a-seats-github-app-token-is-issued-at-use-and-read-only-by-that-seat.md)):
it rides the poll, so both waiters carry it, each for the boxes it polls. A seat's holder on another node reads such a box by asking its
host over the broker, signed under the fleet keyring
([ADR-0031](0031-every-state-log-record-is-signed.md)), and leaves an ending's
or a relaunch's reclaim to the host, telling it to look now. A relaunch away
from the host starts in a fresh box, and one on it continues in its box; until
a relaunch attaches a box of its own, its row names the previous box with that
box's host.

**A launching node is judged gone by its presence lease alone, held absent
for the box timeout.** That is the time its boxes outlive their keepalive,
so a node absent for longer has no box left. The duty records on a running
job's row that it was lost with the node (`launch_node_gone`), and the seat's
holder carries the job's round on in a box of its own; a parked run loses only
its box, and waits for its answer. A node that comes back inside that time
keeps its runs.

**A job lost with its host is carried on, never collected.** A `direct` job is
a process tree of its node's host, and the restart that replaces the engine —
its container, its systemd unit's control group, its host — kills it mid-step
with nothing written to say so. Each local box records the engine process that
last saw its job running; the host's own waiter reads a job that is not running
as ENDED while that process is still there, and collects it, and as LOST once
it is gone too. A lost job — or a box gone from its host, or a node gone from
the fleet — is recorded on the run's row before its completion is announced,
and the seat's holder reads what the job spent off its box where the box is
left, then resumes the turn to relaunch the same round in a fresh CLI session:
in the lost job's box on its host, else in a fresh one from what it pushed. A
round whose calls hold its submission is collected instead, a job that had
asked is parked on its question, and a round is carried on after at most two
lost jobs.

**A host told a run submitted its work polls it at once**, then on a short
schedule that stays inside one poll interval; the interval is the fallback.

## Why the obvious alternatives are wrong

**One waiter for every box** is what the cost above records: only the host can
reach a local box, so a poll from anywhere else is a verdict of loss on a
working job.

**Ending a run when its seat moves** would make a rebalance or a drain cost a
job its work. The seat moves for reasons the job had no part in, and its tool
calls already follow the seat
([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)).

**Reaching the host's boxes over its address** needs an address every node can
dial, which no node records and a satellite does not accept. The broker is
what reaches every node.

**Judging a node gone from one missed lease, or at the next recovery of its
seats**, ends runs on a node that is merely restarting. A draining node gives
its presence up first, and a restart is minutes. Neither has lost a box.

**Collecting a job whose wrapper is gone with no marker**, whatever took it,
reads a job its engine's restart killed as one that crashed: the turn judged a
truncated run as the round's result, and where the box went too the run ended
the turn the park had already acknowledged. Nothing on the job tells the two
apart, so the box records who was watching.

**Ending a lost job's turn**, as `launch_node_gone` once did, loses work a
deploy should never cost: the round's calls, its pushed branch and, where the
box survived, its working tree are all still there to carry on from.

## What this does not decide

Where a run is placed (the member's `run_in`), which node holds a seat, and
how a box reaches its tools ([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)).
A box in an `e2b` cell is touched wherever the work is.

## Amendment: a box read carries its asker's wait, and its host takes it as it arrives

A node that holds a run's seat and not its box asks the box's host to read it
(`sandbox.box_read`), and the host read for its share of the time to the
request's deadline: `0.75 × time.Until(deadline)`, the ASKER's instant read on
the HOST's clock. Two machines' clocks stand apart — up to twice
`objstore.AssumedClockSkew` before any node raises `clock_skew` — so a host
behind its asker by `s` read up to `0.75 × s` past the asker's give-up, taking
a session out of the box for nobody beside the asker's next attempt, and a host
ahead of it by more than the wait refused every read as stale, its asker
hearing silence.

**The decision.**

- THE ASKER CARRIES ITS WAIT AS A DURATION (`BoxReadRequest.BudgetMS`, wire
  version 2): the read's budget, or less where its own context ends sooner.
  The host reads for `sandbox.BoxReadAnswerShare` of that wait, counted from
  the moment the request reached it, on its own clock
  (`BoxReadRequest.AnswerBudget`). The asker's instant stays on the wire only
  as the replay binding.
- THE CHANNEL ALLOWS TWO CLOCKS TWO HOURS (`sandbox.BoxReadClockAllowance`,
  `2 × objstore.AssumedClockSkew`, ADR-0031's per-channel allowance): a host
  ahead of its asker by up to that still takes the read, and past it its
  clock is one the operator must fix.
- THE HOST TAKES EVERY READ AS IT ARRIVES. A request past a registration's
  slots waited unseen in the broker's buffer, where nothing counted its wait;
  the instant had absorbed that wait, and a duration counted from the answer's
  start would not. So the registration is wider than what the host holds
  (`boxReadAnswers`: 64 reading, 64 waiting, and 64 more for the answers made
  at once — busy, another node's, refused), the host makes at most 64 reads at
  once (`boxReadsHeld`, as the registration's own slots bounded it), keeps at
  most 64 more waiting inside the answer with their wait counted
  (`boxReadsWaiting`, a judgment anchored to one more round of reads), and
  answers BUSY (`BoxReadReply.Busy`, `sandbox.ErrBoxHostBusy`) past those, or
  when a waiting read's share runs out — with the rest of its asker's wait
  left for the answer to cross. A read that waited `d` is handed what is left
  of its share, `0.75 × wait − d`, so it ends at its arrival plus the share
  whatever it waited, and its asker's last quarter stays whole for the
  crossings.
- THE REQUEST IS READ WHOLE OR NOT AT ALL. It decodes naming every member this
  build does not declare (`BoxReadRequest.Unread`), and the host refuses such
  a request, one of another version and one carrying no wait, answering
  nothing and logging `sandbox_box_read_refused` with disposition `wire` on
  the named node — as a data node refuses an estate request whose arguments it
  cannot read whole (ADR-0031, "a signed estate request runs as it was signed,
  or not at all").
- A busy host and a host this node could not ask at all
  (`sandbox.ErrBoxNotAsked`: its broker unreachable, or no time left) each
  wrap `sandbox.ErrBoxHostSilent`, so every reader handles them as a silent
  host, as before.
- `BoxReadAnswerShare` stays three quarters: the quarter left now holds only
  the crossings, at most nine minutes at the slowest pace allowed (the
  request's two hops under a mebibyte, the reply's two at eight), against more
  than nineteen at the shortest wait a read asks at its budget, whatever a
  read waited for one of the host's 64.

**Why the obvious alternatives are wrong.**

- THE INSTANT WITH AN ALLOWANCE ALONE admits a host ahead of its asker, but a
  host behind it still reads the distance between the clocks past the asker's
  give-up.
- THE DURATION COUNTED FROM THE ANSWER'S START leaves a read that waited for a
  registration slot in the broker's buffer uncounted: it would read its whole
  share after its asker had spent part of its wait in the queue.
- BUSY AT ONCE PAST 64 serves no burst a host saturated briefly — a drain
  making every box on a host a remote collection at once — that main served
  by queueing it, and a busy loss's read costs that run's session.
- AN ARRIVAL STAMP FROM THE BACKEND would need the subscription's callback
  unblocked, which moves every registration's backlog into unbounded
  goroutines rather than one registration's into a bounded wait.
- AN ABSOLUTE CROSSING MARGIN in place of the share would shorten a dead
  host's wait by about ten minutes of seventy-eight, at the cost of a second
  rule beside the tail's for the same question.
- A SHARE OF WHAT THE ASKER STILL WAITS for a read that waited `d` (main's
  rule, `0.75 × (wait − d)`) ends the read later the longer it waited: at the
  waiting bound's end, three quarters of the way through the asker's wait, it
  reads on for three quarters of the last quarter and leaves a sixteenth of
  the wait for the crossings, under five minutes for an uncapped collection,
  below the nine the slowest pace needs, and the host takes a session for an
  asker that has gone. What the one deadline costs instead is read time: a
  read that waited reads a quarter of its wait less, so one that waited long
  can be cut by its own deadline and answer that it could not read the box,
  an answer its asker hears, where main's rule left it silence.

**Mid-upgrade.** A newer asker's request reaches an older host as version 1
did: the older build drops `budget_ms`, reads by the time to the deadline with
no allowance, and never answers busy — today's behaviour for that pair until
the host rolls. An OLDER asker's request reaches a newer host as version 1 with
no wait and is refused (`wire`): its asker hears silence after its whole wait,
so a collection hands its claim back only after that wait (seventy-eight to
ninety-nine minutes) and asks again from the host's next poll, a stop's read is
written off once the older asker's own rule reads the stop past the box
timeout, after one whole silent ask, and a lost run's session is not kept.
Rolling every node within one read's wait avoids each. Reading the older
request instead would be compatibility with an unreleased build. A later build
that adds a field leaves it at its zero, and a request setting none is read as
before; one that sets a field this build does not declare is refused rather
than read short, and a change of a field's meaning or a reply field an asker
must understand moves the version. No state-log record, record version or
`coord.ProtocolVersion` changes.

**What it leaves.**

- A REQUEST CARRIED OUT LATE — a connection that drops in the instant between
  the broker check and the send, carrying the request at its reconnect — is
  admitted inside the deadline and the allowance and read for its full share
  for an asker that has gone; its overrun is the lateness less a quarter of the
  wait, and the read is one the run's row still licenses. A host behind its
  asker no longer reads late for its clock; only a request delivered late
  does.
- THE REPLAY WINDOW grows by two hours for box reads, at a node that restarted
  inside it, and every serving node holds every box-read id for as long: up to
  the read's wait and four hours of real time on a node two hours behind.
- A BUSY HOST past 64 reading and 64 waiting costs a loss's read its session
  where main might still have read it in time from the broker's buffer.
- A seats-only host's clock is measured by nothing (the `clock_skew` alarm
  holds the data nodes and the register's member); this makes it no worse.
