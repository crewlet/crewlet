package compact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Poll is the interval between two passes of the COMPACTION loop: one indexed
// listing a git node a minute (master §3.8). BenchmarkCompact's steady arm
// confirms it — pushes at three hundred a minute, one pass a simulated minute:
// the repository held 304 live packs at the most, under
// code.CompactTargetPacks plus a minute's pushes (350), every pass merging the
// minute's three hundred (doc.go's Measured).
const Poll = time.Minute

// Estate is the code log as the compactor reaches it: the due listings and an
// expunge's tips through Read; the proposal — a compaction's, or a
// publication's, which is a compaction carrying a layer — through Compact; and
// an expunge's begin and commit through Expunge. The router's code seam
// satisfies it (estate.CodeSeam), its Compact and its Expunge raising this
// node's floors past the record they landed in, so the next listing and the
// sync after it see it on whichever data node answers them.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error)
	Expunge(ctx context.Context, p code.ExpungeProposal) (code.ExpungeResult, error)
}

// Maintainers answers which of many repositories this node MAINTAINS, each
// for work charged at its own charge of a git node's background budget — the
// first git node of the repository's rendezvous order over the live git nodes
// whose ADVERTISED budget holds the charge, the home on a fleet of alike nodes
// (internal/estate's Router.MaintainerOf, ADR-0061) — every one judged against
// ONE LISTING of who serves git and what each advertises: the set this node
// maintains, or an error where that listing cannot be read, on which the
// compactor does none of a maintainer's work rather than guess. Each loop's
// pass asks it once, for every listed repository it must judge
// ([Compactor.Poll], [Compactor.PublishPass]). The router satisfies it.
//
// BOTH LOOPS ASK BY THE PUBLICATION'S CHARGE (cache.Cache.PublishCharge), so a
// repository's compactions and its publications — the two writers of its
// packs — run on ONE node, where a compaction never merges a pack a
// publication in flight planned over ([Compactor.claimCompaction]).
//
// Listed is whether this node is among the git nodes the placement lists —
// the only nodes a maintainer is chosen from (internal/estate's
// Router.Listed), three-valued as MaintainerOf is: what both loops wait on
// before their first pass ([Compactor.Start]).
type Maintainers interface {
	MaintainerOf(charges map[string]int64) (map[string]bool, error)
	Listed() (bool, error)
}

// ListedRecheck is how often a loop that has not passed yet asks whether this
// node is listed among the git nodes ([Maintainers.Listed]).
//
// A SECOND: the presence view hears a node's first beat and lists it within
// about one (coord.LeaseView watches the presence leases), and the question
// is a read of that view in memory — so a loop's first pass begins at most a
// second after the listing that makes this node a candidate, for a lock and a
// slice copy a second while it waits.
const ListedRecheck = time.Second

// Uploader is the object store as the compactor reaches it: the put every
// output and every layer part is stored through (code.StoreBundle's and
// code.StoreLayer's code.Putter), the room a store that holds a byte
// reservation has left (objstore.Store.Usage) — what a repository held back
// for a full store waits on ([Compactor.releaseHeld]) — and what it holds under
// a key (objstore.Store.Stat), which a publication holds the parts of the base
// it builds on to ([Compactor.heldBase]). The *objstore.Store satisfies it.
type Uploader interface {
	code.Putter
	Usage(ctx context.Context) (objstore.Usage, bool, error)
	Stat(ctx context.Context, k objstore.Key) (objstore.Info, error)
}

