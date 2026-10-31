package cache

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BACKGROUND WORK A REPOSITORY'S MAINTAINER ASKS OF THE CACHE. Three
// pieces of whole-repository work run between the maintainer's passes, each on
// the git node a repository's maintenance is placed on (internal/code/compact):
// a COMPACTION's build, which merges the repository's small packs; a
// PUBLICATION's, which writes its next published multi-pack-index layer, the
// layer's bitmap and its commit-graph layer, repacking the non-full packs the
// layer gathers first; and an EXPUNGE's, which sorts some of its packs into
// what a ref reaches and what nothing does, and later rescues what a push
// revived meanwhile. Each is admitted here to a slot of the node's maintenance
// lane and to a charge of its background budget — by ONE function,
// [Cache.admitBackground], since copies of the slot-and-charge rule are how
// one of them drifts from another — and holds both until it lets them go, once
// its last git process has ended: the uploads, the proposal and the adoption
// that follow run no git, and holding the lane through an upload the store
// paces, hours for the largest, would idle the node's maintenance for every
// other repository. A publication that plans again over the same copy moves
// its charge in the slot it holds ([Admission.RechargePublication]), held to
// the same memory rule.

// Admission is one piece of background work's admission to this node's
// maintenance lane — a compaction's build ([Cache.AdmitCompaction]), a
// publication's ([Cache.AdmitPublication]) or an expunge's
// ([Cache.AdmitExpunge]): a maintenance slot reserved
// (gitproc.Reserve) and the work's memory charged against the node's
// background budget as a RUN of the maintainer's, both held until Release.
// Background work is held to the maintainer's rules — never the slot the
// maintainer keeps for a person's check, never a charge the node's memory
// cannot hold, never the room a pass kept for a repository it held back.
type Admission struct {
	c    *Cache
	slot *gitproc.Reserved
	// step is what the work was admitted for, and mem its charge — the one
	// [Admission.RechargePublication] may move, under the flight's lock.
	step backgroundStep
	mem  int64
	// once makes Release idempotent, and released says it ran.
	once     sync.Once
	released atomic.Bool
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

// The three background steps: stepCompactWork a compaction's build,
// stepPublishWork a publication's, and stepExpungeWork an expunge's build or
// its rescue — whose refusal a person waits on (a purge) and no alarm stands
// for, so its line is its own.
var (
	stepCompactWork = backgroundStep{bit: stepCompact, name: stepCompactName,
		line: "code_cache_compaction_refused", what: "compact"}
	stepPublishWork = backgroundStep{bit: stepPublish, name: stepPublishName,
		line: "code_cache_maintenance_refused", what: "publish"}
	stepExpungeWork = backgroundStep{bit: stepExpunge, name: stepExpungeName,
		line: "code_cache_expunge_refused", what: "expunge"}
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
	return c.admitBackground(s, stepCompactWork, func(int64) int64 { return compactionCharge(inputs, limits) })
}

// compactionCharge is what a compaction merging inputs objects is admitted at
// ([Cache.AdmitCompaction]).
func compactionCharge(inputs int64, limits code.Limits) int64 {
	return chargeOver(inputs, CompactBytesPerObject, inputs) + CompactWindowBytes(limits)
}

// AdmitExpunge admits one build of an expunge of s's repository — its kept
// and dropping packs' builds over inputs objects, or its rescue's over the
// dropping packs' — or refuses it at once, having started nothing, as
// [Cache.admitBackground] says, charged at chargeOver(inputs,
// CompactBytesPerObject, objects) + CompactWindowBytes(limits): a merge's own
// figure an object of what it sorts, its delta window, and room beside them
// for a transfer of the WHOLE repository — a publication's repack's charge.
//
// THE WHOLE REPOSITORY'S ROOM, NEVER A MERGE'S, because an expunge's build is
// a walk: `pack-objects --revs` from every tip it keeps reads every commit,
// tree and blob those reach — the repository's whole history, whichever packs
// it sorts — as a clone without a bitmap walks it, at the transfer's figure an
// object (gitproc.TransferBytesPerObject). And its refusal is "slower", as a
// publication's is, never a compaction's "pushes refused": a purge waits, and
// no push does. A node whose budget cannot hold the charge is refused it
// (ErrNoMemory); the expunge is asked again at every pass, and a git node with
// more memory serves it once it becomes the repository's maintainer.
//
// A refusal for memory is logged once an incarnation as
// code_cache_expunge_refused and counted
// (crewlet.code.cache.maintenance.refusals, reason memory, step expunge).
func (c *Cache) AdmitExpunge(s Synced, inputs int64, limits code.Limits) (*Admission, error) {
	if inputs < 0 {
		return nil, fmt.Errorf("%w: an expunge sorts %d objects", ErrInvalid, inputs)
	}
	return c.admitBackground(s, stepExpungeWork, func(objects int64) int64 {
		return expungeCharge(inputs, objects, limits)
	})
}

// expungeCharge is what one build of an expunge sorting inputs objects of a
// repository of objects objects is admitted at ([Cache.AdmitExpunge]).
func expungeCharge(inputs, objects int64, limits code.Limits) int64 {
	return chargeOver(inputs, CompactBytesPerObject, objects) + CompactWindowBytes(limits)
}

// MaintenanceCharge is the charge a repository of n live objects is PLACED by
// on the git node that maintains it (internal/estate's Router.Maintainer,
// internal/code/compact's three loops): the LARGEST charge any step its
// maintainer runs may be admitted at under limits — a publication's root,
// its multi-pack-index layer and bitmap over the whole history; its commit-
// graph layer and its repack over at most the whole repository; a compaction
// merging every one of its objects; an expunge sorting them — so the node a
// repository's maintenance is placed on is one whose own admissions would
// hold all of it.
//
// THE LARGEST, NEVER THE PUBLICATION'S ALONE: a merge's charge carries its
// delta window (CompactWindowBytes, 300 MiB at the default max_blob_bytes)
// beside its objects, which no figure an object holds — so a repository
// placed by its bitmap's 840 bytes an object alone, under some six million
// objects, was kept on a git node that would refuse its whole-repository
// compaction or expunge for memory while a larger node of the fleet could
// have run it, and a compaction refused there costs the repository its
// pushes once code.MaxPacksPerRepo gathers. The window follows the epoch's
// limits, so the charge does too.
func (c *Cache) MaintenanceCharge(n int64, limits code.Limits) int64 {
	return max(c.publicationCharge(n, 0, n, n, limits), compactionCharge(n, limits), expungeCharge(n, n, limits))
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

// AdmitPublication admits a publication of s's repository — a layer over
// layerObjects objects written over base layers of its chain (0 for a root),
// after a repack of repackObjects (0 for none) — or refuses it at once,
// having started nothing, as [Cache.admitBackground] says. Its steps run ONE
// AFTER ANOTHER in the one slot it reserves, so its charge is the LARGEST of
// theirs, each with room for a clone of the whole repository beside it:
//
//   - the multi-pack-index layer and its bitmap, an amount an object of the
//     WHOLE repository: the bitmap walks every commit however few objects
//     the layer adds (git 2.56.0's writer reuses no bitmap of the base) —
//     PublishBytesPerObject for a ROOT, which writes every commit's bitmap,
//     and PublishTailBytesPerObject for a layer over a base, the routine
//     publication, which holds a fraction of that (its figure says how
//     much). The root's figure, never the base's, is what a repository is
//     PLACED by ([Cache.MaintenanceCharge]): its maintainer must be able to
//     publish a root, and does whenever a whole chain is replaced;
//   - the commit-graph layer with every changed-path filter,
//     GraphLayerBytesPerObject an object of the layer and GraphBaseBytes
//     for the trees its filters' diffs hold, whatever the layer's size;
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
func (c *Cache) AdmitPublication(s Synced, base int, layerObjects, repackObjects int64, limits code.Limits) (
	*Admission, error) {

	if base < 0 || layerObjects < 1 || repackObjects < 0 {
		return nil, fmt.Errorf("%w: a publication of a layer over %d objects on %d layers after a repack of %d",
			ErrInvalid, layerObjects, base, repackObjects)
	}
	adm, err := c.admitBackground(s, stepPublishWork, func(objects int64) int64 {
		return c.publicationCharge(objects, base, layerObjects, repackObjects, limits)
	})
	if err == nil {
		s.entry.mu.Lock()
		s.inc.publishRefused = 0
		s.entry.mu.Unlock()
	}
	return adm, err
}

// publicationCharge is what a publication over a repository of objects
// objects holds — a layer over layerObjects written over base layers, after a
// repack of repackObjects (0 for none): the largest of its steps' charges
// ([Cache.AdmitPublication]). Its commit-graph layer computes changed-path
// filters as the maintainer's own graph does, so it is charged that write's
// fixed part, read from the graph's declaration (Write.BaseBytes,
// GraphBaseBytes) as every declared figure is.
func (c *Cache) publicationCharge(objects int64, base int, layerObjects, repackObjects int64,
	limits code.Limits) int64 {

	mem := max(publishCharge(objects, base),
		chargeOver(layerObjects, GraphLayerBytesPerObject, objects)+c.mw.all[c.mw.index(c.mw.graph)].BaseBytes)
	if repackObjects > 0 {
		mem = max(mem, chargeOver(repackObjects, CompactBytesPerObject, objects)+CompactWindowBytes(limits))
	}
	return mem
}

// RechargePublication holds a publication's admission to the charge of a
// plan its holder made again over the same copy s pins — a layer over
// layerObjects objects written over base layers, after a repack of
// repackObjects — in place of the charge it was admitted at, KEEPING ITS SLOT: at once where the new charge
// is no larger, and otherwise as [Cache.AdmitPublication] would admit it
// beside everything else in flight, its own old charge out of the count and
// the room the latest pass kept standing in it. On a refusal the admission
// keeps its old charge, the caller's to release: gitproc.ErrBusy where what
// is in flight leaves the new one no room, and ErrNoMemory — logged and
// counted as a publication's refusal is — where the budget cannot hold it
// alone. ErrInvalid answers an admission of a compaction, one released, and
// a Synced this cache did not hand out.
//
// WHY NOT RELEASE AND ADMIT AGAIN: a release wakes the maintainer, which may
// take the very slot the publication gave back before its second admission
// asks for one — and the publication answers busy, its maintainer asking the
// store for its base again at the next pass, as long as the node stays busy.
// The holder that plans again is the publication whose base the object store
// lost a part of (internal/code/compact), and its new plan replaces more
// layers: a larger layer, a repack the packs it gathers now need, or a ROOT
// where the store lost a part of the chain's root — charged at the root's
// figure, past a layer over a base — may pass the first plan's charge.
func (a *Admission) RechargePublication(s Synced, base int, layerObjects, repackObjects int64,
	limits code.Limits) error {

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
	mem := c.publicationCharge(objects, base, layerObjects, repackObjects, limits)
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
		a.released.Store(true)
		a.slot.Release()
		a.c.maint.done(a.mem)
		a.c.kickMaintainer()
	})
}
