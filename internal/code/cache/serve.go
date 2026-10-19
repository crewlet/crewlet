package cache

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// ServeConfig is the configuration every git process in a cache runs with —
// the host's upload-pack, and this package's own: a round's peel and index,
// Missing's cat-file, the maintainer's index and graph, Verify's fsck — as
// gitproc call-site pairs: ONE value, so no process reads a cache with
// another idea of it.
//
//   - core.multiPackIndex: the multi-pack-index is read when present.
//   - pack.allowPackReuse: `multi` — a clone sends verbatim what it can of
//     EVERY pack the multi-pack-index bitmap covers, rather than of its
//     preferred pack alone (`single`). One value on every node, with no
//     version branch and no knob: every git at the node floor
//     (gitproc.NodeMinVersion) sends a cache's overlapping packs whole,
//     which 2.44 to 2.47 did not — they sent an object twice or a delta
//     without its base, and the client refused the clone (gitproc's
//     TestMultiPackReuseSendsOverlappingPacksWhole, on the node pin and on
//     the floor's own build). What it COSTS a clone, against `single`, is
//     measured by BenchmarkPackReuse over copies this cache made — pushes
//     received thin, a cold round's install, the maintainer's bitmap — and
//     it is more bytes and more of the node's CPU on every layout measured,
//     on the node pin and the floor alike: 200 pushes adding files sent 9%
//     more for 9–27% more CPU, 200 pushes of a history of edits 57% more
//     for 3.5–3.6 times the CPU, and those edits once all but their last
//     20 pushes are one compacted pack 10% more for 43–65% more. The bytes
//     are the layout's: a cache keeps a pack a push, and receive-pack
//     completes a thin push by appending WHOLE every base its deltas
//     borrowed from the packs before it, so an edited object sits in two
//     packs, a delta in its own push's and whole in the next one's. The
//     multi-pack-index keeps ONE copy, in the order git's midx_oid_compare
//     puts them: the preferred pack's; else the copy in the pack whose .pack
//     is newer to the second; and among one second's packs — a cold round
//     installs nearly every pack within one — the first in the write's own
//     listing of the pack directory, the filesystem's order, never the
//     pushes'. So the listing chooses which edited objects the index keeps
//     whole, and `multi` sends as stored the whole copies it keeps, where
//     `single` packs each as it packs any object outside the preferred pack
//     and finds it a delta.
//   - pack.windowMemory: twice the epoch's largest blob (100 MiB at the
//     defaults) — the delta window must hold the largest blob a push may
//     carry and one base for it — which bounds a transfer's memory
//     (gitproc.TransfersPerProcess counts on it).
//   - pack.useBitmaps: the maintainer's bitmap is used.
//   - pack.writeBitmapLookupTable: the bitmap the maintainer writes carries
//     git's lookup table (2.38) — the commits it holds a bitmap for, and
//     where — so a request loads the bitmaps it asks for, where without it
//     every upload-pack that opens the bitmap first reads every commit's
//     entry: over a gigabyte (BenchmarkMidx's lookup arms, three runs,
//     medians) the pack-objects run that answers a blobless clone's lazy
//     fetch of one blob takes 5.0 ms with it and 6.2 ms without, and the one
//     that answers a fetch of one new commit 8.2 and 9.0 ms — the git
//     process, not the request around it. Only a bitmap write reads the
//     key, so no other process in a cache is changed by it, and a bitmap
//     written without the table is read as git always read one.
//     Unconditional, never a knob: every request that opens the bitmap is
//     the cheaper for it, and no git at the node floor lacks it.
//   - uploadpack.allowFilter: partial clones (--filter=blob:none), and the
//     uploadpackfilter keys after it: ONLY THE FILTERS A BITMAP CAN ANSWER.
//     uploadpackfilter.allow=false refuses every filter the keys after it do
//     not name, which are exactly those pack-objects can answer from a
//     bitmap (pack-bitmap.c's filter_bitmap): blob:none, blob:limit,
//     tree at depth 0 (uploadpackfilter.tree.maxDepth=0, which allows
//     tree too), object:type, and a combine of them — checked filter by
//     filter. sparse:oid and tree at any depth past 0 walk every object the
//     fetch reaches whatever the repository holds, and git refuses them in
//     its own words (`filter 'sparse:oid' not supported`, `tree filter
//     allows max depth 0, but got 1`), which the client prints after
//     `remote error:`. Every key predates the node floor. A served filter is
//     answered from the bitmap only where this cache HOLDS one: before the
//     maintainer's first run over an incarnation, and on a repository whose
//     bitmap the node's memory refuses (code_maintenance_refused), every
//     fetch, filtered or not, walks what it reaches, bounded by the transfer
//     lane and its deadlines. HYGIENE, NOT A SECURITY BOUNDARY: a shallow
//     fetch disables the bitmap the same way and is served, bounded the same
//     way. A client loses no way to work: a blobless clone with a sparse
//     checkout reaches the files a sparse:oid names, fetching them as it
//     needs them.
//   - uploadpack.keepAlive: git's own five seconds, stated, because the
//     pool's stall watchdog relies on a transfer moving that often.
//
// NEVER here, and refused by the pool's allowlist anyway: any of
// uploadpack.allowAnySHA1InWant, allowReachableSHA1InWant and
// allowTipSHA1InWant — protocol v0 serves only what an advertised tip
// reaches — and uploadpack.allowRefInWant, protocol v2's want-ref, which
// BREAKS EVERY SHALLOW CLONE: a client offered ref-in-want clones by
// want-ref, and git's upload-pack answers a fetch that both deepens and
// names refs with its wanted-refs section before its shallow-info section
// (upload-pack.c, send_wanted_ref_info then send_shallow_info) while git's
// fetch-pack reads shallow-info before wanted-refs — so `git clone
// --depth=1`, protocol v2 being git's default, died `fatal: expected
// 'packfile', received 'shallow-info'` (measured: a 2.43 client against
// 2.43, 2.47.3 and 2.48.0 servers, and 2.48.0 against itself). Without it
// a client lists the refs (ls-refs, linearizable) and fetches the ids it was
// shown, as it does from a server that never offered want-ref; what that
// costs is a ref moving between the two requests, which the fetch serves at
// the id the client saw. And not here because the POOL sets them for every
// process:
// protocol.allow, gc.auto, maintenance.auto, pack.threads, core.hooksPath.
func (c *Cache) ServeConfig() []gitproc.KV {
	return []gitproc.KV{
		{Key: "core.multiPackIndex", Value: "true"},
		{Key: "pack.allowPackReuse", Value: "multi"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*c.limits().MaxBlobBytes, 10)},
		{Key: "pack.useBitmaps", Value: "true"},
		{Key: "pack.writeBitmapLookupTable", Value: strconv.FormatBool(!c.noLookupTable.Load())},
		{Key: "uploadpack.allowFilter", Value: "true"},
		{Key: "uploadpackfilter.allow", Value: "false"},
		{Key: "uploadpackfilter.blob:none.allow", Value: "true"},
		{Key: "uploadpackfilter.blob:limit.allow", Value: "true"},
		{Key: "uploadpackfilter.tree.maxDepth", Value: "0"},
		{Key: "uploadpackfilter.object:type.allow", Value: "true"},
		{Key: "uploadpackfilter.combine.allow", Value: "true"},
		{Key: "uploadpack.keepAlive", Value: "5"},
	}
}