// Cache is the node's git cache as the compactor reaches it — the
// *cache.Cache satisfies it: the repository synced and pinned, its live packs
// — and its dropping ones — and its published chain as the copy holds it for
// the plans, the charge a publication is placed by, the repositories whose
// copy holds a layer this node refused, each build's admission to the
// maintenance lane, the scratch it writes into — a merge's, a compaction's or
// a publication's repack, leaning on a store of exactly the packs it merges,
// linked from the incarnation; a layer's linking the incarnation and gathering
// its repack's outputs; an expunge's holding its inputs and leaning on a store
// of every other live pack; a rescue's holding the dropping packs and leaning
// on the incarnation — every one pinning it — and the adoption of what it
// built once its rows have applied.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) []code.PackRow
	DroppingPacks(s cache.Synced) []code.PackRow
	Chain(s cache.Synced) cache.Chain
	PublishCharge(objects int64) int64
	Damaged() []code.RepoRef
	AdmitCompaction(s cache.Synced, objects int64, limits code.Limits) (*cache.Admission, error)
	AdmitPublication(s cache.Synced, layerObjects, repackObjects int64, limits code.Limits) (*cache.Admission,
		error)
	AdmitExpunge(s cache.Synced, objects int64, limits code.Limits) (*cache.Admission, error)
	NewMergeScratch(ctx context.Context, s cache.Synced, id string, packs []string) (*cache.Scratch, error)
	NewLayerScratch(ctx context.Context, s cache.Synced, id string, base int) (*cache.Scratch, error)
	StageExpunge(ctx context.Context, s cache.Synced, id string, inputs []string) (*cache.Scratch, error)
	StageRescue(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...code.PackRef) error
	AdoptLayer(ctx context.Context, repoID string, sc *cache.Scratch, row code.LayerRef) error
}

