package checkrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code log as a runner and a home's sweep reach it — the
// router's code seam satisfies it: the engine's own reads of the checks
// (`checks_queued`, `check_box`, `checks_running`) and the writes of an
// attempt's claim and ending, each decided by the repository's checks writer
// on its committing data node.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	Checks(ctx context.Context, r code.ChecksRequest) (code.ChecksResult, error)
}

// Order is a repository's git order — estate.Router.GitOrder: every live git
// node in the repository's rendezvous order, its home first. An error is
// UNKNOWN: a node that cannot read it claims nothing and sweeps nothing.
type Order interface {
	GitOrder(repoID string) ([]string, error)
}

// Boxes is this node's sandbox catalogue as a check's box is made from it —
// internal/sandbox's Manager satisfies it: a box with no coding agent, and the
// backend a remote box is killed through.
type Boxes interface {
	AcquireBox(ctx context.Context, spec sandbox.Spec, setup []sandbox.SetupStep) (sandbox.Sandbox, error)
	Provider(placement sandbox.Placement) (sandbox.Provider, error)
}

// Tokens mints a check's box's credential: the code host's CheckTokens, which
// this package reaches through the engine rather than importing the host. The
// token names the attempt and the repository's name the box is given, and is
// valid for timeout and code.CheckBoxGrace more; "" is none minted.
type Tokens interface {
	Mint(attempt code.CheckKey, repo string, timeout time.Duration) string
}

// Uploader stores a failed run's kept log as one object — the fleet's object
// store (objstore.Store) satisfies it: at most limit bytes of r under a key
// minted for it, answered with what the attempt's code_check_logs row names it
// by, and objstore.ErrFull where the store has no room, which the ending then
// goes without.
type Uploader interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
}

// Nudges is the code log's watch as a runner hears of new check records — the
// engine adapts the home's: nudge is called, one call at a time, for every
// record on a repository's checks subject and every moment the watch may have
// missed one, and stop ends it.
type Nudges interface {
	Watch(ctx context.Context, filter string, nudge func()) (stop func(), err error)
}

// Company is the epoch a check runs under, read at every walk and every
// attempt — never captured, since an apply replaces it: the company's
// configuration (code.checks, the cells it allows) and the resolution of a
// ${VAR} reference in its check secrets.
type Company struct {
	Config  *config.Company
	Resolve func(value string) (string, []string)
}

// Timing is a runner's cadences. [Production] is the engine's; a suite
// shortens them.
type Timing struct {
	// Poll is how often a running check's box is asked whether its command
	// still runs, its TTL refreshed and its row read: [CheckPoll].
	Poll time.Duration
	// Stagger is how long the node at rank r of a repository's git order
	// waits, r times, past an attempt's queuing before it claims it:
	// [ClaimStagger].
	Stagger time.Duration
	// Walk is the least time between two walks of the queue: [WalkGap].
	Walk time.Duration
	// Backstop is the longest time between two walks: [WalkBackstop].
	Backstop time.Duration
}

// The production cadences.
const (
	// CheckPoll is sandbox.DefaultPollInterval, FIFTEEN SECONDS: the
	// coding runs' waiter's cadence, so a box is asked about at one rate
	// whatever runs in it — what a check's ending is late by at most, which
	// a check of minutes does not notice, and one call to the box, one to
	// its TTL and one read of its row a box every fifteen seconds.
	CheckPoll = sandbox.DefaultPollInterval

	// ClaimStagger is TWO SECONDS: the wait of each rank of a repository's
	// git order past the one before it. JUDGMENT, about ten times a healthy
	// claim's round — a record decided and applied on the repository's
	// committing data node, milliseconds — so the node ranked before has
	// claimed an attempt before the next one looks, and a home that cannot
	// (its container cells full, or none) hands its attempt to the runner-up
	// two seconds late, which a check of minutes does not notice.
	ClaimStagger = 2 * time.Second

	// WalkGap is ONE SECOND: the least time between two walks of the queue,
	// however many records nudge the runner meanwhile. Half a ClaimStagger,
	// so a node's claim lands within half a stagger of when its rank falls
	// due, while a burst of a thousand check records — a push fanning out a
	// file's checks, the claims and endings they bring — costs a node one
	// read of the queue a second rather than one a record.
	WalkGap = time.Second

	// WalkBackstop is CheckPoll: the longest a runner goes without walking
	// the queue, so a nudge the watch lost — a gap, its consumer gone —
	// delays a queued check by fifteen seconds at most, and an idle fleet
	// reads the queue four times a minute a git node.
	WalkBackstop = CheckPoll

	// WalkPage is how many queued attempts one read of the walk takes: 256,
	// JUDGMENT — a git node claims at most its free cells' worth of attempts
	// a walk, a few dozen, and finds them among the attempts ranked to it,
	// about one in each of its fleet's git nodes; 256 is a walk of one read
	// on a fleet of up to a few nodes with a queue of a hundred, and a page
	// of definitions (a few hundred bytes each) of under a hundred KiB.
	WalkPage = 256
)

