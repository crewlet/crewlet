# ADR-0050: The assistant acts only through a grant derived from the person's session

- **Status:** accepted
- **Authority:** `internal/assistant`
- **Enforced-by:** `internal/assistant.TestTheAssistantImportsNoWriterAndNoSeat`, `internal/iam.TestAnAssistedPrincipalIsAPersonCarryingItsScope`, `internal/iam.TestAnAssistGrantsNameIsNobodysLogin`, `internal/api/auth.TestAnAssistGrantOverTheWireIsRefused`, `internal/api/auth.TestAnAssistGrantIsTheSessionsPrincipalRecomposed`, `internal/api/auth.TestARevokedSessionEndsItsGrant`, `internal/api/auth.TestAnExecGrantCarriesOnlyItsBoundRequestOnce`, `internal/api/auth.TestAnAssistGrantIsRefusedTheGuardedEdges`, `internal/api/auth.TestAnAssistGrantNeverProvesAFresherIdentityThanItsSession`, `internal/api/auth.TestOnlyASessionPersonDerivesAGrant`, `internal/api.TestTheDispatcherServesTheAPIsOwnHandler`, `internal/api.TestTheGuardAndTheAuthorityTableAgreeOnTheAssistant`, `internal/assistant.TestTheAssistantReachesToolsOnlyAsRequests`, `internal/assistant.TestTheAssistantSendsThroughTheDispatcherAlone`, `internal/assistant.TestNoTokenReachesARecordALogOrAPrompt`, `internal/assistant.TestAGrantEndingMidTurnEndsTheTurn`, `internal/config.TestTheAssistantRefusesAnEntryThatCanRunLocalTools`, `internal/providers/llm/harness.TestTheAssistantArgvLeavesTheHarnessWithNoToolOfItsOwn`, `internal/api.TestPermissionParityAcrossFivePrincipals`, `internal/api.TestTheAssistantRouteTableIsTheAppsOwn`, `internal/assistant/catalog.TestAGenericCallNamesARouteOfTheTable`
- **Cost-when-tried:** the one existing path that runs a model against the company's tools on somebody's behalf, the sandbox MCP bridge, replaces the request's principal with the SEAT (`internal/api/mcpbridge`'s `actingAs`), and a seat holds every app, so a person without the tracker app routed through it would read the tracker; and the agent turn context falls back to a seat's principal when none is set
- **Tag-status:** unreleased

## The decision

**The Dashboard Assistant has no identity of its own and holds no credential
that opens product data.** Every read it makes and every write it executes is
an ordinary request through the API's one handler chain (the same page policy,
canonical path, guard, cross-site check, drain gate, route and authority row as
a click in the dashboard), carrying an **assistant grant**:

- **Derived only from a person's own session.** `internal/api/auth` mints it
  from a request the guard resolved to a person acting through `session:`, and
  from nothing else: a machine token, a Tier A token's session and a service
  account are refused the assistant (decision D04).
- **Re-composed on every request.** The grant names the session it came from,
  and the guard resolves that session through the identity rows each time, as
  the artifact view ticket does (ADR-0035), so a sign-out, a revocation, a
  suspension or a seat move stops the assistant at its next step. The person
  is the author of every row it writes and `assist:<grant>` is the credential
  in its operator column.
- **Useless outside the process.** It is accepted only on a request carrying an
  in-process marker the API's own dispatcher sets; over the wire it is refused,
  and it is never fresher than the request that derived it (its step-up instant is
  clamped to the deriving request's).
- **Narrowing, never widening.** A read grant reaches only reads, and an exec
  grant only the one approved request its proposal binds (ADR-0051). The
  person's own authority decision is still the whole decision; the grant's
  scope and the authority table's assistant column (ADR-0052) only subtract.

**Any route of the person's, and only as a request.** Beside its curated
tools the assistant reaches the whole API through generic tools
(internal/assistant/catalog's `api_request` and `propose_api_change`), whose
calls name a route of the table the API itself mounted (`App.AssistantRoutes`,
bound to the catalogue once the App is built), never a URL: each wildcard is
one escaped segment, and the request is held to the table again before the
dispatcher sends it. That table is a filter on what the model may name, never
authority: the guard, the route's own row and the grant decide every call as
they decide a click.

The orchestrator holds a grant as a handle it can name and time but never
present (`internal/assistant/exec`'s `Grant`), and no package under
`internal/assistant` may import the store, another domain's writer or reader,
the operator surface, the question registry, the tool registry, the builtins,
the MCP bridge, the agent turn context or the credential owner.

**Its model has no tool of its own.** A coding CLI's own shell or file reader
runs on the engine host as the engine user, so it would read the engine's data
directory and keys, a read no request of the person's made. The assistant's
chain therefore admits only an entry whose argv (web denied) leaves the CLI with
no built-in tool at all and no tool server but the engine's own, checked off
that argv by `harnessprofile.Profile.ToolsOffProblem` rather than taken from the
profile's `local_tools` claim, since a deny list is always a release behind the
tools a vendor adds. Codex keeps `apply_patch`, which has no switch and whose
patch check answers whether a host file holds given lines, so Codex is refused.

**Its tools reach it as the engine's one MCP server** (ADR-0049, which the
assistant adopted when it reached main). The turn's catalogue is served to the
CLI as the `crewlet` server, the model calls it natively, and every call is
decided by the turn's own loop under this grant: no tool rides the prompt and
no call is read out of the model's prose. The guarantee above is restated for
it: the call's own arguments turn every built-in tool off and load no server
of their own (`--tools ""`, `--strict-mcp-config`, `--setting-sources ""`),
and the profile's mcp block may allow the engine's tools alone
(`mcp__crewlet__*`), which `ToolsOffProblem` checks and
`TestTheAssistantsQuickModeArgvIsUnchanged` pins. A web denial covers a run
with tools too, where a profile's mcp block runs an argv or writes settings of
its own.

**Amended by ADR-0054, for deep work alone.** A turn may hand its task to a
deep-work run, whose CLI does have tools of its own: it runs only in a box, or
on the engine host under OS confinement the person accepted, and its one way
into Crewlet is this turn's own surface, bridged, so every read it makes is
still a request of the person's under this grant and every write a proposal.
Quick mode, the turn's own model, keeps no tool of its own.

## Why the obvious alternatives are wrong

**A seat, or the sandbox bridge, as the assistant's identity.** A seat holds
every app and is the author of what it writes, so a person would read through
the assistant what their own grants keep from them, and the audit trail would
name an agent for a person's gesture.

**A minted bearer usable over the wire, or a loopback HTTP call to the API
port.** Either makes the grant a network credential: a proxy log, a local
process or a misconfigured bind captures it, and it works from anywhere until
it expires. In process, the same handler is called without a credential ever
crossing a socket, and a request that did not come through the dispatcher
cannot carry the marker at all.

**Calling the operator dispatch or the question registry directly.** That is a
fourth transport into the tools, beside MCP, the act route and the human write
surface, which a later change can let drift from the other three: the exact
failure `internal/api/operator`'s package doc exists to prevent.

## What this does not decide

How a write waits for the person (ADR-0051), how each verb says what the
assistant may do with it (ADR-0052) and where conversations live (ADR-0053).
The grant's token format, lifetimes and revocation registry are
`internal/api/auth`'s own doc.
