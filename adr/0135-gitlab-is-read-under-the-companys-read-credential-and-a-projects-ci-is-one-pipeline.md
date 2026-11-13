# ADR-0135: GitLab is read under the company's read credential, and a project's CI is one pipeline

- **Status:** accepted
- **Authority:** `internal/gitlab`
- **Enforced-by:** `internal/gitlab.TestTheAtlasReadsProjectsPipelinesAndEnvironments`, `internal/gitlab.TestTheReadCredentialIsUsedBeforeTheProvisioningToken`, `internal/gitlab.TestAFailingGitLabDeploymentIsTheHealthOfWhatItDeploys`, `internal/gitlab.TestAProjectGitletMirrorsIsOneEntry`, `internal/gitlab.TestAPartGitLabRefusesIsAFindingAndRetiresNothing`, `internal/gitlab.TestAGroupOrANamedProjectTheCredentialCannotSeeCarriesWhatWasRead`, `internal/gitlab.TestAnEnvironmentThatIsStoppedRetiresNothingItServed`, `internal/gitlab.TestNoCredentialShapeReachesTheAtlasFromGitLab`, `internal/gitlab.TestAListingThatFailsOrIsRateLimitedFailsTheGitLabRead`, `internal/gitlab.TestTheGitLabListingsAreWalkedToTheirBounds`, `internal/atlas/derive.TestANamedRepositoryIsKeyedByItsForgesAddress`, `internal/atlas.TestARepositoryIsKnownByOneKeyWhereverItIsNamed`
- **Cost-when-tried:** not tried. GitHub (ADR-0131) is read through an app per agent because the engine holds no company credential there that can read. GitLab is the first forge where it holds two, one made for reading and one that owns the group, so "which credential" has an answer to choose.
- **Tag-status:** unreleased

## The decision

**GitLab is read under the read credential, and under the provisioning token only where the company gave no read credential.** `integrations.gitlab.token` exists to read: it is the credential the engine already looks up a thread's participants with. Where it resolves, the atlas reads with it and with nothing else. Where it does not, the read is made under `integrations.gitlab.provisioning.admin_token`, which the reconcile loop already reads the group with on every pass. Every call is a `GET` under either. One read uses one credential: a part the read credential is refused is never retried under the wider one.

**The group is read whole, and it proves the credential.** The read resolves `provisioning.group` first. A `401` or a `403` there is a rejected credential and fails the read; a group GitLab does not show is a finding that carries everything an earlier read recorded. The group's projects, its subgroups' included, are then listed to exhaustion, and each project the settings name beside them is read by its path. A group past a thousand projects is a finding and is not read at all: half a listing would retire the other half.

**The parts are per project, and so are the findings.** A project is a repository, with its default branch, whether it is archived and its visibility on GitLab. Its pipeline and its environments are a part each, which take the Reporter role where the project is private and which a credential that lists the project may lack. A `403`, or the `404` GitLab answers for what a credential cannot see, leaves that part unread for that project, and ADR-0130 carries what an earlier read of it recorded. Which part made a claim is read off the claim's own key. A `401` after the group answered, GitLab's rate limit and every other failure fail the read whole, which records nothing and is read again at the duty's retry. A project that is archived, is empty or has the feature switched off is not asked.

**A project's CI is one pipeline, and an environment is another.** A GitLab project has one CI configuration, so it has one pipeline, keyed the repository's key and `#pipeline`, claimed once a pipeline has finished on the default branch, with that pipeline's result as its `last_result`. An environment that is available is a pipeline keyed the repository's key, `#environment:` and its name as one key segment (ADR-0131's escape, now `collect.KeySegment`), with the state of its newest deployment, the environment GitLab's own deployment tier says it is, and the address and the host name it serves, each `deployed_by` it and named, never claimed. An environment that is stopped is not read, which is what every review environment becomes when its branch merges. Of a project's available environments the twenty nearest production are read, by tier and then by name, so the bound keeps the same environments at every read; the rest are counted.

**A repository's key is what its forge's address reduces to.** A project is keyed by `atlas.ForgeRepositoryKey`: the instance's address and the project's path, reduced as a mirror's source address is. The settings key the repositories they name the same way. A GitLab served under a path of its host (`https://git.acme.example/gitlab`) therefore names one entry from the settings, from the read and from a Gitlet mirror of the project.

## Why the obvious alternative is wrong

**Reading only with the read credential** leaves the atlas without GitLab for every company that connected it from the dashboard and never created the optional token, which is the ordinary case.

**Reading always with the provisioning token** puts a group Owner's credential on a path that only reads, in a company that gave the engine a narrower one for exactly that.

**Retrying a refused part under the provisioning token** makes what the atlas shows depend on which credential happened to answer, and hides the finding that says the read credential's owner lacks a role.

**A pipeline per CI job, or per pipeline run** makes entries of things that come and go with every commit. What a project has, and what its default branch's badge shows, is one pipeline and its last result.

**Claiming the pipeline for every project whose CI is switched on** shows a pipeline that has never run for every project that keeps the default setting and has no CI configuration. Whether a configuration exists is a file read, and what a finished pipeline proves needs none.

**Listing environments through the project's deployments**, as GitHub's read must, loses production behind three hundred review deployments. GitLab lists environments themselves, and each one's newest deployment is exact.

**Reading an environment's name for what it is for** guesses what GitLab already states: its tier is set by the project or derived by GitLab from the name, in a closed list.

**Keying a project by the instance's host and its path** names a second entry for every mirrored project of an instance served under a path, since the address the mirror pulls from carries that path.

## What this does not decide

Whether GitLab is read under an identity made for the atlas. Whether a delivery brings a project's read forward, which the event-driven sources decide. Whether a project's deploy and dependency files are read through the API where Gitlet does not mirror it. How a refused part is shown, which the dashboard decides.
