# ADR-0136: A forge repository Gitlet does not mirror has its files read through the forge

- **Status:** accepted
- **Authority:** `internal/atlas/codescan`
- **Enforced-by:** `internal/atlas/codescan.TestNothingOffTheClosedListIsEverAskedOfAForge`, `internal/atlas/codescan.TestAForgeRepositoryIsReadWhenItsDefaultBranchMovesAndOnlyWhatChanged`, `internal/atlas/codescan.TestAPassOverAForgeIsBoundedInFilesBytesAndTrees`, `internal/atlas/codescan.TestARepositoryGitletMirrorsIsNotReadThroughItsForge`, `internal/atlas/codescan.TestARepositoryTheListingDoesNotHoldIsReleasedOnlyWhereTheListingIsWhole`, `internal/atlas/codescan.TestWhatAForgeAnswersIsHeldToWhatItListed`, `internal/atlas/codescan.TestARefusedRepositoryIsLeftAsItWasAndAFailingForgeStopsThePass`, `internal/atlas/codescan.TestTheTwoScansKeepTheirStandingsApart`, `internal/atlas/codescan.TestAForgeRepositorysVoiceIsItsKey`, `internal/atlas/collect.TestAForgesRepositoriesAreReadAsAVisitOfTheirOwn`, `internal/github.TestTheFilesOfAGitHubRepositoryAreReadByTheClosedList`, `internal/github.TestWhatGitHubRefusesOrCutsShortIsNeverReadAsEmpty`, `internal/gitlab.TestTheFilesOfAGitLabProjectAreReadByTheClosedList`, `internal/gitlab.TestAGitLabTreeIsWalkedToItsEndOrNotAtAll`, `internal/gitlab.TestAGitLabProjectThatRefusesItsCodeIsLeftAsItWas`
- **Cost-when-tried:** not tried. ADR-0133 reads a repository's files where Gitlet holds them, so a company whose code lives on GitHub or GitLab and is not mirrored had no file read at all.
- **Tag-status:** unreleased

## The decision

**One reader reads every repository's files, whoever holds the repository.** ADR-0133's closed list, its parsers and its rule that half a commit is never published are one function (`codescan`'s `reader`), which Gitlet's scan and a forge's pass both read through. A forge lists a tree whole and serves the object it is asked for (`collect.CodeHost`, `collect.CodeSession`), and decides nothing: which file is listed, which is read, which is only counted and what a file declares are the reader's. The list is held twice on a forge's path, where a tree's files are chosen and again at the one place a forge is asked for a file's bytes, so no caller can ask a forge for a file off the list or for one listed by its presence.

**Only a repository Gitlet does not mirror is read through its forge.** A repository a Gitlet repository pulls from is read by Gitlet's own scan, under the mirror's name and the mirror's visibility, and is skipped here by the exact key its address reduces to. One that gains a mirror has what this reader said of it withdrawn at once. A pass that cannot learn Gitlet's mirrors reads nothing. Every other repository the forge lists that is neither archived nor empty is read, private ones included: who may read a forge's repository is the forge's to say, and the atlas already shows every holder that repository and its pipelines.

**It is read under the forge's own atlas access, narrowed to reading code.** GitHub is read as the agents' installations that hold Contents, under a token minted for the pass with Read-only of Metadata and Contents and revoked when the pass ends; an installation without Contents serves no file. GitLab is read under ADR-0135's one credential. Every request is a `GET` beside the token's mint and revoke.

**A repository is read when its default branch moves, found by asking.** No forge event is subscribed for it. A pass asks each repository where its default branch is, one request, and reads only where that commit is not the one last read whole; GitHub's listing says when anything was last pushed, so a repository nothing was pushed to is not asked at all. A moved branch costs one listing of its tree and one read per listed file whose object changed. What a forge answers is held to what it listed: a blob's bytes must hash to the id the tree gave it.

**A pass is a visit of the collect duty, bounded, with its own standing.** It runs after the forge's own read, under the same lease, and comes due on its own: at the company's refresh interval, or five minutes after a pass that left work or a fault behind. One pass reads at most 50 files and starts none once it has read a mebibyte, and lists at most 100 trees; the rest wait for the next pass, and a repository is recorded only once every listed file of its tip is read. Each pass records the requests it made, the trees it listed and the files and bytes it read, on the forge and on each repository.

**What a forge does not say is never read as empty.** A repository the forge refuses the code of is left as it was. A repository missing from the listing is released only where the listing is whole: a suspended installation, an installation without Contents, a group or a named project the credential cannot see each make it partial. A tree the forge does not list whole (GitHub's truncated answer, a GitLab tree past ten thousand entries) has none of its files read, and what an earlier tip declared is withdrawn, since a part of a tree would withdraw the rest's claims file by file. One repository's fault does not stop the others; three end the pass.

**A forge repository's voice is its key.** Its claims are made as `atlas.CodeSource` of its key with `:` for every `/` (`github.com:acme:api`), since a source's name takes no slash: exact both ways, and never a Gitlet repository's name. Its standings sit in the scan's own bucket of the coordination store under `forge/`, which Gitlet's reader of that bucket skips.

## Why the obvious alternative is wrong

**A second parser, or a second list, for the forges** is two answers to "is this file read". The one that is edited less is the one that reads an `.env` file.

**Letting each vendor choose the files** puts the never-read rules in two clients of two APIs, each a place to forget one.

**Reading a mirrored repository through its forge as well** says everything twice, and says it in a second voice for a mirror the company keeps to one unit.

**Reading with an agent's own token** makes the read as wide as that agent, and a token that may push is live for the pass's length on a path that only reads.

**Subscribing to push events** turns on a delivery for every push to every branch of every repository to learn what one request an hour per repository answers, and GitHub's listing answers for free.

**A scan of its own duty and its own loop** is a second lease, a second clock and a second posture gate for work that needs the forge's credential, the forge's cadence and the forge's listing.

**Reading a truncated tree, or the pages a walk reached**, lists some manifests and not others. The ones not listed read as deleted, and what they declared is withdrawn while they sit unchanged in the repository.

**Skipping private repositories** leaves the atlas without the code of most companies, to protect a file read that already keeps to names: no value under `environment`, no command and no secret has a field to be read into.

## What this does not decide

Whether a forge's push event brings a repository's pass forward. Whether a repository's standing is shown, which the dashboard decides. Whether a repository Gitlet mirrors only for its unit should be read at all, which ADR-0133 answers for Gitlet and this record does not reopen.
