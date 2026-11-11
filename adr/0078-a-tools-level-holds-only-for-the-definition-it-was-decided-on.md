# ADR-0078: A tool's level holds only for the definition it was decided on

- **Status:** accepted
- **Authority:** `internal/mcpreview`
- **Enforced-by:** `internal/mcpreview.TestAChangedDefinitionHoldsAnAllowedToolAtApproveUntilAccepted`, `internal/mcpreview.TestALoosenedHintIsFlagged`, `internal/mcpreview.TestARemovedToolKeepsItsRow`, `internal/engine.TestAChangedDefinitionHoldsAnAllowedToolAtApproveUntilAccepted`, `internal/engine.TestAServersFirstListingIsAcceptedOnceAndAnAdditionAfterItIsNot`, `internal/engine.TestANodeThatHasNotReadWhatWasAcceptedHoldsEveryTool`, `internal/engine.TestTheSweepKeepsTheDefinitionWhatWasAcceptedNames`, `internal/engine.TestABoxIsGivenNoServerAToolOfWhichChanged`, `internal/mcp.TestARunningServerIsListedAgainOnItsTTLHeldBetweenTheFloorAndTheCeiling`, `internal/mcp.TestAToolAServerChangesWhileItRunsIsRefiledWhenItsTTLRunsOut`, `internal/mcp.TestAListChangedNotificationListsAgainNoSoonerThanTheFloor`, `internal/mcp.TestTheProbeHandsTheSDKAListingThatIsNeverFresh`, `internal/api/mcpapi.TestAChangeIsAcceptedOrKeptBlockedByTheDefinitionReviewed`, `internal/api/mcpapi.TestAServerCannotMakeAChangeLookAccepted`, `internal/api/mcpapi.TestAlwaysAllowAcceptsTheDefinitionTheCallWasMadeUnder`, `internal/coord/memory.TestFleetContract`, `internal/coord/kv.TestFleetContract`, `internal/api/queries.TestADescriptionChangedPastTheListingsCutIsServedWhole`, `internal/api/queries.TestAPartNoRecordCanShowIsSaidUnshown`
- **Cost-when-tried:** a tool's level was read off its name alone, so a server could change what a tool its operator had set to Always allow does (a description that now says it deletes, a read-only hint dropped, a new tool behind a familiar prefix) and every agent went on calling it unasked; and the engine listed a running server only when it started it, so nothing would have seen the change anyway.
- **Tag-status:** unreleased

## The decision

**A tool's level is a person's decision about the tool as its server defined
it then, and holds for that definition alone**, named by its digest
(ADR-0074). A tool the server lists under any other definition, or adds after
its first listing, is UNREVIEWED until somebody accepts the change, and an
unreviewed tool whose level would let it run unasked asks a person before
each call, exactly as a tool at Needs approval does (ADR-0077).

1. **What was accepted is coordination state**: one record per (server, the
   server's own name for the tool) in the positions register
   (`coord.MCPAcceptedTool`), naming the digest, who accepted it and when,
   and the definition's words, written by compare-and-set. The whole company
   decides a call by it, now; it is not configuration, since it names digests
   no operator writes.
2. **A server's first listing is accepted as it is**, by the node that lists
   it first, which reads the store before it writes and creates each record
   only where none is held. A server is baselined once any record names it:
   a removal accepted keeps its record, so a server that drops every tool
   never lists "for the first time" again.
3. **One reading decides every reader** (`internal/mcpreview`): the node that
   files a tool asks it at each call, through the bridge's owner
   (`mcp.Supervision.Unreviewed`), on its watched copy, and a node that has
   not read the copy holds every allowed tool; the server's page lists each
   change with the part it moved, the definition before and after, and
   whether it claims less risk than before (LOOSENED, read off the hints the
   server declared), each definition whole from the record ADR-0074 keeps of
   it, and any part it cannot show named as unshown rather than drawn as
   unchanged; the alarm `mcp_tools_changed` counts the changes that
   hold a tool. A tool no live listing names keeps its row, removed, until a
   person accepts that it went.
4. **A person decides a change by the definition they reviewed**:
   `POST /mcp-servers/{name}/changes` names each change's digest, and is
   refused when the fleet does not list that definition now, so a server
   that swaps a definition between the review and the answer cannot have its
   swap accepted. Accepting writes the record; blocking also sets the tool to
   blocked. Both are `config.write` at a recent step-up. An approval's Always
   allow accepts the definition its call was made under.
5. **A running server is listed again**, on the `ttlMs` its listing states,
   held between one minute and a quarter of an hour (fifteen minutes when it
   states none), and on a list-changed notification, never sooner than the
   minute; the SDK is handed every listing with no TTL, so its cache never
   answers in the server's stead.
6. **A box is given no server a tool of which is unreviewed**: it reaches a
   server whole, and could call the tool without being asked.
7. **The sweep keeps what an accepted record names**: its definition whole,
   for as long as a change is shown beside it, and the records of a server
   the company declares or a node lists.

## Why the obvious alternatives are wrong

**Accepted digests in the company document**, beside the levels: the document
is the operator's configuration, revisioned and rendered by crewlet-operator,
and a digest changes whenever a server does, so every server upgrade would be
a revision nobody wrote, and an operator-managed company could never accept
one.

**Holding a changed tool by refiling it at another level**: a level is fixed
on the tool when it is filed, and accepting a change files nothing, so every
acceptance would have to restart the server or refile every registry on every
node. Asked at each call, the answer moves the moment the record does.

**Accepting by the tool's name alone**: the definition listed when the person
answers may not be the one they read, and a server that controls its
definitions could make any change look accepted by swapping it in between.

**Listing again only on a list-changed notification**: a server that never
sends one, or sends one constantly, decides whether and how often it is
watched; the bounds are the engine's because the server is the party being
watched.

## What this does not decide

Which definition a mid-upgrade fleet should run when two nodes list two:
each is a change of its own, decided apart. And how the dashboard draws a
change (the dashboard design's Drift dialog).