// Options builds a [Compactor]. Every field but Faults, Metrics, Logger and Now
// is required, and New refuses a missing one by name.
type Options struct {
	Cache       Cache
	Objects     Uploader
	Estate      Estate
	Maintainers Maintainers
	// NodeID is this node, which every compaction and publication it builds
	// names as its builder (code.Compaction.Node).
	NodeID string
	// Limits is the CURRENT epoch's code.limits, read at every build — a
	// function, because an activation moves them under a running compactor:
	// the build's window charge and its pack.windowMemory read
	// max_blob_bytes.
	Limits func() code.Limits
	// Faults is nil in production (internal/code/codefault).
	Faults codefault.Injector
	// Metrics nil records nothing.
	Metrics *metrics.Recorder
	// Logger nil is the package's own (component code.compact).
	Logger *slog.Logger
	// Now nil is time.Now.
	Now func() time.Time
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("compact: refused options")

// errClaimed is a compaction of a repository whose publication on this node
// has not planned yet, a publication of one whose compaction is running on
// this node, either of them while an expunge of the repository runs here, or
// an expunge while any build of it does ([Compactor.claimCompaction],
// [Compactor.claimPublication], [Compactor.claimExpunge]): busy, and planned
// again at the next pass.
var errClaimed = errors.New("compact: the repository's other build on this node leaves this one no plan yet")

// Outcome is what became of one compaction, publication or expunge this node
// planned ([Compactor.Compact], [Compactor.Publish], [Compactor.Expunge]).
type Outcome string

// The outcomes, each counted crewlet.code.compact.runs{outcome},
// crewlet.code.publish.runs{outcome} or crewlet.code.expunge.runs{outcome}.
const (
	// OutcomeApplied: its record applied, and its outputs replaced its
	// inputs in the rows — and a publication's layer the top of its chain.
	OutcomeApplied Outcome = "applied"
	// OutcomePending and OutcomeUnknown: its record was appended and is not
	// known applied yet — the committer's own outcomes, read again by the
	// next poll's listing, which reads the rows as they are.
	OutcomePending Outcome = "pending"
	OutcomeUnknown Outcome = "unknown"
	// OutcomeNotDue: the plan found nothing worth merging, or nothing to
	// publish — remembered until the repository's row moves.
	OutcomeNotDue Outcome = "not_due"
	// OutcomeWaiting: a publication's plan met a layer of the chain this
	// node neither installed nor refused — still at its installer — and
	// waits for it rather than replace it; planned again at the next pass.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeStale: an input left the live set first — another compaction,
	// a purge — or the chain moved under a publication, and the next poll
	// plans again from the rows.
	OutcomeStale Outcome = "stale"
	// OutcomeBusy: no maintenance slot or memory room free now, the
	// repository's other build on this node leaving this one no plan yet (a
	// publication still planning, a compaction running), or its committer
	// busy: a moment's state, due again at the next poll.
	OutcomeBusy Outcome = "busy"
	// OutcomeRefused: refused for a reason that holds until something
	// changes — the node's memory, an object store with no room, a layer too
	// large, the repository deleted, the committer's refusal of what was
	// built.
	OutcomeRefused Outcome = "refused"
	// OutcomeFailed: the sync, the build, an upload, the proposal or its
	// answer failed; the next poll tries again.
	OutcomeFailed Outcome = "failed"
)

// Outcomes is the closed set.
func Outcomes() []Outcome {
	return []Outcome{OutcomeApplied, OutcomePending, OutcomeUnknown, OutcomeNotDue, OutcomeWaiting, OutcomeStale,
		OutcomeBusy, OutcomeRefused, OutcomeFailed}
}

// Valid reports whether o is one of [Outcomes].
func (o Outcome) Valid() bool { return slices.Contains(Outcomes(), o) }

// The reasons of crewlet.code.compact.refusals and
// crewlet.code.publish.refusals beside the committer's own refusal codes,
// which each counts by their code: this node's memory, an object store with
// no room for an output or a part — the push's own refusal for it
// (code.RefusalStoreFull), one name for one fact — a build that wrote more
// outputs than one compaction records, an output past the bundle rule even at
// a smaller cut, a layer git wrote without a bitmap, and a layer past what its
// parts hold.
const (
	reasonMemory         = "memory"
	reasonStoreFull      = string(code.RefusalStoreFull)
	reasonOutputs        = "outputs"
	reasonBundleTooLarge = "bundle_too_large"
	reasonNoBitmap       = "no_bitmap"
	reasonLayerTooLarge  = "layer_too_large"
)

// PollReport is what one pass of the compaction loop did: how many
// repositories the listing named, how many of them this node maintains, how
// many a plan was made for, and how many of those compacted, were refused or
// failed; and every repository this node maintains at the pack bound — what
// the alarm reading names.
type PollReport struct {
	Listed, Maintained, Due, Compacted, Refused, Failed int
	Behind                                              []string
}

// loop is one of the compactor's three loops, by what a full store holds back
// of it.
type loop int

const (
	compactionLoop loop = iota
	publicationLoop
	expungeLoop
	loops
)

// holds is what a full object store holds back of one loop: every repository
// it refused an upload of for room (objstore.ErrFull), with what that build
// would store — the estimate the store's room is held to before it is
// planned again — until that loop's own upload of it is stored
// ([Compactor.stored]) or the loop's listing stops naming it; and when it
// last refused one.
type holds struct {
	repos  map[string]int64
	fullAt time.Time
}

// Compactor is one git node's maintenance of the repositories it MAINTAINS:
// three loops, each a pass every minute — COMPACTION, which merges a
// repository's small packs ([Compactor.Poll]); PUBLICATION, which writes its
// next published layer ([Compactor.PublishPass]); and EXPUNGE, which drops what
// no ref reaches when a person asks ([Compactor.ExpungePass]) — so a
// publication of a large repository, minutes of its build, never delays a
// compaction that keeps a hot repository under the pack bound, that
// repository's own included, and neither delays a purge. Each runs one build
// at a time; a compaction never merges a pack a publication of its repository
// in flight on this node planned over, and an expunge rewrites a repository
// with no other build of it on this node.
type Compactor struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	// bounds are the cut and the bundle bound a build is held to — the
	// domain's in production, a suite's scaled ones (export_test.go).
	bounds bounds
	// runBuild runs one git process of a build in its admission's slot — a
	// merge's pack-objects, a layer's multi-pack-index and commit-graph
	// writes: cache.Admission.Run, a field so a suite can stand in a builder
	// whose outputs pass the bundle rule, or count what a build ran
	// (export_test.go).
	runBuild func(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) (gitproc.Result, error)

	// compacting, publishing and expunging are each held for one build's
	// whole length — an expunge's from its scan to its commit: ONE
	// COMPACTION, ONE PUBLICATION AND ONE EXPUNGE AT A TIME A NODE, because
	// each holds a maintenance slot and up to its charge of memory for its
	// whole build, and the maintainer's own runs share that lane.
	compacting, publishing, expunging sync.Mutex

	// mu guards the memories, the claims and the reading below.
	mu sync.Mutex
	// compactions is every repository a compaction of this node is building,
	// and publications every repository a publication of this node is
	// building, with every pack its plan covers by git checksum — its
	// RESERVATION, which a compaction of the repository leaves out — nil
	// until it has planned ([Compactor.claimCompaction]); expunges is every
	// repository an expunge of this node is running, from its scan to its
	// commit, beside which neither of the others builds
	// ([Compactor.claimExpunge]).
	compactions  map[string]bool
	publications map[string]map[string]bool
	expunges     map[string]bool
	// notDue is every repository a compaction's plan found not due, at the
	// version and cap it was planned at ([memo]).
	notDue map[string]memo
	// nothing is every repository a publication's plan found nothing to
	// publish in — or a layer too large — at the version the listing named.
	nothing map[string]statelog.Position
	// built is this node's last build of a publication of each repository
	// that has not LANDED — failed, refused, or its record not known applied
	// — when it began and how long it ran: what holds the repository back
	// ([Compactor.paced]). A build whose record applied leaves nothing here:
	// the row its record wrote paces the next one ([Due]).
	built map[string]lastBuild
	// tails is when this node's publication loop first listed each
	// repository's current tail: what [Due]'s ceiling runs from
	// ([Compactor.sightTails]).
	tails map[string]time.Time
	// caps is every repository whose build wrote more outputs than one
	// compaction records, or an output past the bundle rule twice: the
	// pack bytes its next plan is capped at — a compaction's, and a
	// publication's repack, alike — halved at each failure and floored at
	// its two lightest inputs, kept for the compactor's life.
	caps map[string]int64
	// expungeCaps is the same of an expunge's builds, whose inputs a full
	// pack may be among and whose cap is its own ([ExpungeInputByteCap]),
	// floored at its lightest input.
	expungeCaps map[string]int64
	// held is what a full object store holds back of each loop.
	held [loops]holds
	// behind is the last compaction pass's repositories this node maintains
	// at the pack bound: [Compactor.AlarmReading]'s.
	behind []behindRepo

	// life is the loops' lifetime: Start's goroutines and every build they
	// run end with it.
	life context.Context
	end  context.CancelFunc
	// started and done: whether Start ran the loops, and their end.
	started bool
	done    chan struct{}
}

