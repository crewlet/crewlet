# ADR-0058: A seat's GitHub App token is issued at use, and only that seat's reads are answered with it

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestASeatsAppTokenIsAnsweredOnlyByItsOwnResolution`, `internal/engine.TestADisabledGitHubIssuesNoToken`, `internal/engine.TestAServerNamingAnUninstalledAppsTokenWaitsForTheInstall`, `internal/engine.TestALongRunsTokenFileIsKeptCurrent`, `internal/engine.TestAJobCarriedOnInItsKeptBoxStartsOnACurrentTokenFile`, `internal/github.TestAnIssuedTokenIsScopedToThePermissionsAndRepositoriesItWasMintedFor`, `internal/github.TestAnIssuerHandsOutTheHeldTokenUntilItRunsShort`, `internal/github.TestARefusedMintIsHeldForItsWaitAndAskedAgainAfter`, `internal/engine.TestARefusedMintIsAskedOfGitHubOnceAWaitByEveryPoll`
- **Cost-when-tried:** the agents' own apps were created, installed and scoped to a tier, and then used for one thing: reading who had commented on a thread. Every push, pull request and review an agent made went through a personal access token in `role.sandbox.env`, so a company needed one GitHub account per agent, the tiers bounded nothing an agent wrote, and two seats sharing one account were routed as one.
- **Tag-status:** unreleased

## The decision

**An agent seat's GitHub App installation token is issued by the engine when
a value naming it is read, and never sealed.** Configuration names it as
`${GITHUB_APP_TOKEN_<HANDLE>}` (`github.SeatVar`), wherever a seat's own
credentials are read: its tool servers' `mcp_env`, its coding box's
environment and setup files, and the servers rendered into that box. The
process-wide `github.Issuer` mints it from the app's sealed key, narrowed to
the seat's permissions and repositories, and holds it in memory for as long as it has
life left for the reader asking. A mint GitHub refuses is held too, for the
wait of the restart schedule the seat's tool servers retry it on, and every
reader of that installation meets the held answer until the wait ends: a
refusal that lasts until a person acts at GitHub costs one request a wait,
however many tool servers, launches, running boxes and deliveries read the
token.

**Only the seat's own resolution answers it.** Each of those reads goes
through `Engine.seatResolver`, which lays one source for that seat in front of
the company's chain. Another seat naming the variable, and every shared server,
reads a chain in which it does not exist.

**What holds a token is renewed before the token runs short.** A seat's tool
servers are passed again at `github.RenewAt` and restarted with a fresh token.
A coding run's environment is issued a token good for `github.RunTokenLife`,
and the completion poll rewrites every setup file whose whole body names the
token whenever the token behind it is renewed. So does the launch of a job
that continues in its turn's kept box, before the job starts: that box runs no
setup, and no poll reached it while it was paused. So a credential helper
reading that file never reads an expired one.

**Nothing is issued while `integrations.github` is off.** Switching it off is
how an operator stops every token the engine hands out.

## Why the obvious alternative is wrong

The obvious shape is the one every other integration uses: a provisioning
pass that seals each seat's credential into the secret store under the
variable the seat points at. For an installation token that is a live
credential written to every node and every backup, rewritten every forty
minutes per seat, each write re-activating the company to spread a value any
node holding the key can mint for itself. And it still would not reach a
running box, whose environment was fixed when it started.

The second obvious shape is a company-wide source answering every seat's
variable. It is shorter, and it lets any seat's configuration name another
agent's identity and act as it. The seat's own resolution makes that
impossible to write rather than something a validation rule has to catch.

## What this does not decide

How the seat's app is created, installed and recorded, which is the setup
flow's (`internal/api/setupapi`) and the reconcile's (`internal/github`). How
GitHub's events reach the engine, which this changes only by refusing to
register a hook at an address GitHub cannot reach. And whether an in-box tool
server that outlives `github.RunTokenLife` is restarted: it is not, and that
limit is documented where the in-box servers are.
