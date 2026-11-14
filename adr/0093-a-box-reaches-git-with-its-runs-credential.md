# ADR-0093: A box reaches git with its run's credential, and the engine configures its git

- **Status:** accepted
- **Authority:** `internal/gitlet/host`
- **Enforced-by:** `internal/gitlet/host.TestARunTokenOfAnEndedRunIsRefused`, `internal/gitlet/host.TestABoxIsGivenOnlyItsGrants`, `internal/gitlet/host.TestABoxIsShownNothingItsFloorDoesNotHold`, `internal/api.TestEveryMountedRouteIsOnTheListenerItsPathNames`, `internal/api.TestABoxRouteIsAdmittedByItsRunNotTheGuard`, `internal/engine.TestABoxReachesOnlyWhatItCheckedOut`, `internal/config.TestTheEnginesGitVariablesAreReserved`, `internal/agent/builtin.TestARunsRepositoriesAreReadForWhoeverWokeItsTurn`, `internal/agent/builtin.TestARunsPushesAreDecidedForWhoeverWokeItsTurn`, `internal/engine.TestARunChecksOutForWhoeverWokeItsTurn`, `internal/gitlet/host.TestABoxWorksForWhoeverWokeItsTurn`, `internal/gitlet.TestAGrantUnderAColleaguesCeilingIsHeldToBoth`, `internal/gitlet/host.TestTheGitFloorsPassTheSuite` (its box half), `internal/gitlet/host.TestNeitherTransportTakesTheOthersCredential`, `internal/gitlet/host.TestAStoppedSeatsBoxPushesNothing`, `internal/engine.TestALaunchedRunsCallsStopWhenAPersonStopsItsSeat`, `internal/engine.TestTheEngineStopsOneWayForBoxAndBridge`, `internal/gitlet/gitproc.TestBoxesHoldAtMostHalfTheTransferLane`, `internal/sandbox/executor.TestNothingButTheAttributionSpellsATrailerKey`, `internal/agent/builtin.TestARunsCheckoutCarriesWhomItsTurnWorksFor`, `internal/sandbox.TestPendingStoreContract` (its `ARunWorksForOneRequester`), `internal/engine.TestARunsRowCarriesWhomItsTurnWorksForToItsBoxAndItsNextRound`, `internal/gitlet/host.TestABoxPushCarriesWhomItsRunWorksFor`, `internal/engine.TestARunsOpenIsMadeForWhomItsTurnWorksFor`, `internal/gitlet/host.TestABoxACredentialsTurnLaunchedPushesOnlyItsSeatsOwnWork`, `internal/agent/builtin.TestACheckoutForACredentialWritesOnlyItsSeatsOwnWork`
- **Measured:** `BenchmarkBoxClone`, `BenchmarkBoxWorktree` and `BenchmarkBoxDissociate` (internal/gitlet/host's boxbench_test.go) on two E-cores of an Intel Core Ultra 7 265U, Go 1.27.2, git 2.56.0 in the node and the box (the client floor's 2.40.4 in an arm of its own), over gitlettest's in-memory object store, one count each (ten operations; one at a gigabyte, five for the borrowing): a coding box's clone of a 100 MiB history took 1.25 s for 16.7 MB (0.94 s on 2.40.4) and of a gigabyte 14.6 s for 179 MB, a cone narrowing only the checkout's files (13.3 s); the check's one commit 0.13 and 0.99 s; at 100 000 files a warm status took 142 ms on git's own index settings, 99 ms on the engine's (index version 4 and the untracked cache, adopted: the checkout 1.5 % longer) and 17 ms with git's file-system monitor (62 ms beside git's own), which only a box with a kernel of its own is given, and only while its checkouts take half the watches that kernel allows, since one the kernel starves took 247 ms, every status starting its daemon again; 200 rounds of an agent's commands never ran git's automatic maintenance on the 602 lazy-fetch packs they left (a status's CPU 2.4 to 10.4 ms and a `log -p`'s 49 to 157 ms as they gathered), and every bound git's configuration offers cost the session more than the 5 % of its git CPU a box's maintenance is held to — its gc 12.5 % at a 20 MB history and 40 % at 100 MiB — so maintenance is git's; a copy borrowed from the node and dissociated took 2.59 and 47.8 s against the recipe's 1.14 and 13.0 s — every figure in docs/concepts/code-sandbox.md, "What a clone costs"
- **Cost-when-tried:** not tried here. The alternatives on paper: a box holding the API token (it reads the whole company, and the box runs generated code); a per-seat personal access token (a credential that outlives the run and that somebody must rotate); persons' `/git/` on the public listener (operator credentials on the internet-facing socket — the partition `auth.Public` exists to keep)
- **Tag-status:** unreleased

