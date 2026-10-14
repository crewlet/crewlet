package cache

import (
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/httpx"
)

// EvictInterval is how often the janitor runs a pass when nothing kicked it.
//
// A MINUTE, because the tick is not where the urgent work happens: whatever
// moves something into trash/ — a release of the last pin on a retired
// incarnation, the boot's own clear — and whatever takes the cache over its
// budget — a round that grew it, the release of the last pin on a repository
// too large to keep — kicks the janitor at once. So the tick only bounds how
// long anything a kick did not reach waits — space given back, scratch/ grown
// by a push measured against the budget — and how far the last use a restart
// reads back from the disk can trail the real one (each tick stamps it on
// every repository used since the tick before). A pass under the budget over
// an empty trash/ is two directory reads, and a stamp is one metadata write
// per repository used in the minute, so a minute costs nothing; disk the node
// has already decided to give back is never held for longer than an operator
// watching it would notice, and a minute of last use is nothing against the
// hours a budget's eviction order turns on. Nothing is evicted for being
// idle, so the tick decides no eviction of its own.
const EvictInterval = time.Minute

// NodeFetches is how many pack bundles this node reads from the object store
// at once, across every repository's sync.
//
// httpx.MaxIdleConnsPerHost (32), the one outbound transport's warm
// connections to a host, and an ALIAS rather than a number of its own:
// retuning the transport retunes it. Every GET of a cold-sync burst of
// bundles of one part — most of them: the median push's is near 150 KB, and
// s3obj reads up to PartBytes (8 MiB) as one request — rides a connection the
// transport keeps open, rather than paying a TCP and TLS handshake for one it
// then closes. And it is what a node joining a fleet asks of the store at
// once: 32 small reads in flight at 30 ms to the first byte are about a
// thousand GETs a second, a fifth of what S3 serves one prefix.
//
// IT COUNTS OBJECTS, NOT REQUESTS. A bundle larger than one part is read by
// s3obj as up to four ranges at once, the extra ones on buffers from the
// node's one spare-part allowance, which uploads and downloads share — so
// the node holds at most NodeFetches plus that allowance of bucket requests,
// and the excess costs a fresh connection rather than a refusal
// (MaxIdleConnsPerHost bounds the IDLE pool, not the active one). There is
// no second limiter: the allowance already bounds what a large read takes.
//
// A SLOT IS ONE GET AND ITS COPY BUFFER, and nothing more: the buffer is
// handed out with the slot and back with it, the slot is taken only once the
// bundle's files exist, and it goes back the moment the store's reader
// closes, before the install's checks, syncs and renames. So the node's
// download memory is NodeFetches copy buffers — 8 MiB — however many syncs
// wait for one, and its GET rate is never spent on its disk.
const NodeFetches = httpx.MaxIdleConnsPerHost

// FetchConcurrency is how many of NodeFetches one repository's sync takes:
// the downloads one round runs at once, largest first.
//
// HALF THE NODE, so a second repository's sync starts with half the node's
// fetches free. Waiting in line for the node's slots alone would not do it:
// a second sync would get in only when one of the first's downloads ended,
// and largest first makes those its LONGEST — a bundle up to code.MaxPackBytes
// holds its slot for minutes on a shared link — while every sync gates a
// person's clone or a push's own step. The price is one cold repository alone
// on a node reading at half the node's width, MEASURED (BenchmarkColdSync,
// the S3 fake at 30 ms to the first byte and 80 MB/s a connection, four
// vCPUs): 2 000 bundles of 150 KB arrive at 448 a second at sixteen against
// 680 at thirty-two, so 10 000 pushes' packs take about 22 seconds rather
// than 15. Sixteen is bound by its GETs: sixteen of the 33 ms each took allow
// 484 a second, while this node's own side of a bundle — its files created,
// written, synced and renamed — ran at 878 a second from a store in memory.
// Thirty-two is bound by both: its GETs took 40 ms, the fake's server sharing
// the four cores the node ran on (a real bucket's does not), which allows 794
// a second against the node's own 948. One bundle of 512 MiB among 200 makes
// the two the same (2.5 against 2.6 s): its own ranged read is the round's
// tail. On the broker's object store at three replicas (natsobj's measured
// section), a 64 KiB get that keeps its pull took about 4 ms, a pull the
// cluster dropped cost a second (2 to 124 of 3 000 gets, with the machine's
// load), and gets averaged 7.2–7.3 ms in runs that lost eight — so sixteen in
// flight would come to roughly 2 000 to 4 000 small bundles a second, past
// what the node's own side takes, a second more for each lost pull:
// arithmetic from one get at a time, not a measurement of sixteen at once.
// The share's reason holds on both backends, and a joining node's first clone
// of a large repository is what it costs — a judgment call between that clone
// and the second repository's sync behind it.
const FetchConcurrency = NodeFetches / 2

