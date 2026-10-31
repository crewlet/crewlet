package cache

import (
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE NODE'S MEMORY, AND WHAT THE CACHE'S BACKGROUND WORK MAY HOLD OF IT.
//
// Four git processes the cache starts hold memory in proportion to their
// WHOLE repository: the maintainer's multi-pack-index with its bitmap
// (BitmapBytesPerObject an object), the same index without one — the
// maintainer's where the bitmap cannot be had, and a full round's own
// (IndexBytesPerObject, beside IndexBaseBytes) — the commit-graph
// (GraphBytesPerObject, beside GraphBaseBytes for the trees its changed-path
// filters' diffs keep in git's delta base cache), and Verify's connectivity
// check (FsckBytesPerObject). A fifth, the node's TIP commit-graph over an
// installed chain (TipGraphBytesPerObject), holds memory in proportion to the
// TAIL it covers — the commits no installed layer holds — beside the same
// fixed part for its filters' diffs (GraphBaseBytes), and is
// charged so (Write.charge), with the room for a clone of the whole
// repository beside it that every step keeps; a compaction's build, which
// the cache admits to a slot of the maintenance lane (background.go), in
// proportion to the packs it merges (CompactBytesPerObject), charged so with
// room beside it for a transfer of as many objects — of them, never of the
// whole repository, below — and its delta window (CompactWindowBytes); and a
// publication's, admitted there too at the largest of its steps' charges —
// its layer's bitmap over the whole repository (PublishBytesPerObject), its
// commit-graph layer over the layer's packs (GraphLayerBytesPerObject, beside
// GraphBaseBytes) and
// its repack, a compaction's build over the packs it repacks — each with the
// room for a clone of the whole repository beside it. Each write's figures
// are its declaration's own (derived.go, Write.BytesPerObject and
// Write.BaseBytes), which the plan and a refusal's line read from the
// declaration, never beside it; the check, the build and the publication are
// no writes and keep their own. The largest repository the defaults admit is
// some 60 million objects, whose bitmap only a node of the large class
// docs/guides/deployment.md sizes holds; and on a node with less memory than
// a run needs, every duty cycle's run was killed by the kernel, which may as
// well have chosen the engine. So each is held to a BUDGET of the node's
// memory: what it has, less what it keeps for its other work.
//
// WHAT THE NODE HAS is what its git pool read at boot — the least of its
// physical memory and every cgroup limit set on the process or above it
// (gitproc.Memory) — and READ AGAIN AT EVERY MAINTAINER PASS
// (gitproc.Pool.ReadMemory): a running container's limit can be changed in
// place (Kubernetes resizes a pod's memory without a restart), and a budget
// read once would go on admitting runs past a limit lowered under it. A read
// that fails keeps the last reading, said once until one succeeds.
//
// WHAT IT KEEPS is fixed at Open from the pool's lanes and the memory they
// were sized by: gitproc.TransferMemory for every transfer the pool admits,
// and ServingMemoryPerCore — for the store, the broker, the engine and the
// read lane's processes — for every core the memory BACKS at
// gitproc.CoreMemory a core, never for more. That is what keeps a node with
// many cores and modest memory working: a container with a memory limit and
// no CPU limit sees its host's every core, and kept for each of them, 64 cores
// with 16 GiB kept 112 GiB, left the maintenance nothing, and refused every
// repository. Counted as the cores its memory backs, the same node keeps what
// an eight-core node keeps, and the pool sized its transfer lane the same
// way. At the pool's default lanes every node then gives its maintenance
// MaintenanceMemoryPerCore for each core its memory backs, and a larger node
// everything past what its other work is sized to hold.
//
// WHAT IT ADMITS is every step whose CHARGE fits the budget alone, together
// with everything in flight (maintFlight): a step's own figure an object of
// what it covers, and room beside it for one transfer of the whole repository
// (gitproc.TransferBytesPerObject an object) — a compaction's build's of what
// it merges alone, below. The transfer lane's reserve is
// sized for repositories of about a hundred thousand objects, since no lane
// knows which repository a slot will serve; a run is over one of the
// largest, and a clone of it — a walk of its whole history where it has no
// bitmap — holds a third of what the bitmap does, gigabytes over the largest.
// So the budget keeps room for one such clone beside each run, and holds no
// more: what bounds the transfers themselves is the transfer lane, which
// knows no repository (gitproc). The node's tip, whose own figure is the
// tail's, keeps the same room for a clone of the WHOLE repository beside it
// (chargeOver), since the clone it shares the node with is of the repository;
// so does every step of a publication, whose refusal leaves its tail outside
// every bitmap — slower, never a push refused. A compaction's build keeps room
// for a transfer of the packs it merges and no more, because it is the one
// step whose refusal is not "slower": the maintainer alone merges a
// repository's packs, and a repository whose merges its maintainer refuses
// gathers packs until every push carrying one is refused — so its charge
// follows the merge, never the repository (Cache.AdmitCompaction says what the
// whole repository's room would refuse).
//
// WHAT IT REFUSES is a step whose charge alone is past the budget, which never
// starts on this node, and the repository is served without it — slower,
// never wrong; but for a compaction's build, whose refusal leaves the
// repository's packs to gather toward that bound for as long as it lasts
// (code_compaction_behind). A bitmap refused is not the end of the index: the
// maintainer writes the index WITHOUT a bitmap in its place (a merge of the
// packs' indexes, a tenth of the bitmap's memory or less), so pushed packs go
// on joining an index rather than piling up outside every one — and leaves a
// bitmap it wrote before standing until MidxRoundPacks packs gather outside
// it (maintain.go says why). Each step refused is logged and counted once an
// incarnation (code_cache_maintenance_refused,
// crewlet.code.cache.maintenance.refusals{step}); a refused BITMAP, a refused
// GRAPH, a refused TIP and a refused PUBLICATION are each kept on a gauge
// while they stay refused (crewlet.code.cache.maintenance.refused{step}) — a
// repository refused its graph walks by date for as long, and a push of a
// commit dated before its parent walks its whole history — and a refused
// bitmap or publication raises code_maintenance_refused, whose threshold is
// the budget: a measurement, never a knob. The alarm stands for a refused
// graph too wherever it matters: past about 200 000 objects a graph's charge
// is under the bitmap's over the same objects, so no such repository is
// refused its graph and not its bitmap. Under it the graph's fixed part
// (GraphBaseBytes) outweighs the bitmap, and a node whose background budget
// is under that part can refuse a repository its graph alone — one whose
// walk by date costs at most about two seconds (gitproc.WalkPerObject an
// object), which the gauge says and the alarm, whose meaning is a
// repository served without a bitmap, does not. A
// refused TIP raises nothing: its copy is served through its installed
// chain's bitmap and its commit-graph layers, and only the commits pushed
// since the last publication walk by date — those of the tail, which a
// publication bounds — so the alarm's "served without a bitmap" would be
// false of it, and the gauge says it.

// chargeOver is what a git process the cache starts or admits is charged at,
// over stepObjects objects with room kept for a transfer of roomObjects: its
// own perObject bytes an object of what it covers, and room beside it for one
// transfer of roomObjects at gitproc.TransferBytesPerObject. Every step of the
// maintainer's but the node's tip covers the whole repository, and keeps room
// for a clone of it (stepObjects and roomObjects are both the repository's).
// The node's tip commit-graph covers the tail alone (maintain.go), and a
// publication's commit-graph layer and repack the packs of its layer and of
// its repack (background.go), so the own memory of each follows what it
// covers, while the clone it shares the node with is of the repository —
// charged by the tail or those packs alone, a large repository's tip or
// publication would be admitted beside a clone the budget kept no room for
// (roomObjects is the repository's). A compaction's build (background.go) is
// the one step that keeps room for a transfer of the packs it merges alone
// (roomObjects is its inputs): refused for a room that grew with the
// repository, it would cost the repository its pushes, never just speed.
func chargeOver(stepObjects, perObject, roomObjects int64) int64 {
	return stepObjects*perObject + roomObjects*gitproc.TransferBytesPerObject
}

// charge is what a declared git write is admitted at over objects with room
// kept for a transfer of roomObjects: chargeOver at its own figure an object,
// and its fixed part beside it (Write.BaseBytes) — the one rule every
// maintainer step's charge is read by, so a declaration's figures are never
// read beside it.
func (w Write) charge(objects, roomObjects int64) int64 {
	return chargeOver(objects, w.BytesPerObject, roomObjects) + w.BaseBytes
}

// perBackedCore is share for each core of cores that memory backs at
// gitproc.CoreMemory a core: share a core while the memory holds CoreMemory a
// core or more, and in proportion to the memory below that — never for a core
// the node has no memory for. Computed a mebibyte at a time, so no node's
// memory overflows the product.
func perBackedCore(cores int, memory, share int64) int64 {
	return min(int64(cores)*share, memory/(gitproc.CoreMemory>>20)*(share>>20))
}

// memoryReserve is what a node of memory bytes keeps of it for everything but
// its background work when its git pool admits transfers transfers and has
// cores read slots: TransferMemory a transfer, and ServingMemoryPerCore for
// each core the memory backs.
func memoryReserve(transfers, cores int, memory int64) int64 {
	return int64(transfers)*gitproc.TransferMemory + perBackedCore(cores, memory, ServingMemoryPerCore)
}

// maintenanceBudget is what a node of mem keeps for its background work once
// it has kept reserve for the rest: never below zero. Zero is a node whose
// lanes keep all its memory — an operator's git.max_transfers whose
// transfers' TransferMemory each reach the memory less what its backed cores
// keep (memoryReserve), which is well short of what the memory holds at
// TransferMemory each, or a memory lowered in place below what its lanes were
// sized by — every step refused, and the alarm saying so, rather than a
// refusal to boot: what it serves it still serves.
func maintenanceBudget(mem, reserve int64) int64 {
	return max(0, mem-reserve)
}

// initMemory is Open's: the memory the pool's lanes were sized by, what the
// node keeps of it, the budget the rest leaves, and where each maintainer
// pass reads the memory again.
func (c *Cache) initMemory(pool *gitproc.Pool) {
	m := pool.Memory()
	cores := pool.Slots(gitproc.ClassRead)
	c.memReserve = memoryReserve(pool.Slots(gitproc.ClassTransfer), cores, m.Bytes)
	c.maintMemory = perBackedCore(cores, m.Bytes, MaintenanceMemoryPerCore)
	c.memSource = pool.ReadMemory
	c.memory = m
	c.maint.setBudget(maintenanceBudget(m.Bytes, c.memReserve))
}

// rereadMemory is each maintainer pass's first step: the node's memory read
// again, and the budget moved with it — logged when it moves, so the line
// saying a container was resized in place is the line saying what its
// background work may now hold. A read that fails keeps the reading before it
// and says so once, until a read succeeds again: a budget is never guessed.
func (c *Cache) rereadMemory() {
	c.memMu.Lock()
	read := c.memSource
	c.memMu.Unlock()
	m, err := read()
	c.memMu.Lock()
	defer c.memMu.Unlock()
	if err != nil {
		if !c.memFailing {
			c.memFailing = true
			c.log.Warn("code_cache_memory_unreadable", "error", err, "memory", c.memory.Bytes,
				"memory_from", c.memory.From, "maintenance_budget", c.maint.budgetNow())
		}
		return
	}
	c.memFailing = false
	if m == c.memory {
		return
	}
	was := c.memory
	c.memory = m
	budget := maintenanceBudget(m.Bytes, c.memReserve)
	c.maint.setBudget(budget)
	c.log.Info("code_cache_memory_changed", "memory", m.Bytes, "memory_from", m.From, "was", was.Bytes,
		"was_from", was.From, "reserved", c.memReserve, "maintenance_budget", budget)
}

// BackgroundBudget is what this node gives its background work now: the
// budget at the last reading of its memory (Open's, then every maintainer
// pass's), which every whole-repository step of the cache's — a maintenance
// run, a full round's index, Verify's check — is admitted against together.
// Zero before Open has read the memory, and on a node whose lanes keep all of
// it.
//
// It is what the node ADVERTISES on its presence heartbeat, on every beat
// beside its status (coord.GitBackgroundKey), so the git node a repository's
// maintenance is placed on is one whose own admissions would hold the work:
// the figure, never a second estimate of it beside the one the admissions
// read. A locked read of a figure in memory, which the beat takes unbounded.
func (c *Cache) BackgroundBudget() int64 { return c.maint.budgetNow() }

// nodeMemory is the node's memory as the cache last read it.
func (c *Cache) nodeMemory() gitproc.Memory {
	c.memMu.Lock()
	defer c.memMu.Unlock()
	return c.memory
}

// maintStep is a set of the maintainer's declared writes (maintWrites), bit i
// its all[i]: what a plan takes, and what the node's memory refused an
// incarnation — where three more steps sit past every write's bit, a
// compaction's build ([stepCompact]), a publication's ([stepPublish]) and an
// expunge's ([stepExpunge]). A write's name — a line's and a count's `step` —
// is its write's Step, the compaction's "compact", the publication's
// "publish" and the expunge's "expunge".
type maintStep uint16

// stepCompact, stepPublish and stepExpunge are the background work a
// repository's maintainer runs between passes (background.go:
// Cache.AdmitCompaction, Cache.AdmitPublication and Cache.AdmitExpunge) as
// steps of an incarnation's memoryRefused set: the three steps there that are
// no declared write, at the first bits past every bit a write may take
// (maxMaintWrites), so the set an incarnation keeps of what the node's memory
// refused it — logged and counted once an incarnation, carried across a swap
// — holds a refused compaction, publication and expunge beside the
// maintainer's refused steps, and logs each once however many polls find it
// again.
const (
	stepCompact maintStep = 1 << maxMaintWrites
	stepPublish maintStep = 1 << (maxMaintWrites + 1)
	stepExpunge maintStep = 1 << (maxMaintWrites + 2)
)

// stepCompactName, stepPublishName and stepExpungeName are the three steps'
// `step`, on their lines and their counts.
const (
	stepCompactName = "compact"
	stepPublishName = "publish"
	stepExpungeName = "expunge"
)

// memoryRefusals is what the maintainer's latest walk of the entries found
// refused for memory: how many repositories a bitmap — their own, or the one
// their publication writes — and the largest of those charges. Its lock is a
// leaf.
type memoryRefusals struct {
	mu      sync.Mutex
	repos   int
	largest int64
}

// refusal is one step of one incarnation found refused for memory for the
// first time: what its line names.
type refusal struct {
	repo   string
	step   maintStep
	name   string
	charge int64
}

// refusalWalk gathers what one walk of the entries finds refused: by step,
// for the gauge, and the repositories served without a bitmap of what they
// hold, for the alarm (unbitmapped), with the largest of their charges.
type refusalWalk struct {
	bitmaps, graphs, tips, publications int
	unbitmapped                         int
	largest                             int64
	fresh                               []refusal
}

// note records inc's steps refused on the walk's budget — refused, with each
// step's charge in charges — and marks the ones refused for the first time on
// the incarnation, for their line and count. e.mu is held.
//
// A PUBLICATION REFUSED IS A BITMAP REFUSED: a repository whose maintainer
// cannot publish its next layer serves every pack pushed since its last
// publication outside every bitmap, and every one pushed after, so it counts
// toward the repositories the alarm stands for, its charge beside the
// bitmaps' — once, where the copy's own bitmap is refused as well. Its line
// was logged at its admission (Cache.AdmitPublication), so the walk logs none
// for it.
func (w *refusalWalk) note(mw *maintWrites, id string, inc *incarnation, refused maintStep,
	charges map[maintStep]int64) {

	if refused&(mw.bitmap|stepPublish) != 0 {
		w.unbitmapped++
	}
	if refused&mw.bitmap != 0 {
		w.bitmaps++
		w.largest = max(w.largest, charges[mw.bitmap])
	}
	if refused&stepPublish != 0 {
		w.publications++
		w.largest = max(w.largest, charges[stepPublish])
	}
	if refused&mw.graph != 0 {
		w.graphs++
	}
	if refused&mw.tipGraph != 0 {
		w.tips++
	}
	for i := range mw.all {
		s := maintStep(1) << i
		if refused&s != 0 && inc.memoryRefused&s == 0 {
			inc.memoryRefused |= s
			w.fresh = append(w.fresh, refusal{repo: id, step: s, name: mw.name(s), charge: charges[s]})
		}
	}
}

// noteRefusals records what one walk of the entries found: the repositories
// refused their bitmap — and their graph, their tip and their publication —
// and the repositories served without a bitmap of what they hold with the
// largest of those charges, for the gauges and the alarm, and every step
// refused for the first time on its incarnation — each logged, and counted
// once. Holding no lock of the cache or an entry.
func (c *Cache) noteRefusals(w refusalWalk) {
	slices.SortFunc(w.fresh, func(a, b refusal) int {
		if n := strings.Compare(a.repo, b.repo); n != 0 {
			return n
		}
		return int(a.step) - int(b.step)
	})
	c.refusals.mu.Lock()
	c.refusals.repos, c.refusals.largest = w.unbitmapped, w.largest
	c.refusals.mu.Unlock()
	for _, r := range w.fresh {
		c.logRefusal(r)
	}
	if c.metrics == nil {
		return
	}
	c.metrics.Set(metrics.CodeCacheMaintenanceRefused, float64(w.bitmaps),
		metrics.Attrs{"step": c.mw.name(c.mw.bitmap)})
	c.metrics.Set(metrics.CodeCacheMaintenanceRefused, float64(w.graphs), metrics.Attrs{"step": c.mw.name(c.mw.graph)})
	c.metrics.Set(metrics.CodeCacheMaintenanceRefused, float64(w.tips),
		metrics.Attrs{"step": c.mw.name(c.mw.tipGraph)})
	c.metrics.Set(metrics.CodeCacheMaintenanceRefused, float64(w.publications),
		metrics.Attrs{"step": stepPublishName})
}

// logRefusal logs and counts one step refused an incarnation for the first
// time: the repository, the step, its charge, and what the budget is made of.
func (c *Cache) logRefusal(r refusal) {
	m := c.nodeMemory()
	c.log.Warn("code_cache_maintenance_refused", "repo", r.repo, "step", r.name, "estimate", r.charge,
		"budget", c.maint.budgetNow(), "memory", m.Bytes, "memory_from", m.From, "reserved", c.memReserve)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheMaintenanceRefusals, 1,
			metrics.Attrs{"reason": refusedForMemory, "step": r.name})
	}
}

// refusedForMemory is the reason crewlet.code.cache.maintenance.refusals
// counts a step under: its charge is more than the node can give its
// background work.
const refusedForMemory = "memory"

// AlarmReading fills the git cache's half of an alarm reading
// ([statelog.KindCodeMaintenanceRefused]): how many repositories the
// maintainer cannot write a bitmap of, because the charge of one — the
// bitmap, and room beside it for a clone of the repository — is more than the
// node can give its background work, the largest of those charges, and that
// budget, as the maintainer's latest pass found them.
//
// It is the cache's whole part of a node's reading: the fields stay zero, and
// the alarm silent, on a reading nothing filled from a cache — a node that
// serves no git has none. The engine fills every evaluation of a git node's
// alarm table from it, so a refusal is raised on the node whose cache made it;
// nothing but a reading reaches the alarm, so a cache whose node never asked
// would log, count and gauge every refusal while its alarm stayed silent.
func (c *Cache) AlarmReading(out *statelog.Reading) {
	c.refusals.mu.Lock()
	out.CodeMaintenanceRefused = c.refusals.repos
	out.CodeMaintenanceLargest = c.refusals.largest
	c.refusals.mu.Unlock()
	out.CodeMaintenanceBudget = c.maint.budgetNow()
}
