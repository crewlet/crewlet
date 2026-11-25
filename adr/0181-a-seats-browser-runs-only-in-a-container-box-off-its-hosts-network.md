# ADR-0181: A seat's browser runs only in a container box off its host's network

- **Status:** accepted
- **Authority:** `internal/browser`
- **Enforced-by:**
  - `internal/config.TestABrowserSeatIsRefusedWhereAnyRunOfItLandsOutsideAContainer`
  - `internal/config.TestABrowserSeatIsRefusedInAContainerOnItsHostsNetwork`
  - `internal/config.TestABrowserSeatIsRefusedOnAnEntryThatDeniesTheWeb`
  - `internal/sandbox.TestALaunchOutsideAContainerBoxNeverRunsAnythingInIt`
  - `internal/sandbox.TestARunDeniedTheWebWithholdsItsBrowser`
  - `internal/sandbox.TestABoxThatMayReachItsHostsLoopbackNeverRunsEnsure`
  - `internal/browser/hostreach.TestOnlyATokenThatArrivesFindsARoute`
  - `internal/browser/hostreach.TestTheVerdictTakesTheKernelThenTheRoutesThenTheLine`
  - `internal/browser/hostreach.TestAConnectionQueuedBeforeTheCheckEndedIsReadBeforeTheVerdict`
  - `internal/browser/hostreach.TestAnythingElseOnTheCheckPortWithholdsAsUnchecked`
  - `internal/browser/hostreach.TestTheEngineListensOnItsLoopbackAloneOnOnePort`
  - `internal/browser/hostreach.TestTheBoxsHalfFindsEveryRouteAndProxyItHas`
  - `internal/browser/hostreach.TestAMadeConnectionIsHeldUntilItIsAnsweredOrClosed`
  - `internal/browser/hostreach.TestASocketBoundOnTheBoxsLoopbackFromOutsideItIsFound`
  - `internal/browser/hostreach.TestAProxyOfThisHostThatRefusesEveryLoopbackAskIsAskedElsewhere`
  - `internal/sandbox.TestABoxsRunningBrowserIsStoppedWhereItMayReachItsHostsLoopback`
  - `internal/sandbox.TestTheBrowserServerReachesOnlyABoxWhoseEnsureAnsweredReady`
  - `internal/sandbox.TestOnlyTheLaunchMakesABrowserServer`
  - `internal/sandbox.TestNothingABoxPrintsReachesTheBrief`
  - `internal/e2e.TestABrowserSeatsBoxRunsItsBrowserInAContainer`
  - `internal/e2e.TestABrowserIsWithheldWhereItsBoxReachesItsHostsLoopback`
- **Measured:** on 2026-11-23, with Docker 29.10 on a 12-core Linux host and an image built by code-sandbox.md's recipe 3 on Debian 12 (Node 22.23.5, the expected Chromium 159.0.8154.6, its sandbox waived): ensure took 2.04 to 2.11 s on a fresh container box from the image's npm cache with no network, 2.14 to 2.32 s from the registry with 4 MB fetched, and 0.37 to 0.42 s on a kept box whose browser it found running; on 2026-11-24, built on Debian 13, the recipe's base since, the launch's step took 2.18 to 2.31 s on a fresh box from the npm cache with no network and 0.54 to 0.65 s on a kept one, its check of the box's network included, and the box held 204 MiB with the browser on no page, 304 MiB on one and 523 MiB on five, at a peak of 599 MiB; and in the suite's browser box image on the same Node, the launch's check of the box's network took 0.19 to 0.23 s on a box with no network, 1.70 to 1.76 s on the default bridge, where the host's ufw drops a box's packets to it, and 1.70 to 1.73 s withholding a box on the host's network as `loopback`
- **Cost-when-tried:** the engine-side seat browser of #804 never landed, and the engine's DevTools client had grown 103 commands and events for it, 68 of which nothing else used and which went with it (ADR-0165)
- **Tag-status:** unreleased

## The decision