// MaxSnapshotRestarts is how many times one round reads a snapshot again
// from its first page when a later page answers code.ErrSnapshotMoved.
//
// THREE. A restart converges: the first page decides full or delta afresh,
// and a full snapshot answers the sentinel only when the log under it was
// recreated or reanchored — an operator's act, not traffic. A push between
// pages does not move a snapshot (its tail page serves what the push added),
// so three in one round is a fault worth failing on, never load.
const MaxSnapshotRestarts = 3

// MidxPacks is how many packs gathered beside the maintainer's index make the
// repository due for another run.
//
// SIXTY-FOUR, MEASURED (BenchmarkMidx's stale arms: pushes of 4 KiB landed
// beside the maintainer's index over the 100 MB and 1 GB histories; git
// 2.43, four vCPUs, six counts). A pack outside the index is outside its
// bitmap: a request that reaches it — a clone, a fetch of anything pushed
// since — opens it and walks its commits and trees rather than reading them
// off the bitmap, and that walk is what each such pack costs a request.
// Counting a clone's objects took 4.5 ms with no pack beside the index and
// 12.0 ms with 128 over the 100 MB, 6.9 and 17.9 ms over the gigabyte —
// about 0.06 and 0.09 ms a pack — while a clone's whole pack-objects (1.1 s
// and 10.7 s) moved by less than its own noise up to 128. A run rewrites the
// whole index and bitmap: 1.6 s over the 100 MB, 21 s over the gigabyte. So
// a repository run every M pushes pays a run's T every M pushes and each of
// its q requests a push an average of M/2 packs' δ — least at M = √(2T/qδ):
// for sixteen requests a push (a pipeline's fetches, the clones of the
// coding runs a push starts), 58 at 100 MB and 173 at a gigabyte. Sixty-four
// is the least at 100 MB, and runs a gigabyte's index more often than its
// least to spare each request a few milliseconds, a third of a core-second
// a push; where pushes come faster, MidxMinInterval and MidxDutyCycle bind
// first. A judgment call on q: at four requests a push the least doubles,
// and a count past it costs a request a twentieth of a millisecond a pack.
// Counted in packs rather than objects, because a push here is a seat's
// commit or a few: the one push that carries a whole history, an import, is
// a repository's first pack, which no index covers, so it is due at once.
// Eight, the count this replaced, ran the whole index every eight pushes to
// hold a request's walk under half a millisecond.
const MidxPacks = 64