// Production is the engine's timing.
func Production() Timing {
	return Timing{Poll: CheckPoll, Stagger: ClaimStagger, Walk: WalkGap, Backstop: WalkBackstop}
}

// Options build a [Runner]. Every field but Logs, Nudges, Faults, Metrics,
// Logger and Now is required, and New refuses a missing one by name.
type Options struct {
	// NodeID is this node: the runner every claim names, and the node a
	// repository's git order is searched for.
	NodeID string
	// Estate is the code log; Order the repositories' git orders.
	Estate Estate
	Order  Order
	// Boxes is this node's CURRENT sandbox catalogue — an apply swaps it —
	// and false where it configures none, which runs no check.
	Boxes func() (Boxes, bool)
	// Company is the current epoch, false before a company.
	Company func() (Company, bool)
	// Tokens mints each box's credential.
	Tokens Tokens
	// GitBase is the code host's base as a box dials it — this node's
	// CREWLET_BOX_GIT_URL, as sandbox.ParseBoxURL read it: a public
	// listener and /box/git, its sibling /box/code where a check reads its
	// commit without a clone.
	GitBase string
	// Slots is how many checks this node runs at once in container cells:
	// git.max_checks as config.Git.MaxChecksFor resolved it, 0 none.
	Slots int
	// Logs stores a failed run's log; nil keeps none, and a failed run's
	// ending carries its tail and its failures alone.
	Logs Uploader
	// Timing is the runner's cadences; the zero value is [Production].
	Timing Timing
	// Nudges is the code log's watch, nil for none — the backstop walks.
	Nudges Nudges
	// Faults is the suites' fault script; nil in production.
	Faults codefault.Injector
	// Metrics nil records nothing; Logger nil is the package's own; Now nil
	// is the wall clock in UTC.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("checkrun: refused options")

// boxGitSuffix is what a box's code host base ends in, and boxCodeSuffix its
// sibling a check's box reads its commit at (internal/api/auth's
// BoxCodePrefix).
const (
	boxGitSuffix  = "/box/git"
	boxCodeSuffix = "/box/code"
)

// Runner is one git node's check runner: its walk of the queue and the
// checks it runs, each on a goroutine of its own, until Stop.
type Runner struct {
	o       Options
	timing  Timing
	apiBase string
	log     *slog.Logger
	now     func() time.Time

	// life is every walk's and every run's context, ended by Stop; ending is
	// the context a stopping run records its ending under, set by Stop
	// before life ends, bounded by Stop's own.
	life   context.Context
	cancel context.CancelFunc

	wake chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	started bool
	stopped bool
	ending  context.Context
	// held is the checks this runner runs now, by cell: at most Slots in a
	// container cell.
	held map[config.Placement]int
	// unknown is the cells this node's catalogue lacks that a queued check
	// asked for, each said once until the catalogue holds it.
	unknown map[config.Placement]bool
}

// New builds a runner, refusing what it cannot work without. It starts
// nothing: [Runner.Start] does.
func New(o Options) (*Runner, error) {
	var missing []string
	for _, f := range []struct {
		name   string
		absent bool
	}{
		{"NodeID", !config.ValidNodeID(o.NodeID)}, {"Estate", o.Estate == nil}, {"Order", o.Order == nil},
		{"Boxes", o.Boxes == nil}, {"Company", o.Company == nil}, {"Tokens", o.Tokens == nil},
	} {
		if f.absent {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrOptions, strings.Join(missing, ", "))
	}
	if o.Slots < 0 {
		return nil, fmt.Errorf("%w: Slots %d is no count of container cells", ErrOptions, o.Slots)
	}
	base, ok := strings.CutSuffix(o.GitBase, boxGitSuffix)
	if !ok || base == "" {
		return nil, fmt.Errorf("%w: GitBase: %s must name the code host's public listener ending in %s "+
			"(docs/guides/deployment.md), which a check's box clones from — and its sibling %s, where a check "+
			"reads its commit without a clone", ErrOptions, sandbox.BoxGitURLVar, boxGitSuffix, boxCodeSuffix)
	}
	r := &Runner{o: o, timing: o.Timing, apiBase: base + boxCodeSuffix, log: o.Logger, now: o.Now,
		wake: make(chan struct{}, 1), held: map[config.Placement]int{},
		unknown: map[config.Placement]bool{}}
	if r.timing == (Timing{}) {
		r.timing = Production()
	}
	if r.log == nil {
		r.log = logging.Get("code.checkrun")
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	r.life, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// checksSubjects is the filter a runner watches the code log under: every
// record on a repository's checks subject — an attempt queued, claimed or
// ended, each of which may make a check claimable here.
var checksSubjects = topics.CodeLogPrefix + "." + string(code.KindChecks) + ".>"

// Start begins the runner's watch and its walks, on goroutines of its own,
// until [Runner.Stop]. A second Start does nothing, and so does one once Stop
// has begun. Its caller starts it once the runtime its seams read through is
// published and the node writes: its first walk reads the queue at once.
func (r *Runner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.loop()
	}()
}

// Stop ends the runner: no walk after it, and every check it runs ends —
// each attempt recorded `errored runner_lost` under ctx, so its home queues
// it again at once, and its box closed — and is waited for, until ctx ends.
func (r *Runner) Stop(ctx context.Context) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped, r.ending = true, ctx
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		r.log.WarnContext(ctx, "code_checks_stop_cut", "detail",
			"the runner's stop ended before every check it ran had recorded its ending and closed its box; "+
				"the home's sweep ends what it left running")
	}
}

