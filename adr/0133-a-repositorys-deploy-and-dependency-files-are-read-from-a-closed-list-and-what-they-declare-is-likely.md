# ADR-0133: A repository's deploy and dependency files are read from a closed list, and what they declare is likely

- **Status:** accepted
- **Authority:** `internal/atlas/codescan`
- **Enforced-by:** `internal/atlas/codescan.TestAnEnvironmentValueIsNeverRead`, `internal/atlas/codescan.TestNoParserDeclaresAFieldForAnEnvironmentValue`, `internal/atlas/codescan.TestTheFieldGateCatchesABrokenParser`, `internal/atlas/codescan.TestNoCredentialShapeReachesTheAtlasFromAFile`, `internal/atlas/codescan.TestEachFileDeclaresWhatItNames`, `internal/atlas/codescan.TestARepositoryIsReadWhenItsDefaultBranchMovesAndOnlyWhatChanged`, `internal/atlas/codescan.TestAFileOffTheClosedListIsNeverRead`, `internal/atlas/codescan.TestAPassReadsABoundedNumberOfFilesAndPublishesAWholeCommit`, `internal/atlas/codescan.TestWhatTheScanRefusesIsCountedOnTheStanding`, `internal/atlas/codescan.TestAUnitOnlyRepositoryIsNeverScannedAndAnUnscannedOneIsWithdrawn`, `internal/deployfiles.TestTheListAdmitsOnlyWhatItNames`, `internal/deployfiles.TestAFileThatHoldsSecretsIsNeverListed`, `internal/gitlet/query.TestACommitsDeployFilesAreListedByTheClosedListAndOnlyThoseAreRead`, `internal/gitlet.TestTheDefaultTipsListingNamesEveryActiveProjectRepositoryAndItsTip`, `internal/atlas.TestWhatCodeDeclaresIsLikelyUntilAPersonConfirmsIt`, `internal/atlas.TestAFileNeverContestsWhatAnIntegrationRead`, `internal/atlas.TestAFileNeverSetsMeaningOrDeniesAnything`, `internal/atlasdomain.TestARepositorysFilesAreReadInTheirOwnVoice`
- **Cost-when-tried:** not tried. No build read a company repository's file for the atlas; the nearest reads were the checks file, one fixed path, and an agent's own `code_read`, which is held to the agent's grant and recorded as its read.
- **Tag-status:** unreleased

## The decision

**The engine reads a closed list of files from a Gitlet repository, and nothing else.** The list is `internal/deployfiles`, decided by a file's path alone: a root Dockerfile and Procfile (listed by their presence and never read), compose files, Kubernetes manifests under `k8s`, `kubernetes`, `manifests` or `deploy`, a Helm chart's `Chart.yaml`, Terraform and OpenTofu files, `fly.toml`, a root `render.yaml`, and six dependency files. A dot file, a values file, a variable or state file, a manifest named for secrets, a dot directory and a vendored, test or example tree are refused by path. Gitlet holds the same list: its `code_files` read, the engine's own, lists only regular files on it and refuses a read of any other path before git runs.

**A parser declares a field for what it reads, and for nothing else.** Each format is decoded into closed types. An environment block, a command, a build argument, a label, a Secret's data, a dependency's version or address have no field, so they are never decoded; a reflective gate fails on a field for one, on a map of strings and on any type that takes whatever a file holds. What is decoded reaches a claim only through `collect.Builder`, which refuses a key or a value shaped like a credential and counts it. A name means something only where a closed table says so.

**What a file declares is likely, in the repository's own voice.** A new source kind, `code`, named by the repository, derives the confidence `inferred`: shown as likely, never confirmed, and never hidden, since a file backs it. It outvotes nothing that was seen, sets no fact of meaning and no owner, and denies nothing: a read that no longer finds a claim withdraws it at once (`collect.Replace`), because a file at one commit is read exactly, where a vendor's listing takes two misses a day apart. A person's confirm makes a claim theirs.

**Relations are by exact keys only**: built from this repository where a file says it is built here, built from a forge repository where an image sits in that forge's registry, depends on where a compose file or a chart names the dependency, runs on a platform's account where the platform's own file names the application.

**A repository is read when its default branch moves, and only what changed.** The scan is a worker of the repository's home loop: a record on the repository's subject asks for it, and the home's backstop lists every repository whose tip (`default_tips`, Gitlet's listing) is not the one its standing says was read whole. A pass lists the tip's files in one read, reads only files whose blob it holds no reading of, at most 50 a pass, and publishes once every listed file is read, so the atlas never holds half a commit. The bounds are 64 KiB a file, 200 listed files a repository and 128 claims a file; what falls past one is counted on the standing.

**A repository only its unit may read is never scanned.** What a scan records is on the atlas log, whose history, activity and entries every holder of the atlas reads; a filter at one read would leave the others. A scratch, an archived and an importing repository are not scanned either, and a repository that stops being scanned, or is renamed, has what its files said withdrawn.

**The record is the atlas log's `read`, at record version 4.** A read whose source is `code` is written by `atlasdomain.Writer.Read`, as an integration's is, by the engine's own actor. Each repository's standing is a record in the coordination store, in a bucket of its own; each blob's reading is the home node's memory.

## Why the obvious alternative is wrong

**Reading every file and redacting what looks like a secret** trusts a pattern to know every secret, and an environment value is a secret that looks like any word. A closed list and closed types read nothing they were not written to read, and the gate on the types can fail.

**Recording a file's claims as confirmed, or as an integration's**, calls intent running state: a compose file on a branch nobody deployed would put a database on the map as fact. As an agent's unbacked report they would be hidden, though a file anybody can open backs them.

**Reading on the collect duty's hourly clock** reads every file of every repository every hour for nothing, and shows a push an hour late. The home loop already hears every push and already knows which node holds the repository.

**Filtering a unit-only repository's claims at read time** needs every reader of the log to apply the filter, and the first that does not publishes what only a unit may read.

**Retiring after two misses**, as a vendor's listing does, keeps a service on the map for a day after the commit that deleted it, for a flake a file at a commit cannot have.

## What this does not decide

Repositories on GitHub or GitLab that Gitlet does not mirror, which need a read through each vendor's API. How a suggestion is confirmed or rejected in bulk on the dashboard. A setting to turn the scan off, which nothing has asked for: a repository's files are read by the engine that hosts them, under no credential and at no vendor.
