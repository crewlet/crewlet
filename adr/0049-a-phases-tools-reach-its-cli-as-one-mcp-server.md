# ADR-0049: A phase's tools reach its CLI as one MCP server, and every call passes the loop's gates

- **Status:** accepted
- **Authority:** `internal/agent/toolloop`
- **Enforced-by:** `internal/providers/llm/cliagent.TestNoRequestRendersAToolCatalogue`, `internal/agent/toolloop.TestAClosedFenceStopsTheNextCall`, `internal/agent/toolloop.TestARefusedSpendRunsNoFurtherCall`, `internal/agent/toolloop.TestTheRoundBudgetEndsTheRunAfterItsLastRoundsCalls`, `internal/agent/toolloop.TestThePhaseEndsAtItsTerminatorWithNoFurtherRequest`, `internal/agent/toolloop.TestANoteEndsTheRunAndOpensTheNextAsAUserMessage`
- **Measured:** every one of the twelve CLIs the engine has a profile for is an MCP client in its headless mode; five re-read a changed tool list mid-run (Claude Code, Gemini CLI, OpenCode served, Copilot, pi) and six do not (Codex, Qwen Code, Grok, Muse Code, Kimi Code, Hermes); three give up on a tool call after 60 seconds (Kimi Code, pi, Cursor); three hand the server their own id for a call (Claude Code, Codex, Muse Code); one rewrites a tool name its list does not hold to one it does and runs that instead (Hermes)
- **Cost-when-tried:** the tools rode the prompt as a catalogue and a JSON reply contract, one CLI process per model request with the whole transcript re-sent each time: a model told to prefer its own editor and shell answered in prose instead of the reply, a reply that was itself JSON was read as the contract, the doctor needed a smoke test of its own to say whether a model could follow the contract at all, and every round paid for a cold process and a prompt the vendor's cache had no session to reuse
- **Tag-status:** unreleased

## The decision

**A phase's tools reach its coding CLI as the tools of one MCP server, and
nowhere else.** For the length of one CLI run the tool loop serves the phase's
live surface as a server named `crewlet` (`internal/toolsession`), which the
CLI starts as `crewlet mcp-relay <socket>` (`internal/mcprelay`) — or, for a
seat's executor, its CLI's own run in a box, reaches over the HTTP bridge
(`internal/api/mcpbridge`).
The CLI drives its own loop: it asks its model, calls the tools natively, asks
again. No backend renders a tool catalogue into a prompt, and no backend reads
a tool call out of a model's prose: `llm.Completion` has no calls to carry.

**Every call is decided by the phase, not by the CLI.** The dispatcher the
loop serves (`internal/agent/toolloop`) holds each call to the gates the loop
held each round to: the seat's grant and the skill guard (the surface), the
seat fence before and after the call, the budget, the round cap, the terminator
and the detached calls that run alone, a person's note, which ends the run and
is read as a user message by the next one — never inside a tool result. A gate
that ends the phase ends the CLI's run; the run's record (its requests where the
CLI's own record shows them, its calls, its cost) is folded back into the shape
the loop always kept, so a reader cannot tell a phase the CLI drove from one the
loop drove except by the runs it lists.

**Amended by [ADR-0066](0066-every-executor-is-its-coding-clis-own-run.md), for
a seat's executor.** A seat's executor is its coding CLI's own run in a box,
with its own shell and editor, fully permissioned. The seat's tools still reach
it as this one server, over the bridge, and every bridged call is decided by
the same surface: the seat's grant and the skill guard, the seat fence before
and after the call, the tool's schema through `tools.Invoke` with its
canonical arguments
([ADR-0059](0059-a-tools-contract-is-its-validators-and-every-fault-comes-back-at-once.md)),
and the run's end. Its budget is a floor at its launch and
a charge at its collection, and its rounds are the CLI's own turn cap, so the
loop's budget, round cap and terminator gates hold the phases on this host
(the reviewer, workers, the onboarding pass and the side passes), and the
alternative below of letting the CLI run tools of its own is rejected for
those phases.

## Why the obvious alternative is wrong

**Keeping the envelope**, a catalogue in the prompt and a JSON reply the engine
parses, makes the engine a second harness for a model that already has one. The
vendor's harness knows how its model calls a tool — the grammar, the parallel
calls, the retries on a malformed argument — and ships that with every model;
an envelope asks the model to forget it and follow a contract the vendor never
trained for, and pays a whole process per request to do it.

**Letting the CLI run the tools itself** — its own shell and editor, or an MCP
server the engine merely configures — hands the gates to the CLI. A CLI does not
know the seat's fence, its budget or its round cap, cannot stop a call the
moment a person's note arrives, and keeps calling after the phase has
submitted. The tools are the engine's because the decisions about them are.

**A URL rather than a relay for a phase on this host** needs a listener, and a node may
bind none (`api.port: 0`); the seat's tools would then depend on a setting that
has nothing to do with them.

## What this does not decide

It does not decide **which CLIs are supported or how each is configured** —
that is each profile in `internal/providers/llm/cliagent/cliprofile`, measured
per CLI. It does not decide **where a CLI runs** — a seat's executor is its
CLI's own run in a box, placed by the launching entry's `run_in`
(`internal/sandbox`), and every other phase runs on this host. And it does not change **what a round is**: one
model request, as the CLI's own record shows it, or the whole run for a CLI
whose record shows none.
