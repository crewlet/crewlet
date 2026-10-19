# ADR-0038 — A node-local derived file is declared, lives in its incarnation, and the cache writes none it has not declared

- **Status:** accepted
- **Authority:** `internal/code/cache`
- **Enforced-by:** `internal/code/cache.TestEveryDerivedFileIsDeclared`, `internal/code/cache.TestValidateRefusesAnUnsaidDeclaration`, `internal/code/cache.TestTheSweepKeepsExactlyTheDeclaredImage`, `internal/code/cache.TestADerivedFileFollowsItsDeclarationOnEveryEvent`, `internal/code/cache.TestTheCodeReadsTheDeclarations`, `internal/code/cache.TestEveryPackAddedReadsItsDeclaration`, `internal/code/cache.TestEveryHoldOfADerivedFileReadsItsDeclarationsCheck`, `internal/code/cache.TestADropStopsARunWritingWhatItDrops`, `internal/code/cache.TestAReportADeclarationKeepsItsFilesOnRecordsNoDrop`, `internal/code/cache.TestARequestsDamageNoDropRepairsIsRepairedAsAPacks`, `internal/code/cache.TestTheMaintainersStepsAreTheDeclaredWrites`, `internal/code/cache.TestNothingButSyncWritesACache` (its declared-writers half, held able to fail by its control), `internal/code/cache.TestTheConceptPageNamesEveryDerivedFile`
- **Measured:** at 1152ca322 the two derived files a cache writes — the multi-pack-index (a full round's without a bitmap, the maintainer's with its bitmap or without one) and the commit-graph — were spelled in eighteen of the cache's non-test files (`multi-pack-index`, `commit-graph` or `tmp_bitmap` 122 times): their file names in the sweep, Verify, the check, the scrub and the size measure; their argument lists, charges, lanes and deadlines beside each writer; what a removed pack, rewritten refs and a damage report do to them in the install, the refs' write, the sync and the reports; and the state's own struct. They agreed only because nothing had changed them apart, and M3 adds more kinds.
- **Cost-when-tried:** the sweep's image grammar and Verify's listing were made one predicate a directory because two copies of the grammar had disagreed about what a cache is, and the derived files were the part still spelled beside it — a file a writer leaves that the grammar does not admit is trashed under a running reader, and a file the grammar admits that no writer makes is a hole anyone may fill.
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
and Verify's hash (each by the declaration's check), the maintainer's plan and its refusals, and the clearing
of what a failed write holds read the declarations, and the writers gate refuses a git writer in the cache
that no declaration names, and a declaration whose writer, file or argument list does not exist where it
says. A node-local derived file this process did not write is held to its trailer before it serves, and
every file a copy holds is scrubbed behind the serving. A damage report — git's word — does to a derived
file what its declaration says of one, and every reader of a report (a git process's, a request's, a
round's) asks only that: a file kept on a report is held to its check by the scrub instead. A file that
FAILS its check is dropped whatever its declaration says of a report, since the bytes decided. Open
refuses a build whose declarations leave anything unsaid.

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
them costs or when it is due (the maintainer's and each part's own measurements), what kind of directory
entry the image admits at a declared name, or anything outside one node's cache — a derived object published
for every node is a decision of its own.
