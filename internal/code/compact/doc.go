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
// refuses the second `stale`, one wasted build. A node that cannot read who
// serves git compacts nothing that pass rather than guess (Homes is
// three-valued). ONE COMPACTION AT A TIME A NODE, and a pass compacts every
// due repository it is home for, most live packs first, until the next tick
// would begin: a bound in count would leave a repository at two thousand packs
// behind a queue of barely due ones, while the order is the bound. A
// repository a plan found NOT DUE is remembered at its row's version and its
// byte cap — every input a plan reads but the live packs, which move only with
// the version — so an idle repository costs a pass a map lookup, never a
// sync; an admission the node's memory refused is never remembered, since the
// node's budget is read again at every pass.
//
// THE SYNC IS AT SESSION, floored at the repository's own version the listing
// read, never linearizable: the committer holds every input to its own
// snapshot and refuses a plan from an older one `stale`, so a barrier would
// buy nothing — and at 250 000 pushes a day a compaction's barriers would be
// some 31 000 records a day on the code log, over a third of its git clients'
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
// (internal/code/cache's AdmitCompaction: 300 bytes an object of the inputs,
// provisional until BenchmarkCompact's memory arms measure it, beside room for
// a clone of the repository and twice max_blob_bytes for its delta window) —
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
// (bench_test.go), whose arms and what each decides are listed there.
//
// THE FULL LISTING STANDS (M3-decisions M3-O26): BenchmarkCompact's
// poll/repos=10000 arm read one page of the listing of 10 000 repositories in
// 66.6 ms on the data node serving it (op=page, an in-process estate at stale,
// no statistics gathered; two E-cores of an Intel Core Ultra 7 265U,
// 2026-10-27, five reads), under the 100 ms past which two listings a minute
// from each of sixteen git nodes would hold over a twentieth of one reader and
// a listing of what changed since the last poll would have to be priced; and a
// pass over those 10 000, none of them due, cost 387 ns a repository (op=pass:
// a lookup in the memory of plans found not due, 3.9 ms the pass).
package compact
