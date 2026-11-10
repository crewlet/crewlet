# ADR-0121: A pull request a task waits on reaches it as the engine's record

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/worklet.TestAnItemWaitingOnAPullRequestIsClearedByItsSettle`, `internal/engine.TestAMergeTheWebhookDeliversSettlesTheTasksWaitingOnIt`, `internal/engine.TestThePollSettlesWhatNoDeliveryDid`, `internal/engine.TestAPullRequestOutsideTheCompanysOwnersIsNeitherAcceptedNorRead`, `internal/engine.TestARateLimitedPassResumesWhereItStopped`, `internal/engine.TestAnUnreadablePullRequestBacksOff`, `internal/agent/builtin.TestWaitingOnAPullRequestIsHeldToTheCompanysOwners`, `internal/advisor.TestOnlyThePollSettlingSaysTheWebhookIsSilent`
- **Cost-when-tried:** not tried here. Before this record an item could wait only on another item, so work waiting on a pull request was blocked in words or not at all: the merge reached the agent that opened the pull request, never the item that waited on it, and items stayed blocked after the change they waited on had landed.
- **Tag-status:** unreleased

## The decision

**A task may wait on a pull request on the company's GitHub, and the pull
request's merge or close is recorded on the task by the engine, with no
agent's turn, telling whoever holds the task to update it.**

1. **The wait.** `waiting_on` takes a pull request's URL under an owner the
   company's GitHub configuration names (its organization, or the owner of a
   repository a seat works in, the reading a chat link's preview holds), kept
   under one name (its own page, owner and repository lower-cased). One under
   any other owner is refused naming the owners, and neither the webhook nor
   the poll settles one: the company's credentials are for its own code. The edge has no mirror and is never one-sided; the task
   reads blocked until the pull request settles.
2. **The record.** A merge or close is a task patch at record version 11
   (`TaskPatch.ChangeSettled`): the task keeps it among its `changes`, its
   dependency row reads it, and its holder is told under `assignee`,
   addressed. The task's status is never moved: what follows is the holder's
   call. The record is keyed on the task, the pull request, its state and the
   host's own instant for it, so every copy of one delivery and the poll that
   saw the same thing write once.
3. **Learned by webhook, else by poll.** The GitHub delivery of a closed pull
   request settles it on the node that handles the delivery, through that
   node's own tracker. What no delivery settled, a fleet maintenance job asks
   GitHub about, at most fifty open pull requests a pass, resuming where the
   last pass stopped (a rate limit included, which ends the pass at the change
   it stopped on), through the agents' apps' cached tokens (no token minted per
   pass), and only once the change settled more than ten minutes ago. A pull
   request the host will not show is said once, as a warning naming it, and
   asked again after a wait that grows from an hour to a day.
4. **A silent webhook is said.** When the poll settled a pull request in the
   last seven days and no delivery has settled one since, the advisor raises
   `integration.inbound_silent` for the owner, naming GitHub: the poll keeps
   items moving, ten minutes or more late, until the webhook is fixed.

## Why the obvious alternatives are wrong

**Waking the agent that opened the pull request** is what the merge already
did, and it is the wrong party: the item waiting on it may be somebody else's.

**Moving the item's status on a merge** decides for its holder that the work is
done. A merge can be one step of several, and a close is often a restart.

**A poll alone** is up to a quarter of an hour late on every merge, and **a
webhook alone** loses whatever a missed delivery carried, for ever.

## What this does not decide

Pull requests on another host (GitLab, a self-hosted Gitlet change), a reopened
pull request blocking its tasks again, or how the dashboard draws a wait on a
pull request.
