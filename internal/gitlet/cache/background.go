package cache

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Background work a repository's maintainer asks of the cache, on the Gitlet
// node its maintenance is placed on (internal/gitlet/compact): a COMPACTION's
// build, merging small packs; a PUBLICATION's, writing the next published
// multi-pack-index layer, its bitmap and commit-graph layer after repacking
// the non-full packs it gathers; and an EXPUNGE's, sorting packs into what a
// ref reaches and what nothing does, later rescuing what a push revived. Each
// takes a maintenance slot and a charge of the background budget from ONE
// function, [Cache.admitBackground], so no copy of the rule drifts, and holds
// both until its last git process ends: holding the lane through the uploads
// that follow, hours for the largest, would idle every other repository's
// maintenance. A publication that plans again moves its charge in its slot
// ([Admission.RechargePublication]).

// Admission is one piece of background work's admission to this node's
// maintenance lane ([Cache.AdmitCompaction], [Cache.AdmitPublication],
// [Cache.AdmitExpunge]): a maintenance slot reserved (gitproc.Reserve) and
// its memory charged to the background budget as a RUN of the maintainer's,
// both held until Release. It keeps the maintainer's rules: never the slot
// kept for a person's check, never a charge the node's memory cannot hold,
// never the room a pass kept for a repository it held back.
type Admission struct {
	c    *Cache
	slot *gitproc.Reserved
	// step is what the work was admitted for, and mem its charge, which
	// [Admission.RechargePublication] may move under the flight's lock.
	step backgroundStep
	mem  int64
	// once makes Release idempotent, and released says it ran.
	once     sync.Once
	released atomic.Bool
}

// backgroundStep is what an [Admission] is admitted for: the bit of an
// incarnation's memoryRefused set its refusal is kept under (logged and
// counted once an incarnation, carried across a swap), past every bit a
// declared write may take (maxMaintWrites).
type backgroundStep struct {
	bit  maintStep
	name string
	// line is a refusal for memory's log line, once an incarnation: the
	// compaction's own, which code_compaction_behind's remedy names, and for
	// a publication the maintainer's, since code_maintenance_refused stands
	// for a repository whose bitmap its maintainer cannot write.
	line string
	// what the refusal's error names the work as.
	what string
}

// The three background steps. An expunge's (its build or rescue) has a line
// of its own: a person waits on its refusal (a purge), and no alarm stands
// for it.
var (
	stepCompactWork = backgroundStep{bit: stepCompact, name: stepCompactName,
		line: "code_cache_compaction_refused", what: "compact"}
	stepPublishWork = backgroundStep{bit: stepPublish, name: stepPublishName,
		line: "code_cache_maintenance_refused", what: "publish"}
	stepExpungeWork = backgroundStep{bit: stepExpunge, name: stepExpungeName,
		line: "code_cache_expunge_refused", what: "expunge"}
)

// AdmitCompaction admits a compaction of s's repository merging inputs
// objects, or refuses it at once as [Cache.admitBackground] says, charged at
// compactionCharge: the merge's objects with room for one transfer of as many,
// and its delta window. IT FOLLOWS THE MERGE, never the repository as every
// other maintainer step's clone room does (memory.go): only the maintainer
// merges, a refused merge is refused again every poll, and at
// gitlet.MaxPacksPerRepo live packs every push is refused compaction_behind,
// so a charge growing with the repository would refuse its pushes past a size
// (some 39 million objects on a 16 GiB, four-core node), where the room only
// guards a coincident clone the kernel would kill. A refusal (ErrNoMemory) is
// logged once an incarnation as code_cache_compaction_refused and counted
// (crewlet.code.cache.maintenance.refusals, reason memory, step compact).
func (c *Cache) AdmitCompaction(s Synced, inputs int64, limits gitlet.Limits) (*Admission, error) {
	if inputs < 0 {
		return nil, fmt.Errorf("%w: a compaction merges %d objects", ErrInvalid, inputs)
	}
	return c.admitBackground(s, stepCompactWork, func(int64) int64 { return compactionCharge(inputs, limits) })
}

