# ADR-0065: A local box is driven by the node that launched it

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:** `internal/sandbox.TestEachWaiterDrivesOnlyTheBoxesItCanReach`, `internal/sandbox.TestAPauseIsReapedOnlyOnItsBoxsHost`, `internal/sandbox.TestACollectionOfABoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAHostThatDoesNotAnswerHandsTheClaimBackUncounted`, `internal/sandbox.TestAnEndingOnAnotherNodeLeavesTheBoxToItsHost`, `internal/sandbox.TestARelaunchAwayFromTheBoxsHostStartsFresh`, `internal/sandbox.TestARelaunchOnTheBoxsHostContinuesInIt`, `internal/sandbox.TestAStopOfARelaunchBeforeItsBoxReadsNoJobAndLeavesTheBoxToItsHost`, `internal/sandbox.TestAStopOfARunWhoseBoxIsOnAnotherHostIsReadThere`, `internal/sandbox.TestAHostReclaimsTheBoxesNoRunNamesAnyMore`, `internal/sandbox.TestALaunchNodeGoneForTheBoxTimeoutHasItsRunEnded`, `internal/sandbox.TestALaunchNodeBackWithinTheBoxTimeoutKeepsItsRuns`, `internal/sandbox.TestALocalBoxsTailIsReadOnTheNodeThatLaunchedIt`, `internal/engine.TestABoxOnAnotherHostIsReadBackByThatHost`, `internal/engine.TestABoxReadTheFleetDidNotSignReadsNothing`, `internal/engine.TestBothWaitersKeepARunsTokenFileCurrent`
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
so a node absent for longer has no box left. The duty records the verdict on
a running job's row and the seat's holder ends the job as lost with the reason
`launch_node_gone`; a parked run loses only its box, and waits for its answer.
A node that comes back inside that time keeps its runs.

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

## What this does not decide

Where a run is placed (the member's `run_in`), which node holds a seat, and
how a box reaches its tools ([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)).
A box in an `e2b` cell is touched wherever the work is.