// memo is a plan found not due: the repository's version and its cap — every
// input the plan read but the live set and the chain, which move only with the
// version. A plan made beside a publication's reservation is never one
// ([Compactor.compact]).
type memo struct {
	version statelog.Position
	cap     int64
}

// lastBuild is a build of a publication this node began that has not landed:
// when it began — its admission — and how long its git work took, the time
// it held its maintenance slot, which is what the duty cycle counts
// ([PublishDutyCycle]).
type lastBuild struct {
	began time.Time
	took  time.Duration
}

// behindRepo is one repository at the pack bound this node maintains.
type behindRepo struct {
	id, name string
	packs    int64
}

// bounds are the figures a plan and its build are held to: the cut
// (code.CompactPackBytes), the bound every output's bundle is held to
// (code.MaxPackBytes) and the fewest packs a roll merges below the target
// (code.CompactMinMerge), which is the due listing's floor too. The plan's
// full rule and its byte cap follow the cut. The domain's in production; a
// benchmark's arms vary them (export_test.go).
type bounds struct {
	cut, maxBundle int64
	minMerge       int
}

// production is what every compactor the engine builds is held to.
var production = bounds{cut: code.CompactPackBytes, maxBundle: code.MaxPackBytes, minMerge: code.CompactMinMerge}