// compactionCharge is what a compaction merging inputs objects is admitted at
// ([Cache.AdmitCompaction]).
func compactionCharge(inputs int64, limits gitlet.Limits) int64 {
	return chargeOver(inputs, CompactBytesPerObject, inputs) + CompactWindowBytes(limits)
}

// AdmitExpunge admits one build of an expunge of s's repository (its kept and
// dropping packs' builds over inputs objects, or its rescue's), or refuses it
// at once as [Cache.admitBackground] says, charged at expungeCharge: a merge's
// figure an object it sorts, its delta window, and room for a transfer of the
// WHOLE repository, since the build is a walk: `pack-objects --revs` from each
// kept tip reads the whole history, as a clone without a bitmap does
// (gitproc.TransferBytesPerObject). A due scan is [Cache.AdmitExpungeScan]'s.
// A refusal (ErrNoMemory) refuses no push at once, but what no ref reaches
// keeps counting against code.limits.repo_max_bytes, so one that stands ends
// in pushes refused repo_size. It is logged once an incarnation as
// code_cache_expunge_refused and counted
// (crewlet.code.cache.maintenance.refusals, reason memory, step expunge).
func (c *Cache) AdmitExpunge(s Synced, inputs int64, limits gitlet.Limits) (*Admission, error) {
	if inputs < 0 {
		return nil, fmt.Errorf("%w: an expunge sorts %d objects", ErrInvalid, inputs)
	}
	return c.admitBackground(s, stepExpungeWork, func(objects int64) int64 {
		return expungeCharge(inputs, objects, limits)
	})
}

// AdmitExpungeScan admits the SCAN that judges whether an expunge of s's
// repository is due on its maintainer's schedule (one walk of the whole
// history from what the expunge keeps, beside the pack indexes its caller
// holds, held bytes), or refuses it at once as [Cache.admitBackground] says,
// charged at held + chargeOver(0, 0, objects): the walk's room, as a build's.
//
// NEVER THE BUILD'S CHARGE: the scan sorts no object and searches no delta,
// so a repository found not due (nearly all, nearly every week) costs the
// walk alone, and a node that cannot hold a large build may still learn none
// is owed; the build's figure asked 2.4 GB of a gigabyte's scan, which holds
// under one. A refusal is logged and counted as an expunge's (step expunge).
func (c *Cache) AdmitExpungeScan(s Synced, held int64) (*Admission, error) {
	if held < 0 {
		return nil, fmt.Errorf("%w: an expunge's scan holds %d bytes", ErrInvalid, held)
	}
	return c.admitBackground(s, stepExpungeWork, func(objects int64) int64 { return held + chargeOver(0, 0, objects) })
}

// expungeCharge is what one build of an expunge sorting inputs objects of a
// repository of objects objects is admitted at ([Cache.AdmitExpunge]).
func expungeCharge(inputs, objects int64, limits gitlet.Limits) int64 {
	return chargeOver(inputs, CompactBytesPerObject, objects) + CompactWindowBytes(limits)
}

// MaintenanceCharge is the charge a repository of n live objects is PLACED by
// on the Gitlet node that maintains it (internal/estate's Router.Maintainer,
// internal/gitlet/compact's loops): the LARGEST any maintainer step may be
// admitted at under limits (a publication's root, graph layer and repack; a
// compaction or expunge over every object), so the node holds all of it. Not
// the publication's alone: a merge's delta window (CompactWindowBytes, 300 MiB
// at the default max_blob_bytes) sits beside its objects, so a node placed by
// the bitmap's figure could refuse a whole-repository compaction, costing
// pushes at gitlet.MaxPacksPerRepo, while a larger node could run it. The
// window follows the epoch's limits, so the charge does too.
func (c *Cache) MaintenanceCharge(n int64, limits gitlet.Limits) int64 {
	return c.mw.maintenanceCharge(n, limits)
}

