package cache

import (
	"cmp"
	"context"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BACKGROUND WORK A REPOSITORY'S MAINTAINER ASKS OF THE CACHE. Two pieces
// of whole-repository work run between the maintainer's passes, each on the
// git node a repository's maintenance is placed on (internal/code/compact): a
// COMPACTION's build, which merges the repository's small packs, and a
// PUBLICATION's, which writes its next published multi-pack-index layer, the
// layer's bitmap and its commit-graph layer, repacking the non-full packs the
// layer gathers first. Each is admitted here to a slot of the node's
// maintenance lane and to a charge of its background budget — by ONE
// function, [Cache.admitBackground], since two copies of the slot-and-charge
// rule are how one of them drifts from the other — and holds both until it
// lets them go, once its last git process has ended: the uploads, the
// proposal and the adoption that follow run no git, and holding the lane
// through an upload the store paces, hours for the largest, would idle the
// node's maintenance for every other repository.

// Admission is one piece of background work's admission to this node's
// maintenance lane — a compaction's build ([Cache.AdmitCompaction]) or a
// publication's ([Cache.AdmitPublication]): a maintenance slot reserved
// (gitproc.Reserve) and the work's memory charged against the node's
// background budget as a RUN of the maintainer's, both held until Release.
// Background work is held to the maintainer's rules — never the slot the
// maintainer keeps for a person's check, never a charge the node's memory
// cannot hold, never the room a pass kept for a repository it held back.
type Admission struct {
	c    *Cache
	slot *gitproc.Reserved
	mem  int64
	// once makes Release idempotent.
	once sync.Once
}

// backgroundStep is what an [Admission] is admitted for: the step of an
// incarnation's memoryRefused set a refusal is kept under — logged and
// counted once an incarnation, carried across a swap — beside the
// maintainer's own steps, at the bits past every bit a declared write may
// take (maxMaintWrites).
type backgroundStep struct {
	bit  maintStep
	name string
	// line is what a refusal for memory is logged as, once an incarnation:
	// the compaction's own line, which code_compaction_behind's remedy names,
	// and the publication's the maintainer's own, since a refused publication
	// is a repository its maintainer cannot write a bitmap of — what
	// code_maintenance_refused, whose remedy names that line, stands for.
	line string
	// what the refusal's error names the work as.
	what string
}

// The two background steps: stepCompactWork a compaction's build, and
// stepPublishWork a publication's.
var (
	stepCompactWork = backgroundStep{bit: stepCompact, name: stepCompactName,
		line: "code_cache_compaction_refused", what: "compact"}
	stepPublishWork = backgroundStep{bit: stepPublish, name: stepPublishName,
		line: "code_cache_maintenance_refused", what: "publish"}
)

// AdmitCompaction admits a compaction of s's repository, merging inputs
// objects in all, or refuses it at once, having started nothing — as
// [Cache.admitBackground] says — charged at chargeOver(inputs,
// CompactBytesPerObject, inputs) — the merged packs' objects at the build's
// own figure, and room beside them for one transfer of as many objects — and
// CompactWindowBytes(limits), the build's delta window, which no count of
// objects charges. ALL OF IT FOLLOWS THE MERGE, NEVER THE REPOSITORY, where
// every other step of the maintainer's, a publication's among them, keeps
// room for a clone of its WHOLE repository (memory.go): the build is the one
// background step whose refusal is not "slower". The maintainer alone merges
// a repository's packs, a refusal for memory is asked again and refused again
// at every poll, and once the repository holds code.MaxPacksPerRepo live
// packs every push carrying one is refused compaction_behind — so a charge
// that grew with the repository would refuse every merge of one past a size,
// the floor's sixteen small pushes included, and its pushes with them, for as
// long as the node's memory stays as it is. The whole repository's clone room
// does exactly that: at gitproc.TransferBytesPerObject an object beside the
// window, a 16 GiB node of four cores — 9 GiB of background budget — refuses
// every merge of a repository past some 39 million objects, about 13 GB of a
// source repository's packs and under the 20 GiB code.limits.repo_max_bytes
// default, and a node of 2 GiB a core (1 GiB on four) every merge past some
// 3.2 million. What the room guards against is the build and a walking clone
// of the same repository overrunning the node at once — a coincidence that
// costs the git process the kernel kills, where the refusal costs the
// repository its pushes — and a clone of a repository a little past those
// sizes overruns the budget alone, the build running or not. A node whose
// budget cannot hold the build's own charge is refused it (ErrNoMemory), and
// while that lasts the maintainer's alarm names the repository its pushes are
// refused for (code_compaction_behind).
//
// A refusal for memory is logged once an incarnation as
// code_cache_compaction_refused and counted
// (crewlet.code.cache.maintenance.refusals, reason memory, step compact). A
// git node with more memory compacts it if it becomes the repository's
// maintainer, and this one once its memory is raised.
func (c *Cache) AdmitCompaction(s Synced, inputs int64, limits code.Limits) (*Admission, error) {
	if inputs < 0 {
		return nil, fmt.Errorf("%w: a compaction merges %d objects", ErrInvalid, inputs)
	}
	return c.admitBackground(s, stepCompactWork, func(int64) int64 {
		return chargeOver(inputs, CompactBytesPerObject, inputs) + CompactWindowBytes(limits)
	})
}

// PublishCharge is the charge a repository of n live objects is placed by on
// the git node that publishes it (internal/estate's Router.Maintainer): its
// publication's DOMINANT step, the multi-pack-index layer and the bitmap over
// the whole history, PublishBytesPerObject an object with room for a clone of
// the repository beside it — the figure [Cache.AdmitPublication] charges that
// step at, so the node a publication is placed on is one whose own admission
// would hold it.
func (c *Cache) PublishCharge(n int64) int64 { return publishCharge(n) }

// publishCharge is [Cache.PublishCharge]'s figure.
func publishCharge(n int64) int64 { return chargeOver(n, PublishBytesPerObject, n) }

// AdmitPublication admits a publication of s's repository — a layer over
// layerObjects objects, after a repack of repackObjects (0 for none) — or
// refuses it at once, having started nothing, as [Cache.admitBackground]
// says. Its steps run ONE AFTER ANOTHER in the one slot it reserves, so its
// charge is the LARGEST of theirs, each with room for a clone of the whole
// repository beside it:
//
//   - the multi-pack-index layer and its bitmap, PublishBytesPerObject an
//     object of the WHOLE repository: the bitmap walks every commit however
//     few objects the layer adds (git 2.56.0's writer reuses no bitmap of
//     the base);
//   - the commit-graph layer with every changed-path filter,
//     GraphLayerBytesPerObject an object of the layer;
//   - the repack, a compaction's build over its inputs — CompactBytesPerObject
//     an object of them and the delta window, CompactWindowBytes — beside the
//     whole repository's room, not a compaction's room of its inputs: a
//     publication refused leaves its tail outside every bitmap, slower, while
//     the compaction's room follows its merge only because its refusal costs
//     the repository its pushes ([Cache.AdmitCompaction]).
//
// A refusal for memory is logged once an incarnation as
// code_cache_maintenance_refused with step `publish` and counted
// (crewlet.code.cache.maintenance.refusals, reason memory, step publish), and
// the charge refused is kept on the incarnation, so the maintainer's every
// pass counts the repository on crewlet.code.cache.maintenance.refused{step
// publish} and in code_maintenance_refused for as long as the budget stays
// under it: a repository its maintainer cannot publish serves the packs
// pushed since its last publication unbitmapped, and every one pushed after.
// The record goes when a publication of it is admitted, and when the rows'
// chain moves — another node published it.
func (c *Cache) AdmitPublication(s Synced, layerObjects, repackObjects int64, limits code.Limits) (*Admission,
	error) {

	if layerObjects < 1 || repackObjects < 0 {
		return nil, fmt.Errorf("%w: a publication of a layer over %d objects after a repack of %d", ErrInvalid,
			layerObjects, repackObjects)
	}
	adm, err := c.admitBackground(s, stepPublishWork, func(objects int64) int64 {
		mem := max(publishCharge(objects), chargeOver(layerObjects, GraphLayerBytesPerObject, objects))
		if repackObjects > 0 {
			mem = max(mem, chargeOver(repackObjects, CompactBytesPerObject, objects)+CompactWindowBytes(limits))
		}
		return mem
	})
	if err == nil {
		s.entry.mu.Lock()
		s.inc.publishRefused = 0
		s.entry.mu.Unlock()
	}
	return adm, err
}

// admitBackground admits step's work on s's repository, charged at what
// charge makes of the repository's objects, or refuses it at once, having
// started nothing:
//
//   - ErrNoMemory: the charge alone is past what this node can give its
//     background work — definite for this node at this memory, logged once an
//     incarnation and counted. The budget is read again every maintainer
//     pass, so the caller asks again at its next poll, never remembering the
//     refusal;
//   - gitproc.ErrBusy: no maintenance slot the maintainer may use is free, or
//     the background work in flight — the maintainer's runs, the room a pass
//     kept standing for a repository it held back, a round's index, a check,
//     the other piece of background work — leaves the charge no room: a
//     moment's state;
//   - ErrStopped, and ErrInvalid for a Synced this cache did not hand out.
//
// The repository's objects are the incarnation s pins, read under its entry's
// lock: the count its rounds keep (incarnation.objects), as every maintainer
// step's charge reads it.
//
// ITS PIN, ITS SLOT AND ITS RELEASE are a check's (Cache.AdmitVerify): a slot
// held from the admission to the work's last git process, so every process of
// it — a compaction's build and the rebuild its bundle rule may ask for, a
// publication's repack, layer and graph — runs in it without asking the lane
// again, and the incarnation pinned by s, which its holder keeps until the
// work is answered. ITS MEMORY IS A RUN'S, never a check's
// (maintFlight.admitOther): a check is a person's, admitted beside the
// maintainer's runs and leaving its slots alone, while background work holds
// a maintenance slot for its whole build as a run does — so it is admitted as
// a run, at the maintainer's limit of maintenanceRuns slots and its
// allowance, beside the room the latest pass kept standing, and counted in
// flight until Release.
func (c *Cache) admitBackground(s Synced, step backgroundStep, charge func(objects int64) int64) (*Admission,
	error) {

	if c.isStopped() {
		return nil, ErrStopped
	}
	if err := c.handedOut(s, "a "+step.what+" admission"); err != nil {
		return nil, err
	}
	label := cmp.Or(s.Repo.Name, s.entry.id)
	s.entry.mu.Lock()
	objects := s.inc.objects
	s.entry.mu.Unlock()
	mem := charge(objects)
	refused := func(budget int64) error {
		c.backgroundRefused(s, step, mem)
		return fmt.Errorf("cache: %s %s: %w: it needs about %d bytes, and this node can give its background "+
			"work %d of its memory", step.what, label, ErrNoMemory, mem, budget)
	}
	if budget := c.maint.budgetNow(); mem > budget {
		return nil, refused(budget)
	}
	slot, err := c.git.Reserve()
	if err != nil {
		return nil, fmt.Errorf("cache: %s %s: %w", step.what, label, err)
	}
	lane := c.git.Slots(gitproc.ClassMaintenance)
	switch c.maint.admitStanding(mem, maintenanceRuns(lane), c.maintMemory) {
	case admitted:
		return &Admission{c: c, slot: slot, mem: mem}, nil
	case pastBudget:
		// The budget was lowered between the two reads: as definite as a
		// charge past it at the first.
		slot.Release()
		return nil, refused(c.maint.budgetNow())
	default:
		slot.Release()
		return nil, fmt.Errorf("cache: %s %s: %w: the maintenance this node is running, and the room it keeps "+
			"for a repository it held back, leave work of about %d bytes no slot or no room; it is asked again "+
			"at the next poll", step.what, label, gitproc.ErrBusy, mem)
	}
}

// backgroundRefused logs and counts step's work on s's repository refused for
// memory, once an incarnation however many polls find it again — the
// incarnation's memoryRefused bit for the step, which a swap carries — and,
// for a publication, keeps the charge refused on the incarnation for the
// maintainer's walk to count (memory.go's refusalWalk).
func (c *Cache) backgroundRefused(s Synced, step backgroundStep, mem int64) {
	e, inc := s.entry, s.inc
	e.mu.Lock()
	first := inc.memoryRefused&step.bit == 0
	inc.memoryRefused |= step.bit
	if step.bit == stepPublish {
		inc.publishRefused = mem
	}
	e.mu.Unlock()
	if !first {
		return
	}
	m := c.nodeMemory()
	c.log.Warn(step.line, "repo", cmp.Or(s.Repo.Name, e.id), "step", step.name, "estimate", mem,
		"budget", c.maint.budgetNow(), "memory", m.Bytes, "memory_from", m.From, "reserved", c.memReserve)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheMaintenanceRefusals, 1,
			metrics.Attrs{"reason": refusedForMemory, "step": step.name})
	}
}

// Run runs one git process of the work in the reserved slot
// (gitproc.Reserved.Run): a maintenance-class process, one at a time, and
// nothing once Released.
func (a *Admission) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	return a.slot.Run(ctx, cmd)
}

// GitVersion is the git every process of the work runs as
// (gitproc.Reserved.Version): what a published layer names as the release that
// wrote it, since a git node installs no layer of a git newer than its own.
func (a *Admission) GitVersion() gitproc.Version { return a.slot.Version() }

// Release gives the slot and the charge back, once however often it is
// called — after the work's last git process, before anything it does that
// runs no git (its uploads, its proposal, its adoption) — and wakes the
// maintainer, so a repository held back for the room the work took is taken
// up at once.
func (a *Admission) Release() {
	a.once.Do(func() {
		a.slot.Release()
		a.c.maint.done(a.mem)
		a.c.kickMaintainer()
	})
}
