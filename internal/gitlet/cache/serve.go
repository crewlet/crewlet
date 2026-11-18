package cache

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// ServeConfig is the configuration every git process in a cache runs with
// (the host's upload-pack, and this package's own: a round's peel and index,
// Missing's cat-file, the maintainer's index and graph, Verify's fsck), as
// gitproc call-site pairs: ONE value, so no process reads a cache with
// another idea of it. Each pair's reason is beside it.
//
// NEVER here, and refused by the pool's allowlist anyway:
// uploadpack.allowAnySHA1InWant, allowReachableSHA1InWant and
// allowTipSHA1InWant (protocol v0 serves only what an advertised tip
// reaches), and uploadpack.allowRefInWant (below). Not here because the pool
// sets them for every process: protocol.allow, gc.auto, maintenance.auto,
// pack.threads, core.hooksPath.
func (c *Cache) ServeConfig() []gitproc.KV {
	return []gitproc.KV{
		{Key: "core.multiPackIndex", Value: "true"},
		// `single`: a clone sends the bitmap's preferred pack verbatim and packs
		// every other object as pack-objects does (a stored delta reused, a whole
		// copy searched for a delta). NOT `multi`: receive-pack completes a thin
		// push by appending WHOLE every base it borrowed, so an edited object sits
		// in two packs, and which copy the multi-pack-index keeps (midx_oid_compare:
		// the preferred pack's, else the newer .pack to the second, else the pack
		// directory's listing order) is chance; `multi` sends the whole copies it
		// keeps as stored, where `single` finds each a delta, and sent more bytes
		// for more CPU on every copy measured (package doc, "Measured"). One value,
		// no knob: every git at the node floor serves either whole (gitproc's
		// TestEveryPackReuseModeSendsOverlappingPacksWhole).
		{Key: "pack.allowPackReuse", Value: "single"},
		// Twice the epoch's largest blob (100 MiB at the defaults): the delta
		// window holds the largest blob a push may carry and one base for it,
		// which bounds a transfer's memory (gitproc.TransfersPerProcess counts
		// on it).
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*c.limits().MaxBlobBytes, 10)},
		{Key: "pack.useBitmaps", Value: "true"},
		// git 2.38's lookup table lets a request load only the bitmaps it asks
		// for; without it every upload-pack that opens the bitmap reads every
		// commit's entry (BenchmarkMidx's lookup arms: a lazy blob fetch's
		// pack-objects takes 5.0 ms with it, 6.2 ms without, over a gigabyte).
		// Only a bitmap write reads the key, and a bitmap without the table is
		// read as before. No knob: no git at the node floor lacks it.
		{Key: "pack.writeBitmapLookupTable", Value: strconv.FormatBool(!c.noLookupTable.Load())},
		// Version 2 (git 2.46, below the node floor) for every changed-path filter this cache writes
		// (maintain.go): version 1 hashes a path by the C compiler's char
		// signedness (amd64 signed, arm64 not), so past ASCII a filter written on
		// one build answers wrong on the other, a published layer is read across
		// a mixed fleet, and a path's log skips a commit that changed it. git
		// writes 1 by default. A reader under 2 reads only version-2 filters,
		// every one Gitlet writes (gitlet.CheckLayer holds published layers to
		// it). No knob: version 1's answer is wrong.
		{Key: "commitGraph.changedPathsVersion", Value: "2"},
		// Partial clones, but ONLY THE FILTERS A BITMAP CAN ANSWER (pack-bitmap.c's
		// filter_bitmap): uploadpackfilter.allow=false refuses every filter the
		// keys after it do not name, and tree.maxDepth=0 admits tree at depth 0
		// alone. sparse:oid and a deeper tree walk every object the fetch reaches,
		// and git refuses them in words the client prints after `remote error:`.
		// HYGIENE, NOT A SECURITY BOUNDARY: with no bitmap held (before the
		// maintainer's first run, or refused for memory:
		// code_maintenance_refused), and on any shallow fetch, a fetch walks what
		// it reaches, bounded by the transfer lane and its deadlines. A blobless
		// clone with a sparse checkout still reaches what a sparse:oid names.
		// Every key predates the node floor.
		{Key: "uploadpack.allowFilter", Value: "true"},
		{Key: "uploadpackfilter.allow", Value: "false"},
		{Key: "uploadpackfilter.blob:none.allow", Value: "true"},
		{Key: "uploadpackfilter.blob:limit.allow", Value: "true"},
		{Key: "uploadpackfilter.tree.maxDepth", Value: "0"},
		{Key: "uploadpackfilter.object:type.allow", Value: "true"},
		{Key: "uploadpackfilter.combine.allow", Value: "true"},
		// git's own five seconds, stated: the pool's stall watchdog relies on a
		// transfer moving that often.
		{Key: "uploadpack.keepAlive", Value: "5"},
		// uploadpack.allowRefInWant is NEVER set: it BREAKS EVERY SHALLOW CLONE.
		// upload-pack sends wanted-refs before shallow-info (upload-pack.c,
		// send_wanted_ref_info then send_shallow_info) and fetch-pack reads
		// shallow-info first, so a protocol v2 `git clone --depth=1` dies
		// `fatal: expected 'packfile', received 'shallow-info'` (2.43 to 2.48.0).
		// Without it a client lists the refs (ls-refs, linearizable) and fetches
		// the ids it was shown; a ref moving between the two is served at the id
		// the client saw.
	}
}

// noAlternateRefs is a ref prefix no ref can begin with: a component that
// begins with a dot is no ref name git or gitref admits, so whatever
// repository a view leans on, none of its refs matches it.
const noAlternateRefs = "refs/.none"

// AdvertiseConfig is what a push ADVERTISEMENT's receive-pack adds, in an
// advertisement view (AdvertiseView), to the receive configuration:
// core.alternateRefsPrefixes naming a prefix no ref has (noAlternateRefs), so
// receive-pack does not list its one alternate's refs. The alternate is the
// incarnation, whose packed-refs IS the view's, so that listing finds nothing
// new and only doubled every push's advertisement (BenchmarkAdvertise: 222 ms
// against 117 ms at gitlet.MaxRefsPerRepo).
//
// NEVER for a push's scratch (NewScratch): its refs are empty, so its
// connectivity check (rev-list --not --all --alternate-refs) has the
// alternate's refs as its ONLY negative tips, and without them walks the
// whole history on every push.
func (c *Cache) AdvertiseConfig() []gitproc.KV {
	return []gitproc.KV{{Key: "core.alternateRefsPrefixes", Value: noAlternateRefs}}
}