// MidxRoundPacks is how many packs a full round must leave before it writes a
// multi-pack-index — without a bitmap — before it answers.
//
// 256, MEASURED (BenchmarkPackLookup: a source repository's history split
// into ever more packs, as one push a pack with nothing compacting them
// leaves a repository — 100 MB, 296 000 objects and 42 000 commits, and a
// gigabyte, 3.0 million and 424 000; git 2.43, four vCPUs). The index is a
// merge of the packs' own indexes, so what it costs follows the objects far
// more than the packs — 0.13 s over the 100 MB at 128 packs and at 256, 1.3 s
// over the gigabyte at 128, 256 and 512 — while what it saves a request
// follows the packs. The threshold is decided by a clone's pack-objects: the
// request a cold sync is most often for (a coding run's fresh sandbox makes
// one first), and the one the index makes wait. Six runs of each, medians and
// spreads: at 256 packs the index saves that clone 0.55 s over the 100 MB
// (12.9 s against 12.4; 12.6–13.5 and 11.9–12.9) and 11 s over the gigabyte
// (151 against 140; 145–160 and 133–143), so the cold clone alone pays the
// index back at both sizes, four times over and eight. At 128 it saves
// nothing either size shows — the round's index the slower median both times,
// 10.1 s against 10.2 (10.0–10.8 and 9.6–10.7) and 110 against 111 (108–111
// and 104–116) — so there the cold request waits for an index that saves it
// nothing, and only the requests after it until the maintainer's index lands
// could pay it back: a fetch of the newest four commits saves 0.7 ms over the
// 100 MB and 4.7 ms over the gigabyte, Missing over 1 000 commits 1 and 5 ms,
// so it takes 130 to 190 of them over the 100 MB and 260 to 280 over the
// gigabyte. At 512 a clone saves 3 s and 69 s, at 32 nothing (these and the
// fetches an earlier series, of six runs over the 100 MB and three over the
// gigabyte). The refs' peel the index is written before decides nothing: it
// saves less than the index costs below thousands of tagged packs
// (BenchmarkPeel: 0.5 ms over 256 tags in as many packs and 12 ms over a
// thousand, against writes of 11 and 35 ms).
//
// That window is not "a debounce later": the maintainer indexes a repository
// once its packs have stood still for MidxDebounce and the runs ahead of it
// have ended — on four cores ONE run at a time (maintenanceRuns), most packs
// uncovered first, each at 16 to 30 s a gigabyte (MidxDutyCycle) — so on a
// node that just joined, every repository it syncs due at once, the last
// waits for all of them: a quarter of an hour or more for 50 GB. What a clone
// misses in that window is the maintainer's BITMAP far more than this index —
// 1.4 s with it against 9.6 at 128 packs over the 100 MB, 14 s against 111
// over the gigabyte — which no round writes, since its walk is no request's
// to wait for.
//
// It is also the most packs that gather outside a bitmap the node wrote while
// its memory could hold one, once it no longer can, before the index without
// a bitmap replaces it (plan, maintain.go): the most this cache leaves outside
// an index anywhere else.
const MidxRoundPacks = 256

// MidxDebounce is how long a repository's packs must have stood still before
// the maintainer indexes them — unless the oldest of them has waited
// MidxMaxWait — and how often the maintainer looks.
//
// TEN SECONDS: a burst of pushes — a CI run's branches and tags, an import's
// steps — lands one pack each within seconds of one another, and an index
// written between two of them covers one pack fewer than it could and is due
// again sooner. Ten seconds lets such a burst land before the run that covers
// it, while the repository a person just pushed to is indexed before most of
// the clones that follow it. As the maintainer's tick it bounds how long a due
// repository waits while a maintenance slot is free — a debounce and a tick
// at most; with every slot running, until the first run ends, which kicks the
// maintainer — and a tick over a node's repositories is a few loads each, so
// looking every ten seconds costs nothing.
const MidxDebounce = 10 * time.Second

// MidxMaxWait is the longest the oldest pack no maintainer's index covers
// waits for one, however recently the newest arrived: the debounce's ceiling.
//
// A repository pushed to more often than every MidxDebounce — a monorepo
// under CI, many seats pushing at once — never stands still, and a debounce
// with no ceiling left its packs outside every index for as long as the
// pushes kept coming: at a push every five seconds, 720 an hour, each one
// more .idx searched by every lookup the index does not answer. THE MINIMUM
// INTERVAL, because that is already how often the interval lets a repository
// be indexed: the ceiling gives one that never stands still the runs one that
// does would have, and so adds no work the interval does not allow. Safe
// whatever lands during the run it starts — a run holds a pin, never the
// repository's round — since a pack a delta adds meanwhile is simply not in
// that index, and is the oldest the next run covers.
const MidxMaxWait = MidxMinInterval

