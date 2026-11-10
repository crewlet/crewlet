# ADR-0066: Every executor is its coding CLI's own run

- **Status:** accepted
- **Authority:** `internal/agent/runner`
- **Enforced-by:** `internal/agent/runner.TestTheExecutorLaunchesItsRunAndParks`, `internal/providers/llm/chain.TestEveryMemberSittingOutIsAnExhaustedChain`, `internal/engine.TestALaunchWithEveryRunnerSittingOutIsAnExhaustedChain`, `internal/providers/llm/harness.TestAnExecutorRunThatSpentThePlanSitsItsEntryOut`, `internal/providers/llm/harness.TestARunsResetIsReadAgainstWhenItsEndingWasFirstRead`, `internal/providers/llm/harness.TestAnEndingReadAgainIsTheSameReport`, `internal/engine.TestAResumeWaitsOnTheChainItsFirstCallTakes`, `internal/agent/runner.TestAnAgentRunThatNeverSubmittedIsRescued`, `internal/agent/runner.TestAnAnswerCarriesTheRunOnRatherThanReportingIt`, `internal/config.TestTheExecutorHarnessesAreTheInBoxRunners`, `internal/config.TestAnExecutorChainWithNoRunnerIsRefused`, `internal/config.TestAMixedExecutorChainIsAdvisedAbout`, `internal/config.TestAnAgentSeatNeedsTheCatalogue`, `internal/config.TestARunInOnAHarnessThatLaunchesNoRunIsRefused`, `internal/config.TestACeilingBelowTheExecutorFloorIsRefused`, `internal/engine.TestALaunchPlacesTheRunInItsMembersCell`, `internal/engine.TestALaunchFallsPastASpentHeadToTheNextMemberAndRunsOnIt`, `internal/engine.TestALaunchBelowTheExecutorFloorIsABudgetRefusal`, `internal/engine.TestALaunchRefusedBelowTheFloorParksTheSeat`, `internal/engine.TestARefusedContinuedLaunchIsHeldOnItsOwnFloor`, `internal/engine.TestASessionNoWindowCanCarryIsNotContinued`, `internal/engine.TestAContinuedLaunchIsHeldToItsSessionsFloor`, `internal/engine.TestASessionNoWindowCanCarryStartsFresh`, `internal/sandbox/executor.TestAContinuedClaudeSessionRunsUnderTheEnginesWindow`, `internal/agent/prompts.TestEveryReviewIsToldTheRunsLocalWorkIsInNoLog`, `internal/sandbox/executor.TestTheClaudeInvocationIsHeadlessAndMachineReadable`, `internal/sandbox/executor.TestTheOpenCodeConfigLeavesNoPermissionGateForNobodyToAnswer`, `internal/engine.TestAFirstLaunchHeldBelowItsContinuedFloorParksTheSeatOnThatFloor`, `internal/engine.TestAConversationNoWindowCanCarryStartsFresh`, `internal/engine.TestEveryFreshFallbackIsToldTheFrozenEndings`, `internal/agent/runner.TestABaseContinuationArmsAnEmptyGuard`, `internal/engine.TestAnAskBehindAParkIsOwedOnWhicheverNodeTheSweepRuns`, `internal/engine.TestWhetherAParkHoldsASeatsInboxIsAskedOfItsHolder`, `internal/engine.TestEachSessionALaunchMayContinueIsHeldToItsOwnFloor`
- **Measured:** a run's first request alone is the CLI's harness prompt and tool definitions, about 15k input tokens for Claude Code, before the bridged tool array and the seat's system prompt, which sets the launch floor at 25000 tokens
- **Cost-when-tried:** the executor ran on the engine host as a text phase, its CLI's own shell and editor denied, and code work went through a `run_sandbox` tool that started a second coding agent in a box: two agents and two prompts for one task, a brief the first wrote for the second, the first's whole conversation serialized onto the run's row and re-entered with the second's findings spliced in as a tool reply, and three round caps between them (`max_tool_rounds`, `execute_max_tool_rounds_ceiling` and the box's `max_turns`)
- **Tag-status:** unreleased

## The decision

**Every agent seat's executor is its coding CLI's own agentic run, in a box.**
The CLI drives its own loop with its own shell and editor, fully permissioned
(Claude Code under `bypassPermissions`, OpenCode with every action allowed but
`question`), because a headless run has nobody to answer a permission prompt.
The seat's tools reach it as one MCP server over the bridge
(`internal/api/mcpbridge`), and every bridged call is decided by the same
surface as a phase on this host: the seat's grant and the skill guard, the
seat fence before and after the call, the tool's schema through `tools.Invoke`
with its canonical arguments
([ADR-0059](0059-a-tools-contract-is-its-validators-and-every-fault-comes-back-at-once.md)),
and the run's end. The run ends by calling `submit_work` there. Only a
CLI with an in-box runner can be one, Claude Code and OpenCode
(`config.ExecutorHarnesses`); every other CLI serves the reviewer, delegated workers,
the onboarding pass and the side passes, which stay the engine's own tool loop
on this host. A seat whose executor chain has no such member is refused, and
a chain that mixes one with another CLI is warned about.

**The run is detached, and its turn parks.** The launch picks, from the turn's
pinned epoch and at every launch, the first member of the seat's executor
chain that has a runner and is not sitting out a spent plan, places the run in
that entry's `run_in` cell (else `providers.sandbox.default_run_in`) and parks
the turn with its bookkeeping and no conversation (`internal/agent/execstate`).
The run's collection re-enters the same turn for the engine's delivery check
and the reviewer, whose send-back launches the run again. A person's answer to
a run that parked on a question launches it again too, and so do a job lost
with its host and one refused on a sign-in replaced while it ran. Every such
relaunch continues the turn's own harness session (and, where the turn has kept
none, the session its conversation's last turn left: the amendment below): the kept
capture of the job it follows — never an earlier job's, which holds none of
what the jobs after it were told — restored into whatever box the launch gets,
where the harness's runner can take it up and the seat's budget can carry it
(`internal/sandbox`'s restore.go); else, for an answer or a replaced sign-in,
the session the box it parked in holds, where that box is kept, the member
picked runs the same harness in the same cell and the box is this node's to
drive; and otherwise as a fresh run told the question and the answer, or how
the run before it was cut off, and the branch it pushed. So a company with an agent seat and a model
needs `providers.sandbox`, and a launch is held to a budget floor
(`turn_engine.executor_min_budget_tokens`) and charged on collection, its
rounds bounded by the harness's own turn cap where the company or the seat
sets one. **A relaunch that continues the turn's session is held to more**: the
larger of that floor and the session's context — the whole input of its last
request, cache included, recorded with its capture, kept or not — with the
brief that continues it, since it sends that context again before it reads a
word of the brief: the turn's kept capture and the job's own copy in its box
alike. The floor is read in one place (`internal/engine`'s `launchFloor`), by
the launch's refusal, by the hold that waits on it and by the reason the hold
states, so a resume refused below a continued floor waits for a window with
room for that floor and not the static one; a session whose floor is past every
capped window's smallest ceiling is not continued, and its relaunch starts
fresh on the static floor. The launch decides that, once the brief exists; no
session is refused for its size alone, since a seat with no capped window
protects no budget by refusing it. What bounds a continued run's every request
is its harness's own compaction: Claude Code's at the window of the run's model,
which every run is pinned to (`sandbox.ClaudeCodeWindow`), and OpenCode's near
its model's own window, which the engine cannot set without knowing that window.

**A model's limit reads the same on this road**
([ADR-0064](0064-a-model-at-its-limit-parks-the-seats-it-holds-and-a-seat-says-one-steady-thing.md)).
A launch whose every runner member sits out is the exhausted chain a call
reports, each member's refusal on it (`chain.Pick`), so the seat is parked on
it, whether a dispatch's launch or a resumed segment's relaunch met it, and a
resume waits for the park as for a reviewer's limit. A run that ended on its
vendor's limit report is read by the classification every call takes, the
CLI's own terminal event first (Claude Code's result line and its
`api_error_status`, carried from the box as `sandbox.Result.Event`), against
the instant its ending was first collected (`sandbox.LaunchRecord.CollectedAt`),
and sits its entry out on the resuming node (`harness.Provider.RunEnded`) for
a window that runs from that instant, so a job collected again after a hold
is the same report and benches nothing more. A resume waits on the chain its
first call takes, which the turn gate is told by route: a completion's
reviewer's, an answer's runner members.
And the calls a resume inherits are its run's calls over the bridge, made
before the segment that collects it, or before the run asked the question a
person's answer carries it on from (`turn.Work.Inherited`).

## Why the obvious alternative is wrong

**An executor on the engine host that hands code work to a second agent** is
what the cost above records. A coding CLI is already an agent: denying it its
shell and editor made the executor narrate work it could not do, and the
second agent then did that work from a brief, without the seat's system
prompt or its tools. The two loops had two prompts, two sets of round caps and
a conversation to carry between them, and the engine kept a serialized
transcript whose only purpose was to splice one agent's answer into the
other's.

**Keeping a text mode beside the agentic one** keeps every one of those costs
for the seats that use it, and a second answer to "where does this turn's work
happen" for every reader of a turn.

**Gating each tool call of the run in the engine's loop** would mean running
the CLI's model through the engine again. The bridge holds what a call is owed
(the seat's grant and skill guard, the seat fence before and after the call,
the tool's schema, the run's end); a budget is a floor at launch and a charge at collection, and
rounds are the CLI's own cap.

## What this does not decide

How a phase's tools reach its CLI ([ADR-0049](0049-a-phases-tools-reach-its-cli-as-one-mcp-server.md),
amended by this record), how a box reaches its run's tools through any node
([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)), and which node
drives a box in a local cell
([ADR-0065](0065-a-local-box-is-driven-by-the-node-that-launched-it.md)).
Where each cell runs and how a box is provisioned are `internal/sandbox`'s.

## Amendment: a turn's first launch may continue a session, and parks on the floor it was refused below

A conversation's next turn continues the session its last turn left
([ADR-0073](0073-a-seats-executor-session-is-a-sealed-object-its-turns-row-names.md)'s
amendment), so a turn's FIRST launch may continue a session too, and is held
to that session's floor — its context, as its row recorded it, with the lead
and the brief that continue it — as a relaunch is. A base past every capped
window's smallest ceiling is not continued, and its launch starts fresh on the
static floor. A first launch refused below a continued floor is refused on its
dispatch, which has mail to park rather than a resume to hold: the dispatcher
parks the seat's mail on THAT floor, which the refusal hands it, and an ask a
colleague waits on behind the park is owed for as long as the park holds: the
idle sweep, which runs on whichever node holds the maintenance duty, asks the
node holding the seat whether one of its parks holds the seat's inbox, since a
park lives in its holder's process alone (internal/engine's parkheld.go).
Parked on the static floor, a window with room for a fresh launch read as not
starved, the delivery was NAKed into the same refusal until the broker
dead-lettered it, and the ask read as not owed; recounted on the duty node's
own parks, it read as not owed on every node but the holder. A run that continues the conversation's session arms an empty
skill guard: the skills that session loaded were another turn's, which its lead
says were set up anew.

## Amendment: each session a relaunch may continue is held to its own floor

A relaunch may be offered two sessions at once: the one it was asked to
continue in any box (the turn's kept capture, or the session its conversation's
last turn left) and the job before's own copy in its box, each with a brief of
its own. Its box decides which it takes up as the run starts, the first where it
can restore it and the copy only where it cannot, so the launch is held to the
larger of their floors. It was also held to that larger floor when it asked
whether a session could be carried at all: where the larger one could not, both
were dropped and the run started fresh, though the copy's longer brief is sent
only where the box cannot restore the kept session, whose own floor fitted.
Each session is now held to its own floor (`agentLauncher.continuedFloor`): one
whose floor no capped window, or no request under the window of the model the
launch picked, can carry is dropped alone, the launch still offers the other,
and it is held to the larger floor of the sessions it still offers. Only where
neither is left does its run start a session of its own on the static floor.
