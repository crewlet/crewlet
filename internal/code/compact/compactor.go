package compact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Poll is the interval between two passes: one indexed listing a git node a
// minute (master §3.8). BenchmarkCompact's steady arm — pushes at three hundred
// a minute, one pass a simulated minute — confirms it by the most live packs a
// repository reaches, which must stay under code.CompactTargetPacks plus a
// minute's pushes.
const Poll = time.Minute

// Estate is the code log as the compactor reaches it: the due listing through
// Read, and the proposal through Compact — the router's code seam satisfies it
// (estate.CodeSeam), its Compact raising this node's floors past the record a
// compaction landed in, so the next listing and the adoption's sync see it on
// whichever data node answers them.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error)
}

// Homes answers whether this node is a repository's HOME — the first git node
// of its rendezvous order over the live git nodes (ADR-0059) — three-valued:
// (true, nil), (false, nil), or an error where who serves git cannot be read,
// on which the compactor compacts nothing rather than guess. The router
// satisfies it.
type Homes interface {
	IsHome(repoID string) (bool, error)
}

// Uploader is the object store as the compactor reaches it: the put every
// output is stored through (code.StoreBundle's code.Putter), and the room a
// store that holds a byte reservation has left (objstore.Store.Usage) — what a
// repository held back for a full store waits on ([Compactor.Poll]). The
// *objstore.Store satisfies it.
type Uploader interface {
	code.Putter
	Usage(ctx context.Context) (objstore.Usage, bool, error)
}

// Cache is the node's git cache as the compactor reaches it — the
// *cache.Cache satisfies it: the repository synced and pinned, its live packs
// for the plan, the build's admission to the maintenance lane, the scratch it
// writes into — leaning on a store of exactly the packs it merges, linked
// from the incarnation, which it pins — and the outputs' adoption once their
// rows have applied.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) []code.PackRow
	AdmitCompaction(s cache.Synced, objects int64, limits code.Limits) (*cache.Compacting, error)
	NewMergeScratch(ctx context.Context, s cache.Synced, id string, packs []string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...code.PackRef) error
}

// Options builds a [Compactor]. Every field but Faults, Metrics, Logger and Now
// is required, and New refuses a missing one by name.
type Options struct {
	Cache   Cache
	Objects Uploader
	Estate  Estate
	Homes   Homes
	// NodeID is this node, which every compaction it builds names as its
	// builder (code.Compaction.Node).
	NodeID string
	// Limits is the CURRENT epoch's code.limits, read at every compaction —
	// a function, because an activation moves them under a running
	// compactor: the build's window charge and its pack.windowMemory read
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

// Outcome is what became of one compaction this node planned ([Compactor.Compact]).
type Outcome string

// The outcomes, each counted crewlet.code.compact.runs{outcome}.
const (
	// OutcomeApplied: its record applied, and its outputs replaced its
	// inputs in the rows.
	OutcomeApplied Outcome = "applied"
	// OutcomePending and OutcomeUnknown: its record was appended and is not
	// known applied yet — the committer's own outcomes, read again by the
	// next poll's listing, which reads the rows as they are.
	OutcomePending Outcome = "pending"
	OutcomeUnknown Outcome = "unknown"
	// OutcomeNotDue: the plan found nothing worth merging — remembered until
	// the repository's row moves ([memo]).
	OutcomeNotDue Outcome = "not_due"
	// OutcomeStale: an input left the live set first — another compaction,
	// a purge — and the next poll plans again from the rows.
	OutcomeStale Outcome = "stale"
	// OutcomeBusy: no maintenance slot or memory room free now, or the
	// repository's committer busy: a moment's state, due again at the next
	// poll.
	OutcomeBusy Outcome = "busy"
	// OutcomeRefused: refused for a reason that holds until something
	// changes — the node's memory, an object store with no room, the
	// repository deleted, the committer's refusal of what was built.
	OutcomeRefused Outcome = "refused"
	// OutcomeFailed: the sync, the build, an upload, the proposal or its
	// answer failed; the next poll tries again.
	OutcomeFailed Outcome = "failed"
)

// Outcomes is the closed set.
func Outcomes() []Outcome {
	return []Outcome{OutcomeApplied, OutcomePending, OutcomeUnknown, OutcomeNotDue, OutcomeStale, OutcomeBusy,
		OutcomeRefused, OutcomeFailed}
}

// Valid reports whether o is one of [Outcomes].
func (o Outcome) Valid() bool { return slices.Contains(Outcomes(), o) }

// The reasons of crewlet.code.compact.refusals beside the committer's own
// refusal codes, which it counts by their code: this node's memory, an object
// store with no room for an output — the push's own refusal for it
// (code.RefusalStoreFull), one name for one fact — a build that wrote more
// outputs than one compaction records, and an output past the bundle rule
// even at a smaller cut.
const (
	reasonMemory         = "memory"
	reasonStoreFull      = string(code.RefusalStoreFull)
	reasonOutputs        = "outputs"
	reasonBundleTooLarge = "bundle_too_large"
)

// PollReport is what one pass did: how many repositories the listing named,
// how many of them this node is home for, how many a plan was made for, and
// how many of those compacted, were refused or failed; and every repository
// this node is home for at the pack bound — what the alarm reading names.
type PollReport struct {
	Listed, Home, Due, Compacted, Refused, Failed int
	Behind                                        []string
}

// Compactor is one git node's compaction loop: every [Poll] a pass, one
// compaction at a time.
type Compactor struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	// bounds are the cut and the bundle bound a build is held to — the
	// domain's in production, a suite's scaled ones (export_test.go).
	bounds bounds
	// runBuild runs one git process of a build in its admission's slot:
	// cache.Compacting.Run, a field so a suite can stand in a builder whose
	// outputs pass the bundle rule (export_test.go).
	runBuild func(ctx context.Context, adm *cache.Compacting, cmd gitproc.Cmd) (gitproc.Result, error)

	// compacting is held for one compaction's whole length: ONE AT A TIME A
	// NODE, because a compaction holds a maintenance slot and up to its
	// charge of memory for its whole build, and the maintainer's bitmaps
	// share that lane — two at once would hold two of its slots but one.
	compacting sync.Mutex

	// mu guards the memories and the reading below.
	mu sync.Mutex
	// notDue is every repository a plan found not due, at the version and
	// cap it was planned at ([memo]).
	notDue map[string]memo
	// caps is every repository whose build wrote more outputs than one
	// compaction records, or an output past the bundle rule twice: the
	// pack bytes its next plan is capped at, halved at each failure and
	// floored at its two lightest inputs, kept for the compactor's life.
	caps map[string]int64
	// held is every repository a full object store refused an output of,
	// with what its compaction would store — the estimate a store's room is
	// held to before it is planned again — until its own outputs are stored
	// ([Compactor.stored]) or the listing stops naming it; and fullAt the
	// last such refusal.
	held   map[string]int64
	fullAt time.Time
	// behind is the last pass's repositories this node is home for at the
	// pack bound: [Compactor.AlarmReading]'s.
	behind []behindRepo

	// life is the loop's lifetime: Start's goroutine and every compaction
	// it runs end with it.
	life context.Context
	end  context.CancelFunc
	// started and done: whether Start ran the loop, and its end.
	started bool
	done    chan struct{}
}