**A seat with `role.sandbox.browser` gets a headless Chromium in its
executor's box, and only where every run of the seat lands in a `container`
box off its host's network.** The box's helper launches Chromium over a pipe,
with no port, and the harness drives it through the box's own `browser` MCP
server (`browser.ServerName`).

The rule names the one cell it admits. A seat any of whose runs can land in
another cell, in a container on its host's network, or on an entry that denies
its harness the web is refused when its document is written. A launch that
lands elsewhere anyway (a revision another build admitted, a default a reload
moved) withholds the browser before anything runs in the box:
`cell_not_container` or `web_denied`.

A box off its host's network is one the launch finds so by asking the box
itself, at every launch and before it writes the helper (`host_network`). The
box runs on the engine's own kernel, which it shows by a boot id no box on
another kernel can know, so no runtime that runs its boxes in a virtual machine
or on another machine gets a browser: Docker Desktop, Podman's machine, every
runtime on macOS, and every runtime an engine under WSL drives, whose kernel
every distro shares. It is not in its machine's own network namespace, which a
kernel with fixed inodes for its initial namespaces numbers so. And it shows no
route to the engine's loopback: the engine listens there for the check alone,
and the box dials it at its own loopback, at its gateways, at every address
its hosts file names, beyond a rootless runtime's gateway and through each
proxy its browser would use, each route with a token of its own; a token that
arrives withholds the browser, and so do a proxy of this host that refuses the
check's port whatever the destination, a socket on the box's loopback that no
process in the box holds, a check that does not finish, and a port something
else reached. A browser the box already runs is stopped where the check
withholds it.

Beside the check, the rule requires the engine in its host's network namespace,
beside the runtime; a proxy of this host that refuses the boxes' loopback
destinations by their resolved address and at every port; and no forwarder or
mapping to the host's loopback for a seat's boxes, for any port or address:
each would lead a page in the box's browser to a service listening there.

The server is rendered only into a box whose ensure answered ready. What the
box prints is read into `internal/browser`'s closed values and never into a
prompt. Nothing in the engine speaks DevTools to a box: its one DevTools client
is the artifact render's (ADR-0165).

## Why the obvious alternatives are wrong

**A browser on the engine host**, in a `direct` box or as an `mcp_servers`
entry running Playwright MCP: an untrusted page reaches a fully permissioned
model on the host that holds the fleet's keys. The entry is advised against for
that reason (*A browser on the engine host*).

**A container on its host's network.** It shares the host's network, every
service listening on the host's loopback included, where a page could lead the
agent.

**A rule that lists the cells it refuses.** A cell added later would be
admitted without anybody deciding that it runs a browser. Naming the one
admitted cell refuses a new one until a decision admits it.

**Config's refusal alone.** It is an admission rule, so a revision another
build admitted still applies; a kept box keeps the network it was made on
whatever a later revision says; and it reads only the settings Crewlet passes
the runtime, where a runtime's own configuration (Podman's `containers.conf`, a
rootless Docker allowed the host's loopback) and its design (every virtual
machine runtime leads `host.docker.internal` to its host's loopback) lead a box
there by none of them. So the launch asks the box itself, which shows a route
whatever set it.

**A record of the network a box was made on.** It holds only what config
already refuses, and a second answer beside the box's own can disagree with
it; the check asks the live box every time, a kept one included.

**Asking the runtime what it is.** `docker info`'s operating system, a kernel
version or Podman's remote flag name the runtimes they refuse, so a runtime
added later is admitted unexamined, and a probe that gets no answer must not
read as one; the engine's own boot id admits one kernel, and the box's own user
namespace says whether it dials beyond its gateway.

**Rendering the server from the configuration rather than after ensure.** A
box that cannot run the browser would hand the model a server that fails its
first call.

**An image carrying `@playwright/mcp`.** Every bump of the engine's pin would
leave every image's tree stale. The image carries Chromium and, at most, an npm
cache; the box's npm installs the engine's lock.

## What this does not decide

Watching a seat's browser and taking control of it. How the helper is packaged
and the switches it launches Chromium with, which are
`internal/browser/boxhelper`'s. How the launch's check dials and bounds a
route, which is `internal/browser/hostreach`'s.
