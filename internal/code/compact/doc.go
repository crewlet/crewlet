// Package compact keeps a repository's pack count bounded: the HOME git node's
// compaction loop (ADR-0060).
//
// Every push stores one pack, so a busy repository's pack count grows with
// its pushes, and every cold sync then pays a GET a pack and every request a
// lookup a pack. On every [Poll], a git node lists the repositories holding
// code.CompactMinMerge live packs or more, most first (the code domain's
// `compaction_due` read), and for each it is the HOME of — the first git node
// of the repository's rendezvous order over the live git nodes, the node its
// reads are asked of too (ADR-0059) — merges the small packs geometrically,
// git's own `repack --geometric=2` split over the packs that are not FULL,
// into one or a few pack bundles in the stored format, stores each as one
// object, and proposes ONE compaction to the repository's committer, which
// decides it in one snapshot and retires the inputs into code_packs_retired
// for code.RetireGrace. Every git node's cache drops a retired pack by an
// incarnation swap over hard links, so a reader holding the old directory
// keeps every file it may open (internal/code/cache's swap.go).
//
// # A merge makes no reachability judgement
//
// The build is ONE `pack-objects --stdin-packs --max-pack-size` over the
// inputs with NO pack excluded, in a scratch repository whose one alternate is
// a store holding EXACTLY THE INPUTS — hard links of the packs of the
// incarnation the plan was made over (internal/code/cache's NewMergeScratch)
// — so the inputs are read through it, and the build writes nothing but the
// scratch. Every object of the inputs is in the outputs, an object an input
// shares with a pack that is no input included: two compactions decided from
// different snapshots may each retire the pack the other relied on, and both
// land. So no push decided against the inputs can land a ref whose closure a
// compaction removed, and nothing fences a compaction against a push: what
// drops objects is an expunge, never this (ADR-0060).
//
// NOTHING BUT THE INPUTS IN REACH, because `--stdin-packs` fills in the name
// hash its delta search sorts by with a walk from every commit its inputs hold
// over every tree and blob it can reach, which only an excluded pack stops:
// over the whole incarnation a roll of a few small packs walked the
// repository's history, the build's cost and memory following the history
// while its charge and its deadline follow the roll, so a large repository's
// every roll was killed at its deadline and its pushes refused
// compaction_behind. Over the store the walk ends at the inputs' edge — git
// walks with ignore_missing_links — and the build writes the same packs; its
// cost follows the roll (BenchmarkCompact's history=source/roll=8 arms: a
// roll of eight 20 KB pushes built in 19.6 ms at a peak of 0.8 MiB beside
// the 100 MB source history and in 14.2 ms beside nothing, where over the
// whole copy it took 451 ms and 100.8 MiB; internal/code/cache's scratch.go
// has the run).
//
// # Who compacts, and how often
//
// The HOME, with no lease and no epoch: two nodes that both believe they are
// home — a presence view split for a moment — both build, and the committer
// refuses the second `stale`, one wasted build. A pass judges every listed
// repository it must against ONE READING of who serves git (Homes), and a
// node that cannot read it compacts nothing that pass rather than guess (Homes
// is three-valued). ONE COMPACTION AT A TIME A NODE, and a pass compacts every
// due repository it is home for, most live packs first, until the next tick
// would begin: a bound in count would leave a repository at two thousand packs
// behind a queue of barely due ones, while the order is the bound. A
// repository a plan found NOT DUE is remembered at its row's version and its
// byte cap — every input a plan reads but the live packs, which move only with
// the version — so an idle repository costs its home's pass a map lookup,
// never a sync, and every other git node's — where no plan of it is ever made
// to remember — its rendezvous order over the pass's one reading; an
// admission the node's memory refused is never remembered, since the node's
// budget is read again at every pass.
//
// THE SYNC IS AT SESSION, floored at the repository's own version the listing
// read, never linearizable: the committer holds every input to its own
// snapshot and refuses a plan from an older one `stale`, so a barrier would
// buy nothing — and at 250 000 pushes a day a compaction's barriers would be
// some 17 000 records a day on the code log, a fifth of its git clients'
// census at the gibibyte floor.
//
// # The build's bounds
//
// The plan caps a roll at code.MaxCompactInputs packs and at [InputByteCap]
// of the cut, half code.MaxCompactOutputs cuts — 8 GiB at the nominal 1 GiB
// cut — because pack-objects closes an output before the object that would
// pass the cut and writes whole a delta whose base went to another output, so
// the BUILD'S OWN COUNT is the authority on how many outputs it wrote: more
// than one compaction records, and nothing is uploaded and the repository's
// next plan merges half the bytes. Every output is held to the BUNDLE RULE
// (code.MaxPackBytes for the pack and both its indexes) before any byte is
// stored: a dense roll — many small objects, whose indexes at 32 bytes an
// object would take a bundle past the bound at the nominal cut — is cut
// smaller from the start by the inputs' aggregate bundle-to-pack ratio, and an
// output past the rule anyway is built once more at its own ratio; a second
// miss halves the next plan the same way. The build holds a slot of the
// node's MAINTENANCE lane and its charge of the node's background memory
// (internal/code/cache's AdmitCompaction: 500 bytes an object of the inputs,
// beside room for a clone of the repository and six times max_blob_bytes for
// its delta search, each what BenchmarkCompact's memory arms measured a build
// to hold, # Measured) —
// at the maintainer's limit, never the slot it keeps for a person's verify —
// and gives both back once its last git process has ended, before the
// uploads: an upload the store paces may take hours, and holding the slot
// through it would idle the node's maintenance for every other repository.
//
// # The uploads, and a store with no room
//
// Every output is uploaded AT ONCE ([uploadAll]): a key is minted as its upload
// begins, and outputs uploaded one after another age the first key by every
// later one past what a record may name. A store with no room for an output
// (objstore.ErrFull) refuses the compaction `store_full`, and the repository
// is held back until the store has the room — never retried as a blip, never
// remembered at its version, which no room coming back moves: on the fleet's
// own bucket, whose reservation says its room, once a pass's read of it holds
// what the compaction would store; on a provider's bucket, which says nothing,
// by one held repository let through an hour after the last refusal, the
// collector's own interval, whose outputs stored release every other. On
// either, a repository's OWN hold goes once its own outputs are stored: the
// refusal it was held back for is answered, and a refusal of a later roll
// sets an estimate of its own — kept, the first roll's estimate held back
// every later roll of a repository the listing goes on naming, however small,
// once the room fell below it.
//
// # What it raises
//
// code_compaction_behind, on the home: every listed repository at the most live
// packs a repository may hold (code.MaxPacksPerRepo) that this node is home
// for, read off every pass's listing whatever its compaction decided — at the
// bound a refused push records nothing, so the repository's version stands
// still, and a reading taken only of what reached a compaction would clear
// while every push is refused. The committer raises nothing: the receiver
// refuses most such pushes before any committer sees them.
//
// # Measured
//
// The constants this package's plan reads are the code domain's, each with its
// reason and the arm that confirms it where it is defined (code.CompactFactor,
// code.CompactMinMerge, code.CompactTargetPacks, code.MaxCompactInputs,
// code.MaxCompactOutputs); [Poll], [InputByteCap]'s half and the cache's
// CompactBytesPerObject and CompactWindowBytes are BenchmarkCompact's
// (bench_test.go), whose arms and what each decides are listed there. Every
// figure below is of git 2.56.0 over the in-memory store, so what an S3
// bucket would bill is stated as requests and bytes, never as a bucket's
// time; each arm ran once (-benchtime 1x, one count) on two E-cores of an
// Intel Core Ultra 7 265U, on 2026-10-27 and 28.
//
// THE BUILD (history=source/inputs={4,8,16,64}): one compaction of the
// 100 MB source history — 295 680 objects, 99 MB of bundles pushed as 4, 8,
// 16 or 64 packs — held 139 MiB of anonymous memory at its peak whichever,
// 493.4 to 493.8 bytes an object, the input count adding nothing, which the
// cache's CompactBytesPerObject is rounded up from (500, where 300 had been
// reasoned); its build took 28 to 48 s a gibibyte of the pack bytes it
// merged, more the more packs it read, under a twentieth of the second a
// mebibyte gitproc.PackWorkDeadline gives it; and its outputs held 94.8 MB.
// Eight revisions of one 50 MiB blob, each a delta of the one before
// (blobs=max_blob/inputs=8), held 269.7 MiB, 5.4 blobs, where the build had
// been charged two for its window: the cache's CompactWindowBytes is six of
// the epoch's largest blob since. A roll at its byte cap
// (cap=input_byte_cap/cut=4MiB: the history pushed as
// gittest.HistorySessions packs, the cut scaled to 4 MiB) wrote 8 outputs for
// its 8 cuts, so [InputByteCap]'s half is KEPT: a cap of the whole
// code.MaxCompactOutputs would have held on this history, and the half is
// what holds whatever a history deltas, by the rule at its definition.
//
// A BUSY REPOSITORY (steady/pushes=6000/rate=300/min: 6 000 one-commit
// pushes of 24.6 KB — a 20 KB file, its commit and its two trees — three
// hundred to a simulated minute, a pass every minute, the other git node
// syncing after every push and every pass; counts and bytes, none a time):
// at most 304 live packs, under code.CompactTargetPacks and a minute's
// pushes, which confirms [Poll]; twenty compactions, one a pass, each taking
// the minute's three hundred, so 1.003 packs retired a push; outputs weighing
// 2.38 times what was pushed, and the other git node downloading 3.38 times
// it — every push once, and every output once more; 42.6 bytes a push of
// compaction records on the code log, their signed frames included. A
// thousand pushes cost 3.3 PUTs of outputs (58.5 MB) and 23.3 GETs of them on
// a fleet of eight git nodes (410 MB), and a cold sync of what the 6 000 left
// reads two packs, 125 MB — the same at min-merge 4, 8 and 16, since every
// pass found three hundred packs whatever the floor.
//
// A QUIET REPOSITORY (steady/pushes=1200/rate=1/min: 1 200 such pushes, one a
// minute, a pass every minute, so a merge waits for the floor's packs to
// gather), merged from code.CompactMinMerge's sixteen: 77 compactions, at
// most 20 live packs, 1.05 packs retired a push, outputs weighing 3.73 times
// what was pushed and the other git node downloading 4.73 times it, and
// 108 bytes a push of records; a thousand pushes cost 64 PUTs and 449 GETs of
// the outputs on a fleet of eight, and 13 GETs a cold sync of what was left —
// 527 requests, where merging from eight cost 1 081 (161 compactions, at most
// 13 live packs, 1.13 retired a push, 184 bytes of records) and from four
// 2 212 (331, 9, 1.27, 337): the floor is the one with the fewest requests.
//
// THE SWAP (swap/packs={50,2048}) is the cache's — a compaction is what makes
// one — and internal/code/cache's doc states it.
//
// THE FULL LISTING STANDS (M3-decisions M3-O26): BenchmarkCompact's
// poll/repos=10000 arm read one page of the listing of 10 000 repositories in
// 66.6 ms on the data node serving it (op=page, an in-process estate at stale,
// no statistics gathered; two E-cores of an Intel Core Ultra 7 265U,
// 2026-10-27, five reads), under the 100 ms past which two listings a minute
// from each of sixteen git nodes would hold over a twentieth of one reader and
// a listing of what changed since the last poll would have to be priced. And a
// pass over those 10 000, none of them due here, cost 342 ns a repository on
// their home (op=pass/home=all: a lookup in the memory of plans found not due,
// 3.4 ms the pass) and 340 ns of its own on a git node home to an eighth of
// them (op=pass/home=1of8: the rest judged against the pass's one reading of
// who serves git), beside which the router's judging of every one of those
// costs 575 ns a repository over eight git nodes and 1.28 µs over sixteen
// (internal/estate's BenchmarkHomeOf) — under a microsecond a repository, some
// 9 ms a pass, on each of a fleet of eight (the poll arms and BenchmarkHomeOf
// at -benchtime 20x, one count, on the same machine, 2026-10-28).
package compact