// Nudge asks for a walk soon — at once, unless one ran within the walk's
// gap. Safe from any goroutine.
func (r *Runner) Nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// loop is the runner's walks: one at once, then one whenever a nudge, an
// attempt falling due at this node's rank, a check ending or the backstop
// asks — never two within the walk's gap.
func (r *Runner) loop() {
	ctx := r.life
	stopWatch := r.watch(ctx)
	defer stopWatch()
	for {
		walked := time.Now()
		next := r.walk(ctx)
		wait := r.timing.Backstop
		if !next.IsZero() {
			wait = min(wait, next.Sub(r.now()))
		}
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-r.wake:
			timer.Stop()
		}
		// NEVER TWO WALKS WITHIN THE GAP, however the wait ended.
		if rest := r.timing.Walk - time.Since(walked); rest > 0 {
			gap := time.NewTimer(rest)
			select {
			case <-ctx.Done():
				gap.Stop()
				return
			case <-gap.C:
			}
		}
	}
}

// watch begins the nudges where the runner has a watch, answering what ends
// it; a watch that could not begin leaves the backstop to walk.
func (r *Runner) watch(ctx context.Context) func() {
	if r.o.Nudges == nil {
		return func() {}
	}
	stop, err := r.o.Nudges.Watch(ctx, checksSubjects, r.Nudge)
	if err != nil {
		r.log.WarnContext(ctx, "code_checks_watch_failed", "error", err.Error(), "detail",
			"the runner walks the queue at its backstop alone")
		return func() {}
	}
	return stop
}