// PlacementCharge is [Cache.MaintenanceCharge] over the declarations this
// build ships ([DerivedFiles]), for a node that opens no cache (an ingress
// node answering a person's purge), so it names the same maintainer the
// maintainers' own loops place the repository on. An error is the shipped
// declarations lacking a write the maintainer's plan takes, which every
// cache's Open refuses as well.
func PlacementCharge(n int64, limits gitlet.Limits) (int64, error) {
	mw, err := shippedWrites()
	if err != nil {
		return 0, fmt.Errorf("cache: the charge a repository is placed by: %w", err)
	}
	return mw.maintenanceCharge(n, limits), nil
}

// shippedWrites are the maintainer's writes of the shipped declarations, read
// once through the image Open builds, so a set Open refuses is refused here
// too rather than charged.
var shippedWrites = sync.OnceValues(func() (maintWrites, error) {
	im, err := newImage(DerivedFiles())
	if err != nil {
		return maintWrites{}, err
	}
	return newMaintWrites(im)
})

// maintenanceCharge is [Cache.MaintenanceCharge] over mw's declared writes,
// at a ROOT's publication (base 0): a maintainer publishes one whenever a
// whole chain is replaced.
func (mw maintWrites) maintenanceCharge(n int64, limits gitlet.Limits) int64 {
	return max(mw.publicationCharge(n, 0, n, n, limits), compactionCharge(n, limits), expungeCharge(n, n, limits))
}

// publishCharge is a publication's charge for its multi-pack-index layer and
// bitmap over a repository of n objects, the layer written over base layers
// of its chain ([Cache.AdmitPublication]): a ROOT's (base 0) at
// PublishBytesPerObject, and one over a base at PublishTailBytesPerObject.
func publishCharge(n int64, base int) int64 {
	if base == 0 {
		return chargeOver(n, PublishBytesPerObject, n)
	}
	return chargeOver(n, PublishTailBytesPerObject, n)
}

// AdmitPublication admits a publication of s's repository (a layer over
// layerObjects objects on base layers, 0 for a root, after a repack of
// repackObjects, 0 for none) or refuses it at once as [Cache.admitBackground]
// says. Its steps run in turn in its one slot, so it is charged the largest
// of theirs (publicationCharge), each beside room for a clone of the whole
// repository: the bitmap walks every commit, git 2.56.0 reusing no base's
// bitmap (publishCharge); the repack is a compaction's build beside that
// room, since a refused publication is only slower. A refusal for memory is
// logged once an incarnation (code_cache_maintenance_refused, step publish)
// and counted, its charge kept on the incarnation for every pass to count
// (crewlet.code.cache.maintenance.refused, code_maintenance_refused) until a
// publication is admitted or the rows' chain moves.
func (c *Cache) AdmitPublication(s Synced, base int, layerObjects, repackObjects int64, limits gitlet.Limits) (
	*Admission, error) {

	if base < 0 || layerObjects < 1 || repackObjects < 0 {
		return nil, fmt.Errorf("%w: a publication of a layer over %d objects on %d layers after a repack of %d",
			ErrInvalid, layerObjects, base, repackObjects)
	}
	adm, err := c.admitBackground(s, stepPublishWork, func(objects int64) int64 {
		return c.mw.publicationCharge(objects, base, layerObjects, repackObjects, limits)
	})
	if err == nil {
		s.entry.mu.Lock()
		s.inc.publishRefused = 0
		s.entry.mu.Unlock()
	}
	return adm, err
}

// publicationCharge is the largest of a publication's steps' charges over a
// repository of objects objects ([Cache.AdmitPublication]). Its commit-graph
// layer computes changed-path filters as the maintainer's graph does, so it
// is charged that write's declared fixed part (Write.BaseBytes,
// GraphBaseBytes).
func (mw maintWrites) publicationCharge(objects int64, base int, layerObjects, repackObjects int64,
	limits gitlet.Limits) int64 {

	mem := max(publishCharge(objects, base),
		chargeOver(layerObjects, GraphLayerBytesPerObject, objects)+mw.all[mw.index(mw.graph)].BaseBytes)
	if repackObjects > 0 {
		mem = max(mem, chargeOver(repackObjects, CompactBytesPerObject, objects)+CompactWindowBytes(limits))
	}
	return mem
}

