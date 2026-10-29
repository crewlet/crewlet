# ADR-0056: A node-local derived file is declared, lives in its incarnation, and the cache writes none it has not declared

- **Status:** accepted
- **Authority:** `internal/code/cache`
- **Enforced-by:** `internal/code/cache.TestEveryDerivedFileIsDeclared`, `internal/code/cache.TestValidateRefusesAnUnsaidDeclaration`, `internal/code/cache.TestTheSweepKeepsExactlyTheDeclaredImage`, `internal/code/cache.TestADerivedFileFollowsItsDeclarationOnEveryEvent`, `internal/code/cache.TestTheCodeReadsTheDeclarations`, `internal/code/cache.TestEveryPackAddedReadsItsDeclaration`, `internal/code/cache.TestEveryHoldOfADerivedFileReadsItsDeclarationsCheck`, `internal/code/cache.TestADropStopsARunWritingWhatItDrops`, `internal/code/cache.TestAReportADeclarationKeepsItsFilesOnRecordsNoDrop`, `internal/code/cache.TestARequestsDamageNoDropRepairsIsRepairedAsAPacks`, `internal/code/cache.TestTheMaintainersStepsAreTheDeclaredWrites`, `internal/code/cache.TestNothingButSyncWritesACache` (its declared-writers half, held able to fail by its control), `internal/code/cache.TestTheConceptPageNamesEveryDerivedFile`, `internal/code/cache.TestTheStateIsWhatTheNextBootKnows`, `internal/code/cache.TestADerivedFileGoneFromUnderItsStateIsForgotten`, `internal/code/cache.TestValidateRefusesAnInProcessDeclarationThatClaimsAGitWrite`, `internal/code/cache.TestAMaintenanceRunKeepsTheProvenanceState`, `internal/code/cache.TestAFileUnderCrewletIsCountedInTheBudget`, `internal/code/cache.TestARebuildLinksTheIndex`, `internal/code/cache.TestARewoundLogDropsTheIndex`, `internal/code/cache.TestValidateHoldsAPartToWhoReadsTheFiles`, `internal/code/cache.TestAReportOnAChainedCopyMakesItsScrubDue`, `internal/code/cache.TestTheTipIsWrittenOverTheTailAlone`, `internal/code/cache.TestARetirementKeepsTheTipAndEveryOtherChangeTakesIt`, `internal/code/cache.TestAFailingTipIsDroppedAndWrittenAgain`, `internal/code/cache.TestAPackLeavingInPlaceTakesTheTip`
- **Measured:** at 1152ca322 the two derived files a cache writes — the multi-pack-index (a full round's without a bitmap, the maintainer's with its bitmap or without one) and the commit-graph — were spelled in eighteen of the cache's non-test files (`multi-pack-index`, `commit-graph` or `tmp_bitmap` 122 times): their file names in the sweep, Verify, the check, the scrub and the size measure; their argument lists, charges, lanes and deadlines beside each writer; what a removed pack, rewritten refs and a damage report do to them in the install, the refs' write, the sync and the reports; and the state's own struct. They agreed only because nothing had changed them apart, and M3 adds more kinds.
- **Cost-when-tried:** the sweep's image grammar and Verify's listing were made one predicate a directory because two copies of the grammar had disagreed about what a cache is, and the derived files were the part still spelled beside it — a file a writer leaves that the grammar does not admit is trashed under a running reader, and a file the grammar admits that no writer makes is a hole anyone may fill. And each directory's grammar admitted a file of the root, and one in `objects/info`, as anything but a directory: a symbolic link named `commit-graph`, or named `config` over a file holding the configuration the pool requires, was kept by the sweep and admitted by Verify, and git read the file it pointed at, outside the incarnation, its budget and what a rebuild replaces — the link at `config` passing the pool's shape check too, which reads through it.
- **Tag-status:** unreleased

## The decision