// MidxMinInterval is the least time from the end of one maintenance run of a
// repository to the start of the next.
//
// TEN MINUTES: a run rewrites the repository's whole index and walks its
// history for the bitmap, which for the largest repository the defaults admit
// is a quarter of an hour, so a repository pushed to every few seconds would
// otherwise be rebuilt continuously. In between, the packs pushed since are
// served from their own indexes beside it — never a missing index — and that
// is the bound on how many stand outside it: the pushes of one interval (or
// of MidxDutyCycle times the last run, when that is longer), the run's own
// duration and a tick. At a push every five seconds and runs of seconds,
// about 120 packs, each one more walk of its own commits and trees for a
// request that reaches it — a twentieth of a millisecond or so (MidxPacks).
// A failed run waits it out too, so a repository whose run keeps failing
// costs a run every ten minutes rather than every tick; a run stopped from
// outside (Stop, a full round's sweep, a retire) is no run, and waits
// nothing.
const MidxMinInterval = 10 * time.Minute

// MidxDutyCycle bounds one repository's maintenance to 1/MidxDutyCycle of a
// core's time: a run that took T is followed by the next no sooner than
// MidxDutyCycle × T after it ended, when that is longer than MidxMinInterval.
//
// TEN: a tenth of a core a repository, so a monorepo whose bitmap takes three
// minutes is rebuilt at most every thirty, however fast it is pushed to. The
// node's share is the maintenance lane's — half its cores — and a pass runs
// repositories in every slot of it but the one kept for a person's verify,
// each in a slot of its own and as many as MaintenanceMemoryPerCore holds
// beside the largest, so one repository's long run holds one slot while the
// others due run beside it — and on four cores that is the lane's ONE run, so
// a repository due while the largest runs waits out that run, and the duty
// cycle is what bounds how often. BenchmarkMidx confirms both bounds over a
// source repository's composition, its commits a seventh of its objects —
// what both writes follow, the commit-graph reading every commit and the
// bitmap selecting and walking them: a run takes 16 s a gigabyte of packs at
// 100 MB (1.6 s; 296 000 objects, 42 000 commits), 21 s at 1 GB (3.0 million,
// 424 000) and 30 s at 5 GB (152 s; 14.8 million, 2.1 million), growing
// faster than the bytes. So a run passes a minute — where the duty cycle
// starts to bind — past about 3 GB, and the largest repository the defaults
// admit (code.DefaultRepoMaxBytes, 20 GiB), at the 5 GB arm's rate and its
// growth, runs about a quarter of an hour and is rebuilt at most every two
// and a half; under 3 GB a repository is rebuilt at most every
// MidxMinInterval.
const MidxDutyCycle = 10

// THE MEMORY OF EACH WHOLE-REPOSITORY PROCESS, an object of its repository —
// what the node's memory budget charges each by (memory.go), beside room for
// one transfer of the same repository (gitproc.TransferBytesPerObject). Each
// MEASURED by BenchmarkMidx's memory arms: git 2.43, the peak ANONYMOUS memory
// of the one git process, sampled every millisecond from /proc, over
// BenchmarkMidx's histories — a source repository's composition, commits a
// seventh of the objects, in 64 packs — of 100 MB (296 000 objects) and a
// gigabyte (2.97 million). The packs' own pages a process maps in are not
// counted: the kernel takes those back under pressure rather than killing
// anybody. The objects are the packs' rows' own counts, so a charge is a
// multiplication, never a read of the disk.

// BitmapBytesPerObject is the memory the maintainer's multi-pack-index WITH
// ITS BITMAP holds for every object of its repository.
//
// 1 000 BYTES: it held 1 985 MiB over the gigabyte — 702 bytes an object — and
// 136 MiB over 100 MB (484), the bitmap's walk keeping more of the history at
// once the more of it there is: about 220 bytes an object more for every
// tenfold. At that growth the largest repository the defaults admit
// (code.DefaultRepoMaxBytes, 20 GiB, some 60 million objects) holds about
// 990 an object — some 60 GB for one bitmap, which few nodes have to give,
// and why a refused bitmap leaves the index without one rather than nothing
// (IndexBytesPerObject). 320, the figure this replaced, was 255 bytes an
// object over the gigabyte sampled more coarsely, and fell short of the
// bitmap's peak by a factor of almost three: on a node held to it, a run the
// budget admitted could take three times what the budget kept for it.
const BitmapBytesPerObject = 1000

