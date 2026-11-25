package cache

import (
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/store"
)

// EvictInterval is how often the janitor runs a pass when nothing kicked it.
//
// A MINUTE: whatever fills trash/ or takes the cache over its budget kicks the
// janitor at once, so the tick bounds only what no kick reaches (space given
// back, scratch/ grown by a push) and how far the last use a restart reads
// back can trail the real one (each tick stamps it). A pass under the budget
// over an empty trash/ is two directory reads, and nothing is evicted for
// being idle. It also paces retrying a failed memo listing (memo.go's
// listMemos), which a read's placement waits on under the copy's memo lock:
// a fault the disk keeps costs a measure a minute rather than one a kick.
const EvictInterval = time.Minute

// NodeFetches is how many pack bundles this node reads from the object store
// at once, across every repository's sync.
//
// An ALIAS of httpx.MaxIdleConnsPerHost (32), the bucket transport's warm
// connections, so each GET of a burst of single-part bundles rides one and
// retuning the transport retunes this. It counts objects: a larger bundle's
// extra ranges ride s3obj's spare-part allowance on fresh connections. A slot
// is one GET and its copy buffer, held from the bundle's files existing to its
// reader closing. A node's bound, not the fleet's: 19 to 37 nodes cold-syncing
// one repository reach S3's 5 500 GETs a second a prefix, past which s3obj
// backs off on SlowDown; routing by repository keeps a repository's syncs to
// one or two nodes (docs/concepts/scaling.md).
const NodeFetches = httpx.MaxIdleConnsPerHost

// FetchConcurrency is how many of NodeFetches one repository's sync takes:
// the downloads one round runs at once, largest first.
//
// HALF THE NODE, so a second repository's sync starts with half the node's
// fetches free: queued for the node's slots alone it would wait behind the
// first's longest downloads (up to gitlet.MaxPackBytes, minutes each), while
// every sync gates a clone or a push. The price is a lone cold repository
// taking 1.5 to 2 times as long (BenchmarkColdSync; package doc, "Measured");
// fewer packs, not a wider share, is what shortens a cold sync. A judgment
// call between a joining node's first clone of a large repository and the
// sync behind it; TestASyncLeavesHalfTheNodesFetches holds the ratio.
const FetchConcurrency = NodeFetches / 2

// MaxSnapshotRestarts is how many times one round reads a snapshot again
// from its first page when a later page answers gitlet.ErrSnapshotMoved.
//
// THREE. A restart converges: the first page decides full or delta afresh,
// and a full snapshot answers the sentinel only when the log under it was
// recreated or reanchored, an operator's act. A push between pages does not
// move a snapshot, so three in one round is a fault worth failing on.
const MaxSnapshotRestarts = 3

// MaxFullRounds is how many FULL READS of a repository's rows this node has in
// flight against the estate at once, each holding a slot from its first page
// to its last, first come, first served (read.go, fullRounds); a delta holds
// none (package doc, "A node reads few repositories whole at once").
//
// store.MinReaderConns, EIGHT: the fewest readers a data node opens, all a
// Gitlet node knows of the one it reads. Fewer brings a restarted node up
// nearly twice as slowly; more brings it up no sooner while the fleet's other
// reads wait longer, and sixteen lets four joining nodes push those past a
// second at p99 (BenchmarkColdJoin; package doc, "Measured"). PROVISIONAL:
// crewlet.code.cache.read.page.duration and crewlet.store.pool.wait move it.
// TestFourColdNodesHoldAtMostHalfADataNodesAnswers bounds it.
const MaxFullRounds = store.MinReaderConns

// MidxPacks is how many packs gathered beside the maintainer's index make the
// repository due for another run.
//
// SIXTY-FOUR (BenchmarkMidx stale=k/mode=own; package doc, "Measured"): a pack
// outside the index is outside its bitmap, so a request reaching it walks its
// commits, δ ≈ 0.06 to 0.08 ms a pack, while a run T rewrites the whole index
// (1.16 s at 100 MB, 14.2 s at 1 GB). For q requests a push, a run every M
// pushes costs least at M = √(2T/qδ): 48 and 152 at q = 16. A judgment call on
// q; where pushes come faster, MidxMinInterval and MidxDutyCycle bind first.
// Counted in packs, not objects: a push is a commit or a few, and an import's
// whole history is a first pack no index covers, so it is due at once.
const MidxPacks = 64