## The decision

A coding box reaches Gitlet on `/box/git/` with HTTP Basic `run:<token>`: a `runtoken` in key domain
`crewlet.git.v1` naming the run's turn and its current launch, signed from Tier A `secrets.keys` (required on every
node) so the node that launched the run and the Gitlet node the box dials derive one key. The RUN ROW is the
authority, read on every request and three-valued: an ended, superseded or unknown run is 403, a store that cannot
be read 503, a bad token 401. The run token is the ONLY credential `/box/git/` takes, and `/git/` never takes it:
neither transport admits the other's. The row carries the run's grants — empty at the launch, and one more with each
`code_checkout` the run's agent calls over its bridge, each naming one repository `read` or `write` with its branch,
base, cone and any change to open at the collection, resolved through the code tools' own resolution as the seat and
as whoever woke its turn and written onto the row before the call answers — matched BY NAME before anything else is
read, and a box's GIT does at most what both the
row and the seat's own grant from the chart allow. In a turn a colleague SEAT woke, the row names that colleague,
and the box is also held to the colleague's own grant from the same chart: it fetches nothing the colleague could
not read, and pushes nothing outside the seat's own work that the colleague could not push — the branches under
the seat's own namespace and every branch of a repository it owns are its own work, which anybody who can message
it steers (`gitlet.Grant.Under`); a person bounds nothing,
maintaining every repository. A person's pause that stops the seat's running work refuses the box's every push, and
every bridged call of a tool not proven read-only, until the seat is resumed — both by the engine's one reading of
the stop, so the two edges stop and resume together. Boxes together hold at most half the transfer lane and one
credential's share of the spool, so a person's git is never queued behind them. A box reads at SESSION at its run's
floor, which
every ref answer and every applied push raises, so no box request pays a barrier and none is shown less than it
already holds. `/box/git/` is published on `api.public` beside `/mcp/` and `/otlp/`, exempt from the operator guard.

The engine gives the box its git configuration as the LAUNCH's environment, minted once the store has named the job
(`GIT_CONFIG_COUNT`, appended after the operator's own entries): a reset of the helper list and a credential helper
scoped to `CREWLET_BOX_GIT_URL` that answers the run's token, the seat's commit identity, protocol v2, and index
version 4 with the untracked cache, which the box measurements adopted — which amends the sandbox's "the engine
contributes only tool-agnostic facts" rule for the engine's own surface, as the tool bridge already did;
configuration reserves the names it sets. A box is given its repositories by one recipe (`sandbox.CloneRecipe`):
blobless with an optional cone for an executor, depth-1 blobless at one commit for a check; a coding clone turns
git's file-system monitor on in its own configuration where the box's git and kernel can keep one, as the same
measurements adopted. The clone writes no hook: a commit's `Crewlet-Seat`, `Crewlet-Run` (the turn's id) and
`Crewlet-Work-Item` trailers are the executor's commit attribution (`sandbox.CommitAttribution`), added by the one
`prepare-commit-msg` hook `core.hooksPath` names for the run from its environment; nothing in the engine reads them,
and the push row is the provenance.

## Why the obvious alternative is wrong

