# ADR-0065: Every executor is its coding CLI's own run

- **Status:** accepted
- **Authority:** `internal/agent/runner`
- **Enforced-by:** `internal/agent/runner.TestTheExecutorLaunchesItsRunAndParks`, `internal/agent/runner.TestAnAgentRunThatNeverSubmittedIsRescued`, `internal/agent/runner.TestAnAnswerCarriesTheRunOnRatherThanReportingIt`, `internal/config.TestTheExecutorCLIsAreTheInBoxRunners`, `internal/config.TestAnExecutorChainWithNoRunnerIsRefused`, `internal/config.TestAMixedExecutorChainIsAdvisedAbout`, `internal/config.TestAnAgentSeatNeedsTheCatalogue`, `internal/config.TestARunInOnACLIThatLaunchesNoRunIsRefused`, `internal/config.TestACeilingBelowTheExecutorFloorIsRefused`, `internal/engine.TestALaunchPlacesTheRunInItsMembersCell`, `internal/engine.TestALaunchFallsPastASpentHeadToTheNextMemberAndRunsOnIt`, `internal/engine.TestALaunchBelowTheExecutorFloorIsABudgetRefusal`, `internal/engine.TestALaunchRefusedBelowTheFloorParksTheSeat`, `internal/agent/prompts.TestEveryReviewIsToldTheRunsLocalWorkIsInNoLog`, `internal/sandbox/codingagent.TestTheClaudeInvocationIsHeadlessAndMachineReadable`, `internal/sandbox/codingagent.TestTheOpenCodeConfigLeavesNoPermissionGateForNobodyToAnswer`
- **Measured:** a run's first request alone is the CLI's harness prompt and tool definitions, about 15k input tokens for Claude Code, before the bridged tool array and the seat's system prompt, which sets the launch floor at 25000 tokens; the engine's own executor loop stopped at 48 rounds once its extension judge had granted all it could, for engine-tool work alone, and a run also takes the shell, edit and test steps as turns of its own, which sets the run's default turn cap at twice that, 96
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
(`config.CodingAgents`); every other CLI serves the reviewer, delegated workers,
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
a run that parked on a question launches it again too: in the box it parked
in, continuing its own CLI session, where that box is kept, the member picked
runs the same CLI in the same cell and the box is this node's to drive; and
otherwise as a fresh run told the question, the answer and the branch it
pushed. So a company with an agent seat and a model needs `providers.sandbox`,
and a launch is held to a budget floor (`turn_engine.executor_min_budget_tokens`)
and charged on collection, its rounds bounded by the CLI's own turn cap.

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
([ADR-0064](0064-a-local-box-is-driven-by-the-node-that-launched-it.md)).
Where each cell runs and how a box is provisioned are `internal/sandbox`'s.