// memo is a plan found not due: the repository's version and its cap — every
// input the plan read but the live set, which moves only with the version.
type memo struct {
	version statelog.Position
	cap     int64
}

// behindRepo is one repository at the pack bound this node is home for.
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
	need(o.Homes == nil, "Homes")
	need(strings.TrimSpace(o.NodeID) == "", "NodeID")
	need(o.Limits == nil, "Limits")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing", ErrOptions, strings.Join(missing, ", "))
	}
	c := &Compactor{o: o, log: o.Logger, now: o.Now, bounds: production, runBuild: runInSlot,
		notDue: map[string]memo{}, caps: map[string]int64{}, held: map[string]int64{}, done: make(chan struct{})}
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
func runInSlot(ctx context.Context, adm *cache.Compacting, cmd gitproc.Cmd) (gitproc.Result, error) {
	return adm.Run(ctx, cmd)
}

// Start runs a pass at once and then one every [Poll], on a goroutine of its
// own, until [Compactor.Stop]: AT ONCE, so a node that comes up beside a
// repository at the pack bound starts merging it, and names it in its alarm
// reading, at its first listing rather than a minute later. A pass that runs
// past its interval skips the tick rather than overlapping the next, since a
// ticker drops the ticks its reader is too busy for. A second Start does
// nothing.
func (c *Compactor) Start() {
	c.mu.Lock()
	if c.started || c.life.Err() != nil {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	go func() {
		defer close(c.done)
		tick := time.NewTicker(Poll)
		defer tick.Stop()
		for {
			if _, err := c.Poll(c.life); err != nil && c.life.Err() == nil {
				c.log.Warn("code_compaction_pass_failed", "error", err)
			}
			select {
			case <-c.life.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// Stop ends the loop and the compaction in flight — its build killed, its
// uploads abandoned, its proposal cancelled: an output already stored and
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
			"detail", "a compaction was still running when the stop's wait ended")
	}
}

// AlarmReading fills the reading's compaction half from the last pass: how
// many repositories this node is home for at the pack bound, the most live
// packs among them and that repository's name — what raises
// code_compaction_behind. A pass that has not run, or found none, leaves the
// three at zero, which the alarm table reads as nothing wrong here. Assigned
// here, field by field, so the reading's producers gate reaches each one
// through this concrete method (internal/engine's
// TestEveryAlarmReadingFieldHasAProducer).
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

// fault is the compaction's fault point p, hit at the offset at — nil in
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

// refused counts one refusal by its reason.
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

// halve halves repoID's cap from was, never below floor — the pack bytes of
// the two lightest inputs of the plan that failed — and answers whether the
// cap was already at the floor: a plan that fails there would fail again, and
// is remembered as not due until its repository moves.
func (c *Compactor) halve(repoID string, was, floor int64) (atFloor bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := max(was/2, floor)
	c.caps[repoID] = next
	return was <= floor
}
