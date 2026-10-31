// Package compact keeps a repository's pack count bounded, its bitmap
// published and what no ref reaches dropped when a person asks: the
// MAINTAINER's three loops (ADR-0060, ADR-0061, and the expunge's two records
// that internal/code decides).
//
// Every push stores one pack, so a busy repository's pack count grows with
// its pushes, and every cold sync then pays a GET a pack and every request a
// lookup a pack. And every git node used to write its own whole
// multi-pack-index and bitmap of every repository it serves — O(history) in
// memory and minutes on every node at every duty cycle, refused outright on a
// node whose memory could not hold the largest repository's bitmap. One git
// node a repository, its maintainer, now does both for the fleet:
//
//   - THE COMPACTION LOOP ([Compactor.Poll]): on every [Poll] it lists the
//     repositories holding code.CompactMinMerge live packs or more, most
//     first (the code domain's `compaction_due` read), and for each it
//     maintains merges the small packs no published layer names
//     geometrically, git's own `repack --geometric=2` split over the packs
//     that are not FULL, into one or a few pack bundles in the stored format,
//     stores each as one object, and proposes ONE compaction to the
//     repository's committer, which decides it in one snapshot and retires
//     the inputs into code_packs_retired for code.RetireGrace. Every git
//     node's cache drops a retired pack by an incarnation swap over hard
//     links, so a reader holding the old directory keeps every file it may
//     open (internal/code/cache's swap.go).
//   - THE PUBLICATION LOOP ([Compactor.PublishPass]): on every [PublishPoll]
//     it lists the repositories holding a pack no published layer covers,
//     least recently published first (`publication_due`), and the
//     repositories whose copy on this node holds a layer this node refused,
//     and for each it maintains whose tail is worth a layer ([Due]) — or
//     that is a REPAIR — writes the repository's next PUBLISHED LAYER: git's
//     own incremental multi-pack-index layer with its bitmap, and the
//     commit-graph layer with every changed-path filter, over every live pack
//     the chain beneath it does not cover. It stores the layer's bytes as
//     parts (code.StoreLayer) and proposes it as one compaction carrying the
//     layer, which every git node then installs (internal/code/cache's
//     layers.go).
//   - THE EXPUNGE LOOP ([Compactor.ExpungePass]): on every [ExpungePoll] it
//     lists the repositories with an expunge open or a person's purge
//     requested (`expunge_due`), and for each it maintains runs ONE CYCLE
//     ([Compactor.Expunge]) — finishing an expunge left open from its rows,
//     or serving the purge from its scan to its commit (# The expunge, below).
//
// # Who maintains, and why one node does all three
//
// The MAINTAINER is the first git node of the repository's rendezvous order
// whose ADVERTISED background budget holds its maintenance's charge — the
// largest any of its steps may be admitted at, the publication's bitmap over
// the whole history or a merge of every object with its delta window,
// cache.Cache.MaintenanceCharge — and the home where none can
// (internal/estate's Router.Maintainer): on a fleet of alike nodes it is the
// home, and on a fleet with one large git node every repository only that
// node can maintain is maintained there. All three loops
// ask by the same charge, so the three writers of a repository's packs run on
// ONE node, and on that node a compaction NEVER MERGES A PACK A PUBLICATION IN
// FLIGHT PLANNED OVER ([Compactor.claimCompaction]): once a publication has
// planned, it reserves every pack its layer will name, its repack's inputs
// among them, and a compaction of the repository merges beside it the packs
// pushed since, which the layer neither names nor needs (the decide's rules
// 2, 6c and 6d), so both records apply in either order. Until the publication
// has planned — its sync and its plan, seconds — a compaction of its
// repository answers busy, and a publication asked while a compaction of its
// repository runs answers busy itself, since its plan would cover that
// merge's inputs: either way round, one of the two would otherwise be refused
// stale after its whole build. An EXPUNGE holds the whole repository from its
// scan to its commit, and neither of the others builds beside it
// ([Compactor.claimExpunge]): its begin retires inputs chosen among every live
// pack and drops every layer published over them. No lease and no epoch:
// two nodes whose presence views disagree for a moment may both build, and the
// committer refuses the second `stale`, one wasted build. Each pass judges
// every listed repository it must against ONE LISTING of who serves git and
// what each git node advertises (Maintainers), and a node that cannot read it
// does nothing that pass rather than guess (Maintainers is three-valued); a
// node no listing of the git nodes carries maintains
// nothing — a booting one until its host beats — so every loop waits for this
// node to be listed before its first pass ([Compactor.awaitListed]). THREE
// LOOPS, because a publication of a large repository takes minutes and a hot
// repository gains packs at up to three hundred a minute: behind a publication
// in one loop, it would reach code.MaxPacksPerRepo and refuse pushes — and the
// same holds for the repository being published, whose merges therefore run
// beside its publication rather than wait for it. An expunge's builds walk the
// whole history from every tip, so a purge in either other loop would hold
// back that loop's work on every other repository as long. They share the
// node's maintenance slots and memory through the cache's admissions, which
// bound them. Each runs ONE BUILD AT A TIME and a pass
// works through every due repository it maintains in its listing's order
// until the next tick would begin; a publication that did not land holds its
// repository back by the duty cycle from when its build began
// ([Compactor.paced]), so a repository whose publication keeps failing never
// heads every pass of the loop. A repository a plan found NOT DUE
// (nothing to merge, nothing to publish) is remembered at its row's version —
// and a compaction's at its byte cap — so an idle repository costs its
// maintainer's pass a map lookup, never a sync, and every other git node's —
// where no plan of it is ever made to remember — its rendezvous order over the
// pass's one listing; an admission the node's memory refused is never
// remembered, since the node's budget is read again at every pass, and the
// next pass asks again who maintains the repository, which is how a larger
// node takes it over.
//
// THE SYNC IS AT SESSION, floored at the repository's own version the listing
// read, never linearizable: the committer holds every input and the chain to
// its own snapshot and refuses a plan from an older one `stale`, so a barrier
// would buy nothing — and at 250 000 pushes a day a compaction's barriers
// would be some 17 000 records a day on the code log, a fifth of its git
// clients' census at the gibibyte floor.
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
// drops objects is an expunge, never this (ADR-0060). A PUBLISHED PACK IS
// NEVER A COMPACTION'S INPUT: a node's chain loads only over every pack it
// names, so the plan leaves every pack a layer names out, and a published
// repository's small packs are merged by its publications' repacks instead.
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
// cost follows the roll (BenchmarkCompact's history=source/roll=16 arms: a
// roll of sixteen 20 KB pushes built in 22.9 ms at a peak of 0.95 MiB beside
// the 100 MB source history and in 21.3 ms beside nothing, where over the
// whole copy a roll of eight took 451 ms and 100.8 MiB; internal/code/cache's
// scratch.go has the runs).
//
// # A layer covers every pack its base does not, and is written over exactly it
//
// git writes a bitmap only over a chain whose packs hold everything their
// commits reach — measured on git 2.56.0, a layer over all of a history's
// packs but one, and one over the full packs of a cut alone, were refused
// ("Packfile doesn't have full closure") and wrote no bitmap — and every pack
// live at one position is closed over the packs before it, since a push's pack
// is closed and a compaction drops no object. So a layer covers EVERY live
// pack its base does not, as of its plan (planLayer), and a pack pushed since
// stays outside the chain, which git reads beside it. The layers are merged
// GEOMETRICALLY, git's own policy (LayerSplitFactor, LayerRepackPacks): the
// next layer down is absorbed into the replacement while the objects gathered
// above it, twice over, reach its own, so the chain stays logarithmic in the
// history; and where more than LayerRepackPacks of a layer's packs are not
// full they are repacked first, under a compaction's caps and as a
// compaction's build is, over a store of exactly them in a merge's scratch of
// their own — never in the layer's scratch, whose links of every pack would
// have its walk run the whole history — and held to a compaction's count and
// bundle rules BEFORE the layer is written over the outputs, which those rules
// alone read (held after, an output past either threw a finished bitmap
// away), the outputs then gathered into the layer's scratch
// (cache.Scratch.Gather). A layer is WRITTEN,
// never merged with `multi-pack-index compact`, in a scratch of hard links
// whose chain files name EXACTLY its base (cache.Cache.NewLayerScratch):
// measured on git 2.56.0, the same write with layers above its base still
// named took 196–200 s against 1.03 s, and `compact` of two non-root layers
// with one above them did too; compacting from the root, which reuses bitmaps,
// cannot take the new packs a publication exists to add. A tail that holds no
// commit gives git nothing to bitmap, and a layer without a bitmap switches
// off every bitmap beneath it, so such a plan is made once more over one more
// layer.
//
// # The builds' bounds
//
// A plan caps a roll — a compaction's, and a publication's repack — at
// code.MaxCompactInputs packs and at [InputByteCap] of the cut, half
// code.MaxCompactOutputs cuts — 8 GiB at the nominal 1 GiB cut — because
// pack-objects closes an output before the object that would pass the cut and
// writes whole a delta whose base went to another output, so the BUILD'S OWN
// COUNT is the authority on how many outputs it wrote: more than one record
// takes, and nothing is uploaded and the repository's next plan merges half
// the bytes. Every output is held to the BUNDLE RULE (code.MaxPackBytes for the
// pack and both its indexes) before any byte is stored: a dense roll — many
// small objects, whose indexes at 32 bytes an object would take a bundle past
// the bound at the nominal cut — is cut smaller from the start by the inputs'
// aggregate bundle-to-pack ratio, and an output past the rule anyway is built
// once more at its own ratio; a second miss halves the next plan the same way.
// A layer that would weigh more than code.MaxLayerParts parts of
// code.LayerPartBytes is refused before it is built. Each build holds a slot
// of the node's MAINTENANCE lane and its charge of the node's background
// memory (internal/code/cache's AdmitCompaction and AdmitPublication: a
// compaction's 550 bytes an object of its inputs and room beside them for a
// transfer of as many — never for a clone of the whole repository, whose size
// would refuse every merge of a large one and so its pushes — and six times
// max_blob_bytes for its delta search, the first and the last what
// BenchmarkCompact's and BenchmarkMidx's memory arms measured a build to hold
// (# Measured); a publication's the largest of its bitmap's 600 an object of
// the WHOLE repository, its graph layer's and its repack's, as BenchmarkMidx's
// step=publish arms measured them, each beside room for a clone of
// the repository, since a publication refused is slower, never a push
// refused) — at the maintainer's limit, never the slot it keeps for a
// person's verify — and gives both back once its last git process has ended,
// before the uploads: an upload the store paces may take hours, and holding
// the slot through it would idle the node's maintenance for every other
// repository.
//
// # The uploads, and a store with no room
//
// Every output and every layer part is uploaded AT ONCE ([uploadAll]): a key
// is minted as its upload begins, and uploads one after another age the first
// key by every later one past what a record may name. A store with no room
// (objstore.ErrFull) refuses the build `store_full`, and the repository is
// held back in that loop until the store has the room — never retried as a
// blip, never remembered at its version, which no room coming back moves: on
// the fleet's own bucket, whose reservation says its room, once a pass's read
// of it holds what the build would store; on a provider's bucket, which says
// nothing, by one held repository a loop let through an hour after the last
// refusal, the collector's own interval, whose upload stored releases every
// other. On either, a repository's OWN hold in a loop goes once that loop's
// own upload of it is stored: the refusal it was held back for is answered,
// and a refusal of a later build sets an estimate of its own — kept, the first
// build's estimate held back every later build of a repository the listing
// goes on naming, however small, once the room fell below it. A maintainer
// holds at most code.MaxCompactOutputs + code.MaxLayerParts
// + code.MaxCompactOutputs uploads at once — 48, a publication's sixteen parts
// and sixteen outputs beside a compaction's sixteen — 384 MiB of S3 first
// buffers out of the serving share, never the maintenance budget (M3-decisions
// M3-O40).
//
// # Self-healing
//
// A layer part the store lost, or one whose bytes are damaged, is refused
// `missing` or `corrupt` on every git node, its maintainer included, and the
// node serves from the prefix below it. The maintainer's next pass finds the
// refusal in its own cache (cache.Cache.Damaged) whether or not the repository
// has a tail, and REPAIRS it: the plan replaces the refused layer and every
// layer above it, paced by PublishMinInterval alone. A restore carries no
// layer (ADR-0061), so every node serves in own mode until each repository's
// maintainer publishes again. None of this alarms: a lost layer costs a
// publication, never data.
//
// # The expunge
//
// An EXPUNGE drops what no ref reaches, in the two records internal/code
// decides on the repository's subject — its BEGIN, which rewrites some live
// packs into KEPT packs (live) and DROPPING ones and moves the repository's
// drop epoch, and its COMMIT, which retires the dropping packs and keeps a
// RESCUE of them live — and a person asks for one with a third, a purge
// REQUEST, which the maintainer serves. A request is what begins a cycle, and
// its grace is none: a purge keeps no object a force-push or a deletion left
// behind. An expunge open with no request — one keeping code.PruneGrace — is
// finished with that grace.
//
// THE SCAN is the copy's refs — its packed-refs as a scratch links it, one
// moment's with the packs the scratch holds — and the drops the grace keeps
// (the `expunge_tips` read; none for a purge), and its INPUTS the live packs
// whose content dates from before the request (code.PackRow.Content), every
// row of each file together, oldest first and capped at
// code.MaxExpungeInputs rows and [ExpungeInputByteCap] of the cut: a FULL pack
// and a pack a published layer names among them, since what a purge drops may
// lie in any pack, and the layers over them dropped with the begin
// (code.LayersOver, the decide's own reading). A pack pushed after the
// request is no input, so a purge of a busy repository rewrites what it was
// asked about and nothing pushed since.
//
// THE BUILD SORTS, never judges a record: in a scratch whose OWN packs are the
// inputs and whose one alternate is a store of every other live pack
// (cache.Cache.StageExpunge), `pack-objects --revs --local` from every tip
// packs what a tip reaches of the inputs and of no other pack — the KEPT
// packs — and `pack-objects --stdin-packs` over the inputs, excluding the kept
// packs and every pack of the store by name so its name-hash walk stops at
// their objects, packs the rest — the DROPPING packs. Every object of the
// inputs is in the kept, the dropping or the store's packs, so to every reader
// the begin is a merge. A drop's object an earlier purge already took out is
// no tip — a walk from it would refuse the build — so the drops are held to
// what the scratch holds first, one `cat-file --batch-check`. An input set the
// rest holds whole writes neither, and is packed again as dropping alone,
// which a begin must name one output of. The builds are admitted at a
// publication's repack's charge (cache.Cache.AdmitExpunge): the walk reads the
// whole history whatever few packs it sorts, and a refused expunge is a purge
// waiting, never a push refused.
//
// THE RESCUE IS VALID AT ANY POSITION AT OR AFTER THE BEGIN, which is what
// makes a crash resumable from the rows alone. A push checked after the begin
// relies on no dropping object — its receiver's view of the copy leaves them
// out — and one checked before it is refused `expunging` by the drop epoch, so
// the only refs that reach a dropping object at the rescue's sync are ones a
// push landed between the scan and the begin, and nothing that lands later
// adds one. So the rescue reads the refs at its own sync — in a scratch whose
// own packs are the dropping packs and whose alternate is the copy's live
// packs alone (cache.Cache.StageRescue) — and packs what they reach of the
// dropping packs alone, `--not` the scan's tips where the cycle still holds
// them, which bounds its walk to what moved since; resumed, it walks the whole
// history. A copy whose objects/ still holds the begin's inputs would hide
// every object the rescue must save, so a rescue over one answers busy and
// the next pass syncs again (cache.ErrInputsLeaving). The commit's one
// precondition is its expunge, open at its epoch, so no push starves it. Its
// crash points are codefault.ExpungePath: a cycle stopped at any of them
// leaves nothing, an expunge open — which the next pass, on whichever node
// maintains the repository, finishes from the rows — or an expunge
// committed; what it stored that no row names is the collector's.
//
// WHAT A PURGE LEAVES BEHIND: the commit retires the dropping packs into
// code_packs_retired, so a reader already on one finishes, and the store
// deletes their bytes after code.RetireGrace and the collector's grace — the
// purge's crewlet.code.expunge.dropped_bytes. Nothing of this loop is
// measured yet: [ExpungePoll] is the listing's, and [ExpungeInputByteCap] the
// compaction's half of a record's outputs.
//
// # What it raises
//
// code_compaction_behind, on the maintainer: every listed repository at the
// most live packs a repository may hold (code.MaxPacksPerRepo) that this node
// maintains, read off every pass's listing whatever its compaction decided —
// at the bound a refused push records nothing, so the repository's version
// stands still, and a reading taken only of what reached a compaction would
// clear while every push is refused. The committer raises nothing: the
// receiver refuses most such pushes before any committer sees them. A
// publication the node's memory refuses counts toward code_maintenance_refused
// through the cache, for as long as the budget stays under its charge.
//
// # Measured
//
// The constants the plans read are the code domain's, each with its reason
// and the arm that confirms it where it is defined (code.CompactFactor,
// code.CompactMinMerge, code.CompactTargetPacks, code.MaxCompactInputs,
// code.MaxCompactOutputs, code.MaxPublishedLayers, code.MaxLayerParts); [Poll],
// [InputByteCap]'s half and the cache's CompactBytesPerObject and
// CompactWindowBytes are BenchmarkCompact's (bench_test.go), whose arms and
// what each decides are listed there; the publication's cadence ([Due]'s
// constants) and the cache's PublishBytesPerObject and GraphLayerBytesPerObject
// are BenchmarkMidx's (internal/code/cache's layers_bench_test.go), measured
// on the pin and stated where each is defined, which re-checks
// CompactBytesPerObject by a publication's repack too. Every figure below is
// of git 2.56.0 over the in-memory store, so what an S3
// bucket would bill is stated as requests and bytes, never as a bucket's
// time; each arm ran once (-benchtime 1x, one count) with Go 1.27.2 on two
// E-cores of an Intel Core Ultra 7 265U, which has SHA instructions, on
// 2026-10-27 and 28.
//
// THE BUILD (history=source/inputs={4,8,16,64}): one compaction of the
// 100 MB source history — 295 680 objects, 99 MB of bundles pushed as 4, 8,
// 16 or 64 packs — held 139 MiB of anonymous memory at its peak whichever,
// 493.4 to 493.8 bytes an object, the input count adding nothing; a
// publication's repack of the same history's 64 packs (BenchmarkMidx's
// step=publish/layer=root, 2026-10-29) held 147.5 MiB, 523 bytes an object,
// on the pin and on the node floor's own build alike; the cache's
// CompactBytesPerObject is the larger rounded up to the next 50 bytes (550;
// it was 500 from the first arm alone, where 300 had been reasoned); its
// build took 28 to 48 s a gibibyte of the pack bytes it
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
// 2 212 (331, 9, 1.27, 337): the floor is the one of the three with the
// fewest requests, each doubling having halved them — the largest the arms
// measure, not a minimum they bracket (code.CompactMinMerge says what bounds
// a higher one). And a day after each quarter hour of it, the code-retention
// duty's pass swept what that quarter hour's merges had retired: 75
// retire_sweep records for the 77 merges, about 700 bytes each and 44 bytes
// a push on the code log beside the compactions' 108 — merges some sixteen
// minutes apart are nearly all swept alone (measured since, the arm at the
// floor alone: -run '^$' -bench
// 'BenchmarkCompact/steady/pushes=1200/rate=1/min/min-merge=16' -benchtime
// 1x -count 1, the same machine, 2026-10-28, every other figure of it
// unchanged).
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
// pass over those 10 000, none of them due here, cost 326 ns a repository on
// their maintainer (op=pass/maintains=all: a lookup in the memory of plans
// found not due, 3.3 ms the pass) and 383 ns of its own on a git node
// maintaining an eighth of them (op=pass/maintains=1of8: the rest judged
// against the pass's one listing of the git nodes, each by its publication's
// charge), beside which the router's judging of every one of those costs
// 569 ns a repository over eight git nodes and 1.11 µs over sixteen
// (internal/estate's BenchmarkMaintainerOf) — under a microsecond a
// repository, some 9 ms a pass, on each of a fleet of eight (the poll arms and
// BenchmarkMaintainerOf at -benchtime 20x, one count, on the same machine,
// 2026-10-29).
package compact