// MidxRoundPacks is how many packs a full round must leave before it writes a
// multi-pack-index — without a bitmap — before it answers.
//
// 256, decided by a clone's pack-objects, the request a cold sync is most
// often for and the one the index makes wait (BenchmarkPackLookup; package
// doc, "Measured"): at 256 the index saves it 0.55 s at 100 MB and 11 s at
// 1 GB for writes of 0.13 and 1.3 s; at 128 it saves nothing measurable. The
// window lasts until the maintainer indexes the copy, ten minutes or more on a
// joining node, and what a clone misses there is mostly the maintainer's
// bitmap, which no round writes. It is also the most packs left outside a
// bitmap once memory can no longer hold one (maintain.go's plan).
const MidxRoundPacks = 256

// MidxDebounce is how long a repository's packs must have stood still before
// the maintainer indexes them — unless the oldest of them has waited
// MidxMaxWait — and how often the maintainer looks.
//
// TEN SECONDS: a burst of pushes (a CI run's refs, an import's steps) lands
// within seconds, and an index written mid-burst is due again sooner; ten
// seconds lets it land while the repository a person just pushed to is still
// indexed before most clones that follow. As the tick it bounds a due
// repository's wait for a free slot, and a tick costs a few loads a repository.
const MidxDebounce = 10 * time.Second

// MidxMaxWait is the longest the oldest pack no maintainer's index covers
// waits for one, however recently the newest arrived: the debounce's ceiling.
//
// A repository pushed to more often than every MidxDebounce never stands
// still, and without a ceiling its packs stay outside every index while the
// pushes last. THE MINIMUM INTERVAL, which already lets it be indexed that
// often, so the ceiling adds no work. Safe whatever lands mid-run: a run holds
// a pin, never the round, and a pack added meanwhile is the next run's oldest.
const MidxMaxWait = MidxMinInterval

// MidxMinInterval is the least time from the end of one maintenance run of a
// repository to the start of the next.
//
// TEN MINUTES: a run rewrites the whole index and walks history for the
// bitmap, up to a quarter of an hour for the largest repository the defaults
// admit, so one pushed every few seconds would be rebuilt nonstop; packs
// pushed meanwhile serve from their own indexes (MidxPacks prices each). A
// failed run waits it out too. A run stopped from outside (Stop, a sweep, a
// rebuild's or purge's retire) waits nothing, but a swap's stop (swap.go's
// stopRun) counts as a run: a repository compacted faster than its run lasts
// would otherwise hold a slot forever writing nothing. The cost: that copy
// serves from its packs' own indexes until the next run.
const MidxMinInterval = 10 * time.Minute

// MidxDutyCycle bounds one repository's maintenance to 1/MidxDutyCycle of a
// core's time: a run that took T is followed by the next no sooner than
// MidxDutyCycle × T after it ended, when that is longer than MidxMinInterval.
//
// TEN: a tenth of a core a repository, so a monorepo whose bitmap takes three
// minutes is rebuilt at most every thirty. On four cores the maintenance lane
// runs ONE repository at a time, so one due while the largest runs waits it
// out, and the duty cycle bounds how often. A run passes a minute, where this
// binds, past about 3 to 4 GB of packs; the largest repository the defaults
// admit (20 GiB) runs about a quarter of an hour and is rebuilt at most every
// two and a half (BenchmarkMidx; package doc, "Measured"). A first graph
// computes every filter (graphArgs), stretching the next interval alike.
const MidxDutyCycle = 10

// THE MEMORY OF EACH WHOLE-REPOSITORY PROCESS, an object of its repository:
// what the node's memory budget charges each by (memory.go), beside room for
// one transfer of the same repository (gitproc.TransferBytesPerObject). Each
// is the git process's peak ANONYMOUS memory over BenchmarkMidx's histories
// (package doc, "Measured"); mapped pack pages are not counted, since the
// kernel takes those back under pressure rather than killing anybody. The
// objects are the pack rows' own counts, so a charge is a multiplication,
// never a read of the disk.

