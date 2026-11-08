# ADR-0077: A tool call that needs approval parks its run in the shape a run's ask does

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:** `internal/tools.TestACallToAToolAtNeedsApprovalNeverRunsUntilAPersonApprovesIt`, `internal/agent/builtin.TestAHeldCallIsRecordedAsADecisionWithItsArgumentsSealed`, `internal/agent/builtin.TestAHeldCallIsNotAnsweredByAnswerExecutor`, `internal/sandbox.TestPendingStoreContract`, `internal/sandbox.TestAChatReplyNeverAnswersAHeldCall`, `internal/agent/runner.TestAHeldCallRunsOnceAfterOnceAndNeverAfterDeny`, `internal/agent/runner.TestAHeldCallIsMadeOnlyAsItWasApproved`, `internal/api/mcpapi.TestAHeldCallIsAnsweredByItsSeatsLeadOrAnAdministrator`, `internal/api/mcpapi.TestAHeldCallIsAnsweredByNobodyElseAndOnce`, `internal/api/queries.TestAHeldCallIsListedForAnAdministratorAndNoMember`, `internal/mcp.TestAToolNothingSetsStartsAtTheLevelItsHintsGiveIt`, `internal/e2e.TestAHeldCallIsMadeOnceAfterItsApprovalAndTheRunCarriesOn`, `internal/engine.TestTheCallAPersonApprovedIsADeliveryOfItsTurn`, `internal/sandbox.TestOnlyWhoAQuestionWasPutToAnswersItInTheConversation`, `internal/sandbox.TestAReplyBySomebodyTheQuestionWasNotPutToLeavesTheRunParked`, `internal/sandbox.TestAQuestionsSeatAnswersOnlyWhileItsHolderMayAct`, `internal/engine.TestAReplysWriterIsTheSeatTheRegistryNames`, `internal/engine.TestASeatAnswersOnlyWhileItsHolderMayAct`
- **Cost-when-tried:** with only `allow` and `blocked`, an operator who wanted a person to look at each call to a destructive tool had one choice, blocking it, and the agent then could not do the work at all; and the executor's harness runs fully permissioned in its box, so its own permission prompts were never an option to fall back on.
- **Tag-status:** unreleased

## The decision

**A call an executor's run makes to an MCP tool at Needs approval is not made:
it is recorded as the job's question, and the run parks on it exactly as it
parks on `ask_and_wait`** ([ADR-0068](0068-a-run-asks-a-person-in-the-shape-a-decision-is-asked.md)).
A tool is at Needs approval by its own level (`mcp_servers[].tools.levels`),
by the entry's `default`, or, where the entry sets neither, because its hints
start it there: a tool its server marks read-only starts at `allow`, every
other tool at `approve`, one that declares nothing included (`mcp.RiskOf`,
`mcp.StartingLevel`).

1. **One seam decides the call**: the seat's surface (`internal/tools`),
   which every call reaches. An executor's surface hands it to its holder,
   which records it on the run's row (`PendingRun.HeldCall`) with the
   question the person reads, a `decision.Decision` offering `once`, `always`
   and `deny`, put to the seat's lead, citing the work item. The harness is
   told the call was not made and to end its turn, the bridge records it as
   the call that ended the job, and the collection parks the run with its
   seat freed. Every other phase (the reviewer, delegated workers, the
   onboarding pass, the side passes) has no run to park and is refused the
   call as `forbidden`, told that the seat's own run may ask.
2. **The arguments are sealed at rest**: encrypted under the fleet keyring
   (`CallSeal`), bound to the run, the job and the call, kept out of the
   question, the call log and the turn's ledger, and opened only for a person
   who may approve the call (an administrator's `approvals`, the seat's
   trail on the executors board) and for the resume that makes it.