// IndexBytesPerObject is the memory the multi-pack-index WITHOUT A BITMAP —
// a full round's own, and the maintainer's where the bitmap cannot be had —
// holds for every object of its repository.
//
// 80 BYTES, a fifth past the larger measurement: 182 MiB over the gigabyte
// (64 bytes an object) and 18.5 MiB over 100 MB (66). It is a merge of the
// packs' indexes, an entry an object, so it follows the objects and nothing
// else — a ninth of the bitmap's at the gigabyte and less beyond it, which is
// what lets a node index a repository whose bitmap it cannot hold.
const IndexBytesPerObject = 80

// GraphBytesPerObject is the memory the commit-graph holds for every object of
// its repository.
//
// 56 BYTES, a fifth past the larger measurement: 112 MiB over the gigabyte
// (39.5 bytes an object) and 12.7 MiB over 100 MB (45). It reads every commit,
// a seventh of a source repository's objects, so it is charged by the objects
// the rows count, as the rest are.
const GraphBytesPerObject = 56

// FsckBytesPerObject is the memory Verify's connectivity check (fsck
// --connectivity-only) holds for every object of its repository.
//
// 192 BYTES, a fifth past the larger repository's figure: 452 MiB over the
// gigabyte (160 bytes an object) and 110 MiB over 100 MB (390, a fixed part of
// some 70 MiB showing). A check of the largest repository the defaults admit
// holds some 11 GB — as much as the node's background work is ever given, so
// it is held to the same budget (Cache.Verify).
const FsckBytesPerObject = 192

// MaintenanceMemoryPerCore is how much memory the maintenance runs BESIDE
// THE LARGEST ONE IN FLIGHT may be charged at together, for every core the
// node's memory backs at gitproc.CoreMemory a core (the read lane's slots,
// fewer where the memory is short of them): a pass starts a run only while
// that holds, so a node that has just joined, whose largest repositories
// fall due together, indexes them a few at a time rather than all at once.
//
// THE LARGEST RUN IS NOT CHARGED AGAINST IT. A repository's bitmap is the
// price of serving it — a node that serves a repository of N objects holds
// N × BitmapBytesPerObject once to write it, at whatever moment it is
// written — so what the allowance bounds is the multiplication, up to a
// lane's worth of the largest repositories together, not the largest one.
// What bounds THAT is the node's MEMORY BUDGET (memory.go): everything in
// flight, the largest run included, is held to it together, and a step whose
// charge alone is past the budget never starts on the node at all.
//
// A QUARTER OF A GIBIBYTE A CORE: gitproc.TransfersPerProcess holds the
// transfer lane under 896 MiB a core inside the 2 GiB a vCPU of the
// compute-optimised instance shapes (gitproc.CoreMemory), leaving about
// 1.1 GiB a core for the store, the broker, the engine and the read lane's
// processes; a quarter of a GiB of that is the maintainer's, and the rest is
// ServingMemoryPerCore. On a four-core node it is 1 GiB — the bitmaps of
// some 860 000 objects beside the largest, or every slot of repositories of
// typical size (under 100 000 objects: 118 MiB each with the room for a
// transfer beside them) at once — and it binds only when several
// repositories of hundreds of thousands of objects are due together. A
// JUDGMENT CALL: larger lets a joining node's backlog clear sooner at the
// risk of the memory the store and the broker need; smaller runs the largest
// repositories one after another, each holding back the rest of the backlog
// behind it for as long as its bitmap takes.
const MaintenanceMemoryPerCore int64 = 256 << 20