// BitmapBytesPerObject is the memory the maintainer's multi-pack-index WITH
// ITS BITMAP holds for every object of its repository.
//
// 600 BYTES: the largest measured share, 560 at 100 MB (388 at 10 MB, 287 at
// 1 GB), rounded up to the next 50. The share falls past 100 MB, so the
// largest repository the defaults admit (20 GiB, some 60 million objects)
// would hold about 15.5 GB on the measured line and is charged 36 GB: room for
// growth past the gigabyte no arm has measured (the 5 GB arm, under
// CREWLET_BENCH_LARGE, would). No fixed part shows at the small end, so none
// is charged. A refused bitmap still leaves an index without one
// (IndexBytesPerObject).
const BitmapBytesPerObject = 600

// IndexBytesPerObject is the memory the multi-pack-index WITHOUT A BITMAP —
// a full round's own, and the maintainer's where the bitmap cannot be had —
// holds for every object of its repository.
//
// 80 BYTES, a fifth past the larger measurement: 66 bytes an object at 100 MB
// and 64 at 1 GB. A merge of the packs' indexes, an entry an object, it
// follows the objects — a quarter of the bitmap's at the gigabyte, which lets
// a node index a repository whose bitmap it cannot hold — beside a small fixed
// part (IndexBaseBytes).
const IndexBytesPerObject = 80

// IndexBaseBytes is the memory a multi-pack-index without a bitmap holds
// WHATEVER its repository's size, charged beside IndexBytesPerObject's (its
// declarations' Write.BaseBytes).
//
// ONE MEBIBYTE: at 10 MB the index held 81 bytes an object, past the 80 it is
// charged; the line through the larger sizes meets an empty repository at
// about 0.6 MB, the merge's own part, which a repository of fewer than about
// 40 000 objects carries past its charge an object. A mebibyte holds it with
// room, under a thirtieth of a gigabyte's index charge.
const IndexBaseBytes = 1 << 20

// GraphBytesPerObject is the memory the commit-graph holds for every object of
// its repository, beside the fixed part its changed-path filters' diffs hold
// (GraphBaseBytes, its declaration's Write.BaseBytes).
//
// 67 BYTES, a fifth past the larger measurement (55 bytes an object at 100 MB,
// 45 at 1 GB, BenchmarkMidx step=whole). It reads every commit, a seventh of a
// source repository's objects, so it is charged by the objects the rows count.
// Computing every filter its commits lack (graphArgs) measured a line of 38.7
// bytes an object plus 95 MiB, the delta base cache GraphBaseBytes charges
// (internal/gitlet/query's own/max-new-filters=all arms); later writes read
// the filters back rather than computing them again.
const GraphBytesPerObject = 67

// TipGraphBytesPerObject is the memory the node's TIP commit-graph layer
// (maintain.go) holds for every object of the TAIL it is written over (the
// packs no installed published layer names), beside GraphBaseBytes and the
// room for one clone of the whole repository (memory.go's chargeOver): a split
// write walks only the commits no layer beneath holds, while the clone beside
// it is of the repository.
//
// 50 BYTES (BenchmarkMidx tail= arms, op=tip-graph/filters=100): 40 to 45
// bytes an object of the tail past the shortest, the larger rounded up to the
// next ten. What computing every filter adds past this is the trees their
// diffs read, which the delta base cache holds up to its cap (GraphBaseBytes).
const TipGraphBytesPerObject = 50

// GraphBaseBytes is the memory a commit-graph write that computes changed-path
// filters holds WHATEVER it covers, charged beside its figure an object (its
// declarations' Write.BaseBytes and a publication's graph layer,
// Cache.AdmitPublication): a filter diffs a commit against its first parent
// through git's DELTA BASE CACHE, which keeps what it read up to
// core.deltaBaseCacheLimit, 96 MiB by default, which nothing the engine runs
// lowers. It grows with the filters and the trees they diff, never past that.
//
// 100 MIB: the cache's 96 and 4 MiB of the write's own, twice the 2 MiB the tip
// held past the cache (internal/gitlet/query's BenchmarkBlame and
// BenchmarkLogPath, tree=deep). A bound on new filters bounds the cache only
// as far as the trees are small: a wide directory fills it at a hundred.
const GraphBaseBytes = 100 << 20

