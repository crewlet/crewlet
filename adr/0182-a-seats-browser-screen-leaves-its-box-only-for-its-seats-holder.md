# ADR-0182: A seat's browser screen leaves its box only for its seat's holder

- **Status:** accepted
- **Authority:** `internal/browser/screen`
- **Enforced-by:** `internal/browser/screen.TestTheBoxsScreenReachesTheHubOnlyWhileWatched`, `internal/api/mcpbridge.TestTheBrowserRoutesReachTheHubOnlyThroughTheirToken`, `internal/api/mcpbridge.TestABrowserCallReachingItsOpeningAsItEndsIsTurnedAwayToAskAgain`, `internal/api/mcpbridge.TestABrowserPostIsRelayedToTheSeatsHolder`, `internal/sandbox/executor.TestARelayStartsOnTheTokenOfTheSessionItsBoxStarted`, `internal/sandbox/executor.TestEveryRunnersJobRunsItsRelayBesideItsHarness`, `internal/sandbox.TestOnlyAReadyBrowserAtOrAboveTheFloorIsWiredToItsRun`, `internal/browser.TestAFrameIsTakenOnlyWhereItsBytesAreTheJPEGItsMetadataNames`, `internal/browser/boxhelper/live.TestNoFrameLeavesTheBoxWhileNobodyWatches`, `internal/browser/boxhelper/live.TestTheRelayCarriesItsTokenOnNoCommandLineAndInNoLog`
- **Measured:** on 2026-11-25, on Chrome for Testing 153.0.8010.52, the newest patch of each major from 154 to 159 and Playwright's expected 159.0.8154.6, three runs each: from 154 on, a screencast held to one frame in flight keeps one frame unacknowledged and sends the last picture painted 9 to 19 ms after its acknowledgement, while 153 keeps three and sends it only once the page paints again, so on the older builds a box takes one capture of its own once its screen falls quiet; and on 2026-11-26, on a 12-core Linux host outside a container, at each pinned Chromium with its Node, a job's relay held 56 to 65 MiB resident, the supervisor 3 to 10 MiB more with its live view attached than without it, and the supervisor and Chromium together 30 to 43 MiB more while the screen was watched at two frames a second than while it was not
- **Tag-status:** unreleased

## The decision

**A box's browser screen leaves the box only through its job's relay, to the
job's hub on the node holding its seat, under the run's own bridge token.** The
relay is one process for each job, which the job's script starts beside the
harness on the token of the session its box started, and ends with the harness.
It posts what the box's browser shows (frames, the state of its tabs, a
check-in) to the bridge's browser routes under that token, which any node
relays to the seat's holder as it relays the run's tool calls, and it holds one
poll whose answer says whether anybody watches.

There is one hub for each bridge opening of a job whose launch asked for a
browser, held in memory alone. It charges every post to its job's limits and
its node's before it reads it, refuses one past any bound, never decodes a
frame past its header, keeps the newest of each, and decides nothing from what
the box says. It serves only once the job's ensure answered ready at or above
the floor (Chromium 153), so a browser withheld or below it has no live view.
An opening that has ended tells the relay to ask again, never to leave. Nothing
is captured while nobody watches.

## Why the obvious alternatives are wrong

**A DevTools connection from the engine to the box's browser.** It would make
the engine a second DevTools client (ADR-0165) and open a port into every
browser box, whose browser the helper launches over a pipe with none
(ADR-0181). The relay carries pictures and a tab list, never the protocol.

**A credential of the relay's own.** It would be a second thing to mint, hand
over and end beside the run's token, which already names the job and its seat,
says which session its box started (one of the job's two tokens), and ends with
the opening. Under another credential the screen could outlive the run, or
speak for a session the box never started.

**A hub on whichever node a post lands on.** One job's screen would be split
across nodes, each with a newest frame and a watch of its own, and its limits
with it. The holder already serves the run; the fleet's relay brings every post
there.

**Frames over the notes poll.** The notes route is the harness's, its poll held
for what the model reads next. A box posts its screen several times a second,
under bounds and rates of its own, and nothing the box sends about its browser
belongs on the route the model's notes take.

**A relay step in each runner's session gate.** Twelve gates choose a session
each in its own way, and a step in each would be twelve copies of one rule. The
one shared step reads the choice every gate already records.

## What this does not decide

Where a seat's browser may run, which is ADR-0181's; and who may watch one, and
how a person's view reaches its hub: the hub's watch is the seam, and only a
test sets one today.
