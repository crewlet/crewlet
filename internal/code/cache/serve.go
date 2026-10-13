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
//   - pack.allowPackReuse: Pool.PackReuse — `multi` on a git that accepts
//     it (2.44), so a clone sends the packs a cache holds verbatim.
//   - pack.windowMemory: twice the epoch's largest blob (100 MiB at the
//     defaults) — the delta window must hold the largest blob a push may
//     carry and one base for it — which bounds a transfer's memory
//     (gitproc.TransfersPerProcess counts on it).
//   - pack.useBitmaps: the maintainer's bitmap is used.
//   - uploadpack.allowFilter: partial clones (--filter=blob:none).
//   - uploadpack.allowRefInWant: protocol v2's want-ref.
//   - uploadpack.keepAlive: git's own five seconds, stated, because the
//     pool's stall watchdog relies on a transfer moving that often.
//
// NEVER here, and refused by the pool's allowlist anyway: any of
// uploadpack.allowAnySHA1InWant, allowReachableSHA1InWant and
// allowTipSHA1InWant — protocol v0 serves only what an advertised tip
// reaches. And not here because the POOL sets them for every process:
// protocol.allow, gc.auto, maintenance.auto, pack.threads, core.hooksPath.
func (c *Cache) ServeConfig() []gitproc.KV {
	return []gitproc.KV{
		{Key: "core.multiPackIndex", Value: "true"},
		{Key: "pack.allowPackReuse", Value: c.git.PackReuse()},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*c.limits().MaxBlobBytes, 10)},
		{Key: "pack.useBitmaps", Value: "true"},
		{Key: "uploadpack.allowFilter", Value: "true"},
		{Key: "uploadpack.allowRefInWant", Value: "true"},
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