// PublishBytesPerObject is the memory a ROOT publication's multi-pack-index
// layer and its bitmap hold for every object of the WHOLE repository
// (Cache.AdmitPublication), beside room for one clone (memory.go's
// chargeOver), and the figure a repository's maintenance is PLACED by
// (Cache.MaintenanceCharge): a bitmap is O(history), so the layer's own few
// objects say nothing of it. A layer over a base is charged
// PublishTailBytesPerObject; placement uses the root's, since the maintainer
// must be able to publish one (its first, or after the store loses a part).
//
// 600 BYTES (BenchmarkMidx step=publish, merge=root-write), what own mode's
// whole index with its bitmap holds. docs/concepts/scaling.md's 12 GB figure
// is extrapolated until BenchmarkBigNode's node=publish measures it.
const PublishBytesPerObject = 600

// PublishTailBytesPerObject is the memory a publication's multi-pack-index
// layer and its bitmap hold for every object of the WHOLE repository when the
// layer is written over a BASE (every publication after a repository's
// first), charged in PublishBytesPerObject's place (Cache.AdmitPublication):
// git still walks the whole history for the layer's bitmap, but holds a
// fraction of a root's.
//
// 350 BYTES (BenchmarkMidx step=publish/layer=tail): 312 bytes an object at
// 100 MB, the largest, rounded up to the next 50. The root's 600 would reserve
// some 50 GB of a large node's background budget for the largest repository's
// routine publication; this charges 35 GB, still past the gigabyte's figure
// extrapolated, since no arm past the gigabyte has measured a tail layer.
const PublishTailBytesPerObject = 350

// GraphLayerBytesPerObject is the memory a publication's commit-graph layer
// (`commit-graph write --stdin-packs --split=no-merge --changed-paths`, every
// filter computed, since it runs once for the fleet) holds for every object of
// the LAYER it covers (Cache.AdmitPublication), beside GraphBaseBytes and room
// for one clone of the repository: a split layer reads only its own packs'
// commits.
//
// 350 BYTES (BenchmarkMidx step=publish/layer=root): 317 bytes an object of
// the layer at 100 MB and 92 at 1 GB, the larger rounded up to the next 50.
// Part of that is the delta base cache's, which GraphBaseBytes charges beside
// it, so a small layer is charged it too.
const GraphLayerBytesPerObject = 350

// FsckBytesPerObject is the memory Verify's connectivity check (fsck
// --connectivity-only) holds for every object of every object directory it is
// handed, so while an expunge is open the dropping view's packs count as much
// as the live ones (Cache.AdmitVerify), though a clone beside it sends none.
//
// 192 BYTES, a fifth past the gigabyte's 160 (390 at 100 MB, a fixed part of
// some 70 MiB showing). The largest repository the defaults admit holds some
// 11 GB, as much as background work is ever given, so a check is held to the
// same budget, charged at its admission.
const FsckBytesPerObject = 192

// CompactBytesPerObject is the memory a compaction's build (one
// `pack-objects --stdin-packs` over the packs it merges, internal/gitlet/compact)
// holds for every object of those packs, charged at admission
// (Cache.AdmitCompaction) beside room for one transfer of as many objects
// (memory.go's chargeOver, over the inputs alone) and CompactWindowBytes.
//
// 550 BYTES (BenchmarkCompact history=source, BenchmarkMidx
// step=publish/layer=root): 493 to 523 bytes an object at 100 MB whatever the
// input count, rounded up to the next 50. Any fixed part is folded into the
// objects, so a large merge is overcharged, never under, and a small one's is
// far under its CompactWindowBytes. Charged by the inputs alone: a charge
// growing with the repository would refuse a large one's merges and pushes.
const CompactBytesPerObject = 550

// CompactWindowBytes is the delta search a compaction's build is charged
// beside its objects: what its pack-objects holds around its window, whatever
// the objects number.
//
// SIX OF THE EPOCH'S LARGEST BLOB (BenchmarkCompact blobs=max_blob/inputs=8):
// eight packs, each a 50 MiB revision deltified against the one before (the
// worst composition a window meets), held 5.4 blobs. pack.windowMemory holds
// the window itself to two blobs (compact's PackConfig, at pack.threads=1);
// the rest is the object being deltified, its delta and the bases it reads,
// which that bound does not cap. Rounded up to a whole blob and scaled by the
// epoch's limit, since what it holds follows the blobs' size.
func CompactWindowBytes(limits gitlet.Limits) int64 { return 6 * limits.MaxBlobBytes }

