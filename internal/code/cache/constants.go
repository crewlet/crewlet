package cache

import (
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/store"
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
// httpx.MaxIdleConnsPerHost (32), the warm connections to a host the bucket's
// own transport keeps — s3obj's clone of httpx's, over HTTP/1.1 alone, which
// keeps httpx's pool settings — and an ALIAS rather than a number of its own:
// retuning the transport retunes it. Every GET of a cold-sync burst of bundles
// of one part — most of them: the median push's is near 150 KB, and s3obj reads
// up to PartBytes (8 MiB) as one request — rides a connection that transport
// keeps open, rather than paying a TCP and TLS handshake for one it then
// closes. And it is what a node joining a fleet asks of the store at once: S3
// answers in about 100 to 200 ms to the first byte (AWS's performance
// guidelines), and 32 small reads in flight there are about 150 to 290 GETs a
// second — the S3 fake answering 100 and 200 ms late gave 285 and 150 bundles a
// second (BenchmarkColdSync's firstbyte arms; Measured in doc.go). The node's
// own side of a bundle, about 900 a second, is what bounds it only at a first
// byte under about 35 ms, the fake's 30 ms arm among them.
//
// A NODE'S BOUND, NEVER THE FLEET'S. Every object is named under one `files/`
// prefix (objstore.Key.Name, ADR-0026), and every git node that syncs a
// repository it has never served asks this much of it: about 19 to 37 git
// nodes cold-syncing at once, each at its full thirty-two — twice as many at
// one repository's sixteen — reach the 5 500 GETs a second S3 serves one
// prefix (a fresh fleet, or a repository imported and then fetched through
// every node by a balancer that spreads /git/ evenly), past which it answers
// SlowDown until it has partitioned the prefix. The bucket's client then
// slows every request the node sends (internal/objstore/s3obj), so those
// syncs slow down and nothing they hold is wrong. How many nodes warm one
// repository is where its requests land, which routing /git/<name> by
// repository keeps to one or two (docs/concepts/scaling.md).
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
// than 15. At S3's own 100 to 200 ms it is the same share of a longer wait:
// the fake answering 100 and 200 ms late gave sixteen 150 and 76 bundles a
// second against 285 and 150 at thirty-two, so 10 000 pushes' packs take
// about 67 to 131 seconds rather than 35 to 67. A cold sync's length is its
// pack count, and fewer packs is what shortens it — not a share wider than
// the bucket's transport's warm connections allow the node. Sixteen is bound by
// its GETs: sixteen of the 33 ms each took allow
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

// MaxFullRounds is how many FULL READS of a repository's rows this node has in
// flight against the estate at once: a snapshot read whole — every full
// round's (a repository's first round in a process, a rebuild, the log
// recreated or gone back under the cache), and the two other reads of a whole
// snapshot, a check's and a hint's of its pack rows — each holding one of
// these slots from its first page to its last, granted first come, first
// served (read.go, fullRounds). A delta's pages hold none.
//
// store.MinReaderConns, EIGHT: the fewest reader connections a data node's
// store opens, and so the most pages the smallest data node scans at once. A
// full read asks its pages one at a time, and each is ONE read transaction on
// the data node it reads (statelog.Reader.Read): served out of one of the
// queue.ConcurrentAnswers (64) answers that node's shared lane gives every
// read of every domain (estate's laneShared) — a seat's tracker read, a page
// write, the code.resolve every git request is admitted by — and scanned on
// one of its reader connections, max(8, GOMAXPROCS) of them, about 59 ms for
// a full page of refs (code.MaxSnapshotItems; code's
// BenchmarkServeASnapshotPage), nearly all of it the driver's scan. Every
// other read the node serves takes one of the same connections, which
// database/sql hands to a waiting read drawn at random, never the one that
// has waited longest. A git node that holds the data reads its own copy
// instead, through the pool that serves those reads to every other node, so
// the cap is every node's. Uncapped, a node that comes up with hundreds of
// repositories asked of it — a balancer moving traffic onto it, CI fetching
// through it, the first fetches after a restart — starts a full read of every
// one at once, every request of the fleet that reaches that data node draws
// for a reader against all of their pages, and an asker that waits a whole
// read attempt there (estate.ReadAttempt, ten seconds) takes the node for
// gone: it asks it last for half a minute, and sends every repository it
// commits to its runner-up. Capped, a node asks no more of the smallest data
// node than it scans at once, comes up at that node's own rate, and leaves
// its other reads a page or two from a reader rather than a draw against
// every page it holds; and four such nodes at once hold half of a data node's
// answers, so the lane is never what fills first.
//
// A DELTA IS EXEMPT: it is the round every git node runs after every push,
// usually one page, and a cap on it would put every fetch behind the full
// reads of a node coming up. A delta the log answers in full (page 1 decides:
// one that would outweigh the full answer, a floor below the swept tombstones
// or retirements, another generation) is a full read from its second page on.
//
// THE WAIT IS THE CALLER'S. A round waits for its slot under the cache's
// lifetime, as all its work does, and its callers wait for the round under
// their own bounds — a seat's read its statelog.ReadBudget, a git request its
// client's patience — so a slot's wait is part of the sync a caller asked
// for, never a budget of its own; a caller that gives up leaves the round in
// line, and the repository comes up for its next request.
//
// PROVISIONAL, MEASURED against a MODEL of the smallest data node — its 64
// answers and its eight readers, every page a full page holding one of each
// for 60 ms, a reader given back to a waiting read drawn at random — by the
// rule that its other traffic, two hundred reads a second, waits under a
// second for its answer and its reader at the 99th percentile
// (BenchmarkColdJoin; one run, -benchtime 1x, git 2.56.0 on four cores of a
// Core Ultra 7 265U), in the two events a cap governs:
//
//   - ONE NODE RESTARTED over the cache it kept, every rolling restart's
//     case: its 500 repositories' first rounds download nothing, so their
//     reads are its whole bring-up. It came up in 7.6 s capped at four and in
//     3.9 to 4.3 s at eight and at every cap above it — the eight readers'
//     own rate — while the other reads waited 0, 105, 323, 842 and 1 030 ms
//     (p99) at caps of four, eight, sixteen, thirty-two and sixty-four, and
//     3.6 s uncapped.
//   - FOUR NODES JOINING together with fresh caches, 500 repositories asked
//     of each: the 2 000 reads took about 16 s at every cap, the readers'
//     rate again, while the other reads waited 352, 792, 1 030 and 1 556 ms
//     (p99) at four, eight, sixteen and thirty-two, and 15 s uncapped.
//
// The other reads' wait follows the full pages in flight per reader,
// whichever nodes hold them — sixteen, thirty-two and sixty-four in flight
// gave about the same in either event — and stays under the rule at four a
// reader, not at eight. So EIGHT, the readers themselves, is where the two
// events meet: the least that brings a restarted node up at the smallest data
// node's own rate — four takes nearly twice as long, and more brings it up no
// sooner while every other read waits longer — and the most that keeps four
// nodes joining together under the rule, which sixteen breaks. A data node of
// more cores opens more readers and would bring a restart up sooner at a
// higher cap, and one whose store.max_open_conns sets fewer scans fewer; a
// git node knows neither of the node it reads, so the cap is the fewest a
// store opens unconfigured. In that join's sixteen seconds each node
// downloads five to nine packs for each of its repositories from S3 (150 to
// 290 small bundles a second, NodeFetches), so for repositories of more
// history the downloads, not the reads, set how long it takes. The model's
// every page is a full page of refs, the dearest a page is: a repository of a
// few hundred refs is one page of a few milliseconds, which holds its slot
// and its reader a twentieth as long. What moves the cap is production's page
// times: crewlet.code.cache.read.page.duration on the git node, a page's
// whole round trip, and crewlet.store.pool.wait on the data node it reads.
const MaxFullRounds = store.MinReaderConns

// MidxPacks is how many packs gathered beside the maintainer's index make the
// repository due for another run.
//
// SIXTY-FOUR, MEASURED (BenchmarkMidx's stale=k/mode=own arms: pushes of
// 4 KiB landed beside the maintainer's index over the 100 MB and 1 GB
// histories; git 2.56.0, one bounded run of each arm — -benchtime 1x, one
// count — on two E-cores of an Intel Core Ultra 7 265U, 2026-10-29). A pack
// outside the index is outside its bitmap: a request that reaches it — a
// clone, a fetch of anything pushed since — opens it and walks its commits
// and trees rather than reading them off the bitmap, and that walk is what
// each such pack costs a request. Counting a clone's objects took 3.8 ms
// with no pack beside the index and 11.8 ms with 128 over the 100 MB, 5.5
// and 15.4 ms over the gigabyte — about 0.06 and 0.08 ms a pack — while a
// clone's whole pack-objects (0.64 s and 6.5 s) moved by little more than
// its own noise up to 128. A run rewrites the whole index and bitmap and
// writes the commit-graph: 1.16 s over the 100 MB, 14.2 s over the gigabyte
// (step=whole). So a repository run every M pushes pays a run's T every M
// pushes and each of its q requests a push an average of M/2 packs' δ —
// least at M = √(2T/qδ): for sixteen requests a push (a pipeline's fetches,
// the clones of the coding runs a push starts), 48 at 100 MB and 152 at a
// gigabyte. Sixty-four costs the 100 MB a twenty-fifth past its least, and
// runs a gigabyte's index more often than its least to spare each request a
// few milliseconds; where pushes come faster, MidxMinInterval and
// MidxDutyCycle bind first. A judgment call on q: at four requests a push
// the least doubles, and a count past it costs a request a twentieth of a
// millisecond a pack. On git 2.43 the same arms gave 0.06 and 0.09 ms a pack
// and runs of 1.6 and 21 s — a least of 58 and 173 — so the pin moved
// neither the figure nor the reason.
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
// gigabyte, 3.0 million and 424 000; git 2.43, four vCPUs) and CONFIRMED on
// the node pin (git 2.56.0, the 100 MB at 128, 256 and 512 packs, one
// bounded run of each arm — -benchtime 1x, one count — on two E-cores of an
// Intel Core Ultra 7 265U, 2026-10-29): the round's index, written in 0.11,
// 0.08 and 0.09 s, saves a clone 0.11, 0.62 and 3.1 s (6.31 against 6.20,
// 8.42 against 7.80, 12.8 against 9.7) — nothing past its own write at 128
// and the cold clone alone paying it back from 256, as below. The index is a
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
// thousand, against writes of 11 and 35 ms). And against the round it rides
// in, the index is a small share: over 2 000 pushes of a 100 000-file tree
// (a 334 MB pack) it is 0.15 s of a 13.5–17.6 s cold sync from the S3 fake
// at a 100 ms first byte on the node pin, the GETs the rest
// (internal/code/host's BenchmarkCloneCold, op=round-index).
//
// That window is not "a debounce later": the maintainer indexes a repository
// once its packs have stood still for MidxDebounce and the runs ahead of it
// have ended — on four cores ONE run at a time (maintenanceRuns), most packs
// uncovered first, each at 12 to 30 s a gigabyte, more the larger the
// repository (MidxDutyCycle) — so on a node that just joined, every
// repository it syncs due at once, the last waits for all of them: ten
// minutes or more for 50 GB. What a clone
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
// request that reaches it — a twentieth of a millisecond or so (MidxPacks). A
// failed run waits it out too, so a repository whose run keeps failing costs
// a run every ten minutes rather than every tick. A run stopped from outside
// (Stop, a full round's sweep, a rebuild's or a purge's retire) is no run,
// and waits nothing. One stop alone counts as a run: a SWAP's (swap.go's
// stopRun), recorded as the repository's last run, ended at the stop after
// what it had spent. A repository compacted more often than its index run
// lasts would otherwise have every run stopped by the next swap and every
// next one due at once, holding a maintenance slot for ever and writing
// nothing. The cost: after such a stop the copy — which the swap's retirement
// has just left with no multi-pack-index and no bitmap — goes without them
// until the next run has written them, due this interval after the stop (or
// MidxDutyCycle times what the stopped run had spent, where that is longer),
// on each git node whose run the swap stopped — served meanwhile from its
// packs' own indexes, which a compacted repository keeps few of.
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
// bitmap selecting and walking them. On the node pin (git 2.56.0, step=whole:
// the index with its bitmap, then the graph with its filters; one bounded run
// of each arm on two E-cores of an Intel Core Ultra 7 265U, 2026-10-29) a run
// takes 11.6 s a gigabyte of packs at 100 MB (1.16 s; 296 000 objects, 42 000
// commits) and 14.2 s at 1 GB (14.2 s; 3.0 million, 424 000), growing faster
// than the bytes as it did on git 2.43 — 16 s, 21 s and 30 s a gigabyte at
// 100 MB, 1 GB and 5 GB (152 s; 14.8 million, 2.1 million) on four vCPUs, the
// one run of the 5 GB arm there is. So a run passes a minute — where the duty
// cycle starts to bind — past about 3 to 4 GB, and the largest repository the
// defaults admit (code.DefaultRepoMaxBytes, 20 GiB), at the 5 GB arm's rate
// and its growth, runs about a quarter of an hour and is rebuilt at most
// every two and a half; under 3 GB a repository is rebuilt at most every
// MidxMinInterval.
const MidxDutyCycle = 10

// THE MEMORY OF EACH WHOLE-REPOSITORY PROCESS, an object of its repository —
// what the node's memory budget charges each by (memory.go), beside room for
// one transfer of the same repository (gitproc.TransferBytesPerObject). Each
// is the peak ANONYMOUS memory of the one git process, sampled every
// millisecond from /proc, over BenchmarkMidx's histories — a source
// repository's composition, commits a seventh of the objects, in 64 packs —
// of 100 MB (296 000 objects) and a gigabyte (2.97 million): the maintainer's
// writes on the node pin (step=whole: git 2.56.0, the writes the declarations
// run, over every pack after a tail of sixty-four pushes; one bounded run of
// each arm — -benchtime 1x, one count — on two E-cores of an Intel Core Ultra
// 7 265U, 2026-10-29), the check and a clone on git 2.43 (the memory arms).
// The packs' own pages a process maps in are not counted: the kernel takes
// those back under pressure rather than killing anybody. The objects are the
// packs' rows' own counts, so a charge is a multiplication, never a read of
// the disk.

// BitmapBytesPerObject is the memory the maintainer's multi-pack-index WITH
// ITS BITMAP holds for every object of its repository.
//
// 600 BYTES: it held 158 MiB over 100 MB (560 bytes an object) and 812 MiB
// over the gigabyte (287), the larger rounded up to the next 50. Unlike git
// 2.43's, whose walk kept more of the history at once the more of it there
// was — 136 MiB and 1 985 MiB, 484 and 702 bytes an object, about 220 more for
// every tenfold, which set the 1 000 this replaced — the pin's bitmap grows
// about 257 bytes an object past a fixed part of some 86 MiB (the two sizes'
// line), so its share an object FALLS with the repository's size: the largest
// repository the defaults admit (code.DefaultRepoMaxBytes, 20 GiB, some 60
// million objects) would hold about 15.5 GB on that line, which this charges
// at 36 GB — room for a growth past the gigabyte no arm here has measured
// (the 5 GB arm, under CREWLET_BENCH_LARGE, is the one that would). A
// refused bitmap still leaves the index without one rather than nothing
// (IndexBytesPerObject). 320, the figure before the 1 000, was 255 bytes an
// object over the gigabyte sampled more coarsely, and fell short of git
// 2.43's peak by a factor of almost three.
const BitmapBytesPerObject = 600

// IndexBytesPerObject is the memory the multi-pack-index WITHOUT A BITMAP —
// a full round's own, and the maintainer's where the bitmap cannot be had —
// holds for every object of its repository.
//
// 80 BYTES, a fifth past the larger measurement: 18.6 MiB over 100 MB (66
// bytes an object) and 182 MiB over the gigabyte (64) on the pin, as on git
// 2.43 (66 and 64). It is a merge of the packs' indexes, an entry an object,
// so it follows the objects and nothing else — a quarter of the bitmap's at
// the gigabyte and less beyond it, which is what lets a node index a
// repository whose bitmap it cannot hold.
const IndexBytesPerObject = 80

// GraphBytesPerObject is the memory the commit-graph holds for every object of
// its repository.
//
// 67 BYTES, a fifth past the larger measurement of the maintainer's own write
// — its version-2 changed-path filters, 1 024 of them new a write, more than
// the MaxNewFilters it computes since: 15.6 MiB over 100 MB (55 bytes an
// object) and 126 MiB over the gigabyte (45). It reads every commit, a
// seventh of a source repository's objects, so it is charged by the objects
// the rows count, as the rest are.
// The filters add a bounded amount a write rather than an amount an object:
// a write computes at most MaxNewFilters new ones, and reads every filter an
// earlier write left from the graph it replaces, which git maps rather than
// holds. 56, the figure this replaced, was git 2.43's graph without filters
// (12.7 and 112 MiB, 45 and 39.5 bytes an object).
const GraphBytesPerObject = 67

// TipGraphBytesPerObject is the memory the node's TIP commit-graph layer
// (maintain.go) holds for every object of the TAIL it is written over — the
// packs no installed published layer names — beside its fixed part
// (TipGraphBaseBytes) and the room for one clone of the whole repository
// every whole-repository step is charged (memory.go's chargeOver): a split
// write walks only the commits no layer beneath it holds, so its own memory
// follows the tail, and the clone it shares the node with is of the
// repository.
//
// 50 BYTES, MEASURED (BenchmarkMidx's tail= arms, op=tip-graph — the
// declaration's own write, MaxNewFilters new filters at most — over tails of
// 22 722, 90 909 and 363 636 objects outside a gigabyte's published root:
// git 2.56.0, one bounded run of each arm — -benchtime 1x, one count — on
// two E-cores of an Intel Core Ultra 7 265U, 2026-10-30): it held 2.4, 5.0
// and 16.7 MiB, 40 to 45 bytes an object of the tail past the shortest (37
// to 47 when the write computed 1 024 filters), the larger rounded up to the
// next ten. 1 700, the provisional figure this replaced, was 40 MiB over
// 5 000 commits with every filter computed, divided by the tail's objects:
// what it held was the filters, which a write bounds whatever its tail
// (TipGraphBaseBytes).
const TipGraphBytesPerObject = 50

// TipGraphBaseBytes is the memory the node's tip holds WHATEVER its tail's
// length, charged beside TipGraphBytesPerObject's (its declaration's
// Write.BaseBytes): the write's own fixed part and its changed-path filters,
// at most MaxNewFilters of them held until it ends.
//
// TWO MEBIBYTES, MEASURED (the same arms, and step=tip-graph's tail of 64
// pushes, 64 commits under each filter bound): the line through the three
// long tails meets a tail of no objects at 1.1–1.6 MiB, by which pair of
// them it is drawn through, and a tail of 64 commits held 0.4 MiB over
// 100 MB and 0.9 over the gigabyte — the part a write holds whatever its
// tail — rounded up to the next whole MiB past a fifth more. It is the
// filters' part: computing 1 024 a write, the same three tails held 5.8, 8.2
// and 20.4 MiB, a line meeting no tail at 4.2–4.6 MiB, about 4 KiB a filter.
// Added because it is past a tenth of what TipGraphBytesPerObject charges a
// tail of compact.PublishTailObjects (4.5 MB), the rule its arms set:
// charged by its tail alone, a short tail's tip would be admitted at a
// fraction of what it holds. It follows MaxNewFilters, and moves with it.
const TipGraphBaseBytes = 2 << 20

// PublishBytesPerObject is the memory a publication's multi-pack-index layer
// and its bitmap hold for every object of the WHOLE repository
// (Cache.AdmitPublication, internal/code/compact), beside the room for one
// clone of the repository every whole-repository step is charged (memory.go's
// chargeOver) — and the figure a repository's publication is PLACED by
// (Cache.PublishCharge, the git node its maintenance falls to): a bitmap is
// O(history), so the layer's own few objects say nothing of what it holds.
//
// 600 BYTES, MEASURED (BenchmarkMidx's step=publish and merge=root-write: git
// 2.56.0, one bounded run of each arm — -benchtime 1x, one count — on two
// E-cores of an Intel Core Ultra 7 265U, 2026-10-29, the in-memory store): a
// repository's FIRST publication, a root layer and its bitmap over every pack,
// held 158 MiB over 100 MB (560 bytes an object of the repository) and 812 MiB
// over the gigabyte (287) — what own mode's whole index with its bitmap holds,
// the writer walking every commit and reusing no bitmap of a base — and each
// later one, over the tail of compact.PublishPacks pushes on that root, 88 and
// 254 MiB (312 and 90); the node floor's own build, git 2.55.0, the same. The
// largest, rounded up to the next 50 as BitmapBytesPerObject is: a
// publication's charge follows its repository's history whatever its layer
// adds, so a repository is placed by its root's. 900, the provisional figure
// this replaced, was a tip over a 219 679-object history (628 and 801 bytes an
// object at that size) rounded up for git 2.43's growth with the history,
// which the pin does not show (BitmapBytesPerObject). BenchmarkBigNode's
// node=publish measures the root at 5 and 12 GB where a machine holds them;
// until one has, docs/concepts/scaling.md states the 12 GB figure as the
// extrapolation it is.
const PublishBytesPerObject = 600

// GraphLayerBytesPerObject is the memory a publication's commit-graph layer —
// `commit-graph write --stdin-packs --split=no-merge --changed-paths`, every
// changed-path filter computed, since it runs once for the fleet — holds for
// every object of the LAYER it covers (Cache.AdmitPublication), beside the
// room for one clone of the repository: a split layer reads only the commits
// of its own packs.
//
// 350 BYTES, MEASURED (BenchmarkMidx's step=publish/layer=root, as
// PublishBytesPerObject is): the root publication's graph layer over every
// commit of the history held 89 MiB over 100 MB (317 bytes an object of the
// layer) and 261 MiB over the gigabyte (92), the node floor's build the same —
// a part of it fixed, which a small layer's few objects carry alone (0.4 MiB
// over a tail of sixty-four pushes, whose charge the clone's room beside it
// covers many times) — the larger rounded up to the next 50. 700, the
// provisional figure this replaced, was 117 MiB over 35 000 commits (620
// bytes an object) rounded up.
const GraphLayerBytesPerObject = 350

// MaxNewFilters is the most changed-path filters one commit-graph write
// computes — the node's tip and own mode's whole graph alike, git's
// --max-new-filters, spelled whole in the argument lists that run it
// (maintain.go's graphArgs and tipGraphArgs, held to this by
// TestTheFilterFlagIsTheConstant). A commit without a filter is answered by
// `git log -- <path>` diffing it, as every commit was before the filters. Own
// mode's whole graph keeps every filter the graph it replaces holds, so each
// write computes up to this many more until every commit has one. The node's
// tip is written whole over the installed layers alone, so it computes its
// filters afresh at every write, and a tail of more commits than this leaves
// the rest without one until a publication covers them.
//
// 100, MEASURED, the smallest of 100, 1 024 and every filter that keeps a
// path's log and a file's blame under half of gitproc.ReadDeadline with a
// tail of the publication's bound outside the chain (internal/code/query's
// BenchmarkBlame and BenchmarkLogPath: the gigabyte's 455 680 commits under a
// published graph layer, compact.PublishTailObjects objects outside it under
// the node's tip; git 2.56.0, one bounded run of each arm — -benchtime 1x,
// one count — on two E-cores of an Intel Core Ultra 7 265U, 2026-10-29): a
// blame took 40 ms with 100 new filters, 44 ms with 1 024 and 18 ms with
// every one, a page of a path's log 70, 68 and 47 ms, and with no filter at
// all 51 and 96 ms — every one under a hundredth of the deadline. What the
// bound buys is the write's: a filter is a diff of a commit against its
// first parent, about 0.05 ms and 4 KiB held until the write ends
// (TipGraphBaseBytes), so 100 are some 5 ms and 0.4 MiB where 1 024, the
// provisional bound this replaced, were 50 ms and 4 MiB.
const MaxNewFilters = 100

// FsckBytesPerObject is the memory Verify's connectivity check (fsck
// --connectivity-only) holds for every object of its repository.
//
// 192 BYTES, a fifth past the larger repository's figure: 452 MiB over the
// gigabyte (160 bytes an object) and 110 MiB over 100 MB (390, a fixed part of
// some 70 MiB showing). A check of the largest repository the defaults admit
// holds some 11 GB — as much as the node's background work is ever given, so
// it is held to the same budget, charged at its admission (Cache.AdmitVerify).
const FsckBytesPerObject = 192

// CompactBytesPerObject is the memory a compaction's build — one
// `pack-objects --stdin-packs` over the packs it merges (internal/code/compact)
// — holds for every object of those packs, charged at its admission
// (Cache.AdmitCompaction) beside room for one transfer of as many objects
// (memory.go's chargeOver, over the inputs alone) and the build's delta window
// (CompactWindowBytes).
//
// 550 BYTES, MEASURED (internal/code/compact's BenchmarkCompact, its
// history=source/inputs={4,8,16,64} arms, and BenchmarkMidx's
// step=publish/layer=root, a publication's repack of the same composition:
// git 2.56.0 over the in-memory store, Go 1.27.2, the build's peak ANONYMOUS
// memory sampled every millisecond, the method of the figures above; one
// bounded run, -benchtime 1x and one count, on two E-cores of an Intel Core
// Ultra 7 265U, which has SHA instructions, 2026-10-27 and 29): one build
// merging the 100 MB history's 295 680 objects held 139 MiB whether they came
// in 4, 8, 16 or 64 pushes through a host — 493.4 to 493.8 bytes an object,
// the input count adding nothing — and 147.5 MiB merging the same history's
// 64 packs as BenchmarkMidx stores them (523 bytes an object; 289 over the
// gigabyte's 2.97 million), on the node pin and on the node floor's build
// alike: the larger figure rounded up to the next 50 bytes. 500, the figure
// this replaced, was the first arm's alone, and the publication's re-check
// held a build a twentieth past it. The 300 before it was reasoned from git's
// struct object_entry and a merge reusing every delta its inputs carry, and
// fell short of the measurement by two fifths. The history is one size, so
// the figure folds whatever part of the build's memory does not grow with the
// objects into the objects: a merge of more objects is charged more than it
// holds, never less, and a merge of a few is charged CompactWindowBytes
// beside them, far more than any fixed part. Charged by the inputs' objects
// alone, never the repository's, and so is the transfer's room beside them: a
// compaction merges the small packs, so its charge follows what it merges,
// and a charge that grew with the repository would refuse a large
// repository's every merge, and with them its pushes (Cache.AdmitCompaction).
const CompactBytesPerObject = 550

// CompactWindowBytes is the delta search a compaction's build is charged
// beside its objects: what its pack-objects holds around the window it runs
// under, whatever the objects number — no per-object figure charges it.
//
// SIX OF THE EPOCH'S LARGEST BLOB, MEASURED (internal/code/compact's
// BenchmarkCompact, its blobs=max_blob/inputs=8 arm: git 2.56.0 over the
// in-memory store, Go 1.27.2, the build's peak anonymous memory sampled every
// millisecond; one bounded run, -benchtime 1x and one count, on two E-cores
// of an Intel Core Ultra 7 265U, which has SHA instructions, 2026-10-27): a
// merge of eight packs, each a revision of one blob of the default
// max_blob_bytes, 50 MiB, every revision a delta of the one before, held
// 269.7 MiB — 5.4 blobs, where the two this charged before had admitted it
// at 170 MiB less than it held. The window
// itself is held to pack.windowMemory (internal/code/compact's CompactConfig:
// twice the largest blob, the serve configuration's figure, a per-thread
// bound the pool's pack.threads=1 makes the build's whole window), so the
// other 3.4 blobs are what the build holds beside its window — the object it
// deltifies, its delta, and the bases it reads deltas through — which that
// bound does not cap. Rounded up to the next whole blob, the measurement
// being of the worst composition a window meets — every revision deltas
// against the one before it, so the search holds two at once — and scaled by
// the epoch's own limit, since what it holds follows the size of the blobs it
// searches.
func CompactWindowBytes(limits code.Limits) int64 { return 6 * limits.MaxBlobBytes }

// VerifyHashPerMiB is what a check's hashing is given for each mebibyte of
// the bundles it reads (CheckBudget), beside the connectivity check's own
// gitproc.PackWorkDeadline.
//
// A TENTH OF A SECOND. BenchmarkVerify's op=hash reads and hashes a bundle at
// 0.95–1.08 ms a mebibyte (about 1 GB/s: SHA-256 with the CPU's SHA
// extensions, from the page cache, on the machine the package doc's
// Measured names). A cold read from a disk at 50 MB/s is 20 ms, and a CPU
// without the extensions hashes at a few hundred megabytes a second, so a
// tenth of a second is five times the slowest of those and a hundred times
// the measured — room for a disk the store, the broker and the node's clones
// share, without letting a check wedged on a disk hold its slot and the
// operator's sweep for days.
const VerifyHashPerMiB = 100 * time.Millisecond

// CheckPieceBytes is how much of a file a hash reads in one slot of the read
// lane (gitproc.Pool.HoldRead) before it gives the slot back and queues for
// the next: a hint's confirmation (hint.go) hashes up to the largest bundle a
// push stores, MaxPackBytes with its index, and held whole that is one core
// for its whole read — 1.3 s warm, about 11 s cold at a gp3 volume's 125 MB/s
// — with every read queued on a node of few slots waiting behind it.
//
// 64 MiB. A piece is 61–69 ms of hashing at BenchmarkVerify's 0.95–1.08 ms a
// mebibyte (SHA-256 with the CPU's extensions, from the page cache), about
// 0.2 s without them (272–347 MB/s measured for this hash on a CPU without),
// and 0.54 s read cold at 125 MB/s: the most a read the lane queues behind
// it waits, in arrival order. Smaller, and a hash on a saturated lane — where
// every piece queues behind every read already waiting — makes less progress
// a turn for the same waits; larger, and a read waits longer behind each.
const CheckPieceBytes = 64 << 20

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
// some 1.2 million objects beside the largest, or every slot of repositories
// of typical size (under 100 000 objects: 80 MiB each with the room for a
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
// 16 GiB on four cores gives 9 GiB, a bitmap of up to some 11 million
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

// ScrubInterval is how long after its bytes were last held to their
// checksums a USED copy is scrubbed again (scrub.go): every .pack against its
// own trailer and every index file by the check's rule, behind the serving.
// Damage that lands on a disk after this process checked a file, and that no
// git process reports — a pack body upload-pack sends verbatim, a bitmap
// that fails every clone silently — is served until the next scrub: at most
// this long for a repository used meanwhile, and from its first use after it
// for one idle meanwhile.
//
// A DAY: RebuildQuiet's, the window the cache already treats as the one in
// which "a disk can damage what a rebuild wrote", so one figure governs both.
// A JUDGMENT CALL between how long such damage fails every clone of its
// repository and what the scrub costs the node. A scrub reads 1.13 times a
// copy's pack bytes, at 0.86 to 1.38 s a gigabyte of packs from the page
// cache (BenchmarkScrub, 100 MB and 1 GB histories, six runs each) and at the
// volume's throughput read cold — so for 1 TB of packs all used every day, 14
// to 23 minutes of one core a day from the page cache, and at most 2.5 hours
// of a 125 MB/s volume's baseline (10.5% of its day) for the share read cold;
// nothing for a repository nobody reads. Half the interval doubles that
// share; what a shorter one finds sooner is only damage arriving after a
// check that nothing else reaches, which a restart, a report and an
// operator's `crewlet code verify` each reach as well.
const ScrubInterval = RebuildQuiet

// ScrubCooldown is how long after a scrub that found nothing a report — a
// pack or an object a git process could not read, a pack named in words, a
// crash — makes no scrub due (report.go), WHATEVER IS WRITTEN MEANWHILE: a
// request that can provoke a report (a git that crashes on what it is asked)
// beside a busy repository is bounded by this and nothing else.
//
// AN HOUR: the largest repository the defaults admit (20 GiB, 21.5 GB of
// packs) then costs at most one scrub an hour at a report's word — 18 to 30 s
// of one core from the page cache (0.5 to 0.8% of it), or 194 s of a 125 MB/s
// volume read cold (5.4% of its hour) — whatever a request provokes, while a
// report about damage the last scrub could not have seen waits at most an
// hour for the scrub that finds it, beside the copy's daily one.
const ScrubCooldown = time.Hour

// MaxProvenanceRuns is the most runs an index of pushes holds (provenance.go):
// a commit's lookup binary-searches every run, newest first, so the bound is
// what a lookup costs at worst. The backfill merges before an index reaches
// it, and a round's fold at it merges its two cheapest runs first.
//
// 32 (M3-decisions M3-O22's provisional value, which the measure keeps): the
// largest of {8, 16, 32, 64} at which a lookup that misses — every run
// searched — costs under 1% of a warm tree read, the answer it decorates:
// BenchmarkCodeRead's warm p50 listing of a root is 3.1 ms (the query
// package's Measured section), so 31 µs. BenchmarkProvenance's lookup arm,
// over four million commits — 96 MB of commit records, past the last-level
// cache at every run count — missed in 0.54 µs at one run, 3.7 at 8, 15.5 at
// 32 and 29.8 at 64. The large model's 18 million pushes, some 54 million
// commits, make each run's binary search about four steps deeper, every one
// a miss to memory at about 100 ns: some 27 µs at 32 runs and 55 µs at 64,
// the first under the bound and the second past it — an extrapolation of the
// measured arm, not a measure of that index. Geometric merging keeps
// O(log n) runs — eight balanced runs a merge (ProvenanceMergeWidth), about
// ten for that index and never more than 17, simulated over its 18 million
// pushes in folds of one to twenty — so the bound is reached only while the
// backfill lags a burst of deltas.
const MaxProvenanceRuns = 32

// ProvenanceMergeFactor is how much of a merge one run may be: a span of runs
// the backfill merges is BALANCED, none of them holding more than this many
// times the pushes of the others together (mergeDue).
//
// TWO, git's own geometric factor (`git repack --geometric=2`): no run is
// rewritten for fewer than half its own pushes again, so runs grow
// geometrically as they merge — O(log n) runs, each push written O(log n)
// times over its life — and a large run beside a stream of small ones is
// never rewritten for each of them.
const ProvenanceMergeFactor = 2

// ProvenanceMergeWidth is how many adjacent runs one merge of the backfill
// merges (mergeDue): the cheapest balanced span of this many.
//
// EIGHT. A fold writes a run of one delta's pushes — one push, where every
// push is synced on its own. Merged a pair at a time whenever two neighbours
// were within ProvenanceMergeFactor of each other, such runs merged on nearly
// every push (999 995 merges in a million one-push folds, simulated), each
// merge four syncs — the staged run's, crewlet/'s, the state's two — and a
// state write, on every node holding the copy, beside the rounds' own syncs
// on the request path; and that pair rule met a burst's folds — runs of every
// size, each beside one it is not within a factor of — by filling the index
// to MaxProvenanceRuns, where every fold's merge rewrote whatever pair was
// cheapest: simulated, 64 to 2 358 rewrites a push. Simulated over one-push
// folds, bursts of one to twenty, two hundred and five thousand pushes a
// fold, a history read below and a gap read above
// (TestTheMergePolicyKeepsAnIndexSmallAndMergesItSeldom holds three), eight
// runs a span merge once in seven one-push folds and rewrite each push 6 to
// 15 times, and no index held more than 24 runs; ten a span held 27 and
// twelve 31, one short of the bound, and sixteen sat there with a history
// read below, while four and six merge once in three and once in five
// one-push folds. Eight is the widest whose indexes stay eight runs clear of
// MaxProvenanceRuns.
const ProvenanceMergeWidth = 8

// BackfillIdle is how long the backfill waits after each page it reads, as a
// multiple of that page's own duration and of the fleet's ratio of live git
// nodes to live data nodes (backfill.go): every git node backfills from the
// data nodes, so its share of them is the ratio.
//
// TWO: a git node then reads a third of the time at most against one data
// node's worth of git nodes, so the fleet's backfills hold about a third of
// one reader of each data node — the code retention pass's share of the
// estate's write lane (code.RetentionLaneShare), applied to a read — however
// many git nodes share them, and the serving keeps the other two thirds.
const BackfillIdle = 2

// HorizonEvery is how often the backfill asks the rows for the oldest push
// they keep — an index's horizon (provenanceState.Oldest) — and drops what
// it holds below it: runs wholly below at once, and records below it at
// their next merge.
//
// A DAY: the rows drop pushes past code.PushHistory (181 days) as the log
// moves, so a day's lag holds at most a day's pushes past them — under 1% of
// the index — for one page of one row a day a repository.
const HorizonEvery = 24 * time.Hour

// BackfillRetry and BackfillRetryCeiling bound the backfill's waits after a
// step that failed — a page the estate could not serve, a run it could not
// write or merge — each a window of the failing repository's own, while the
// other repositories' steps go on (backfill.go); the backfill's wait as a
// whole after failures that span repositories; and its wait on a fleet whose
// live nodes could not be counted (Options.Fleet). Each doubles from the
// first to the second, jittered, and the step is asked again — never a page
// paced on a guess.
//
// FIVE SECONDS TO FIVE MINUTES: a data node that restarts or a router that
// lost its placement answers again within seconds, and nobody waits on a
// backfill, so a long outage costs one read every five minutes a git node
// rather than a stream of them against a fleet that is already down, and
// one repository whose rows cannot be served costs one read of them every
// five minutes — the shape of the code reconciler's retries (engine's
// codeReposRetryBase).
const (
	BackfillRetry        = 5 * time.Second
	BackfillRetryCeiling = 5 * time.Minute
)

// MappedRunShare bounds the runs of the indexes of pushes the cache holds
// mapped at once (runmaps.go): the process's count of mappings
// (vm.max_map_count, read at Open) over this.
//
// A QUARTER: every mapping the engine's process makes counts against the one
// sysctl — the Go heap's, the store's library and its threads', every run's —
// and past it every one fails, the runtime's own heap growth with them, which
// is fatal. The rest of the process holds a few thousand at most, so three
// quarters leave it more than ten times that; and a quarter of Linux's
// default, 16 382 runs, holds the indexes of some three thousand copies kept
// mapped for their next reader — at the five or so runs an index holds
// between merges — while the readers of one node, each holding the runs of
// one index for the length of one read, hold at most a few hundred.
const MappedRunShare = 4

// DefaultMapCount is Linux's default vm.max_map_count (DEFAULT_MAX_MAP_COUNT,
// 65 536 less 6): the count a darwin build holds itself to (mapcount_other.go)
// and a Linux node whose kernel answers none (Open logs it).
const DefaultMapCount = 65530
