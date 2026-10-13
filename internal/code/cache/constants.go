package cache

import (
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// EvictInterval is how often the janitor runs a pass when nothing kicked it.
//
// A MINUTE, because the tick is not where the urgent work happens: whatever
// moves something into trash/ — a release of the last pin on a retired
// incarnation, the boot's own clear — kicks the janitor at once, so the tick
// only bounds how long anything a kick did not reach waits before its space is
// given back, and how far the last use a restart reads back from the disk can
// trail the real one (each tick stamps it on every repository used since the
// tick before). A pass over an empty trash/ is one directory read, and a stamp
// is one metadata write per repository used in the minute, so a minute costs
// nothing; disk the node has already decided to give back is never held for
// longer than an operator watching it would notice, and a minute of last use
// is nothing against the hours a budget's eviction order turns on.
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
// multi-pack-index — without a bitmap — before it answers.
//
// EIGHT: without the index, every object lookup searches each pack's own
// .idx in turn, and one push is one pack with nothing compacting them, so a
// repository reaches thousands. Under eight, a lookup is a handful of binary
// searches and the index would be a git process on a cold request for
// nothing; past it, the per-pack searches start to show. The index without a
// bitmap is a merge of the packs' own indexes, proportional to their objects.
const MidxPacks = 8