// RowReadAttempt bounds a read of a repository's rows the cache makes for its
// own work, with no asker's deadline to inherit: one estate read attempt
// (estate.ReadAttempt), which holds a linearizable read's barrier through an
// election. This package imports no engine package, so estate's
// TestTheCachesRowReadIsOneReadAttempt holds the two equal.
const RowReadAttempt = 15 * time.Second

// VerifyHashPerMiB is what a check's hashing is given for each mebibyte of
// the bundles it reads (CheckBudget), beside the connectivity check's own
// gitproc.PackWorkDeadline.
//
// A TENTH OF A SECOND: a hundred times the measured 1 ms a mebibyte
// (BenchmarkVerify op=hash, SHA extensions, page cache) and five times the
// slower of a cold 50 MB/s disk (20 ms) or a CPU without the extensions: room
// for a disk shared with the store, the broker and clones, without letting a
// check wedged on a disk hold its slot and the operator's sweep for days.
const VerifyHashPerMiB = 100 * time.Millisecond

// CheckPieceBytes is how much of a file a hash reads in one slot of the read
// lane (gitproc.Pool.HoldRead) before giving it back and queuing again: a
// hint's confirmation (hint.go) hashes up to MaxPackBytes with its index, and
// held whole that is one core for 1.3 s warm, about 11 s cold at 125 MB/s,
// with every read on a node of few slots behind it.
//
// 64 MiB: about 65 ms of hashing with SHA extensions, 0.2 s without and
// 0.54 s cold at 125 MB/s, the most a queued read waits behind it. Smaller,
// and a hash on a saturated lane progresses less for the same waits; larger,
// and each read waits longer.
const CheckPieceBytes = 64 << 20

// MaintenanceMemoryPerCore is how much memory the maintenance runs BESIDE THE
// LARGEST ONE IN FLIGHT may be charged at together, for every core the node's
// memory backs at gitproc.CoreMemory once the engine's pools beside Gitlet
// are kept (gitproc.Pool.Reserved): a pass starts a run only while that
// holds, so a joining node's largest repositories, due together, are indexed
// a few at a time. The largest run is not charged here (its bitmap is the
// price of serving it); the node's memory budget (memory.go) bounds it.
//
// A QUARTER OF A GIBIBYTE A CORE, of the ~1.1 GiB a core the transfer lane
// leaves inside CoreMemory; the rest is ServingMemoryPerCore. A JUDGMENT CALL:
// larger clears a joining node's backlog sooner at the risk of the store's and
// broker's memory; smaller runs the largest repositories one after another.
const MaintenanceMemoryPerCore int64 = 256 << 20

// ServingMemoryPerCore is what a Gitlet node keeps, for each core of its git
// pool its memory backs once what the engine keeps beside Gitlet is kept
// (internal/nodebudget's lines), for the broker, the engine's own heap and the
// rest of the store: what the background work's budget leaves out.
//
// WHAT gitproc.CoreMemory LEAVES, 896 MiB, after the transfer lane's default
// share and MaintenanceMemoryPerCore: a node with CoreMemory a core gives
// maintenance exactly MaintenanceMemoryPerCore, a larger one everything past
// this (docs/concepts/gitlet.md works the sizes through, held by
// TestTheConceptPageSizesAGitletNodeByTheCachesOwnFigures), and one short of
// memory keeps it only for the cores its memory backs. Derived, not measured:
// nothing that shares it is held to a figure of its own.
const ServingMemoryPerCore = gitproc.CoreMemory - gitproc.TransfersPerProcess*gitproc.TransferMemory -
	MaintenanceMemoryPerCore