// ServingMemoryPerCore is what a git node keeps, for each core of its git
// pool its memory backs, for the store, the broker, the engine and the read
// lane's processes: with the transfer lane's, what the background work's
// budget leaves out.
//
// WHAT gitproc.CoreMemory LEAVES, 896 MiB: a core's 2 GiB less the transfer
// lane's share at its default four transfers (gitproc.TransfersPerProcess ×
// gitproc.TransferMemory) and less the maintainer's own
// (MaintenanceMemoryPerCore). So at the pool's default lanes a node gives its
// maintenance MaintenanceMemoryPerCore for each core its memory backs — the
// share every figure here was sized with — and a node with more memory than
// CoreMemory a core everything past what its other work is sized to hold:
// 16 GiB on four cores gives 9 GiB, a bitmap of up to some 7.7 million
// objects with room for a clone of the repository beside it, an index without
// one of up to some 30 million (docs/concepts/code-host.md works these
// through, and TestTheConceptPageSizesAGitNodeByTheCachesOwnFigures computes
// them). And a node with less — 16 GiB seen beside 64 cores, a container
// limited in memory and not in CPU — keeps it only for the eight cores its
// memory backs, as its pool sized its transfer lane, so it gives its
// maintenance 2 GiB rather than nothing. Derived rather than measured, as the
// lanes' own bounds are: the store's page caches, the broker and the engine
// share it with the read lane's processes, and none of them is held to a
// figure of its own.
const ServingMemoryPerCore = gitproc.CoreMemory - gitproc.TransfersPerProcess*gitproc.TransferMemory -
	MaintenanceMemoryPerCore

// RebuildCooldown is how soon after a repository's last rebuild ATTEMPT
// another is refused, when the first of a line of attempts did not end the
// failure and something has written into the repository's cache since; each
// attempt after it doubles the wait, up to RebuildBackoffCap.
//
// TEN MINUTES: a rebuild re-downloads the whole repository — a GET a push —
// and one that did not end a failure rarely ends it a second time: the
// damage is then the object store's or the rows', which `crewlet code verify`
// names, so a request failing again inside the window is answered at once
// rather than paying for another whole download. Ten minutes is short enough
// that a transient object-store fault which cut a rebuild short is retried
// within the time an operator takes to look. A judgment call, and one that
// is measured: crewlet.code.cache.rebuild.refusals{rule=cooldown} counts
// every rebuild it refuses, and code_cache_rebuild_refused names the
// repository.
const RebuildCooldown = 10 * time.Minute

// RebuildBackoffCap is the longest the window after a rebuild attempt grows
// to, however many attempts in a row have not ended a repository's failure —
// and how long a cache holding exactly what that attempt read is refused
// another (Cache.Rebuild's `unchanged` rule).
//
// A DAY — reached at the ninth attempt in a row, ten minutes doubled eight
// times being longer — at which a repository whose damage no rebuild ends
// costs one whole download a day on each git node a failing request reaches,
// where the cooldown alone made it 144: 10 000 GETs a day for 10 000 pushes
// rather than 1.44 million. The `unchanged` rule is held to the same day
// rather than to the next push: the same verified rows and objects do make
// the same directory, but the object store can come to hold what it lacked
// (a bucket restored, a replica healed) and a directory can be damaged on
// its own disk after a rebuild wrote it, and a rebuild can see neither
// coming — so a fault that clears on its own is retried within a day without
// anybody acting, on a repository nobody pushes to as well, at the one
// download a day the cap already prices.
const RebuildBackoffCap = 24 * time.Hour

// RebuildQuiet is how long a repository goes with no rebuild ASKED of it —
// refused or not — before the next ask starts a new line of attempts: the
// count its window doubles by goes back to none, and its cache is no longer
// taken to hold what the last attempt read.
//
// THE CAP, AND NO SHORTER. Quiet is the only sign the cache has that a
// rebuild ENDED a failure — it never sees a request succeed, only one fail —
// and every ask after a quiet is met as new damage, at once. So a quiet of
// RebuildCooldown would rebuild a repository whose damage no rebuild ends at
// every ask just over ten minutes apart: the 144 downloads a day the cap
// exists to prevent. At the cap, damage that persists costs at most one
// download a day however often or rarely it is asked about — asked within a
// day, its line goes on and the window grows to the cap; asked less often,
// its asks are a day apart — while damage that arrives a day or more after
// the last ask is met as the first attempt it is, ten minutes from a retry,
// rather than at the window an earlier, repaired incident grew.
const RebuildQuiet = RebuildBackoffCap