// New builds a Compactor, refusing every required field missing, by name, in
// one error listing them all. It starts nothing: [Compactor.Start] does.
func New(o Options) (*Compactor, error) {
	var missing []string
	need := func(absent bool, name string) {
		if absent {
			missing = append(missing, name)
		}
	}
	need(o.Cache == nil, "Cache")
	need(o.Objects == nil, "Objects")
	need(o.Estate == nil, "Estate")
	need(o.Maintainers == nil, "Maintainers")
	need(strings.TrimSpace(o.NodeID) == "", "NodeID")
	need(o.Limits == nil, "Limits")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing", ErrOptions, strings.Join(missing, ", "))
	}
	c := &Compactor{o: o, log: o.Logger, now: o.Now, bounds: production, runBuild: runInSlot,
		compactions: map[string]bool{}, publications: map[string]map[string]bool{}, expunges: map[string]bool{},
		notDue: map[string]memo{}, nothing: map[string]statelog.Position{}, built: map[string]lastBuild{},
		tails: map[string]time.Time{}, caps: map[string]int64{}, expungeCaps: map[string]int64{},
		done: make(chan struct{})}
	for i := range c.held {
		c.held[i].repos = map[string]int64{}
	}
	if c.log == nil {
		c.log = logging.Get("code.compact")
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.life, c.end = context.WithCancel(context.Background())
	return c, nil
}

// runInSlot runs cmd in the admission's reserved slot.
func runInSlot(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) (gitproc.Result, error) {
	return adm.Run(ctx, cmd)
}

// Start runs the three loops, each on a goroutine of its own, until
// [Compactor.Stop]: a pass of each AT ONCE once this node is LISTED among the
// git nodes ([Compactor.awaitListed]) — so a node that comes up beside a
// repository at the pack bound starts merging it, and names it in its alarm
// reading, at the first listing that can place it rather than a minute later,
// and one that comes up beside an expunge a crash left open finishes it —
// and then one every [Poll], every [PublishPoll] and every [ExpungePoll]. The pass at once holds
// only where its seams answer from the start: its listings go through Estate,
// which this node's own copy answers through the runtime it joins, so its
// caller starts it once that runtime is published (the engine's, with the
// native half's duties). A pass that runs past its interval skips the tick
// rather than overlapping the next, since a ticker drops the ticks its reader
// is too busy for. A second Start does nothing.
func (c *Compactor) Start() {
	c.mu.Lock()
	if c.started || c.life.Err() != nil {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	var running sync.WaitGroup
	running.Add(3)
	go c.loop(&running, Poll, "code_compaction_pass_failed", func(ctx context.Context) error {
		_, err := c.Poll(ctx)
		return err
	})
	go c.loop(&running, PublishPoll, "code_publication_pass_failed", func(ctx context.Context) error {
		_, err := c.PublishPass(ctx)
		return err
	})
	go c.loop(&running, ExpungePoll, "code_expunge_pass_failed", func(ctx context.Context) error {
		_, err := c.ExpungePass(ctx)
		return err
	})
	go func() {
		running.Wait()
		close(c.done)
	}()
}

// loop runs pass once this node is listed and then every interval, until the
// compactor's life ends, logging a pass that failed as failed.
func (c *Compactor) loop(running *sync.WaitGroup, every time.Duration, failed string,
	pass func(context.Context) error) {

	defer running.Done()
	if !c.awaitListed() {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := pass(c.life); err != nil && c.life.Err() == nil {
			c.log.Warn(failed, "error", err)
		}
		select {
		case <-c.life.Done():
			return
		case <-tick.C:
		}
	}
}

// awaitListed waits until this node is listed among the git nodes
// ([Maintainers.Listed]), asking every [ListedRecheck], and reports whether it
// is — false where the compactor's life ended first.
//
// A NODE NO LISTING CARRIES MAINTAINS NOTHING, whatever it would rank
// (internal/estate's Router.Maintainer): its peers cannot see it. A booting
// node's presence is listed only once its host beats — after the engine has
// built the code host and started this compactor — so a pass at once would
// find every repository placed elsewhere or nowhere: wasted, and logged as a
// placement this node could not read, when it read one that does not carry
// it yet. A listing that cannot be read is waited out alike, since a pass
// would do none of a maintainer's work either. After the first pass the loops
// never wait again: a node that leaves the listing — one draining — finds
// itself no repository's maintainer on each pass.
func (c *Compactor) awaitListed() bool {
	tick := time.NewTicker(ListedRecheck)
	defer tick.Stop()
	for {
		if listed, err := c.o.Maintainers.Listed(); err == nil && listed {
			return true
		}
		select {
		case <-c.life.Done():
			return false
		case <-tick.C:
		}
	}
}

// Stop ends the loops and the builds in flight — each killed, its uploads
// abandoned, its proposal cancelled: an output or a part already stored and
// named by no row is the collector's, and a proposal its committer decided
// lands in the rows whatever this node does — and waits for them to end,
// until ctx does. Idempotent.
func (c *Compactor) Stop(ctx context.Context) {
	c.end()
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		c.log.Warn("code_compaction_stop_unfinished", "error", ctx.Err(),
			"detail", "a compaction or a publication was still running when the stop's wait ended")
	}
}