// RebuildCooldown is how soon after a repository's last rebuild ATTEMPT
// another is refused, when the first of a line of attempts did not end the
// failure and something has written into the repository's cache since; each
// attempt after it doubles the wait, up to RebuildBackoffCap.
//
// TEN MINUTES: a rebuild re-downloads the whole repository, and one that did
// not end a failure rarely ends it twice (the damage is the store's or the
// rows', which `crewlet gitlet verify` names), yet ten minutes retries a
// transient store fault within the time an operator takes to look.
// crewlet.code.cache.rebuild.refusals{rule=cooldown} counts each refusal, and
// code_cache_rebuild_refused names the repository.
const RebuildCooldown = 10 * time.Minute

// RebuildBackoffCap is the longest the window after a rebuild attempt grows
// to, however many attempts in a row have not ended a repository's failure —
// and how long a cache holding exactly what that attempt read is refused
// another (Cache.Rebuild's `unchanged` rule).
//
// A DAY (reached at the ninth attempt in a row): damage no rebuild ends costs
// one whole download a day per Gitlet node a failing request reaches, not the
// cooldown's 144. `unchanged` holds to the day rather than the next push: the
// store can come to hold what it lacked and a disk can damage what a rebuild
// wrote, neither visible in advance, so such a fault is retried within a day
// without anybody acting, at the price the cap already sets.
const RebuildBackoffCap = 24 * time.Hour

// RebuildQuiet is how long a repository goes with no rebuild ASKED of it —
// refused or not — before the next ask starts a new line of attempts: the
// count its window doubles by goes back to none, and its cache is no longer
// taken to hold what the last attempt read.
//
// THE CAP, AND NO SHORTER: quiet is the cache's only sign a rebuild ENDED a
// failure (it never sees a request succeed), and an ask after a quiet is met
// at once. A quiet of RebuildCooldown would rebuild persistent damage at asks
// just over ten minutes apart, the 144 a day the cap prevents; at the cap it
// costs at most one a day, while damage arriving a day after the last ask is
// met as the first attempt it is.
const RebuildQuiet = RebuildBackoffCap

// ScrubInterval is how long after its bytes were last held to their
// checksums a USED copy is scrubbed again (scrub.go): every .pack against its
// trailer and every index file by the check's rule, behind the serving.
// Damage no git process reports (a pack body sent verbatim, a bitmap failing
// clones silently) is served until the next scrub: at most this long for a
// copy used meanwhile, and until its first use after for one left idle.
//
// A DAY, RebuildQuiet's window for a disk to damage what a rebuild wrote. A
// JUDGMENT CALL against cost: 1.13 times a copy's pack bytes at about 1 s a
// gigabyte warm (BenchmarkScrub), so 1 TB used daily is 14 to 23 core-minutes
// a day, at most 10.5% of a 125 MB/s volume cold. A shorter one finds sooner
// only damage a restart, a report or `crewlet gitlet verify` does not reach.
const ScrubInterval = RebuildQuiet

// ScrubCooldown is how long after a scrub that found nothing a report (a pack
// or object a git process could not read, a pack named in words, a crash)
// makes no scrub due (report.go), WHATEVER IS WRITTEN MEANWHILE: it alone
// bounds a request that can provoke reports beside a busy repository.
//
// AN HOUR: the largest repository the defaults admit (21.5 GB of packs) costs
// at most a scrub an hour at a report's word, 0.5 to 0.8% of a core warm or
// 5.4% of a 125 MB/s volume cold, while damage the last scrub could not have
// seen waits at most an hour, beside the copy's daily scrub.
const ScrubCooldown = time.Hour

// MaxProvenanceRuns is the most runs an index of pushes holds (provenance.go):
// a commit's lookup binary-searches every run, newest first, so the bound is
// what a lookup costs at worst. The backfill merges before an index reaches
// it, and a round's fold at it merges its two cheapest runs first.
//
// 32: the largest of {8, 16, 32, 64} at which a lookup that misses costs under
// 1% of the warm tree read it decorates (3.1 ms, the query package's
// Measured), so 31 µs: 15.5 µs measured over four million commits
// (BenchmarkProvenance), some 27 µs extrapolated to the large model's 54
// million, where 64 runs pass it. Geometric merging keeps about ten runs there
// (ProvenanceMergeWidth), so the bound is reached only while the backfill lags.
const MaxProvenanceRuns = 32

