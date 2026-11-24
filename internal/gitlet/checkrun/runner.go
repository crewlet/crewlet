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

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code log as a runner and a home's sweep reach it (the
// router's code seam): the reads of the checks (`checks_queued`, `check_box`,
// `checks_running`) and an attempt's claim and ending, each decided by the
// repository's checks writer on its committing data node.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error)
}

// Order is a repository's git order — estate.Router.GitOrder: every live Gitlet
// node in the repository's rendezvous order, its home first. An error is
// UNKNOWN: a node that cannot read it claims nothing and sweeps nothing.
type Order interface {
	GitOrder(repoID string) ([]string, error)
}

// Boxes is this node's sandbox catalogue (sandbox.Manager): a box with no
// harness, the cells the CURRENT catalogue configures (Placements, the only
// ones a new box is made in), of which a check needs the container, the name
// the engine host answers to in that cell's boxes, and for reclaiming a box an
// attempt left (package doc, "A box its attempt left") the boxes on this node
// recording an attempt and the backend that reaches each.
type Boxes interface {
	AcquireBox(ctx context.Context, spec sandbox.Spec, setup []sandbox.SetupStep) (sandbox.Sandbox, error)
	Placements() []sandbox.Placement
	EngineHost(placement sandbox.Placement) sandbox.EngineHost
	CheckBoxes() ([]sandbox.CheckBox, error)
	Provider(placement sandbox.Placement) (sandbox.Provider, error)
}

// Tokens mints a check's box's credential: Gitlet's CheckTokens, reached
// through the engine rather than by importing the host. The token names the
// attempt and the repository and is valid for timeout plus
// gitlet.CheckBoxGrace.
type Tokens interface {
	Mint(attempt gitlet.CheckKey, repo string, timeout time.Duration) (string, error)
}

// Uploader stores a failed run's kept log as one object (objstore.Store): at
// most limit bytes of r, answered with what the attempt's code_check_logs row
// names it by, or objstore.ErrFull where the store has no room, which the
// ending then goes without.
type Uploader interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
}

// Nudges is the code log's watch, which the engine adapts from the home's:
// nudge is called, one call at a time, for every record on a repository's
// checks subject and every moment the watch may have missed one; stop ends it.
type Nudges interface {
	Watch(ctx context.Context, filter string, nudge func()) (stop func(), err error)
}

// Company is the epoch a check runs under, read at every walk and attempt,
// never captured, since an apply replaces it: the company's configuration and
// the resolver of a ${VAR} reference in its check secrets.
type Company struct {
	Config  *config.Company
	Resolve func(value string) (string, []string)
}

// Timing is a runner's cadences. [Production] is the engine's; a suite
// shortens them.
type Timing struct {
	// Poll is how often a running check's row is read and its box asked
	// whether its command still runs, and how often a box an attempt may have
	// left is judged again: [CheckPoll].
	Poll time.Duration
	// Stagger is how long the node at rank r of a repository's git order
	// waits, r times, past an attempt's queuing before it claims it:
	// [ClaimStagger].
	Stagger time.Duration
	// Walk is the least time between two walks of the queue: [WalkGap].
	Walk time.Duration
	// Backstop is the longest time between two walks: [WalkBackstop].
	Backstop time.Duration
	// End is how long a run's ending may be read from its box: [EndPolls]
	// polls, which the home's sweep counts into its patience ([SweepPolls]).
	End time.Duration
}

