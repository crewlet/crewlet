# ADR-0080: A git node runs git 2.55 or newer from one pin, and a client needs 2.40

- **Status:** accepted
- **Authority:** `internal/code/gitproc`
- **Enforced-by:** `internal/code/gitproc.TestAGitNodeRefusesAGitBelowItsFloor`, `internal/code/gitproc.TestErrTooOldNamesItsFloor`, `internal/code/gitproc.TestThePinsMeetTheFloors`, `internal/code/gitproc.TestTheNodeFloorWritesALayerOverANamedBase`, `internal/code/gitproc.TestEveryPackReuseModeSendsOverlappingPacksWhole`, `internal/code/gitproc.TestTheFormatTableIsWhatGitSays`, `internal/code/gitproc.TestARequestCannotNameTheDamage`, `internal/code/gitproc.TestEveryNodeGitClaimRunsOnBothEnds`, `internal/code/gitproc/gittest.TestTheFloorJudgementIsTheFloors`, `internal/code/host.TestTheGitFloorsPassTheSuite`, `internal/engine.TestAGitNodeBootsOnlyAtItsFloor`, `cmd/crewlet.TestTheCLIRefusesAGitBelowTheClientFloor`, and the image's own build step comparing its git with the pin
- **Measured:** a whole multi-pack-index and bitmap rewrite costs 16, 21 and 30 s a gigabyte at 100 MB, 1 GB and 5 GB of a source repository's history and about 1 000 bytes an object of memory (internal/code/cache's BenchmarkMidx, git 2.43) — so a 16 GiB, four-core node is refused the bitmap of a repository past some 7.7 million objects, and git 2.50's incremental layers with bitmaps and 2.55's layer written over a named base without a chain file (`multi-pack-index write --incremental --base --no-write-chain-file`) are what let one maintainer publish a layer every git node installs, rather than every node rewriting its whole index. The suite passes on 2.56.0 built by the one recipe, its floors' arm with every pool on 2.55.0 and every person's git on 2.40.4, and gitproc's claims about what git says and does in a pool — the format table, what a request can reach, what the pool's environment keeps from git — on 2.56.0 and on 2.55.0 alike (2026-10-19). The recipe builds 2.56.0 in about 2 minutes on four cores; in the image, the linux/arm64 git compiled under QEMU took 812 s against 87 s cross-compiled, and the release image, both platforms in one build, takes about a minute longer than with Debian's git (187–191 s against 246–248 s on one four-vCPU machine; `RELEASING.md`).
- **Cost-when-tried:** one floor for the whole design (2.40) left the code host on the git a distribution ships: Debian trixie's 2.47.3, whose multi-pack reuse sent a pack every client refused on a full clone of any repository the maintainer had indexed (the reuse mode was chosen per binary to work around it, a branch the floor removed: every git node now serves with `pack.allowPackReuse=single`, which sends a cache's clones fewer bytes for less CPU than `multi` does, `internal/code/cache`'s `BenchmarkPackReuse`), and below the git every incremental layer needs. A floor raised later raises it under deployed nodes. And a floor measured only through the pin was no measurement: the format table kept rows and expectations for 2.40 to 2.47 (the required-chunk reports 2.43 reworded, a segmentation fault where 2.47 names a multi-pack-index position, a request format whose trailing newline 2.47 dropped) that no pool could meet any more, while its expectations for 2.55 itself were wrong in three arms — the maintainer's write over a damaged reverse index, which 2.55 survives as 2.56 does, and two reads through a damaged delta, which name their packed object on 2.55 — because nothing ran it there.
- **Tag-status:** unreleased

## The decision

There are two floors: `gitproc.NodeMinVersion` (2.55.0), checked wherever a pool is built — every git a git node
runs — and `gitproc.ClientMinVersion` (2.40.0) for a git that talks to a git node — a person's, a CI runner's, the
operator CLI's, the suite's client, a box's — checked where the engine meets one (the operator CLI before it runs its
git, `gittest.Require`). A git below its floor is refused by name (`*gitproc.ErrTooOld`, naming the floor). The git a git node runs in the engine image is BUILT from one pin,
`scripts/git-pin.env` — the newest release by version and SHA-256 — by one recipe, `scripts/git.sh`, which the image,
CI and a contributor's machine all run; the suite runs that pin, never the machine's git, gitproc's claims about what
git says and does in a pool run on the node floor's own build beside it, and one arm of the code host's suite runs the
node floor's build against the client floor's, so each floor is a measured claim. A weekly
check holds the pin to upstream, verifying each release's signature, because no Dependabot ecosystem watches a
tarball.

## Why the obvious alternative is wrong

The obvious git is the distribution's package. trixie ships 2.47 and its backports none, so the node's git would be
whatever the base image happens to carry — below the floor today, with security fixes arriving on the distribution's
schedule and features never. And the obvious floor is one number: but a client's git is a person's laptop or a CI
image the operator does not control, while a git node's is the engine's own; holding the client to 2.55 would refuse
every person on a current Debian or Ubuntu LTS, and holding the node to 2.40 would forbid the incremental layers the
cache is to be built on.

## What this does not decide

Which of 2.55's features the cache uses and when (each with a measurement of its own), where a coding box's own git is
checked against the client floor (a box's git is a client's, and the check belongs to whatever launch hands a box the
code host), or a data-less git node.