// ProvenanceMergeFactor is how much of a merge one run may be: a span of runs
// the backfill merges is BALANCED, none of them holding more than this many
// times the pushes of the others together (mergeDue).
//
// TWO, git's own geometric factor (`git repack --geometric=2`): no run is
// rewritten for fewer than half its own pushes again, so runs grow
// geometrically — O(log n) runs, each push written O(log n) times — and a
// large run beside a stream of small ones is never rewritten for each.
const ProvenanceMergeFactor = 2

// ProvenanceMergeWidth is how many adjacent runs one merge of the backfill
// merges (mergeDue): the cheapest balanced span of this many.
//
// EIGHT. A fold writes one delta's pushes, often one, and each merge is four
// syncs and a state write on every node holding the copy. Merging pairs merged
// on nearly every push and filled the index to MaxProvenanceRuns under
// bursts. Simulated over one-push folds and bursts up to five thousand
// (TestTheMergePolicyKeepsAnIndexSmallAndMergesItSeldom holds three), eight
// merges once in seven one-push folds, rewrites each push 6 to 15 times and
// held at most 24 runs; ten held 27, twelve 31, and four and six merge more
// often. Eight is the widest that stays eight runs clear of MaxProvenanceRuns.
const ProvenanceMergeWidth = 8

// BackfillIdle is how long the backfill waits after each page it reads, as a
// multiple of that page's own duration and of the fleet's ratio of live Gitlet
// nodes to live data nodes (backfill.go): every Gitlet node backfills from the
// data nodes, so its share of them is the ratio.
//
// TWO: the fleet's backfills then hold about a third of one reader of each
// data node — the code retention pass's share of the write lane
// (gitlet.RetentionLaneShare), applied to a read — however many Gitlet nodes
// share them, and the serving keeps the other two thirds.
const BackfillIdle = 2

// HorizonEvery is how often the backfill asks the rows for the oldest push
// they keep — an index's horizon (provenanceState.Oldest) — and drops what
// it holds below it: runs wholly below at once, and records below it at
// their next merge.
//
// A DAY: the rows drop pushes past gitlet.PushHistory (181 days) as the log
// moves, so a day's lag holds at most a day's pushes past them — under 1% of
// the index — for one page of one row a day a repository.
const HorizonEvery = 24 * time.Hour

// BackfillRetry and BackfillRetryCeiling bound the backfill's waits after a
// failed step (a page the estate could not serve, a run it could not write or
// merge), each a window of the failing repository's own while the others go
// on (backfill.go); its wait after failures across repositories; and its wait
// on a fleet whose live nodes could not be counted (Options.Fleet). Each
// doubles, jittered, and the step is asked again, never paced on a guess.
//
// FIVE SECONDS TO FIVE MINUTES: a restarted data node or router answers within
// seconds, and nobody waits on a backfill, so a long outage costs one read
// every five minutes a node; the code reconciler's shape (engine's
// codeReposRetryBase).
const (
	BackfillRetry        = 5 * time.Second
	BackfillRetryCeiling = 5 * time.Minute
)

// TagsRetry is how long a tag index set aside or never built waits after a
// failed step before a round builds it again: at most one whole build a
// minute, about a second at 100 000 tags (BenchmarkTagIndex step=whole).
const TagsRetry = time.Minute

// MappedRunShare bounds the runs of the indexes of pushes — and the files of
// the code indexes, which share the bound (codeindex.go) — the cache holds
// mapped at once (runmaps.go): the process's vm.max_map_count, read at Open,
// over this.
//
// A QUARTER: every mapping the process makes counts against the one sysctl,
// and past it every one fails, the runtime's heap growth with them, fatally.
// The rest of the process holds a few thousand, so three quarters leave it ten
// times that; a quarter of Linux's default, 16 382, keeps some three thousand
// copies' indexes mapped (five runs each), while one node's readers, each
// holding one index (at most codeindex.MaxSegments+1 files) for one read, hold
// a few hundred.
const MappedRunShare = 4

// DefaultMapCount is Linux's default vm.max_map_count (DEFAULT_MAX_MAP_COUNT,
// 65 536 less 6): the count a darwin build holds itself to (mapcount_other.go)
// and a Linux node whose kernel answers none (Open logs it).
const DefaultMapCount = 65530
