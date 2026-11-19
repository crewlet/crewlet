// Package compact keeps a repository's pack count bounded, its bitmap
// published and what no ref reaches dropped — when a person asks, and weekly
// once it is a quarter of the repository: the MAINTAINER's three loops
// (ADR-0090, ADR-0091, and the expunge's two records that internal/gitlet
// decides, ADR-0099).
//
// Every push stores one pack, so every cold sync pays a GET a pack and every
// request a lookup a pack. A multi-pack-index and bitmap written by every
// Gitlet node is O(history) in memory and minutes at every duty cycle. So one
// Gitlet node a repository, its maintainer, does both for the fleet:
//
//   - THE COMPACTION LOOP ([Compactor.Poll]) lists the repositories holding
//     gitlet.CompactMinMerge live packs or more, most first (`compaction_due`),
//     merges the small packs no published layer names geometrically (git's
//     `repack --geometric=2` split, over the packs that are not FULL) into one
//     or a few bundles, and proposes ONE compaction to the repository's
//     committer — `code.compact`, idempotent by the id its build minted once,
//     so a retry across a failover is one compaction — which decides it in one
//     snapshot and retires the inputs into code_packs_retired for
//     gitlet.RetireGrace. Each node's cache drops a retired pack by an
//     incarnation swap over hard links, so a reader keeps every file it may open
//     (internal/gitlet/cache's swap.go).
//   - THE PUBLICATION LOOP ([Compactor.PublishPass]) lists the repositories
//     holding a pack no published layer covers, least recently published first
//     (`publication_due`), and those whose copy on this node holds a layer this
//     node refused, and for each whose tail is worth a layer ([Due]), or that
//     is a REPAIR, writes the next PUBLISHED LAYER: git's incremental
//     multi-pack-index layer with its bitmap, and the commit-graph layer, over
//     every live pack the chain beneath it does not cover. It stores the bytes
//     as parts (gitlet.StoreLayer) and proposes one compaction carrying the
//     layer, which every node installs (internal/gitlet/cache's layers.go).
//   - THE EXPUNGE LOOP ([Compactor.ExpungePass]) lists the repositories with an
//     expunge open or a purge requested, then every other one last judged a
//     week ago or never (`expunge_due`), and runs ONE CYCLE
//     ([Compactor.Expunge]) on each (# The expunge, below).
//
// # Who maintains, and why one node does all three
//
// The MAINTAINER is the first Gitlet node of the repository's rendezvous order
// whose ADVERTISED background budget holds its maintenance's charge
// (cache.Cache.MaintenanceCharge), and the home where none can (internal/estate's
// Router.Maintainer). All three loops ask by the same charge, so the three
// writers of a repository's packs run on ONE node, and a compaction NEVER
// MERGES A PACK A PUBLICATION IN FLIGHT PLANNED OVER
// ([Compactor.claimCompaction]): once a publication has planned, it reserves
// every pack its layer will name, and a compaction merges beside it the packs
// pushed since, which the layer neither names nor needs (the decide's rules 2,
// 6c and 6d), so both records apply in either order. Until the publication has
// planned a compaction of its repository answers busy, and a publication asked
// while a compaction runs answers busy itself, since its plan would cover that
// merge's inputs: either way round one would be refused stale after its whole
// build.
//
// An EXPUNGE reserves its inputs the same way ([Compactor.claimExpunge]): a
// compaction answers busy only while the expunge syncs and plans, then merges
// beside it every pack but the inputs it chose, so a repository being purged is
// never refused compaction_behind for it. No publication builds beside an
// expunge (the begin drops every layer published over its inputs and the decide
// refuses a layer while an expunge is open), and an expunge waits for a running
// compaction or publication, whose records would move what its plan reads.
//
// No lease and no epoch: two nodes whose presence views disagree may both
// build, and the committer refuses the second `stale`, one wasted build. Each
// pass judges every listed repository against ONE LISTING of who serves git
// and what each node advertises (Maintainers), and a node that cannot read it
// does nothing that pass rather than guess; a node no
// listing carries maintains nothing, so every loop waits to be listed first
// ([Compactor.awaitListed]).
//
// THREE LOOPS, because a publication of a large repository takes minutes and a
// hot repository gains up to three hundred packs a minute: behind a
// publication in one loop it would reach gitlet.MaxPacksPerRepo and refuse
// pushes, and a purge walks the whole history. They share the node's
// maintenance slots and memory through the cache's admissions. Each runs ONE
// BUILD AT A TIME; a publication that did not land holds its repository back by
// the duty cycle from when its build began ([Compactor.paced]). A plan that
// found a repository NOT DUE is remembered at its row's version (a compaction's,
// at its byte cap), so an idle one costs a map lookup, never a sync; an admission the node's memory refused is
// never remembered, which is how a larger node takes the repository over.
//
// THE SYNC IS AT SESSION, floored at the version the listing read, never
// linearizable: the committer refuses a plan from an older snapshot `stale`, so
// a barrier would buy nothing and cost thousands of records a day.
//
// # A merge makes no reachability judgement
//
// The build is ONE `pack-objects --stdin-packs --max-pack-size` over the
// inputs with NO pack excluded, in a scratch repository whose one alternate is
// a store holding EXACTLY THE INPUTS — hard links of the packs of the
// incarnation serving the plan's sync (internal/gitlet/cache's NewMergeScratch)
// — so it writes nothing but the scratch. Every object of the inputs is in the
// outputs, an object an input shares with a pack that is no input included:
// two compactions decided from different snapshots may each retire the pack
// the other relied on, and both land. So no push can land a ref whose closure a
// compaction removed, and nothing fences a compaction against a push: what
// drops objects is an expunge, never this (ADR-0090). A PUBLISHED PACK IS
// NEVER A COMPACTION'S INPUT: a node's chain loads only over every pack it
// names, so a published repository's small packs are merged by its
// publications' repacks instead.
//
// NOTHING BUT THE INPUTS IN REACH, because `--stdin-packs` fills in the name
// hash its delta search sorts by with a walk from every commit its inputs hold
// over everything it can reach, which only an excluded pack stops: over the
// whole incarnation a roll of a few small packs walked the whole history, so
// cost followed the history while charge and deadline followed the roll, and a
// large repository's rolls were killed and its pushes refused
// compaction_behind. Over the store the walk ends at the inputs' edge.
//
// # A layer covers every pack its base does not, and is written over exactly it
//
// git writes a bitmap only over a chain whose packs hold everything their
// commits reach — a layer over all of a history's packs but one is refused
// ("Packfile doesn't have full closure") — and every pack live at one position
// is closed over the packs before it, since a push's pack is closed and a
// compaction drops no object. So a layer covers EVERY live pack its base does
// not, as of its plan (planLayer), and a pack pushed since stays outside the
// chain, which git reads beside it.
//
// Layers are merged GEOMETRICALLY, git's own policy (LayerSplitFactor,
// LayerRepackPacks): the next layer down is absorbed while the objects gathered
// above it, twice over, reach its own, so the chain stays logarithmic. Where
// more than LayerRepackPacks of a layer's packs are not full they are repacked
// first, as a compaction's build is, in a merge's scratch of their own (the
// layer's scratch links every pack, so its walk would run the whole history),
// and held to a compaction's count and bundle rules BEFORE the layer is
// written, since an output past either would throw a finished bitmap away.
//
// A layer is WRITTEN, never merged with `multi-pack-index compact`, in a
// scratch of hard links whose chain files name EXACTLY its base
// (cache.Cache.NewLayerScratch): with layers above its base still named the
// write takes minutes against a second, and compacting from the root cannot
// take the new packs a publication exists to add. A tail that holds no commit
// gives git nothing to bitmap, and a layer without a bitmap switches off every
// bitmap beneath it, so such a plan is made once more over one more layer.
//
// # The builds' bounds
//
// A plan caps a roll — a compaction's, and a publication's repack — at
// gitlet.MaxCompactInputs packs and at [InputByteCap] of the cut, half
// gitlet.MaxCompactOutputs cuts, because pack-objects closes an output before
// the object that would pass the cut and writes whole a delta whose base went
// to another output, so the BUILD'S OWN COUNT is the authority on how many
// outputs it wrote: if more than one record takes, nothing is uploaded and the
// next plan merges half the bytes. Every output is held to the BUNDLE RULE
// (gitlet.MaxPackBytes for the pack and both its indexes) before any byte is
// stored: a dense roll is cut smaller from the start by the inputs'
// bundle-to-pack ratio, an output past the rule anyway is built once more at
// its own ratio, and a second miss halves the next plan. A layer over
// gitlet.MaxLayerParts parts is refused before it is built.
//
// Each build holds a slot of the node's MAINTENANCE lane, never the slot kept
// for a person's verify, and its charge of the background memory
// (internal/gitlet/cache's AdmitCompaction and AdmitPublication). A
// compaction's charge is 550 bytes an object of its inputs and room beside them
// for a transfer of as many — never a clone of the whole repository, which
// would refuse every merge of a large one and so its pushes — and six times
// max_blob_bytes for its delta search. A publication's is the largest of its
// bitmap's, graph layer's and repack's, each beside room for a clone, since a
// publication refused is slower, never a push refused. A build gives both back
// once its last git process has ended, before the uploads: an upload the store
// paces may take hours and would idle the node's maintenance.
//
// # The uploads, and a store with no room
//
// Every output and every layer part is uploaded AT ONCE ([uploadAll]): a key
// is minted as its upload begins, and uploads one after another age the first
// key by every later one past what a record may name. A store with no room
// (objstore.ErrFull) refuses the build `store_full`, and the repository is
// held back in that loop until the store has the room — never retried as a
// blip, never remembered at its version, which no room coming back moves. On
// the fleet's own bucket that is once the reservation shows the room; on a
// provider's, which says nothing, by one held repository a loop let through an
// hour after the last refusal. A repository's own hold goes once that loop's
// upload of it is stored, and a later build's refusal sets an estimate of its
// own. A maintainer holds at most 48 uploads at once
// (gitlet.MaxCompactOutputs twice and gitlet.MaxLayerParts), 384 MiB of S3
// first buffers out of the serving share, never the maintenance budget
// (M3-decisions M3-O40).
//
// # Self-healing
//
// A layer part the store lost or damaged is refused `missing` or `corrupt` on
// every Gitlet node, its maintainer included, and the node serves from the
// prefix below it. The maintainer's next pass finds the refusal in its own
// cache (cache.Cache.Damaged) whether or not the repository has a tail, and
// REPAIRS it: the plan replaces the refused layer and every layer above it,
// paced by PublishMinInterval alone. A restore carries no layer (ADR-0091), so
// every node serves in own mode until each maintainer publishes again. None of
// this alarms: a lost layer costs a publication, never data. Each loop's crash
// points are gitletfault's CompactPath, PublishPath and ExpungePath: a loop
// stopped at one leaves each record applied or absent, and what it stored that
// no row names is the collector's.
//
// # The expunge
//
// An EXPUNGE drops what no ref reaches, in the two records internal/gitlet
// decides on the repository's subject — its BEGIN, which rewrites some live
// packs into KEPT packs (live) and DROPPING ones and moves the drop epoch, and
// its COMMIT, which retires the dropping packs and keeps a RESCUE of them
// live — and a person asks for one with a purge REQUEST, served with no grace:
// a purge keeps no object a force-push or a deletion left behind. THE
// MAINTAINER'S OWN SCHEDULE begins one too (M6-D8), with gitlet.PruneGrace, for
// every active or archived repository, scratch ones included (a push is
// refused repo_size on live bytes whoever owns them), last JUDGED
// gitlet.ExpungeInterval ago or never. Its cycle first judges whether an
// expunge is DUE: what no ref, no drop the grace keeps and no row in flight
// reaches OF THE PACKS A CYCLE MAY TAKE — those whose newest content arrived
// gitlet.PruneGrace or longer ago — is MORE than 1/gitlet.ExpungeShare of the
// live pack bytes AND at least gitlet.ExpungeMinBytes, below which the fleet's
// re-download of the kept packs costs more than it frees. Weighed over the
// whole repository, garbage in a pack younger than the grace would make it due
// while no pack a cycle could take held any, so it would rewrite clean packs weekly.
//
// The weighing is ONE `rev-list --objects --use-bitmap-index` from what is
// kept, each printed object looked up in each such pack's index, and what none
// printed weighed at the bytes it takes in that pack. A repository whose live
// bytes are under gitlet.ExpungeMinBytes can never be due, so it is judged from
// its listed row alone: no sync, no walk, no download. A repository found NOT
// DUE is RECORDED so (gitlet.ExpungeScan, one record on its subject), which
// takes it out of the weekly arm for gitlet.ExpungeInterval on EVERY NODE —
// never a memo in the scanning process, which a restart would lose — keyed by
// the record's instant, never the row's version, which a push moves and would
// rescan a hot repository every pass. An expunge open with no request is
// finished with gitlet.PruneGrace.
//
// THE SCAN is the copy's refs, the drops the grace keeps (`expunge_tips`; none
// for a purge) and, for a purge too, what rows hold IN FLIGHT (M6-D2): every
// revision head of a draft or open change, every live queue entry's head,
// every candidate's tip (a candidate is rows, never a ref) and every green row;
// and its INPUTS the live packs whose content dates from before the request
// (gitlet.PackRow.Content), oldest first, capped at gitlet.MaxExpungeInputs
// rows and [ExpungeInputByteCap] of the cut: a FULL pack or a published one
// among them, since what a purge drops may lie in any pack, and the layers over
// them dropped with the begin (gitlet.LayersOver). ON THE SCHEDULE
// the inputs are instead the live packs whose newest content ARRIVED
// gitlet.PruneGrace or longer ago (gitlet.PackRow.Arrived) that the scan found
// would drop a byte, DENSEST FIRST, so EVERY PACK ADDED WITHIN THE GRACE IS KEPT
// WHOLE (M6-D2), a push in flight's unreferenced objects among them; a pack
// whose arrival is not known is passed over, the side the grace keeps. The
// cycle reads the live packs ONCE, at the scan, and the schedule reserves every
// pack it may take from the scan through its walk, narrowing to its plan's
// inputs after.
//
// THE BUILD SORTS, never judges a record: in a scratch whose OWN packs are the
// inputs and whose one alternate is a store of every other live pack
// (cache.Cache.StageExpunge), `pack-objects --revs --local` from every tip
// packs what a tip reaches of the inputs and of no other pack — the KEPT
// packs — and `pack-objects --stdin-packs` over the inputs, excluding the kept
// packs and every pack of the store so its name-hash walk stops there, packs
// the rest — the DROPPING packs. Every object of the inputs is in the kept, the
// dropping or the store's packs, so to every reader the begin is a merge. The
// drops are held to what the scratch holds first, since a walk from an object
// an earlier purge took out would refuse the build. The builds are admitted at
// a publication's repack's charge beside a clone's room of the WHOLE
// repository, since the walk reads the whole history; the due scan, which sorts
// nothing, at the room and indexes it holds alone (cache.Cache.AdmitExpunge,
// AdmitExpungeScan, [indexBytes]). Once the begin has applied, EVERY OUTPUT IS
// ADOPTED from the build's scratch (cache.Cache.Adopt, AdoptDropping), not
// downloaded.
//
// THE RESCUE IS VALID AT ANY POSITION AT OR AFTER THE BEGIN, which is what
// makes a crash resumable from the rows alone. A push checked after the begin
// relies on no dropping object, and one checked before it is refused
// `expunging` by the drop epoch, so the only refs that reach a dropping object
// at the rescue's sync are ones a push landed between the scan and the begin.
// So the rescue reads the refs at its own sync, in a scratch whose own packs are
// the dropping packs and whose alternate is the copy's live packs alone
// (cache.Cache.StageRescue), and packs what they reach of the dropping packs,
// `--not` the scan's tips where the cycle still holds them. A copy whose
// objects/ still holds the begin's inputs would hide every object the rescue
// must save, so a rescue over one answers busy (cache.ErrInputsLeaving); so does
// every pass whose copy a swap replaced after its sync pinned it
// (cache.ErrSuperseded), else a scan planned over no packs would judge clean a
// repository it never weighed. The commit's one precondition is its expunge,
// open at its epoch.
//
// WHAT A PURGE LEAVES BEHIND: the commit retires the dropping packs into
// code_packs_retired, and the store deletes their bytes after gitlet.RetireGrace
// and the collector's grace; it is announced once as code_expunged
// (Options.Events). gitlet.ExpungeMinBytes stays PROVISIONAL: BenchmarkExpunge
// placed the bytes freed where a cycle stops being mostly its walk between 24
// and 48 MiB for a 100 MB repository, and could not place it for a gigabyte.
//
// # What it raises
//
// code_compaction_behind, on the maintainer: every listed repository at
// gitlet.MaxPacksPerRepo that this node maintains, read off every pass's
// listing whatever its compaction decided: at the bound a refused push records
// nothing, so the version stands still and a reading taken only of what reached
// a compaction would clear while every push is refused. A publication the node's
// memory refuses counts toward code_maintenance_refused through the cache.
//
// # Measured
//
// The plans' constants are the code domain's, each with its reason and arm where
// defined. [Poll], [InputByteCap]'s half and the cache's CompactBytesPerObject
// and CompactWindowBytes are BenchmarkCompact's; [Due]'s cadence and the
// cache's Publish*BytesPerObject are BenchmarkMidx's (git 2.56.0).
package compact