// stale is the level every walk and every poll reads at: the queue and a row
// as this node's nearest copy holds them — each claim and ending that follows
// is decided again by the repository's checks writer.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// walk reads the queue oldest first and claims, and starts, every attempt this
// node may run now: one it ranks for that has waited its rank's staggers, in a
// cell this node has room in. It answers the earliest instant an attempt it
// read falls due at this node's rank, zero for none.
func (r *Runner) walk(ctx context.Context) time.Time {
	company, ok := r.o.Company()
	if !ok || company.Config == nil || !company.Config.Code.Native {
		return time.Time{}
	}
	boxes, ok := r.o.Boxes()
	if !ok || boxes == nil {
		return time.Time{}
	}
	var (
		next   time.Time
		cursor string
		ranks  = map[string]int{}
	)
	for !r.full(boxes) {
		a, err := r.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChecksQueued, Cursor: cursor, Limit: WalkPage,
			Fresh: stale})
		if err != nil {
			if ctx.Err() == nil {
				r.log.WarnContext(ctx, "code_checks_walk_failed", "error", err.Error(), "detail",
					"the queue is read again at the next walk")
			}
			return next
		}
		for _, c := range a.Runnable {
			if due := r.consider(ctx, company, boxes, ranks, c); !due.IsZero() && (next.IsZero() || due.Before(next)) {
				next = due
			}
			if ctx.Err() != nil {
				return next
			}
		}
		if a.Next == "" {
			break
		}
		cursor = a.Next
	}
	return next
}

// rank is this node's place in repoID's git order, -1 where it holds none —
// read once a walk a repository, so a walk ranks every attempt of one against
// one reading.
func (r *Runner) rank(ranks map[string]int, repoID string) int {
	if at, ok := ranks[repoID]; ok {
		return at
	}
	order, err := r.o.Order.GitOrder(repoID)
	at := slices.Index(order, r.o.NodeID)
	if err != nil {
		at = -1
	}
	ranks[repoID] = at
	return at
}

// consider is one queued attempt of the walk: claimed and started where this
// node may run it now, answered the instant it falls due at this node's rank
// where that is still to come, and zero otherwise.
func (r *Runner) consider(ctx context.Context, company Company, boxes Boxes, ranks map[string]int,
	c code.RunnableCheck) time.Time {

	if c.Def == nil {
		// A PERSON'S CI REPORTS IT: no runner of the engine's runs it.
		return time.Time{}
	}
	at := r.rank(ranks, c.RepoID)
	if at < 0 {
		return time.Time{}
	}
	if due := c.QueuedAt.Add(time.Duration(at) * r.timing.Stagger); r.now().Before(due) {
		return due
	}
	cell, why := company.Config.CheckCell(config.Placement(c.Def.RunIn))
	if why != "" {
		// A CELL THE COMPANY DOES NOT ALLOW, OR NONE: no node runs it, and
		// the node whose turn it is ends it saying why.
		r.endUnrun(ctx, c, why)
		return time.Time{}
	}
	if !r.reserve(boxes, cell) {
		return time.Time{}
	}
	started := r.now()
	if !r.claim(ctx, c, cell, started) {
		r.release(cell)
		return time.Time{}
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.release(cell)
		r.run(c, cell, started)
	}()
	return time.Time{}
}

// full reports a node with no room for any check: no container cell free —
// or none configured — and no remote cell configured.
func (r *Runner) full(boxes Boxes) bool {
	if _, err := boxes.Provider(sandbox.E2B); err == nil {
		return false
	}
	if _, err := boxes.Provider(sandbox.Container); err != nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[config.PlacementContainer] >= r.o.Slots
}