A box on `/git/` as a person would be either a credential that is not a person's on the persons' transport or an
operator token in a box; a token that names the turn alone keeps admitting a job a relaunch superseded; a
linearizable read per box request would put every seat's fetch and push on the barrier budget the code log's census
counts persons for; and a floor raised only by pushes lets a box push, on top of what a node ahead showed it, to a
node behind, which does not hold the pack's bases. A box is generated code: a work-conserving share lets boxes hold
every transfer slot with client-paced clones and refuse a person's push `busy`, a name resolved before the grant is
a barrier per guess, a run row consulted without the seat's pause lets a seat a person stopped keep pushing, a stop
held to the box's git alone lets the run go on writing as the seat through its bridged tools, a checkout that
resolved its repositories for the seat alone would hand a box a repository the colleague or stranger who woke the
turn could not read, and a box held to the seat's grant alone would move, for a colleague who only reads a
repository, every branch the seat may — its `main`, its `release/*` — on the colleague's say-so. And a box granted
every repository the seat's chart reaches, rather than the ones its run checked out, would let generated code fetch
and push wherever the seat may, and would lose the per-run access, branch and base a checkout states; repositories
named once at the launch would make the agent that does the work decide nothing about what it needs.

## What this does not decide

Check boxes (a check's box: user name `check`, the same token domain and the `single-sha` recipe); a box's change
options; what a `run_in: direct` box can read beyond its grant — nothing here binds a box that runs as the engine's
own user, which the sandbox's security model already states; and a run's BRIDGED tool calls, which are
the seat's own surface (`api/mcpbridge`, `Session.Surface`) and are held to the seat's grant and to a person's stop
(`Session.Fence`, the seat's fence a stop closes), never to the run's `repos` — a run given a repository to read limits its box's git, not the
code tools the seat itself holds (stated in docs/concepts/code-sandbox.md beside the grants). Nor the TRANSPORT's confidentiality: `CREWLET_BOX_GIT_URL` may
be plain `http://`, as the bridge's and the telemetry receiver's URLs may, and the run's token and the code the box
fetches and pushes then cross in clear every network between the box and the host the URL names — a remote box's
always, a local box's whenever the URL names another node or a balancer; serving such a URL over https is the
operator's, as docs/concepts/code-sandbox.md's "Security model" says.

## Amendment: a box's pushes carry whom its run works for, and a credential's run pushes only its seat's own work

A box's push, and the change its run's collection opens, are the run's turn's writes made late. Since
crewlet#798's piece 4c-i the run's checkout composes whom that turn works for as the turn's own code tools state it
([ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)), and the row keeps it beside the
colleague's ceiling: one requester a job, a later checkout made for anybody else a conflict, and one made for the
same credential holding fewer apps narrowing it, never widening it. Every push the box makes, and the open, state
it.

A credential acting as no seat pushes to no repository itself, and its turn's own code writes were already held to
the seat's own work ([ADR-0110](0110-reaching-a-repository-takes-the-gitlet-permission.md)), but its executor was
not: the row named no colleague, the admission held the box to the seat's grant alone, and the open carried no
ceiling. The admission now holds such a box under the credential's own grant — every repository read, none pushed —
so it fetches what its run was given and pushes the seat's own work alone, the checkout prechecks the same, and the
open is asked under the ceiling of nobody. Whom the row names is held first to the rule every carrier is held to. A
row naming a stranger, or naming whom it works for in a shape no checkout writes (no kind at all, a credential naming
no app), has its box refused every request, the closed end; and one naming whom it works for by a kind this build
does not know, a newer build's run, is answered `503 carrier_unknown` rather than held to a bound this one could only
guess at: a node of a newer build judges it, and the harness's push again reaches one.

A run launched by a build before the carrier carries nobody, and a credential's box keeps the seat's bound alone,
until the run ends; a Gitlet node of such a build admits a newer run's box by its colleague's ceiling and ignores
the credential until it upgrades. Such a run's grants were taken for nobody, and nobody is one requester too, so a
run granted for nobody is never used for somebody, whichever build wrote its row: once a node of the newer build
carries it on in a turn that works for somebody, every checkout the run asks is refused as a conflict, of a
repository it holds already as much as of another, and the seat is told that a turn working for somebody starts a
new run. The refusal fails closed, since the run keeps what it was given and pushes it for nobody, and it is kept
rather than worked around: an arm adopting the first carrier a checkout names, or a mark of the build that wrote a
row, would be code for a build no tag shipped (ADR-0109's roll windows).
