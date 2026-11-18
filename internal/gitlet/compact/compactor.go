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

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Poll is the interval between two passes of the COMPACTION loop: one indexed
// listing a Gitlet node a minute (master §3.8), which holds a repository taking
// three hundred pushes a minute under gitlet.CompactTargetPacks plus a minute's
// pushes (BenchmarkCompact's steady arm; package doc, "Measured").
const Poll = time.Minute

// Estate is the code log as the compactor reaches it: the due listings and an
// expunge's tips through Read; a compaction's or a publication's proposal (a
// compaction carrying a layer) through Compact; an expunge's begin and commit
// through Expunge. The router's code seam satisfies it (estate.CodeSeam), its
// Compact and Expunge raising this node's floors past the record they landed
// in, so the next listing and sync see it on whichever data node answers.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	Compact(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error)
	Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
}

// Maintainers answers which of many repositories this node MAINTAINS, each at
// its own charge of a Gitlet node's background budget, all judged against ONE
// LISTING of who serves git and what each advertises (internal/estate's
// Router.MaintainerOf, ADR-0091), or an error where that listing cannot be
// read, on which the compactor does no maintainer's work rather than guess.
// Each loop's pass asks once. Every loop asks by one charge
// ([Compactor.PlacedBy]), so a repository's three pack writers run on ONE node
// (package doc, "Who maintains, and why one node does all three").
//
// Listed is whether this node is among the Gitlet nodes the placement lists
// (Router.Listed), three-valued as MaintainerOf is: what every loop waits on
// before its first pass ([Compactor.awaitListed]). The router satisfies it.
type Maintainers interface {
	MaintainerOf(charges map[string]int64) (map[string]bool, error)
	Listed() (bool, error)
}

// ListedRecheck is how often a loop that has not passed yet asks whether this
// node is listed ([Maintainers.Listed]): a SECOND, since the presence view
// (coord.LeaseView) lists a node within about one beat and the question is an
// in-memory read, so the first pass begins at most a second after the listing.
const ListedRecheck = time.Second

// Uploader is the object store as the compactor reaches it: the put every
// output and layer part is stored through (gitlet.Putter), the room a store
// with a byte reservation has left (objstore.Store.Usage), which a repository
// held back for a full store waits on ([Compactor.releaseHeld]), and what it
// holds under a key (objstore.Store.Stat), against which a publication checks
// its base's parts ([Compactor.heldBase]). The *objstore.Store satisfies it.
type Uploader interface {
	gitlet.Putter
	Usage(ctx context.Context) (objstore.Usage, bool, error)
	Stat(ctx context.Context, k objstore.Key) (objstore.Info, error)
}

// Cache is the node's git cache as the compactor reaches it; the *cache.Cache
// satisfies it. It syncs and pins a repository; reads its live, dropping and
// published packs for the plans, the charge its maintenance is placed by and
// the repositories holding a layer this node refused; admits each build to the
// maintenance lane; stages the scratch a build writes into (a merge's, a
// layer's, an expunge's or a rescue's, each pinning the incarnation); and
// adopts what a build made once its rows have applied, an expunge's dropping
// packs into the copy's dropping view.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) ([]gitlet.PackRow, error)
	DroppingPacks(s cache.Synced) ([]gitlet.PackRow, error)
	Chain(s cache.Synced) cache.Chain
	MaintenanceCharge(objects int64, limits gitlet.Limits) int64
	Damaged() []gitlet.RepoRef
	AdmitCompaction(s cache.Synced, objects int64, limits gitlet.Limits) (*cache.Admission, error)
	AdmitPublication(s cache.Synced, base int, layerObjects, repackObjects int64, limits gitlet.Limits) (*cache.Admission,
		error)
	AdmitExpunge(s cache.Synced, objects int64, limits gitlet.Limits) (*cache.Admission, error)
	AdmitExpungeScan(s cache.Synced, held int64) (*cache.Admission, error)
	NewMergeScratch(ctx context.Context, s cache.Synced, id string, packs []string) (*cache.Scratch, error)
	NewLayerScratch(ctx context.Context, s cache.Synced, id string, base int) (*cache.Scratch, error)
	StageExpunge(ctx context.Context, s cache.Synced, id string, inputs []string) (*cache.Scratch, error)
	StageRescue(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...gitlet.PackRef) error
	AdoptDropping(ctx context.Context, repoID string, sc *cache.Scratch, packs ...gitlet.PackRef) error
	AdoptLayer(ctx context.Context, repoID string, sc *cache.Scratch, row gitlet.LayerRef) error
}

