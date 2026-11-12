# ADR-0131: GitHub is read through the agents' installations, and a workflow and an environment are pipelines

- **Status:** accepted
- **Authority:** `internal/github`
- **Enforced-by:** `internal/github.TestTheAtlasReadsRepositoriesWorkflowsAndEnvironments`, `internal/github.TestAFailingDeploymentIsTheHealthOfWhatItDeploys`, `internal/github.TestARepositoryGitletMirrorsIsOneEntry`, `internal/github.TestAPartNoInstallationMayReadIsAFindingAndRetiresNothing`, `internal/github.TestAnInstallationThatCannotBeReadCarriesWhatOnlyItCovered`, `internal/github.TestAnEnvironmentThatIsGoneRetiresNothingItDeployed`, `internal/github.TestNoCredentialShapeReachesTheAtlasFromGitHub`, `internal/github.TestAListingThatFailsOrIsRateLimitedFailsTheRead`, `internal/atlas.TestAFailingPipelineIsASignalOnWhatItDeploys`, `internal/atlas.TestAVisibilityIsAFactOfEachHost`
- **Cost-when-tried:** not tried. The two sources before this one each read under one company credential, so "whose access" had one answer; GitHub is the first vendor where the engine holds no company credential that can read, only an app per agent.
- **Tag-status:** unreleased

## The decision

**GitHub is read through the installations of the agents' own apps.** They are the only access to GitHub the engine holds that can read: the organization token registers a hook and nothing else. For each agent whose GitHub access is switched on, the read mints one token carrying Read-only of what that installation holds among Metadata, Actions and Deployments, whatever the agent's own tokens carry, and revokes it when the read ends, however it ends. A repository several installations cover is read as the first, in the order of the agents' handles, that holds the part's permission.

**The parts are per repository, and so are the findings.** Repositories take Metadata, which every app holds; workflows take Actions; deployments take Deployments. Where no installation covering a repository holds a part's permission, or GitHub refuses that listing, the part is unread for that repository, and ADR-0130 carries what an earlier read of it recorded. Which part made a claim is read off the claim's own key. GitHub's rate limit is never a refusal: it fails the read whole, which records nothing and is read again at the duty's retry.

**An installation the read cannot be made as carries what only it covered.** An agent's app that is not installed, is suspended, sits on another account or whose key does not sign is counted in one finding, and every claim about a repository this read did not list is carried, since which repositories that installation covers cannot be known. An agent switched off is not read through and is no finding: its installation is suspended on purpose, and it is access the company withdrew. With no agent's app switched on the source is not configured.

**A workflow and a deployment environment are pipelines, keyed after their repository.** A repository is `<host>/<owner>/<name>`, the key a Gitlet mirror's address reduces to, so the fold's exact-key merge makes a mirrored repository one entry. A workflow is the repository's key, `#workflow:` and its file's name; an environment is the repository's key, `#environment:` and its own name. A name is one key segment, exact both ways: a byte outside letters, digits and `. _ + -` is written as `~` and its two hexadecimal digits. Both carry `last_result`: a workflow's is its newest completed run on the default branch, an environment's the state of its newest deployment.

**A repository's visibility is a fact of each host it lives on.** A repository GitHub holds privately and Gitlet mirrors for the whole company has two visibilities, and both are true: they answer the question of two hosts, never one question twice. The vocabulary marks such a fact (`Spec.PerHost`), and the fold keeps one value for each member of a merged entry, shown with the member it is of (`FactView.Of`), so only the voices on one host are held against each other: a person and GitHub disagreeing about GitHub's visibility still send the entry to Review, and a mirror's two hosts never do. Each host takes its own closed words and no other's (GitHub's `public`, `private` and `internal`; Gitlet's `company` and `unit`), a claim in another host's word is refused, and a person's correction is written on the member they named. Gitlet's visibility follows the same rule as GitHub's, which this read now reports.

**What a deployment put its code behind is named, never claimed.** The newest deployment's environment address names an endpoint and its host name by their exact keys, each `deployed_by` the environment, which is why `deployed_by` runs from an endpoint and a host name as well as a service. Neither lives at GitHub, so by ADR-0130 an environment that is gone releases them and retires nothing.

**A failing pipeline is a signal on it and on what it deploys.** A pipeline whose `last_result` is `failed` or `errored` raises `pipeline_failing`, a warning, on itself and on every entry a `deployed_by` relation joins to it, computed at every read as a monitor's is. It holds for every pipeline, Gitlet's checks included, whose `last_result` is likewise the default branch's.

## Why the obvious alternative is wrong

**Reading with the organization token** asks a company to widen a credential minted for one hook into one that reads every repository, and leaves the atlas empty for every company that set none, which the per-agent apps made the ordinary case.

**Reading with an agent's own token** makes the read as wide as that agent: a token that may write contents is live for the read's length on a path that only ever reads.

**Skipping an installation that cannot be read** treats it as covering nothing. The repositories only it covers are then missed, and retired a day after somebody suspends an app.

**One visibility for the entry** makes every mirrored repository a standing disagreement: the two hosts' words are disjoint, so the fold would dispute each mirror for good, and a Review that is always raised is one nobody reads.

**Not reading GitHub's visibility** keeps the mirror quiet by hiding the one fact that says a repository is public.

**A second fact, one per host** (`forge_visibility` beside `visibility`) settles two hosts and no third: a repository mirrored to two forges has the same question again, and each fact needs its own words on screen and in every tool.

**A kind for a deployment environment** adds an entry kind, a lane and a key grammar for something the vocabulary already names: a pipeline is what builds or deploys code, and `deployed_by` already ends at one.

**Relating a deployed name to the repository** puts a failed staging deployment on the production name: only the environment knows which of a repository's deployments reached which address.

**A danger signal for a failed deployment** says something is broken now. A failed run or deployment leaves what ran before it in place; what needs attention is the pipeline, and a monitor says when the service is down.

## What this does not decide

Whether any other fact is one of each host: a default branch and an archived status are still facts of the entry, and two hosts that disagree on one are shown as disagreeing. Whether a delivery brings a repository's read forward, which the event-driven sources decide. Whether GitHub is read under an identity made for the atlas. How a refused part is shown, which the dashboard decides.
