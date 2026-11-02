# ADR-0054: Deep work runs the assistant's CLI with its own tools, only in a box

- **Status:** accepted
- **Authority:** `internal/assistant`
- **Enforced-by:** `internal/assistant.TestDeepWorkIsOfferedOnlyWhereItCanRun`, `internal/assistant.TestADeepRunReachesCrewletOnlyThroughTheTurnsSurface`, `internal/assistant.TestAProposalParksTheRunAndTheApprovalContinuesIt`, `internal/assistant.TestAStopKillsTheRun`, `internal/assistant.TestARunsCrewletCallsAreCapped`, `internal/assistant.TestARunNeverOutlivesTheTurnsGrant`, `internal/assistant.TestARunsTokensAreChargedToTheTurn`, `internal/assistant.TestTheBrandRedesignDeliversAssetsAndProposesThemIntoTheDraft`, `internal/assistant.TestAFilesTypeIsReadFromItsBytes`, `internal/engine.TestADeepRunReachesCrewletOnlyThroughItsSurface`, `internal/engine.TestADeepRunIsStoppedAtItsTokenCap`, `internal/engine.TestAnUnconfinableHostRefusesDeepWork`, `internal/engine.TestTheHostConfinementDeniesTheEnginesPaths`, `internal/engine.TestADeepWorkBoxIsMadeUnderTheCompanysBoxRoot`, `internal/engine.TestADeepWorkBoxIsReachedAfterItsBoxRootMoves`, `internal/sandbox.TestAConfinedHostBoxCannotReachTheEngine`, `internal/sandbox.TestAConfinementWithoutAMechanismIsRefused`, `internal/api/mcpbridge.TestASeatSessionIsByteIdenticalOnTheWire`, `internal/api/mcpbridge.TestACallIsDecidedAsItsCallerNeverAsTheRequest`, `internal/api/mcpbridge.TestASessionsTTLCapsItsToken`, `internal/api/mcpbridge.TestAClosedDeepRunsTokenIsRefused`, `internal/api/mcpbridge.TestALoopbackSessionRefusesAnotherHost`, `internal/config.TestDeepWorkIsHandedOutOnlyForAClaudeCodeEntry`, `internal/config.TestDeepWorkPrefersAConfiguredSandbox`, `internal/providers/llm/cliagent.TestTheAssistantArgvLeavesTheCLIWithNoToolOfItsOwn`, `internal/providers/llm/cliagent.TestTheAssistantsQuickModeArgvIsUnchanged`
- **Cost-when-tried:** quick mode alone answers a single read or a single change well, and cannot do a task that needs research, a script, a generated file, an attached image or a hundred steps: the person asked for a team redesign from a month of work, or a new set of brand assets, and the assistant could only describe what it would have done
- **Tag-status:** unreleased

## The decision

**A Dashboard Assistant turn may hand its task to a deep-work run: the
company's `assistant.deep_work` entry, its Claude Code CLI's own run in a box,
with its own shell, files, web search and fetch, and image reading.** The assistant starts in
quick mode, whose model still has no tool of its own (ADR-0050), and escalates
by calling `start_deep_work` itself; the drawer shows the run as one step of
the turn, with its progress, and the Crewlet calls it makes nested under it.

**Its only way into Crewlet is the turn's own surface**, served to the run over
the engine's MCP bridge: the person's own assistant tools, every call an
ordinary request under the turn's read grant through the API's one handler
chain, every write a proposal the person approves (ADR-0051). The bridge
decides each call as nobody, so the request that carried it adds nothing, and
no engine credential, API token or grant is ever placed in the run's box: the
box holds a per-run bridge token that names the run alone, expires with the
run's wall clock, stops working the moment the run ends, and, for a run on the
engine host, is admitted from loopback only. A leaked token reaches nothing the
bridge would not have served the run itself, and its writes still park.

**A proposal suspends the run.** The run is told its change waits and ends; its
box and CLI session are kept for the proposal's lifetime; the person's approval
resumes the turn, which continues the run in the same box and session with the
decision as its next message. A turn that ends parked any other way discards
the box.