// The production cadences.
const (
	// CheckPoll is sandbox.DefaultPollInterval, FIFTEEN SECONDS: the
	// executors' waiter's cadence, so a box is polled at one rate whatever
	// runs in it. An ending is at most that late, which a check of minutes
	// does not notice, for two calls a box (its row, its job).
	CheckPoll = sandbox.DefaultPollInterval

	// ClaimStagger is TWO SECONDS: the wait of each rank of a repository's
	// git order past the one before it. JUDGMENT, about ten times a healthy
	// claim's round (milliseconds), so the node ranked before has claimed
	// before the next one looks, and a home that cannot claim hands its
	// attempt on two seconds late, which a check of minutes does not notice.
	ClaimStagger = 2 * time.Second

	// WalkGap is ONE SECOND: the least time between two walks of the queue,
	// however many records nudge the runner meanwhile. Half a ClaimStagger,
	// so a claim lands within half a stagger of when its rank falls due,
	// while a burst of check records costs a node one read of the queue a
	// second rather than one a record.
	WalkGap = time.Second

	// WalkBackstop is CheckPoll: the longest a runner goes without walking
	// the queue, so a nudge the watch lost delays a queued check fifteen
	// seconds at most, and an idle Gitlet node reads the queue four times a
	// minute.
	WalkBackstop = CheckPoll

	// ReclaimPerWalk is how many of this node's check boxes no run of its
	// runner holds one walk judges, a linearizable read of its attempt's row
	// each and a kill each it reclaims: FOUR, JUDGMENT. A walk claims nothing
	// while it reclaims, and a kill (`docker rm -f` and the box's directory)
	// takes about a second, so four hold a walk a few seconds at most, which
	// costs a home's attempts the stagger to its runner-up; and a crash's
	// leftovers, at most the node's gitlet.max_checks, go within a few walks.
	ReclaimPerWalk = 4

	// WalkPage is how many queued attempts one read of the walk takes: 256,
	// JUDGMENT. A Gitlet node claims at most its free containers' worth a
	// walk (a few dozen), among the attempts ranked to it, about one in each
	// Gitlet node of its fleet; 256 is one read a walk for a fleet of a few
	// nodes and a queue of a hundred, a page of definitions under a hundred
	// KiB. It holds because a walk reads only while its node has a container
	// free and stops once it has none (Runner.walk).
	WalkPage = 256
)

// Production is the engine's timing.
func Production() Timing {
	return Timing{Poll: CheckPoll, Stagger: ClaimStagger, Walk: WalkGap, Backstop: WalkBackstop,
		End: EndPolls * CheckPoll}
}

// valid reports whether every cadence is one: a duration above zero.
func (t Timing) valid() bool {
	return t.Poll > 0 && t.Stagger > 0 && t.Walk > 0 && t.Backstop > 0 && t.End > 0
}

