package cache

import (
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE NODE'S MEMORY, AND WHAT THE CACHE'S BACKGROUND WORK MAY HOLD OF IT
// (package doc, "Background work never takes a person's slot"). A run past the
// node's memory is OOM-killed, and the kernel may as well pick the engine. So
// each git process the cache starts or admits is charged by what it covers
// (chargeOver, Write.charge) and runs only while its charge, beside everything
// in flight (maintFlight), fits the BUDGET: the node's memory (gitproc.Memory,
// re-read every pass: rereadMemory) less memoryReserve. A step whose charge
// alone is past it never starts here, and the repository is served slower,
// never wrong: a refused bitmap leaves an index without one (maintain.go), a
// refused compaction lets packs gather (code_compaction_behind). Each refusal
// is logged and counted once an incarnation and gauged while it stands
// (noteRefusals); the alarm's threshold is the budget, never a knob.

// chargeOver is what a git process the cache starts or admits is charged at:
// perObject bytes an object of the stepObjects it covers, and room for one
// transfer of roomObjects (gitproc.TransferBytesPerObject). The transfer lane's
// reserve is sized for repositories of about 100 000 objects, while a clone of
// the largest, walked without a bitmap, holds a third of what its bitmap does;
// so every step keeps room for a clone of the whole repository (roomObjects is
// the repository's), even the node's tip and a publication's steps, whose own
// figure covers only the tail or the layer's packs. A compaction's build alone
// keeps room for its inputs only: refused for a room that grows with the
// repository, it would gather packs until pushes are refused (Cache.AdmitCompaction).
func chargeOver(stepObjects, perObject, roomObjects int64) int64 {
	return stepObjects*perObject + roomObjects*gitproc.TransferBytesPerObject
}

// charge is what a declared git write is admitted at: chargeOver at its own
// figure, plus its fixed part (Write.BaseBytes). Every maintainer step's charge
// is read through it, never from the declaration's figures directly.
func (w Write) charge(objects, roomObjects int64) int64 {
	return chargeOver(objects, w.BytesPerObject, roomObjects) + w.BaseBytes
}

// perBackedCore is share for each of cores that memory backs at
// gitproc.CoreMemory a core, never for a core with no memory behind it: a
// container with a memory limit and no CPU limit sees every host core, and
// keeping for each refused every repository on a many-core, modest-memory node.
// Computed a mebibyte at a time, so the product never overflows.
func perBackedCore(cores int, memory, share int64) int64 {
	return min(int64(cores)*share, memory/(gitproc.CoreMemory>>20)*(share>>20))
}

// memoryReserve is what a node of memory bytes keeps from its background work:
// gitproc.TransferMemory for each of transfers, and ServingMemoryPerCore (the
// store, the broker, the engine and the read lane) for each core it backs.
func memoryReserve(transfers, cores int, memory int64) int64 {
	return int64(transfers)*gitproc.TransferMemory + perBackedCore(cores, memory, ServingMemoryPerCore)
}

// maintenanceBudget is what a node of mem gives its background work after
// reserve, never below zero. Zero (a gitlet.max_transfers whose TransferMemory
// takes the memory, or memory lowered in place below what the lanes were sized
// by) refuses every step and raises the alarm rather than refusing to boot:
// what the node serves it still serves.
func maintenanceBudget(mem, reserve int64) int64 {
	return max(0, mem-reserve)
}

// initMemory is Open's: the memory the pool's lanes were sized by, the reserve
// fixed from them, MaintenanceMemoryPerCore for each backed core, the budget
// left, and where each pass re-reads the memory.
func (c *Cache) initMemory(pool *gitproc.Pool) {
	m := pool.Memory()
	cores := pool.Slots(gitproc.ClassRead)
	c.memReserve = memoryReserve(pool.Slots(gitproc.ClassTransfer), cores, m.Bytes)
	c.maintMemory = perBackedCore(cores, m.Bytes, MaintenanceMemoryPerCore)
	c.memSource = pool.ReadMemory
	c.memory = m
	c.maint.setBudget(maintenanceBudget(m.Bytes, c.memReserve))
}

// rereadMemory is each maintainer pass's first step: a container's limit can
// change in place (Kubernetes resizes a pod's memory without a restart), and a
// budget read once would admit runs past a lowered limit. A change is logged
// with the new budget; a failed read keeps the last reading and says so once
// until a read succeeds: a budget is never guessed.
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

// BackgroundBudget is what this node gives its background work now, as of the
// last memory reading: every whole-repository step is admitted against it
// together. Zero before Open, and on a node whose lanes keep all its memory.
//
// The node advertises it on every presence beat (coord.GitBackgroundKey), so a
// repository's maintenance is placed on a node whose own admissions would hold
// it: the same figure, never a second estimate. A locked read, which the beat
// takes unbounded.
func (c *Cache) BackgroundBudget() int64 { return c.maint.budgetNow() }

// nodeMemory is the node's memory as the cache last read it.
func (c *Cache) nodeMemory() gitproc.Memory {
	c.memMu.Lock()
	defer c.memMu.Unlock()
	return c.memory
}

// maintStep is a set of the maintainer's declared writes (maintWrites), bit i
// its all[i], with three non-write steps past them ([stepCompact],
// [stepPublish], [stepExpunge]): what a plan takes, and what the node's memory
// refused an incarnation. A step's name (a line's and a count's `step`) is its
// write's Step, or "compact", "publish", "expunge".
type maintStep uint16

// stepCompact, stepPublish and stepExpunge are the background work run between
// passes (background.go: Cache.AdmitCompaction, Cache.AdmitPublication,
// Cache.AdmitExpunge and Cache.AdmitExpungeScan), on the bits past every
// write's (maxMaintWrites), so an incarnation's memoryRefused set, carried
// across a swap, logs each refusal once however many polls find it again.
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

// memoryRefusals is what the maintainer's latest walk found refused for
// memory: how many repositories were refused a bitmap (their own, or their
// publication's), and the largest of those charges. Its lock is a leaf.
type memoryRefusals struct {
	mu      sync.Mutex
	repos   int
	largest int64
}

// refusal is one step of one incarnation first found refused for memory.
type refusal struct {
	repo   string
	step   maintStep
	name   string
	charge int64
}

// refusalWalk gathers one walk's refusals: by step for the gauge, and the
// repositories served without a bitmap (unbitmapped) with their largest charge
// for the alarm. A refused graph or tip is gauged, never alarmed: past about
// 200 000 objects a graph's charge is under its bitmap's, so a graph refused
// alone is a small repository whose walk by date costs about two seconds at
// most (gitproc.WalkPerObject); a refused tip is still served through its
// installed chain's bitmap, its tail walked by date.
type refusalWalk struct {
	bitmaps, graphs, tips, publications int
	unbitmapped                         int
	largest                             int64
	fresh                               []refusal
}

// note records inc's refused steps (each charge in charges) and marks those
// refused for the first time on the incarnation, for their line and count.
// e.mu is held.
//
// A PUBLICATION REFUSED IS A BITMAP REFUSED: every pack pushed since the last
// publication stays outside every bitmap, so it counts toward the alarm, once
// even where the copy's own bitmap is refused too. Its line was logged at
// admission (Cache.AdmitPublication), so the walk logs none.
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

// noteRefusals publishes one walk: the gauges per step, the alarm's count and
// largest charge, and a line and count for each first-time refusal. Holds no
// lock of the cache or an entry.
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

// logRefusal logs and counts one step first refused an incarnation, with its
// charge and what the budget is made of.
func (c *Cache) logRefusal(r refusal) {
	m := c.nodeMemory()
	c.log.Warn("code_cache_maintenance_refused", "repo", r.repo, "step", r.name, "estimate", r.charge,
		"budget", c.maint.budgetNow(), "memory", m.Bytes, "memory_from", m.From, "reserved", c.memReserve)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheMaintenanceRefusals, 1,
			metrics.Attrs{"reason": refusedForMemory, "step": r.name})
	}
}

// refusedForMemory is crewlet.code.cache.maintenance.refusals' reason for a
// step whose charge is past the node's background budget.
const refusedForMemory = "memory"

// AlarmReading fills the git cache's half of an alarm reading
// ([statelog.KindCodeMaintenanceRefused]): how many repositories the maintainer
// cannot write a bitmap of because its charge (with room for a clone) is past
// the budget, the largest of those charges, and the budget, as of the latest
// pass.
//
// It is the cache's whole part of the reading; a node serving no git leaves the
// fields zero and the alarm silent. The engine fills every Gitlet node's alarm
// evaluation from it, so a refusal is raised where it was made: nothing else
// reaches the alarm.
func (c *Cache) AlarmReading(out *statelog.Reading) {
	c.refusals.mu.Lock()
	out.CodeMaintenanceRefused = c.refusals.repos
	out.CodeMaintenanceLargest = c.refusals.largest
	c.refusals.mu.Unlock()
	out.CodeMaintenanceBudget = c.maint.budgetNow()
}
