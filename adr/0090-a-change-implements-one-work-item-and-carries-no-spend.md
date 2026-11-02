# ADR-0090: A change implements one work item and carries no spend

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestChangesHaveNoSpendColumns`, `internal/code.TestTheCodeFeedDerivesOnlyWakes`, `internal/engine.TestACodeWakeNamesItsWorkItem`
- **Tag-status:** unreleased

## The decision

A change (`web!42`) implements **at most one work item** — the item its
owner named at the open or an edit set since (`code_changes.work_backend`,
`work_id`, `work_key`, `work_project`) — and **carries no spend**: no table of
the code domain has a column for the tokens or the money a turn spent.

Every wake about a change names its item. The code log's feed
(`code.FeedGroup`, `crewlet-code-feed`) derives a seat's wake from the
committed record and the routing its writer stamped on it, the item among it
(`code.ChangeRouting.WorkItem`), and the wake's metadata carries the item
(`code.Prompt.WorkItem`), so the turn the wake starts is charged to that item
by [ADR-0022](0022-a-turn-is-charged-to-one-work-item.md)'s trigger rule —
the review a change was asked for, the fix its findings asked for, the
answer to its conflict are all work on the item the change implements. A
change that names no item charges its wakes' turns to none.

Spend stays where ADR-0022 puts it: on the turn, charged to the item —
counted on a native task's row, and in the usage history for every item.
What a change says about the work done on it is counts of what happened to
it (its revisions, its reviews, its findings), never a sum of what a turn
spent.

## Why the obvious alternative is wrong

**Spend columns on the change** would count one turn twice: a turn woken by a
review request is charged to the change's item already, and a sum on the
change beside the item's would be a second total that disagrees with the
first the moment a turn touches two changes of one item, or a change outlives
a re-pointed item. And it would make the change a second place a turn is
charged to, which is the split ADR-0022 refuses: nothing in a turn states
what share of it was the change's and what share the item's.

**A wake that names the change and no item** would leave the turn on nothing
at dispatch, and its spend found only by rule 4's sole write at completion —
which a review, a comment and a fix pushed to a branch never are, since none
of them writes the item. Every turn a change's review cycle costs would be
charged to nothing.

## What this does not decide

Which item a change names, and who may set it (the change's lifecycle, its
open and its edit); what a seat's day counts of the changes it opened and the
reviews it gave (the usage history's); and who a change's records wake —
which is its routing's, and a person is never woken from the queue at all.