// noAlternateRefs is a ref prefix no ref can begin with: a component that
// begins with a dot is no ref name git or gitref admits, so whatever
// repository a view leans on, none of its refs matches it.
const noAlternateRefs = "refs/.none"

// AdvertiseConfig is what a push ADVERTISEMENT's receive-pack adds, in an
// advertisement view (AdvertiseView), to the receive configuration every
// receive-pack runs with: core.alternateRefsPrefixes naming a prefix no ref
// has (noAlternateRefs), so the view's receive-pack does not list the refs
// of its one alternate. The alternate is the incarnation, and the view's
// packed-refs IS the incarnation's file, so every tip that listing finds is
// one the view already advertises, and receive-pack prints nothing more for
// it; but the listing is a second for-each-ref over the same refs, and it
// doubled the advertisement's cost — every push pays it once, before its
// POST (BenchmarkAdvertise, the same bytes out of every arm: 26 ms in a view
// against 14 ms in the cache at 10 000 refs, 222 ms against 112 ms at
// code.MaxRefsPerRepo, and with this 18 ms and 117 ms). A prefix filter,
// which runs nothing: git still starts the listing's process, which then
// reads nothing.
//
// NEVER for a push's scratch (NewScratch): its refs are empty, so its
// receive-pack's connectivity check — rev-list --not --all --alternate-refs —
// has the alternate's refs as its ONLY negative tips, and with them filtered
// out it would walk the repository's whole history on every push.
func (c *Cache) AdvertiseConfig() []gitproc.KV {
	return []gitproc.KV{{Key: "core.alternateRefsPrefixes", Value: noAlternateRefs}}
}