// Options build a [Runner]. Every field but Logs, Nudges, Faults, Metrics,
// Unread, Logger and Now is required, and New refuses a missing one by name.
type Options struct {
	// NodeID is this node: the runner every claim names, and the node a
	// repository's git order is searched for.
	NodeID string
	// Estate is the code log; Order the repositories' git orders.
	Estate Estate
	Order  Order
	// Boxes is this node's CURRENT sandbox catalogue — an apply swaps it —
	// and false where it configures none, which runs no check here.
	Boxes func() (Boxes, bool)
	// Company is the current epoch, false before a company.
	Company func() (Company, bool)
	// Tokens mints each box's credential.
	Tokens Tokens
	// GitBase is Gitlet's base in this node's host's network — its
	// CREWLET_BOX_GIT_URL, as sandbox.ParseBoxURL read it: a public listener
	// and /box/git — which a check's box dials under the engine host's name
	// in its container where it names loopback (Boxes.EngineHost).
	GitBase string
	// CodeBase is where a check's box reads its commit without a clone, in
	// the same network and handed the same way: the same listener and the
	// API's route for a check's reads (internal/api/auth's BoxGitletPrefix);
	// CREWLET_CHECK_API is it plus the repository's name. GIVEN, NEVER
	// SPELLED HERE: the engine builds it from the route itself, so a rename of
	// the route cannot leave every check's read answering 404.
	CodeBase string
	// Slots is how many checks this node runs at once, a container each:
	// gitlet.max_checks as config.Gitlet.MaxChecksFor resolved it, 0 none.
	Slots int
	// Logs stores a failed run's log; nil keeps none, and a failed run's
	// ending carries its tail and its failures alone.
	Logs Uploader
	// Timing is the runner's cadences; the zero value is [Production].
	Timing Timing
	// Nudges is the code log's watch, nil for none — the backstop walks.
	Nudges Nudges
	// Faults is the suites' fault script; nil in production.
	Faults gitletfault.Injector
	// Metrics nil records nothing; Logger nil is the package's own; Now nil
	// is the wall clock in UTC.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
	// Unread reports an error the estate answers where nothing ran a request and a data node
	// refused a key it carries; an ending so refused is sent again without its counts. Nil sends
	// no ending again without its counts.
	Unread func(error) bool
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("checkrun: refused options")

// boxGitSuffix is what a box's Gitlet base ends in: what New holds
// CREWLET_BOX_GIT_URL to, since a value naming the listener alone, or another
// route of it, would fail every box's clone.
const boxGitSuffix = "/box/git"

// Runner is one Gitlet node's check runner: its walk of the queue and the
// checks it runs, each on a goroutine of its own, until Stop.
type Runner struct {
	o      Options
	timing Timing
	log    *slog.Logger
	now    func() time.Time

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
	// running is the attempts this runner runs now, each from its reserved
	// container to its box closed: at most Slots.
	running map[gitlet.CheckKey]bool
	// unconfigured is a catalogue with no container backend said once, until
	// one has it again.
	unconfigured bool

	// judged is when each check box the loop's reclaim last judged and kept
	// was judged, by id; unlisted a listing failure said once, until the boxes
	// list again. The loop's own, so unlocked.
	judged   map[string]time.Time
	unlisted bool
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
		return nil, fmt.Errorf("%w: Slots %d is no count of containers", ErrOptions, o.Slots)
	}
	if base, ok := strings.CutSuffix(o.GitBase, boxGitSuffix); !ok || base == "" {
		return nil, fmt.Errorf("%w: GitBase: %s must name Gitlet's public listener ending in %s "+
			"(docs/guides/deployment.md), which a check's box clones from", ErrOptions, sandbox.BoxGitURLVar,
			boxGitSuffix)
	}
	if o.CodeBase == "" {
		return nil, fmt.Errorf("%w: CodeBase: a check's box reads its commit without a clone there "+
			"(CREWLET_CHECK_API), and no base names it", ErrOptions)
	}
	r := &Runner{o: o, timing: o.Timing, log: o.Logger, now: o.Now, wake: make(chan struct{}, 1),
		running: map[gitlet.CheckKey]bool{}, judged: map[string]time.Time{}}
	if r.timing == (Timing{}) {
		r.timing = Production()
	}
	if !r.timing.valid() {
		return nil, fmt.Errorf("%w: Timing %+v: every cadence is above zero, or the whole of it unset for "+
			"Production", ErrOptions, o.Timing)
	}
	if r.log == nil {
		r.log = logging.Get("gitlet.checkrun")
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
var checksSubjects = topics.CodeLogPrefix + "." + string(gitlet.KindChecks) + ".>"

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
// asks — never two within the walk's gap. Each begins by reclaiming the boxes
// attempts left on this node ([Runner.reclaim]), the first a predecessor's.
func (r *Runner) loop() {
	ctx := r.life
	stopWatch := r.watch(ctx)
	defer stopWatch()
	for {
		walked := time.Now()
		r.reclaim(ctx)
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

// walk reads the queue and claims, and starts, every attempt this node may run
// now: one it ranks for that has waited its rank's staggers, with a container
// free. It answers the earliest instant an attempt it read falls due at this
// node's rank, zero for none.
//
// IT READS ONLY WHAT IT CAN ACT ON (package doc, "Every Gitlet node runs
// checks, ranked per repository"): under a company that runs checks, only
// while this node makes containers and has one free (Runner.room); under one
// that runs none (config.Company.ChecksRefusal), all of it, each attempt ended
// unrun by the node whose turn it is — whatever this node's catalogue, since
// such a company may configure none, and an attempt no node ends waits queued
// for code_checks_backlog.
func (r *Runner) walk(ctx context.Context) time.Time {
	company, ok := r.o.Company()
	if !ok || company.Config == nil || company.Config.CodeBackendFor() != config.CodeGitlet {
		return time.Time{}
	}
	w := &walking{ranks: map[string]int{}, refusal: company.Config.ChecksRefusal()}
	if w.refusal == "" && !r.configured() {
		return time.Time{}
	}
	var (
		next   time.Time
		cursor string
	)
	for {
		if w.refusal == "" && !r.room() {
			return next
		}
		a, err := r.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChecksQueued, Cursor: cursor,
			Limit: WalkPage, Fresh: stale})
		if err != nil {
			if ctx.Err() == nil {
				r.log.WarnContext(ctx, "code_checks_walk_failed", "error", err.Error(), "detail",
					"the queue is read again at the next walk")
			}
			return next
		}
		for _, c := range a.Runnable {
			// A NODE WITH NO CONTAINER LEFT reads and considers no further.
			if w.refusal == "" && !r.room() {
				return next
			}
			if due := r.consider(ctx, w, c); !due.IsZero() && (next.IsZero() || due.Before(next)) {
				next = due
			}
			if ctx.Err() != nil {
				return next
			}
		}
		if a.Next == "" {
			return next
		}
		cursor = a.Next
	}
}

// walking is what one walk has learned that the rest of it reads: each
// repository's rank, and why its company runs no check ("" where it runs them).
type walking struct {
	// ranks is this node's place in each repository's git order, read once
	// a walk, so every attempt of a repository is ranked against one reading.
	ranks map[string]int
	// refusal is config.Company.ChecksRefusal under the walk's epoch: the
	// summary each attempt is ended unrun with, "" where checks run.
	refusal string
}

// rank is this node's place in repoID's git order, -1 where it holds none.
func (r *Runner) rank(w *walking, repoID string) int {
	if at, ok := w.ranks[repoID]; ok {
		return at
	}
	order, err := r.o.Order.GitOrder(repoID)
	at := slices.Index(order, r.o.NodeID)
	if err != nil {
		at = -1
	}
	w.ranks[repoID] = at
	return at
}

// consider is one queued attempt of the walk: claimed and started where this
// node may run it now, answered the instant it falls due at this node's rank
// where that is still to come, and zero otherwise.
func (r *Runner) consider(ctx context.Context, w *walking, c gitlet.RunnableCheck) time.Time {
	if c.Def == nil {
		// NO DEFINITION, NOTHING TO RUN: only an attempt errored at its
		// dispatch carries none, and it is queued already ended
		// (gitlet.CheckQueued); a person's CI queues nothing
		// (gitlet.QueueReasons). Skipped rather than dereferenced.
		return time.Time{}
	}
	at := r.rank(w, c.RepoID)
	if at < 0 {
		return time.Time{}
	}
	if due := c.QueuedAt.Add(time.Duration(at) * r.timing.Stagger); r.now().Before(due) {
		return due
	}
	if w.refusal != "" {
		// A COMPANY THAT RUNS NO CHECK: no node runs it, and the node whose
		// turn it is ends it saying why.
		r.endUnrun(ctx, c, w.refusal)
		return time.Time{}
	}
	if !r.reserve(c.CheckKey) {
		return time.Time{}
	}
	started := r.now()
	if !r.claim(ctx, c, started) {
		r.release(c.CheckKey)
		return time.Time{}
	}
	r.wg.Add(1)
	go func() { //nolint:contextcheck // The run is the runner's, not this walk's: it runs under r.life, so a walk that ends never cuts the box it started, and records its ending under Stop's context.
		defer r.wg.Done()
		defer r.release(c.CheckKey)
		r.run(c, started)
	}()
	return time.Time{}
}

// configured reports whether this node's CURRENT catalogue makes containers
// (sandbox.Manager.Placements: never a dropped cell, whose backend the manager
// keeps only for the boxes already on it), and says once, until it does again,
// where it makes none.
func (r *Runner) configured() bool {
	var cells []sandbox.Placement
	if boxes, ok := r.o.Boxes(); ok && boxes != nil {
		cells = boxes.Placements()
	}
	has := slices.Contains(cells, sandbox.Container)
	r.mu.Lock()
	defer r.mu.Unlock()
	if has {
		r.unconfigured = false
		return true
	}
	// A COMPANY THAT RUNS CHECKS, A CATALOGUE WITH NO CONTAINER BACKEND: the
	// company's own validation builds one, so this is a node behind its
	// company's apply or one whose backend could not be built — said once,
	// and the checks left queued for a node that has one.
	if !r.unconfigured {
		r.unconfigured = true
		r.log.Warn("code_checks_container_unconfigured", "configured", cells, "detail",
			"this node's sandbox catalogue has no container backend, so it claims no check: a check runs in "+
				"a container")
	}
	return false
}

// room reports whether this node can start one more check: a container free
// under gitlet.max_checks.
func (r *Runner) room() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running) < r.o.Slots
}

