# ADR-0176: A seat's runs see and message each other, and a run may wait for another

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestListRunsShowsATitleOnlyForTheSameRequester`, `internal/engine.TestAMessageIsRefusedAcrossRequestersToItselfPastItsBoundsAndToARunNotWorking`, `internal/engine.TestAnotherRunsMessageIsReadFencedAtTheNextStepAsInformation`, `internal/engine.TestAWaitParksAndIsAnsweredOnEachWayItsRunLeaves`, `internal/engine.TestAWaitThatWouldCloseACycleOfRunsIsRefused`, `internal/sandbox.TestARunsMessageIsAnItemOfItsOwnWithinItsJobsBounds`, `internal/sandbox.TestAJobsMessagesToOtherRunsAreCountedWithinTheirBound`, `internal/sandbox.TestTwoRunsShareARequesterOnlyWhereTheirCeilingsNameOne`, `internal/sandbox.TestARunWaitingOnAnotherKeepsItsKeysUntilItResumesOrEnds`, `internal/agent/runner.TestALaunchListsTheRunsAroundItAndOffersTheirToolsOnlyWhenThereAreSome`, `internal/agent/prompts.TestARunsMessageIsToldAsInformationInItsSendersFence`
- **Cost-when-tried:** with a cap above one ([ADR-0169](0169-a-seat-runs-up-to-its-cap-of-runs-one-per-conversation.md)) a seat runs several runs at once, and nothing told one of the others: two could take the same work item, push the same branch or answer one person twice, each finding out only from the wreckage.
- **Tag-status:** unreleased

## The decision

**An executor's run launched while its seat holds other runs is told of them and given three tools.** Its brief lists each run in flight on the seat (its turn, stage, work item, and, for a run working for the same requester, its conversation's kind and whether it is the caller's, the branch it named, when it started and its task's title), with one line on when to use the tools: overlap on the same item, branch or conversation; an unrelated run is unrelated. The run's surface then offers `list_runs`, `message_run` and `wait_for_run`, beside `ask_and_wait`. Both are read once, from one listing of the seat's runs, with no model call; a launch that finds none is told nothing and offered none.

**What a run is shown of another follows whom each works for.** Two runs share a requester when their ceilings name one principal, read in the order the ceiling composes it ([ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)), a run's own work as its seat's; a stranger is nobody's. Another requester's run shows its stage and item alone, since its task's words carry that requester's data.

**`message_run` files a message as an item on the receiving run's record**, read at its next step under a wake's heading, fenced as the sending run's executor's words ([ADR-0040](0040-content-a-model-reads-carries-its-provenance.md)), naming the sending run and whom it works for. It is information, never a request: the receiving turn's task, requester and apps are unchanged. It is refused:
- between runs of different requesters, since a run's words carry its requester's data and no label decides whom data is for; the refusal says to tell the requester instead;
- to the caller's own run;
- to a run not launching or running, or whose harness reads no note;
- past the sending job's two messages or the receiving job's two, never cut.

The message rides a wake item ([ADR-0167](0167-a-busy-seats-wake-from-its-requester-joins-the-running-run.md)), an item of its own, sealed in its own domain (`seat.run.message`) and naming its sending run in the clear: so it is served, confirmed, told again to a continued session and kept as the turn's once read, and holds one of the job's four wake items and its hook take. Two each way, because one message tells what a run is doing and one answers it; a third is a conversation for the requester. And never more than half the job's wake items, so the requester's own follow-ups keep room to join.

**`wait_for_run` parks the run as a question does**, with its own words for why, on a run of its seat holding a slot. The wait is put to nobody. The node holding the seat watches the waits on its seats and answers one as an answer by turn once its run leaves its seat's slots (it ends, fails or parks), carrying how: ended and its outcome, parked and on what, or gone with nothing on record; what it pushed; and, for a run of the same requester, the first lines of what its reviewer said landed, inside that account's fence. A run leaving on the node nudges every watch; a watch reads again at least every minute. The waiting run's requester is told once, on the conversation's line, why its work waits. **While it waits, its keys stay held**, unlike a question's: its conversation and its own key, so its mail waits aside behind it ([ADR-0169](0169-a-seat-runs-up-to-its-cap-of-runs-one-per-conversation.md)'s aside) rather than starting a second turn on the work it parked to go on with; its resume enters them again, and its ending while parked readmits them. A wait whose run outlives it ends as an unanswered question does: its box is reaped at its pause TTL and it waits in `reseed` for the answer.

**A wait that would close a cycle is refused**, followed through the seat's runs' waits for at most eight runs. No cycle can hang (each waiting run has parked, which answers the runs waiting on it), so the refusal saves both runs a park whose only answer is that the other parked.

## Why the obvious alternatives are wrong

**A new item kind beside wakes.** A session read asks about at most sixty-four hook takes, and notes, late calls and wakes use all of them; a message from another run is a wake's shape, read where a wake is read.

**Messages between any two runs of a seat.** A run working for one person passing what it read to a run working for another would hand one person's data to the other, the ceiling ADR-0109 keeps; "the same seat" is no requester.

**Waiting inside the tool call.** It would hold the harness, the box and the slot for as long as the other run takes, which is the reason a question parks ([ADR-0077](0077-a-tool-call-that-needs-approval-parks-its-run-in-the-shape-a-runs-ask-does.md)).