// RechargePublication moves a publication's admission to the charge of a plan
// made again over the copy s pins (arguments as [Cache.AdmitPublication]'s),
// KEEPING ITS SLOT: at once where the new charge is no larger, otherwise as
// AdmitPublication would admit it beside everything in flight and the room the
// latest pass kept standing, its own old charge out of the count. A refusal
// keeps the old charge, the caller's to release: gitproc.ErrBusy for no room,
// ErrNoMemory (logged and counted) past the budget alone. ErrInvalid answers
// another step's admission, a released one, or a Synced not handed out here.
//
// Not release and admit again: a release wakes the maintainer, which may take
// the freed slot first, so the publication (one whose base the object store
// lost a part of) would answer busy for as long as the node stays busy.
func (a *Admission) RechargePublication(s Synced, base int, layerObjects, repackObjects int64,
	limits gitlet.Limits) error {

	c := a.c
	switch {
	case a.step.bit != stepPublish:
		return fmt.Errorf("%w: a %s admission holds no publication's charge", ErrInvalid, a.step.what)
	case a.released.Load():
		return fmt.Errorf("%w: a publication's admission released before its charge moved", ErrInvalid)
	case base < 0 || layerObjects < 1 || repackObjects < 0:
		return fmt.Errorf("%w: a publication of a layer over %d objects on %d layers after a repack of %d",
			ErrInvalid, layerObjects, base, repackObjects)
	}
	if err := c.handedOut(s, "a publication's charge"); err != nil {
		return err
	}
	label := cmp.Or(s.Repo.Name, s.entry.id)
	s.entry.mu.Lock()
	objects := s.inc.objects
	s.entry.mu.Unlock()
	mem := c.mw.publicationCharge(objects, base, layerObjects, repackObjects, limits)
	switch c.maint.recharge(&a.mem, mem, c.maintMemory) {
	case admitted:
		s.entry.mu.Lock()
		s.inc.publishRefused = 0
		s.entry.mu.Unlock()
		return nil
	case pastBudget:
		c.backgroundRefused(s, stepPublishWork, mem)
		return fmt.Errorf("cache: publish %s: %w: it needs about %d bytes, and this node can give its background "+
			"work %d of its memory", label, ErrNoMemory, mem, c.maint.budgetNow())
	case notDue:
		return fmt.Errorf("%w: the publication of %s holds no charge in flight", ErrInvalid, label)
	default:
		return fmt.Errorf("cache: publish %s: %w: the maintenance this node is running, and the room it keeps for "+
			"a repository it held back, leave work of about %d bytes no room; it is asked again at the next poll",
			label, gitproc.ErrBusy, mem)
	}
}

// admitBackground admits step's work on s's repository, charged at charge of
// the objects its pinned incarnation counts (incarnation.objects), or refuses
// it at once, having started nothing: ErrNoMemory where the charge alone is
// past the background budget (logged once an incarnation and counted; the
// caller asks again at its next poll, as every pass rereads the budget);
// gitproc.ErrBusy where no slot is free or what is in flight leaves no room;
// ErrStopped; ErrInvalid for a Synced this cache did not hand out. Its pin
// (held until the work is answered), slot and release are a check's
// (Cache.AdmitVerify), but its memory is a RUN's (maintFlight.admitOther): it
// holds a maintenance slot for its whole build, so it is admitted at
// maintenanceRuns' limit and allowance beside the room the latest pass kept
// standing, until Release.
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
		return &Admission{c: c, slot: slot, step: step, mem: mem}, nil
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
// memory, once an incarnation (its memoryRefused bit, which a swap carries),
// and for a publication keeps the refused charge on the incarnation for the
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
// (gitproc.Reserved.Version), which a published layer names, since a Gitlet
// node installs no layer of a git newer than its own.
func (a *Admission) GitVersion() gitproc.Version { return a.slot.Version() }

// Release gives back the slot and the charge, once however often called,
// after the work's last git process and before its uploads, proposal and
// adoption, and wakes the maintainer, so a repository held back for the room
// is taken up at once.
func (a *Admission) Release() {
	a.once.Do(func() {
		a.released.Store(true)
		a.slot.Release()
		a.c.maint.done(a.mem)
		a.c.kickMaintainer()
	})
}