// claimCompaction reports whether this node may compact repoID now, and takes
// the repository's compaction until [Compactor.unclaimCompaction], answering
// the RESERVATION of a publication of it building on this node: every pack
// that publication's layer will name, its repack's inputs among them, by git
// checksum — what the compaction's plan leaves out beside the packs a
// published layer names ([plan]) — and nil where none is building.
//
// BESIDE A PUBLICATION, NEVER OVER ITS PACKS. A publication of a large
// repository runs for minutes of its build, and a hot repository gains packs at
// up to three hundred a minute (the doc's busy repository), so a repository
// whose compactions waited for its publication would pass code.MaxPacksPerRepo
// and have every push carrying a pack refused compaction_behind until the
// publication answered. Nothing the committer decides needs more than the
// reservation: a pack pushed after the publication's plan was live at no
// position the plan read, so the layer need not name it (decide rule 6d) and
// does not, and a merge of such packs retires nothing the layer names (rules
// 2 and 6c) — so the two records apply in either order.
//
// REFUSED WHILE THE PUBLICATION HAS NOT PLANNED — its sync and its plan,
// seconds — since a compaction planned then could merge packs the layer is
// about to name, and one of the two would be refused stale: the publication
// after a whole build, or the merge. A later plan of the same publication (a
// base the store lost, a tail git could not bitmap) adds to the reservation
// only packs a layer of the chain names, which the compaction's plan leaves
// out already, so the reservation handed here holds for its whole length.
//
// REFUSED WHILE AN EXPUNGE OF THE REPOSITORY RUNS HERE, from its scan to its
// commit ([Compactor.claimExpunge]).
func (c *Compactor) claimCompaction(repoID string) (reserved map[string]bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, publishing := c.publications[repoID]
	if c.compactions[repoID] || (publishing && held == nil) || c.expunges[repoID] {
		return nil, false
	}
	c.compactions[repoID] = true
	return maps.Clone(held), true
}

// unclaimCompaction gives repoID's compaction back.
func (c *Compactor) unclaimCompaction(repoID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.compactions, repoID)
}

// claimPublication reports whether this node may publish repoID now — no
// compaction of it building here — and takes the repository's publication,
// its reservation unset until [Compactor.reserve], until
// [Compactor.unclaimPublication].
//
// A PUBLICATION WAITS FOR A RUNNING COMPACTION: its plan would cover the
// merge's inputs, live as of it, so whichever record landed second would be
// refused stale — the publication's after its whole build. A merge is bounded
// by what it merges, seconds at a busy repository's rate, so the next pass
// finds it done. And it waits for an expunge of the repository running here
// ([Compactor.claimExpunge]).
func (c *Compactor) claimPublication(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, publishing := c.publications[repoID]; publishing || c.compactions[repoID] || c.expunges[repoID] {
		return false
	}
	c.publications[repoID] = nil
	return true
}

// reserve adds every pack p covers — its members, its repack's inputs among
// them — to the reservation of the publication of repoID this node is
// building: from its first plan on, a compaction of the repository is
// admitted beside it and leaves those packs out ([Compactor.claimCompaction]).
func (c *Compactor) reserve(repoID string, p layerPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reserved := c.publications[repoID]
	if reserved == nil {
		reserved = make(map[string]bool, len(p.members))
		c.publications[repoID] = reserved
	}
	for _, m := range p.members {
		reserved[m.checksum] = true
	}
}

// unclaimPublication gives repoID's publication back, with its reservation.
func (c *Compactor) unclaimPublication(repoID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.publications, repoID)
}

