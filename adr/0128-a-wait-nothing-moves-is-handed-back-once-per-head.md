# ADR-0128: A wait nothing moves is handed back once per head

- **Status:** accepted
- **Authority:** `internal/pickup`
- **Enforced-by:** `internal/engine.TestAWaitOnAPullRequestNothingMovesIsHandedBackOncePerHead`, `internal/engine.TestAWaitSomebodyMovesIsLeftAlone`, `internal/engine.TestAnItemSomebodyMovesIsNotHandedBack`, `internal/engine.TestAWaitTheEngineCannotReadWakesNobody`, `internal/worklet.TestADeadWaitIsRecordedOnItsTaskAndMakesItReady`, `internal/worklet.TestADeadWaitIsReadyOnlyWhereNothingElseMovesTheItem`, `internal/pickup.TestADeadWaitIsKeyedOnItsHeadsAlone`, `internal/github.TestEveryMoverOfAPullRequestIsSeen`
- **Measured:** on a company running this engine, an agent opened a pull request for its own work item and answered the item's pickup with `waiting_on` that pull request. The wait was typed, so it was accepted, and the item left every wake: no reviewer was requested, nobody reviewed it, no merge was asked, and three hours later nothing had moved. The agent's own note said it waited on a merge decision that nobody had been asked for.
- **Cost-when-tried:** not tried here. ADR-0121 made the wait typed and ADR-0122 made a typed wait an answer to a pickup, and neither asked whether the thing waited on could ever happen; the only backstop was a person reading the board.
- **Tag-status:** unreleased

## The decision

**A wait on an open pull request that nothing moves is recorded on the task by
the engine, and an item whose every open wait is such a wait is handed back to
its holder by the work pickup, once per head.**

1. **What moves a pull request.** A reviewer or a team asked for, a review
   submitted on its current head by anybody but its author, a merge armed on
   the host, or an `ask_to_merge` call held for it and still waiting on its
   answer. A pull request that is open and has none of these is moved by
   nothing, a draft included.
2. **Read by the poll that settles it.** The fleet's poll of open pull requests
   (ADR-0121) reads each one's movers with its settle, through the same agents'
   apps, and the fleet's held calls once a pass. It records a dead wait on each
   task that has waited on the pull request at least 15 minutes, at the head it
   read (`TaskPatch.DeadWait`, record version 15), and ends it once something
   moves it (`TaskPatch.DeadWaitEnded`). A settle, or the task no longer
   waiting, drops it. The record notifies nobody, and the dependency edge
   carries its head (`tracker_task_deps.dead_head`).
3. **Handed back once per head.** An item whose every open blocker is a dead
   wait is ready for its holder unless a reviewer holds it in review or its
   holder's own question on it is open. The pickup's key is the heads nothing
   moves and nothing else, so it is handed once per head and never rechecked
   daily, and its turn is counted in the turn ledger as `work_item:dead_wait`.
   Its brief names the pull request and the next step: review asked on the
   item or the pull request, or a merge asked.
4. **What cannot be read wakes nobody.** A pull request the host refuses, its
   reviews refused, or the held calls unread records nothing either way, and a
   record already made stands at its head.

## Why the obvious alternatives are wrong

**Refusing the wait** when nobody reviews the pull request yet is wrong at the
moment it matters: a seat waits on its pull request and asks for its review in
the same turn, in either order. **A daily recheck** of a blocked item, as
ADR-0122 rechecks a ready one, hands the item back every day whatever moves
it, and a review that takes a week is not a stall. **Reading the code host at
pickup time** spends a host read per idle seat per minute, and leaves the board
unable to say why an item waits; the poll already reads every open pull request
a task waits on.

## What this does not decide

Waits on a Gitlet change or a GitLab merge request, which `waiting_on` does
not take (ADR-0121). Whether a review that requested changes is being acted
on: a submitted review counts as movement, and its author is woken by the
review itself. How long a reviewer may sit on a requested review.