// Publisher is a node's event stream as an expunge's commit announces itself
// on it (code_expunged): queue.Queue satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// eventBudget bounds the announcement of one committed expunge: FIVE SECONDS,
// the JetStream client's request timeout, as every Gitlet announcement is
// (internal/gitlet's checkEventBudget). The commit has applied by then, so a
// stream that has not taken it costs the event log only that account, and the
// repository's detail still names its latest expunge.
const eventBudget = 5 * time.Second

// Options builds a [Compactor]. Every field but Events, Faults, Metrics, Logger
// and Now is required, and New refuses a missing one by name.
type Options struct {
	Cache       Cache
	Objects     Uploader
	Estate      Estate
	Maintainers Maintainers
	// NodeID is this node, which every compaction and publication it builds
	// names as its builder (gitlet.Compaction.Node).
	NodeID string
	// Limits is the CURRENT epoch's code.limits, read at every build: a
	// function, because an activation moves them under a running compactor
	// (a build's window charge and pack.windowMemory read max_blob_bytes).
	Limits func() gitlet.Limits
	// Events is this node's event stream, which every expunge commit this
	// node proposes and hears applied is announced on (code_expunged); nil
	// announces none.
	Events Publisher
	// Faults is nil in production (internal/gitlet/gitletfault).
	Faults gitletfault.Injector
	// Metrics nil records nothing.
	Metrics *metrics.Recorder
	// Logger nil is the package's own (component gitlet.compact).
	Logger *slog.Logger
	// Now nil is time.Now.
	Now func() time.Time
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("compact: refused options")

// errClaimed is a build the repository's other build on this node leaves no
// plan yet ([Compactor.claimCompaction], [Compactor.claimPublication],
// [Compactor.claimExpunge]): busy, and planned again at the next pass.
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
// crewlet.code.publish.refusals beside the committer's own refusal codes.
// reasonStoreFull is the push's own code for it (gitlet.RefusalStoreFull):
// one name for one fact. reasonSuperseded is busy, not refused: a swap
// replaced the copy after the pass's sync pinned it (cache.ErrSuperseded),
// which the next pass syncs past.
const (
	reasonSuperseded     = "superseded"
	reasonMemory         = "memory"
	reasonStoreFull      = string(gitlet.RefusalStoreFull)
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
// refused an upload for room (objstore.ErrFull), with the estimate of what its
// build would store that the room must hold before it is planned again, until
// that loop's own upload of it is stored ([Compactor.stored]) or the listing
// stops naming it; and when the store last refused one.
type holds struct {
	repos  map[string]int64
	fullAt time.Time
}

// Compactor is one Gitlet node's maintenance of the repositories it MAINTAINS:
// three loops, each a pass every minute — COMPACTION merges small packs
// ([Compactor.Poll]), PUBLICATION writes the next published layer
// ([Compactor.PublishPass]) and EXPUNGE drops what no ref reaches, when a
// person asks and weekly once it is a quarter of the repository
// ([Compactor.ExpungePass]) — so minutes of a large publication's or a purge's
// build never delay a compaction keeping a hot repository under the pack
// bound, nor each other. Each runs one build at a time; how they share a
// repository: package doc, "Who maintains, and why one node does all three".
type Compactor struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	// bounds are the cut and the bundle bound a build is held to — the
	// domain's in production, a suite's scaled ones (export_test.go).
	bounds bounds
	// runBuild runs one git process of a build in its admission's slot
	// (cache.Admission.Run): a field so a suite can stand in a builder whose
	// outputs pass the bundle rule, or count what a build ran (export_test.go).
	runBuild func(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) (gitproc.Result, error)

	// compacting, publishing and expunging are each held for a whole build
	// (an expunge's scan to commit): ONE OF EACH AT A TIME A NODE, since each
	// holds a maintenance slot and up to its charge of memory throughout, and
	// the maintainer's own runs share that lane.
	compacting, publishing, expunging sync.Mutex

	// mu guards the memories, the claims and the reading below.
	mu sync.Mutex
	// compactions is every repository a compaction of this node is building;
	// publications every one a publication is building, with its RESERVATION
	// (every pack its plan covers, by git checksum; nil until it has planned);
	// expunges every one an expunge is running, scan to commit
	// ([expungeClaim]). The claims read them ([Compactor.claimCompaction]).
	compactions  map[string]bool
	publications map[string]map[string]bool
	expunges     map[string]*expungeClaim
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
	// compaction records, or an output past the bundle rule twice: the pack
	// bytes its next plan (a compaction's or a publication's repack) is capped
	// at, halved at each failure ([Compactor.halveCap]), kept for the
	// compactor's life.
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

// memo is a plan found not due: the repository's version and its cap, every
// input the plan read but the live set and the chain, which move only with the
// version. A plan made beside a reservation is never one ([Compactor.compact]).
type memo struct {
	version statelog.Position
	cap     int64
}

// lastBuild is a build of a publication this node began that has not landed:
// when it was admitted, and how long its git work held its maintenance slot,
// which is what the duty cycle counts ([PublishDutyCycle]).
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
// (gitlet.CompactPackBytes), the bound every output's bundle is held to
// (gitlet.MaxPackBytes) and the fewest packs a roll merges below the target
// (gitlet.CompactMinMerge), which is the due listing's floor too. The plan's
// full rule and its byte cap follow the cut. The domain's in production; a
// benchmark's arms vary them (export_test.go).
type bounds struct {
	cut, maxBundle int64
	minMerge       int
	// expungeMin is the unreachable bytes below which an expunge on the
	// maintainer's own schedule is never due (gitlet.ExpungeMinBytes).
	expungeMin int64
}

// production is what every compactor the engine builds is held to.
var production = bounds{cut: gitlet.CompactPackBytes, maxBundle: gitlet.MaxPackBytes, minMerge: gitlet.CompactMinMerge,
	expungeMin: gitlet.ExpungeMinBytes}

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
		compactions: map[string]bool{}, publications: map[string]map[string]bool{}, expunges: map[string]*expungeClaim{},
		notDue: map[string]memo{}, nothing: map[string]statelog.Position{}, built: map[string]lastBuild{},
		tails: map[string]time.Time{}, caps: map[string]int64{}, expungeCaps: map[string]int64{},
		done: make(chan struct{})}
	for i := range c.held {
		c.held[i].repos = map[string]int64{}
	}
	if c.log == nil {
		c.log = logging.Get("gitlet.compact")
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

// Start runs the three loops, each on its own goroutine, until
// [Compactor.Stop]: a pass of each AT ONCE once this node is listed
// ([Compactor.awaitListed]), so a node beside a repository at the pack bound
// merges it and names it in its alarm reading at the first listing that can
// place it, and one beside an expunge a crash left open finishes it; then one
// every [Poll], [PublishPoll] and [ExpungePoll]. Its listings go through
// Estate, which this node's copy answers through the runtime it joins, so the
// caller starts it once that runtime is published (the engine, with the native
// half's duties). A pass past its interval skips the tick rather than overlap
// the next, as a ticker drops ticks. A second Start does nothing.
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

// awaitListed waits until this node is listed among the Gitlet nodes
// ([Maintainers.Listed]), asking every [ListedRecheck], and reports whether it
// is — false where the compactor's life ended first.
//
// A NODE NO LISTING CARRIES MAINTAINS NOTHING (internal/estate's
// Router.Maintainer), and a booting node is listed only once its host beats,
// after this compactor starts: a pass before then would find every repository
// placed elsewhere and log a placement it could not read. An unreadable
// listing is waited out alike. After the first pass the loops never wait
// again: a node leaving the listing (draining) maintains nothing on each pass.
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
// its compaction until [Compactor.unclaimCompaction], answering the
// RESERVATION its plan leaves out ([plan]): by git checksum, every pack a
// publication of it building here will name and every input an expunge of it
// chose, until that expunge's rescue has synced past its begin; nil if neither.
// It is refused only while either has not planned (seconds): a merge then could
// take a pack about to be reserved, and one record would be refused stale. The
// copy holds for the compaction's length: a later plan of the publication adds
// only packs a chain layer names, which [plan] leaves out already. Why the
// reservation is all the committer needs (decide rules 2, 6c, 6d; the begin's
// rules 2-4; an expunge's build store is hard links a retirement leaves): the
// package doc, "Who maintains, and why one node does all three".
func (c *Compactor) claimCompaction(repoID string) (reserved map[string]bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, publishing := c.publications[repoID]
	x := c.expunges[repoID]
	if c.compactions[repoID] || (publishing && held == nil) || (x != nil && !x.begun && x.reserved == nil) {
		return nil, false
	}
	c.compactions[repoID] = true
	reserved = maps.Clone(held)
	if x != nil && !x.begun {
		if reserved == nil {
			reserved = make(map[string]bool, len(x.reserved))
		}
		maps.Copy(reserved, x.reserved)
	}
	return reserved, true
}

// unclaimCompaction gives repoID's compaction back.
func (c *Compactor) unclaimCompaction(repoID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.compactions, repoID)
}

// claimPublication reports whether this node may publish repoID now — no
// compaction or expunge of it building here — and takes the repository's
// publication, its reservation unset until [Compactor.reserve], until
// [Compactor.unclaimPublication]. It waits for a running compaction, whose
// inputs its plan would cover, so the later record would be refused stale (a
// merge takes seconds; the next pass finds it done), and for an expunge's
// whole cycle ([expungeClaim]).
func (c *Compactor) claimPublication(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, publishing := c.publications[repoID]
	if publishing || c.compactions[repoID] || c.expunges[repoID] != nil {
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
// compaction, publication or other expunge of it building here — and takes
// the repository's expunge, PLANNING, until [Compactor.unclaimExpunge]
// ([expungeClaim]). A running compaction's record would retire packs its plan
// chose, refusing the begin stale after a whole build; a merge takes seconds,
// so the next pass finds it done. No expunge builds beside a publication.
func (c *Compactor) claimExpunge(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, publishing := c.publications[repoID]
	if publishing || c.compactions[repoID] || c.expunges[repoID] != nil {
		return false
	}
	c.expunges[repoID] = &expungeClaim{}
	return true
}

// expungeClaim is an expunge this node is running, claim to commit
// ([Compactor.claimExpunge]), by what may build beside it:
//
//   - PLANNING, through its sync and plan: a compaction answers busy, since it
//     could merge packs the plan is about to choose;
//   - RESERVED ([Compactor.reserveExpunge]) until its rescue has synced past
//     its begin: a compaction merges beside it, leaving its inputs out;
//   - BEGUN ([Compactor.beganExpunge]), the begin having retired every input
//     here: a compaction builds beside it as though none ran.
//
// No publication builds beside any of the three: a begin drops every layer
// over its inputs and the decide refuses a layer while one is open (rule 6g).
type expungeClaim struct {
	// reserved is every input the expunge's plan chose, by git checksum: nil
	// while it plans, and once it has begun.
	reserved map[string]bool
	// begun: the expunge's rescue has synced past its begin.
	begun bool
}

// reserveExpunge records every input p chose as the reservation of repoID's
// running expunge ([expungeClaim]'s RESERVED), which a compaction of the
// repository then leaves out. Each call REPLACES it: the schedule's cycle
// reserves every pack it may take for its walk, then narrows to its plan's
// inputs ([Compactor.cycle]). The bundle rule's narrower rebuild reserves
// nothing again: the reservation already holds every input it chose.
func (c *Compactor) reserveExpunge(repoID string, p planned) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x := c.expunges[repoID]
	if x == nil || x.begun {
		return
	}
	x.reserved = make(map[string]bool, len(p.inputs))
	for _, in := range p.inputs {
		x.reserved[in.checksum] = true
	}
}

// beganExpunge marks repoID's expunge this node is running BEGUN
// ([expungeClaim]): its rescue has synced this node's copy past its begin,
// which retired every input, so a compaction of the repository no longer
// leaves any out.
func (c *Compactor) beganExpunge(repoID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if x := c.expunges[repoID]; x != nil {
		x.begun, x.reserved = true, nil
	}
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
// No pass yet, or none found, leaves the three at zero: nothing wrong here.
// Assigned field by field so the producers gate reaches each one through this
// concrete method (internal/engine's TestEveryAlarmReadingFieldHasAProducer).
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
func (c *Compactor) fault(ctx context.Context, p gitletfault.Point, at int64) error {
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

// halveCap halves repoID's cap after a build of p, capped at cap, failed: to
// HALF WHAT THE BUILD MERGED, since halving a cap far above a small roll's
// bytes would plan the same roll again, floored at its two lightest inputs. It
// answers whether the cap was already at the floor, where a plan would fail
// again and is remembered until its repository moves.
func (c *Compactor) halveCap(repoID string, p planned, cap int64) (atFloor bool) {
	floor := p.inputs[0].packBytes + p.inputs[1].packBytes
	was := min(cap, p.packBytes())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[repoID] = max(was/2, floor)
	return was <= floor
}