**It runs in a box, and on the engine host only confined.** The run goes to the
company's e2b or image-backed container cell when it configures one. Otherwise
it runs on the engine host, which the person accepted, and then only inside the
strongest OS confinement the host enforces (sandbox-exec on macOS, bubblewrap
over namespaces or Landlock on Linux): a box root of its own, an allowlisted
environment with no engine secret, and no read or write of the engine's data
directory, stream store, log, keyring and config files, any CLI login, any
seat's box or the operator's home. That box root is a directory of its own
beneath the company's local box root (`providers.sandbox.local.state_dir`,
else its default), where its seats' boxes are made, so a run's writes land on
the volume sized for boxes and never on the one holding the store and the
stream, and a seat's backend, which lists and reaps only its own directory
there, never reaches one. Every box root the node has made a run's box under
stays denied to every other run after a revision moves it. A host that can
enforce none of these runs no deep work, and an administrator is told why; it
never runs unconfined.

**The box is the only place it writes, temporary files included.** Every
temporary root its processes read (`TMPDIR`, `TMP`, `TEMP` and Claude Code's
`CLAUDE_CODE_TMPDIR`, which the CLI uses instead of `TMPDIR` and otherwise sets
to a literal `/tmp`) is pointed at the box's own `.tmp`, rather than the
confinement being widened to the host's `/tmp`: a shared temporary directory
holds other processes' files, the engine's among them. Each directory above the
box's home may be stat'ed by its literal path, and never listed, read or
written, so a script's `mkdir -p` of an absolute path into the box works on
macOS, whose `mkdir` reads a refused component as a failure unless it can see
the component is there. A tool that ignores every variable and takes the
system's per-user temporary directory (`mktemp` without a template on macOS)
is refused, and the run is told to make temporary files under `$TMPDIR`.
Claude Code's own command sandbox must stay off inside a box: macOS refuses a
second sandbox-exec profile under the box's (`sandbox_apply: Operation not
permitted`). It is off by default and a box's home never turns it on, so the
confinement is the engine's alone; a host whose managed Claude Code settings
force it on cannot run a deep-work run's commands.

**Its limits are its own and the turn's.** A run's wall clock never passes the
turn's read grant; its Crewlet calls and its CLI's rounds are capped by
`max_steps`, and its tokens by `max_tokens`, across the continuations of one
task; its tokens are charged to the turn's meter and filed on the spend
history; Stop kills it.

**Files come back, never in.** What the run leaves in its outbox is kept in the
conversation's file store, typed from its bytes against an allowlist (never
HTML), served to its owner only as an attachment with `nosniff` and a sandbox
CSP, and previewed in the drawer only through an image element. A file enters
Crewlet only through a proposal the person approves, by its id (a draft change
on the screen they have open, which the screen saves through its own route).

## The residual risk the person accepted

**Web access is always on.** A page the run reads can steer what it does, and
the run can carry what it read in Crewlet out in a URL it fetches. The run is
told to treat every page as data and never to put the person's data into a
request, its report is fenced as the run's own words, and every proposal it
makes is marked as made after reading text nobody in the company wrote. What
it can do in Crewlet is still only what the person's own grant reads and what
the person approves; what it can send out is not bounded by anything but the
person's choice to run deep work.

**The engine host's confinement is a deny list beside an allowlist.** On macOS
and under bubblewrap the denied paths are named by the engine, so a sensitive
file the engine does not know of, outside the operator's home, stays readable
to the run as it is to the engine user. Landlock is an allowlist and has no
such gap. A remote or contained cell has neither.

## Why the obvious alternatives are wrong

**The CLI's own tools in quick mode**: every turn would run a shell on the
engine host for a question that needed one read, and ADR-0050's guarantee that
the model reads only what the person's requests return would hold for no turn.

**The seat's sandbox coordinator**: it is built around a seat's lease, inbox and
fence, and the assistant is no seat. The turn watches its own run, and a node
that dies with it ends the turn as interrupted, as any turn on that node.

**An API token in the box**: it would be a network credential for the whole
of the person's access, usable from anywhere until it expired, which is exactly
what ADR-0050 refuses the assistant.
