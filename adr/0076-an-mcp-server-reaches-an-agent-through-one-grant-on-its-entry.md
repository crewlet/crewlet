# ADR-0076: An MCP server reaches an agent through one grant on its entry

- **Status:** accepted
- **Authority:** `internal/config`
- **Enforced-by:** `internal/config.TestASeatHoldsAServerOnlyThroughItsGrant`, `internal/config.TestAnMCPEnvBlockForAServerThatDoesNotReachItsSeatIsRefused`, `internal/config.TestAGrantNamesOnlyWhatTheChartHolds`, `internal/config.TestAServerBoundToAVendorCredentialTakesNoAccess`, `internal/engine.TestASharedServersToolsReachOnlyTheSeatsItIsGrantedTo`, `internal/engine.TestAPerAgentServerStartsForTheSeatsItsGrantReaches`, `internal/engine.TestABoxIsGivenOnlyAServerItsSeatIsGranted`, `internal/api/queries.TestTheDetailSaysWhoAServerReachesAndWhy`
- **Cost-when-tried:** a shared server reached every agent seat with no way to narrow it, and a per-agent server reached exactly the seats holding an `mcp_env` block for it, so a server that needs no credential could not be granted without a placeholder key, a unit could not be granted anything its seats did not each carry a block for, and removing a block to rotate a value silently took the seat's tools away.
- **Tag-status:** unreleased

## The decision

**A server reaches an agent seat through `access` on its `mcp_servers` entry,
and through nothing else** (`config.MCPServer.Reach`). Absent, it reaches every
agent seat. Written, it reaches exactly the seats it names: each unit in
`units` stands for every agent seat in that unit and in every unit beneath it,
each handle in `seats` for that seat, and `except` leaves out seats a named
unit holds. A person's seat runs no tools and is reached by nothing.

**One grant for both kinds of server.** A shared server's one instance files
its tools into the epoch's registry, and every surface a seat's turn is built
against is cut from that registry to the shared servers the seat is granted
(`engine.grantedSurface`). A per-agent server starts a child for each seat it
is granted to and for no other. An executor box is given a server listed in
`sandbox.mcp.servers` only where the seat is granted it. `Grants` is `Reach`
on a server that is on: a server turned off keeps its grant and grants nobody.

**A server reading each agent's own vendor credential is granted by the
integration's lane** (`roles[].integrations.<lane>.enabled`) and takes no
`access`. The lane is where the engine provisions, seals and binds each
agent's credential for that vendor, so it is already the one switch for who
acts there; a second one on the entry would let the two disagree.

**`mcp_env` holds values and grants nothing.** A seat's block for a server
that does not reach it, a unit's block for a server that reaches none of its
own agents, and a box listing for a server the seat is not granted are refused
by admission, so changing a grant fails the write that changes it rather than
leaving a block that reads like a grant. The blocks the engine reads by their
own name are the engine's as well as a server's and are not held to it.

**A grant names only what the chart holds**, matched exactly as `lead:` and
`manages:` are: a unit's key, or an agent seat's handle. Removing a seat or a
unit a grant still names is refused at the grant, so a seat or unit created
later under the same handle or key is never granted by a grant written for
another. Every rule here is an admission rule: a stored revision that breaks
one runs by the grant as written and warns.

The server's detail answers who it reaches and why (`reaches[] {seat, reason,
via}`), from the same function, for an administrator.

## Why the obvious alternatives are wrong

**Keep `mcp_env` presence as the grant for a per-agent server**: a value block
and a grant are two facts in one field, so a server that needs no per-agent
value could only be granted with a placeholder key, and removing the last key
of a block revoked the server without anything saying so.

**Grant on the seat or the unit** (`roles[].mcp.<server>: on`): a grant is a
decision about the server, made by whoever may change the server. Seats and
units are written by unit leads within their own subtree, so a grant there
would let a lead give their agents any tool server the company declares. A
lead narrowing a grant for their own teams is a separate, narrowing-only
decision.

**Clear a removed seat's or unit's name from every grant in the same write**:
seats and units are removed through the whole document or by replacing the
unit they sit in, by a writer who may not be allowed to change a server's
entry, and a grant silently shrinking is the failure this record exists to
stop. Refusing the write names the grant to change first.

**Expand a unit to its direct members only**, as a unit's `mcp_env` values
are: values are an identity and stay one level deep on purpose, while a grant
is permission, and a grant to a unit that skipped the teams inside it would
read as reaching people it does not.
