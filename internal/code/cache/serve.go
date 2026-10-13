package cache

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// ServeConfig is the configuration every git process in a cache runs with —
// the host's upload-pack, and this package's own peel and index — as
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