// reserve takes a container under gitlet.max_checks for attempt, false where
// none is free or this runner runs attempt already: a queue read behind its
// claim lists it again, and claiming it again would be refused.
func (r *Runner) reserve(attempt gitlet.CheckKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.running) >= r.o.Slots || r.running[attempt] {
		return false
	}
	r.running[attempt] = true
	r.gauge()
	return true
}

// release gives back what reserve took for attempt.
func (r *Runner) release(attempt gitlet.CheckKey) {
	r.mu.Lock()
	delete(r.running, attempt)
	r.gauge()
	r.mu.Unlock()
	// A CONTAINER FREED is a walk's worth of room: a check left queued for
	// want of it is claimable now.
	r.Nudge()
}

// gauge records the checks held. Called under mu.
func (r *Runner) gauge() {
	if r.o.Metrics != nil {
		r.o.Metrics.Set(metrics.CodeChecksRunning, float64(len(r.running)), nil)
	}
}

// Held is how many checks this runner runs now, a container each, which
// gitlet.max_checks bounds.
func (r *Runner) Held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

// runs reports whether this runner runs attempt now.
func (r *Runner) runs(attempt gitlet.CheckKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[attempt]
}

// claim asks for c claimed by this node at started, and answers whether this
// node holds it: applied with this node the holder the join kept, or,
// appended and not yet known applied, read back at the claim's position. A
// claim refused, lost or unreadable starts no box (package doc, "The claim
// before the box"); an attempt held with nothing run is ended by the home's
// sweep once its claim outlives its check's timeout.
func (r *Runner) claim(ctx context.Context, c gitlet.RunnableCheck, started time.Time) bool {
	res, err := r.o.Estate.Checks(ctx, gitlet.ChecksRequest{OpID: statelog.NewOpID(started, "code_check_claim"),
		Started: &gitlet.CheckStarted{CheckKey: c.CheckKey, Runner: r.o.NodeID,
			Placement: gitlet.PlacementContainer, StartedAt: started}})
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
	back, err := r.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCheckBox, CheckBox: &key,
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
	return row.State == gitlet.CheckStateRunning && row.Runner == r.o.NodeID &&
		row.StartedAt.UnixMicro() == started.UnixMicro()
}

// endUnrun ends c `errored defs_invalid` with why, unclaimed: a check of a
// company that runs none (config.Company.ChecksRefusal), which no node runs.
func (r *Runner) endUnrun(ctx context.Context, c gitlet.RunnableCheck, why string) {
	at := r.now()
	f := gitlet.CheckFinished{CheckKey: c.CheckKey, State: gitlet.CheckStateErrored, Reason: gitlet.CheckReasonDefsInvalid,
		Summary: summaryOf(why), FinishedAt: at}
	r.finish(ctx, f, statelog.NewOpID(at, "code_check_unrun"))
}