3. **Only its approval route answers it**: `POST /mcp-approvals/{id}`,
   `tools.approve`, an administrator or a lead of the seat, a person and
   never the assistant. A chat reply never answers a held call,
   `answer_executor` refuses one, and the seat's holder records only an
   answer that names the held call by its id. One answer per question: a
   second is refused `stale_version`, and the holder records the first it
   takes.
4. **The call is made at most once**, by the resume the answer drives,
   through the surface the run carries on with: marked on the row under the
   claim's fence before it is made, its result sealed on the row after, and
   neither cleared by a claim given back. The call itself is a call of the
   run's round, beside the calls its jobs made over the bridge and as the
   surface answered it, with its arguments and output left sealed
   (`BridgeCall.Approved`): every reader of whether the turn replied — the
   collection's own delivery check, a stop's or a loss's account of the turn,
   a later round's earlier calls — reads the round's calls, and a call
   recorded nowhere among them read as a reply that never went out. The row
   and the round's log are two keys the store writes in no one step, so the
   write that records the result records the call's entry on the row with it
   (`HeldCall.Call`), and the call is appended to the log after it, once by
   the held call's id (`BridgeCall.HeldID`). Where that append never lands,
   the call is still the round's: an ending reads it off the row
   (`PendingStore.TurnLog`), and the reset that opens the next job carries it
   on the row to that job's opening (`PendingRun.OwedCall`), which appends it
   where the log lacks it, until a job of the round has launched past it.
   A later attempt at the same answer
   hands the run the recorded result, or says the call may or may not have
   taken effect; it never makes it again. A tool blocked or changed since the
   call was held is not called.
5. **`always` is a configuration write**, decided as `config.write` at its
   recent step-up and made before the answer is sent: the tool is set to
   `allow` by the server's own name for it. `deny` makes no call and tells the
   run it was refused, as `forbidden`.
6. **A box reaches a server whole**, so `sandbox.mcp.servers` is refused for
   a server whose every tool is not certain to run at `allow`: one whose entry
   says `default: allow` and holds no tool back.

## Why the obvious alternatives are wrong

**Waiting inside the tool call** holds the harness, the box and the seat for
as long as a person takes, which is minutes on a good day and days on a bad
one, and a headless harness gives up on a call long before that.

**Parking the whole turn for a phase on the engine host** would hold the seat
behind one reviewer's call for as long as the person takes, with the turn loop
holding a conversation it has no way to persist; the seat's own run is the
one place a question already parks.

**Answering through `answer_executor`** would let a run's requester approve a
call their own grant does not reach, and would leave Always allow without the
configuration write it is.

## What this does not decide

How the dashboard draws a held call (the dashboard design's: it composes the
Inbox's decision row), and what a changed tool definition does to an allowed
tool (a later decision holds it at Needs approval until accepted).

## Amendment: a reply in the conversation answers only somebody the question was put to

A chat reply still never answers a held call. Since
[ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)'s amendment, one turn works for
one writer, and a reply in the conversation answers any other parked question only when its writer
is somebody the question was put to: a seat of the question's audience, resolved once at the park,
or the run's requester, who owns an executor's question on the dashboard's route too. Whoever holds
that seat must also be able to act when the reply arrives, read then and never at the park, so a
person suspended or removed since, or a human seat nobody holds, answers nothing.

The writer is the seat the engine's party registry resolves the reply to, never the reply's words. A
registry withholds a suspended or removed holder's identities, so a vendor's chat from them names
nobody. Anybody else's reply (a stranger, a person or seat outside the audience, a credential, which
holds no seat) is not the answer. It runs as the ordinary message it is, its own turn bounded by its
own writer, and the run stays parked. A question whose park named neither an audience nor a
requester admits nobody from the conversation, never anybody; the dashboard's route, decided by
authority, still answers it. A directory that cannot say whether the holder may act keeps the
reply's place rather than spending it either way.

The answer is what the run carries on with, inside a turn worked for its requester and held to their
apps. A reply from somebody the question was never put to would steer a run working for somebody
else, with that somebody's apps.