Every node-local file the cache derives from a repository's rows is a DECLARATION in `cache.DerivedFiles()`
(derived.go): what it is a function of, every write that makes it (trigger, lane, deadline, memory charge,
the file and git subcommand that run it, the argument list), the files it owns and the files a write holds
while it runs, how its bytes are checked, the part of a repository a damage report names it by, what it
promises a reader, and what each change to its inputs — a pack added, a pack removed, the refs rewritten,
its own files reported damaged — does to it. Its files live inside the repository's incarnation, so they are
counted by the budget, leave with the repository and are written again by a rebuild. The sweep's image,
Verify's listing, the one removal before a change (`dropFor`, which first stops a maintenance run writing what
it drops and also takes what it dropped out of what the incarnation records it holds), the check, the scrub
and Verify's hash (each by the declaration's check), the maintainer's plan and its refusals, the clearing
of what a failed write holds, and the state file — one entry a declaration, keyed by its name, whose bytes a
boot sums into what the cache holds — read the declarations, and the writers gate refuses a git writer in
the cache that no declaration names, and a declaration whose writer, file or argument list does not exist
where it says. A node-local derived file this process did not write is held to its trailer before it serves, and
every file a copy holds is scrubbed behind the serving. A damage report — git's word — does to a derived
file what its declaration says of one, and every reader of a report (a git process's, a request's, a
round's) asks only that: a file kept on a report is held to its check by the scrub instead. A file that
FAILS its check is dropped whatever its declaration says of a report, since the bytes decided. Open
refuses a build whose declarations leave anything unsaid. Every file of the image, a declared file or one of
the root's fixed files, is a regular file; anything else at its name is outside the image.

## Why the obvious alternative is wrong

The obvious way to add a derived file is beside the others: a pattern in the sweep, a case in Verify, a
field in the state, a charge in the plan, a drop at each change, a row in the gate — each correct on the day
it is written. That is the shape this replaces, and its failure is silent in both directions: a forgotten
pattern trashes a running reader's file, a forgotten drop leaves an index naming a pack that is gone, and a
forgotten writer row is the one place a writer of objects no row names could hide. With one declaration a
kind, adding a kind is one change the readers cannot miss, and a test holds the declarations to the files
real git writes.

## What this does not decide

Which derived files exist after this (later parts add their own, each by a declaration), what any one of
them costs or when it is due (the maintainer's and each part's own measurements), or anything outside one
node's cache — a derived object published for every node is a decision of its own.

## Amendment — a derived file the cache's own code writes

The index of which push introduced each commit is the first derived file no git process writes or reads:
the cache's own Go code builds it from the code domain's push rows. Its declaration's writes are IN-PROCESS
— no lane, deadline, memory charge an object or argument list, which nothing would read, but a sentence of
what each holds (`Write.Charge`) — and a declaration's writes are all of one kind, since who writes a file
says who reads it. No damage report can name a file git never reads, so an in-process declaration has no
part and keeps its files on a report; its check is its own (a run's SHA-256 trailer), held before anything
reads a file this process did not write, and by the scrub. Its files live under the incarnation's
`crewlet/` — the engine's root, never `objects/`, the namespace a push's scratch names as its one
alternate — and a boot's walk of a copy with no state reads every root a declaration owns, read from the
declarations. A declaration need not own a file whose presence is its own (`Owned.Main`): a set of runs is
present by its record, which a measure keeps whole. Two more answers join the changes a declaration states:
what a log gone back under the node does to it (`OnHistoryGone` — the index's positions then name other
records, so it is dropped before anything reads the copy), and what a rebuild does with its files (`Seed`:
written again over the new packs, or LINKED into the new incarnation with the record and the vouch of each
file, for a file that is a function of the rows a rebuild reads again unchanged). The writers gate ties an
in-process write to its file's filesystem row, as a git write is tied to its subcommand's.

## Amendment — a derived file the cache installs

A cache also holds derived files no git process on its node wrote: the multi-pack-index and commit-graph
LAYERS of a repository's published chain, each fetched from the object store, held to the row that names it
and placed byte for byte, and the two chain files the cache writes over them from the rows' order (`layers`
and `graph-layers`, `layers.go`). Their one write, the install, is in-process, yet every git process reads
their files, so the rule above is restated by who READS a declaration's files: a declaration has no part
only where every write is in-process AND no git process reads its files — then it keeps them on a report —
and otherwise has the part a damage report names its files by, whoever writes them. A layer is valid only
over its exact base, so these declarations answer a pack leaving, the chain moving and a failed check alike
with a CUT (`Prefix`): the longest prefix of the chain that the change leaves valid stays, through the one
cut both chains share (`prefixOf`), and every layer above it goes — a layer whose file failed fetched again
rather than written, since no node writes a layer of its own. A report keeps them and makes the scrub due,
which holds each file to its check. Whether and how a derived object is published for every node is not this
record's (What this does not decide); this says only how a cache declares and keeps what it installs.

## Amendment — a declaration git reads may be written by both kinds of writer

Over an installed chain each node writes one file of its own: its TIP, git's split commit-graph layer of the
commits the installed graph layers lack, written by the maintainer in its lane over exactly those layers.
It is a `graph-<checksum>.graph` beside the installed graph layers, under the same chain file in the same
directory, which ownership by name cannot divide, so the tip is a second write of `graph-layers` — a git one
beside the install. The first amendment's "a declaration's writes are all of one kind" is restated: EACH
WRITE is of one kind and said whole for that kind (an in-process write's sentence of what it holds; a git
write's lane, deadline, charge an object, argument list and the files it holds while it runs), and a
declaration may have writes of both kinds where git reads its files. Git writing any of a declaration's files
holds the whole declaration to what a git-written one owes — one main file whose presence is its own, a part
a damage report names it by, no directory owned whole — so no git process writes a file no git process
reads. The tip is a function of the packs and the refs over its exact base, so it goes with every cut,
every placement and every pack removed, and crosses a retirement that keeps the whole chain beneath it,
since a merge drops no commit; one that fails its check is dropped and written again, never fetched, since
no row names it.