// reserve takes room for one check in cell — a container slot under
// git.max_checks, or nothing for a remote cell — where the catalogue
// configures the cell, false where there is none.
func (r *Runner) reserve(boxes Boxes, cell config.Placement) bool {
	_, err := boxes.Provider(sandbox.Placement(cell))
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// THE CATALOGUE LACKS A CELL A CHECK ASKS FOR: the company's own
		// validation builds every cell a check may run in, so this is a
		// node behind its company's apply — said once, and the check left
		// queued for a node that has it.
		if !r.unknown[cell] {
			r.unknown[cell] = true
			r.log.Warn("code_checks_cell_unconfigured", "cell", string(cell), "error", err.Error(), "detail",
				"this node's sandbox catalogue has no backend for the cell, so it claims no check that runs "+
					"there")
		}
		return false
	}
	delete(r.unknown, cell)
	if cell == config.PlacementContainer && r.held[cell] >= r.o.Slots {
		return false
	}
	r.held[cell]++
	r.gauge(cell)
	return true
}

// release gives back what reserve took.
func (r *Runner) release(cell config.Placement) {
	r.mu.Lock()
	r.held[cell]--
	r.gauge(cell)
	r.mu.Unlock()
	// A CELL FREED is a walk's worth of room: a check left queued for want
	// of it is claimable now.
	r.Nudge()
}

// gauge records the checks held in cell. Called under mu.
func (r *Runner) gauge(cell config.Placement) {
	if r.o.Metrics != nil {
		r.o.Metrics.Set(metrics.CodeChecksRunning, float64(r.held[cell]), metrics.Attrs{"placement": string(cell)})
	}
}

// Held is how many checks this runner runs now in cell — a slot each, whose
// container cells git.max_checks bounds.
func (r *Runner) Held(cell config.Placement) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[cell]
}

// claim asks for c claimed by this node at started, and answers whether this
// node holds it: applied with this node the holder the join kept — or,
// appended and not yet known to have applied where the writer answered, read
// back at the claim's position. A claim refused, lost to another runner, or
// one whose holder could not be read back starts no box: an attempt this node
// holds and runs nothing for is ended by the home's sweep once its claim has
// outlived its check's timeout.
func (r *Runner) claim(ctx context.Context, c code.RunnableCheck, cell config.Placement, started time.Time) bool {
	res, err := r.o.Estate.Checks(ctx, code.ChecksRequest{OpID: statelog.NewOpID(started, "code_check_claim"),
		Started: &code.CheckStarted{CheckKey: c.CheckKey, Runner: r.o.NodeID,
			Placement: code.CheckPlacement(cell), StartedAt: started}})
	switch {
	case err != nil:
		if ctx.Err() == nil {
			r.log.WarnContext(ctx, "code_check_claim_failed", "repo_id", c.RepoID, "oid", c.OID,
				"check", c.Name, "attempt", c.Attempt, "error", err.Error())
		}
		return false
	case res.Refusal != "":
		return false
	case res.Outcome == statelog.OutcomeApplied && res.Row != nil:
		return res.Holder == r.o.NodeID
	}
	key := c.CheckKey
	back, err := r.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &key,
		Fresh: statelog.FloorAt(statelog.ReadSession, res.Position)})
	if err != nil || len(back.Checks) != 1 {
		if ctx.Err() == nil {
			r.log.WarnContext(ctx, "code_check_claim_unknown", "repo_id", c.RepoID, "oid", c.OID,
				"check", c.Name, "attempt", c.Attempt, "outcome", string(res.Outcome), "detail",
				"the claim was appended and its holder could not be read back, so this node makes no box; "+
					"the repository's home ends the attempt once it outlives its timeout")
		}
		return false
	}
	row := back.Checks[0]
	return row.State == code.CheckStateRunning && row.Runner == r.o.NodeID &&
		row.StartedAt.UnixMicro() == started.UnixMicro()
}

// endUnrun ends c `errored defs_invalid` with why, unclaimed: a check whose
// cell the company does not allow, which no node runs.
func (r *Runner) endUnrun(ctx context.Context, c code.RunnableCheck, why string) {
	at := r.now()
	f := code.CheckFinished{CheckKey: c.CheckKey, State: code.CheckStateErrored, Reason: code.CheckReasonDefsInvalid,
		Summary: summaryOf(why), FinishedAt: at}
	r.finish(ctx, f, statelog.NewOpID(at, "code_check_unrun"))
}