// claimExpunge reports whether this node may expunge repoID now — no
// compaction, publication or other expunge of it building here — and takes the
// repository until [Compactor.unclaimExpunge], which neither of the other two
// claims while it holds it ([Compactor.claimCompaction],
// [Compactor.claimPublication]).
//
// THE WHOLE REPOSITORY, FROM ITS SCAN TO ITS COMMIT — one rewrite of a
// repository at a time on its maintainer, never a reservation of packs as a
// publication's is: a begin retires every input its scan chose among the live
// packs, full and published ones included, and drops every published layer
// over them, so a merge planned beside it could take one of its inputs, and a
// layer planned beside it would name one or sit over a layer the begin drops
// — either refusing the begin stale after its whole build, or itself. So a
// repository's merges wait on this node for the length of one cycle, at the
// rate a person asks a purge: a cycle or two a request.
func (c *Compactor) claimExpunge(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, publishing := c.publications[repoID]; publishing || c.compactions[repoID] || c.expunges[repoID] {
		return false
	}
	c.expunges[repoID] = true
	return true
}

// unclaimExpunge gives repoID's expunge back.
func (c *Compactor) unclaimExpunge(repoID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.expunges, repoID)
}

// AlarmReading fills the reading's compaction half from the last pass: how
// many repositories this node maintains at the pack bound, the most live packs
// among them and that repository's name — what raises code_compaction_behind.
// A pass that has not run, or found none, leaves the three at zero, which the
// alarm table reads as nothing wrong here. Assigned here, field by field, so
// the reading's producers gate reaches each one through this concrete method
// (internal/engine's TestEveryAlarmReadingFieldHasAProducer).
func (c *Compactor) AlarmReading(out *statelog.Reading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out.CodeCompactionBehind = len(c.behind)
	if len(c.behind) == 0 {
		return
	}
	most := c.behind[0]
	for _, b := range c.behind[1:] {
		if b.packs > most.packs {
			most = b
		}
	}
	out.CodeCompactionBehindLargest = most.packs
	out.CodeCompactionBehindRepo = most.name
}

// fault is the build's fault point p, hit at the offset at — nil in
// production, where every call is this nil check.
func (c *Compactor) fault(ctx context.Context, p codefault.Point, at int64) error {
	if c.o.Faults == nil {
		return nil
	}
	return c.o.Faults.Hit(ctx, p, at)
}

// counted records one compaction's outcome and how long it took.
func (c *Compactor) counted(o Outcome, began time.Time) {
	if c.o.Metrics == nil {
		return
	}
	c.o.Metrics.Add(metrics.CodeCompactRuns, 1, metrics.Attrs{"outcome": string(o)})
	c.o.Metrics.Observe(metrics.CodeCompactDuration, c.now().Sub(began), metrics.Attrs{"outcome": string(o)})
}

// refused counts one compaction refused by its reason.
func (c *Compactor) refused(reason string) {
	if c.o.Metrics != nil {
		c.o.Metrics.Add(metrics.CodeCompactRefusals, 1, metrics.Attrs{"reason": reason})
	}
}

// capFor is the pack bytes repoID's next plan is capped at: its halved cap
// where a build of it wrote too many outputs or too large a bundle, else
// [InputByteCap] of the nominal cut.
func (c *Compactor) capFor(repoID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cap, ok := c.caps[repoID]; ok {
		return cap
	}
	return InputByteCap(c.bounds.cut)
}

// halveCap halves repoID's cap after a build of p, capped at cap, failed —
// HALF WHAT THE BUILD MERGED, which a cap far above a small roll's bytes would
// otherwise halve to no effect, the next plan the same roll — floored at its
// two lightest inputs, and answers whether the cap was already at the floor:
// a plan that fails there would fail again, and is remembered until its
// repository moves.
func (c *Compactor) halveCap(repoID string, p planned, cap int64) (atFloor bool) {
	floor := p.inputs[0].packBytes + p.inputs[1].packBytes
	was := min(cap, p.packBytes())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[repoID] = max(was/2, floor)
	return was <= floor
}
