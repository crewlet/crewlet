package cache

import (
	"time"

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
// handed out with the slot and back with it, and the slot goes back the
// moment the store's reader closes, before the install's checks and fsyncs.
// So the node's download memory is NodeFetches copy buffers — 8 MiB — however
// many syncs wait for one, and its GET rate is never spent on its disk.
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
// on a node reading at half the node's width: at 30 ms to the first byte and
// 80 MB/s a connection, bundles of 150 KB arrive at about 500 a second rather
// than a thousand, so 10 000 pushes' packs take about 20 seconds rather than
// 10. On the broker's object store at three replicas (natsobj's measured
// section), a 64 KiB get that keeps its pull took about 4 ms, a pull the
// cluster dropped cost a second (2 to 124 of 3 000 gets, with the machine's
// load), and gets averaged 7.2–7.3 ms in runs that lost eight — so sixteen in
// flight come to roughly 2 000 to 4 000 small bundles a second, a second more
// for each lost pull: arithmetic from one get at a time, not a measurement of
// sixteen at once. The share's reason holds on both backends.
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

// MidxPacks is how many packs a full round leaves before it writes a
// multi-pack-index — without a bitmap — before it answers, and how many packs
// gathered beside the maintainer's index make the repository due again.
//
// EIGHT: without the index, every object lookup searches each pack's own
// .idx in turn, and one push is one pack with nothing compacting them, so a
// repository reaches thousands. Under eight, a lookup is a handful of binary
// searches and the index would be a git process on a cold request for
// nothing; past it, the per-pack searches start to show. The index without a
// bitmap is a merge of the packs' own indexes, proportional to their objects.
// Eight is when a repository is DUE, not how many packs can stand outside the
// index: those are the packs that land before the run that covers them,
// which the debounce's ceiling and the interval bound (MidxMinInterval).
const MidxPacks = 8

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
// is minutes, so a repository pushed to every few seconds would otherwise be
// rebuilt continuously. In between, the packs pushed since are served from
// their own indexes beside it — never a missing index — and that is the bound
// on how many stand outside it: the pushes of one interval (or of
// MidxDutyCycle times the last run, when that is longer), the run's own
// duration and a tick. At a push every five seconds and runs of seconds,
// about 120 packs, each one more .idx a lookup the index does not answer
// searches. A failed run waits it out too, so a repository whose run keeps
// failing costs a run every ten minutes rather than every tick; a run stopped
// from outside (Stop, a full round's sweep, a retire) is no run, and waits
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
// others due run beside it. A judgment call that BenchmarkMidx's figures
// confirm or retune.
const MidxDutyCycle = 10

// MaintenanceBytesPerObject is the memory one maintenance run is estimated
// to hold for every object its repository's packs hold: what a run is
// charged against MaintenanceMemoryPerCore.
//
// 256 BYTES, MEASURED (git 2.43, four shared vCPUs, synthetic histories of
// fifty files a commit fast-imported into 65 packs): the costlier of a run's
// two steps is the multi-pack-index's BITMAP, whose peak anonymous memory
// was 401 MiB over about 2.1 million objects — 198 bytes an object — 247 MiB
// over 1 060 000 (244), and 86 MiB over 212 000 (424, a fixed part still
// showing); this repository's own history, 19 000 objects, took 10 MiB. The
// commit-graph took 12 MiB over the 2.1 million. Resident size was larger
// (2.0 GiB there), but the rest was the packs' own pages mapped in, which
// the kernel takes back under pressure, so it is not what a run holds.
// Rounded up past the million's figure, so the estimate never falls short
// of a repository of a million objects or more, where it is the estimate
// that matters. The objects are the packs' rows' own counts, so the estimate
// is a multiplication, never a read of the disk.
const MaintenanceBytesPerObject = 256

// MaintenanceMemoryPerCore is how much memory the maintenance runs BESIDE
// THE LARGEST ONE IN FLIGHT may be estimated at together, for every core
// the git pool has (its read lane's slots): a pass starts a run only while
// that holds, so a node that has just joined, whose largest repositories
// fall due together, indexes them as memory allows rather than all at once.
//
// THE LARGEST RUN IS NOT CHARGED. A repository's bitmap is the price of
// serving it — a node that serves a repository of N objects holds N × 256
// bytes once to write it, at whatever moment it is written — so a run over
// a repository past the whole allowance starts whenever a slot is free, and
// only what the allowance holds runs beside it: never two such repositories
// at once. What the allowance bounds is the multiplication — up to a lane's
// worth of the largest repositories together — not the largest one.
//
// A QUARTER OF A GIBIBYTE A CORE: gitproc.TransfersPerProcess holds the
// transfer lane under 896 MiB a core inside the 2 GiB a vCPU of the
// compute-optimised instance shapes, leaving about 1.1 GiB a core for the
// store, the broker, the engine and the read lane's processes; a quarter of
// a GiB of that is the maintainer's. On a four-core node it is 1 GiB — the
// runs of four million objects beside the largest, or every slot of
// repositories of typical size (under 100 000 objects: 25 MiB each) at once
// — and it binds only when several repositories of millions of objects are
// due together. A JUDGMENT CALL: larger lets a joining node's backlog clear
// sooner at the risk of the memory the store and the broker need; smaller
// runs the largest repositories one after another, each holding back the
// rest of the backlog behind it for as long as its bitmap takes.
const MaintenanceMemoryPerCore int64 = 256 << 20

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
