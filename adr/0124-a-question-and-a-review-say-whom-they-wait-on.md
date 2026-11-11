# ADR-0124: A question and a review say whom they wait on

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:** `internal/worklet.TestASeatsOwnOpenAsksToAPersonAreReadNewestFirst`, `internal/agent/prefetch.TestATurnWithAPersonListsTheSeatsOpenAsksToThem`, `internal/agent/builtin.TestAWithdrawalNamesWhatNowCoversTheQuestion`, `internal/worklet.TestAWithdrawnQuestionSaysWhyAndWhatCoversIt`, `internal/agent/builtin.TestAnAgentsReviewNamesItsReviewer`, `internal/worklet.TestAReviewIsReadyForTheSeatThatHoldsIt`, `internal/advisor.TestAnIdleReviewNamesWhoItWaitsOn`, `internal/worklet.TestHoldingUpCountsTheAgentWorkWaitingOnAPerson`
- **Measured:** on a company running this engine, 32 of 64 open items waited on its one person while 2 reached that person's Inbox as asks. Two items an agent had moved into review itself waited 8 and 12 days on that person with only a mention, held by nobody. Three items assigned to the person in review sat 8 days with no question. Two date waits named a condition the agent's own later comments had replaced with a pull request, and the person's open question about the replaced step stayed open. Ten agent items waited, through typed `waiting_on` chains, behind three items the person held or was asked about, and the person's list of what they held up named one.
- **Cost-when-tried:** the agents had `withdraw_ask` since ADR-0111, and a person told them in chat to consider anything they had open; no turn could see its own open questions, so none was withdrawn.
- **Tag-status:** unreleased

## The decision

**A turn with a person is handed the seat's open questions to them.** Whenever a person triggers a turn (a chat message, a thread reply, an item comment), the turn-start context lists the seat's own open asks to that person: the item, the decision's one-sentence question, its age and the literal `withdraw_ask` call, at most 10 across the turn's people, the rest counted with the read that lists them. Ten is the page a seat acts on line by line with its own tools; a seat with more open questions to one person has a queue the count names. The list is rendered once at turn start and frozen with the turn, like every other prefetched block.

**A withdrawn question says why and what covers it.** `withdraw_ask` takes `replaced_by`, another work item or a link (a pull request that now runs what the person was asked to run), and the resolve that closes the question records a `Withdrawal` naming the reason the asker posted in its thread and that replacement. It is a new field on the comment a resolve carries, so the record is stamped at version 12 and a build reading 11 holds it back rather than resolve the question and drop why. The person's Inbox reads the withdrawal: the closed card says the asker withdrew it, in the asker's words, and links what covers it. The tool texts and the pickup's brief say to withdraw a covered question in the same step that records the covering work.

**A review names its reviewer.** An agent that moves an item it keeps into `in_review` names `reviewer`. A colleague seat is handed the item, so the review is ready for that seat as one somebody else moved into review. A person is asked: the agent must hold an open ask to them on the item, which is how the review reaches their Inbox, and the item stays with the agent until they answer. An item in review its holder submitted itself, with no question open on it, is ready for that holder again, with its own reason in the pickup: nobody reviews it, so it acts on the review it was given or names a reviewer. Ready work names who submitted each review, and the advisor's `review.idle` names whom an idle review waits on: the agent holding it, the person its holder asked, or nobody.

**What a person holds up counts the agent work behind it.** `holding_up` lists the open items a person holds that other work waits on: those assigned to them, and those whose holder put an open question to them, where the item itself waits on their answer. Each row walks the typed `waiting_on` chains behind it, bounded as before, and counts the agent items among them from the roster the reader is handed. The block's summary counts the held items that agent items wait on and the distinct agent items, and the Inbox leads with "You hold N items that M agent items wait on" when M is above zero.

## Why the obvious alternatives are wrong

**Listing the open asks in the tool answers only** (a `my_asks` read the agent can call) leaves the decision to look with a model that does not know it has anything open; the turn with the person is when it matters, so the engine hands it over.

**Deriving the withdrawal reason from the thread** (the asker's last reply before the resolve) reads prose for a fact the withdrawal can state, and is wrong the first time an agent replies in the thread for another reason.

**A reviewer field on the item** would be a second holder beside the assignee, which every reader of the queue, the pickup and the advisor would have to consult. The holder and an open question already say whom a review waits on.

**Counting only items assigned to the person** misses the agent work behind a question nobody answered, which is where most of it waited.

## What this does not decide

It does not decide when a pickup is handed or what answers it (ADR-0112, ADR-0122). It does not make a question's options change after it is asked (ADR-0100): a reworded question is withdrawn and asked again. It does not wake the person when a question to them is withdrawn; the closed card is in their Inbox's answered group.
