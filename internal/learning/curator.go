package learning

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// The BACKGROUND passes, which are the half of learning no turn drives.
//
// Everything else here hangs off a completed turn. These do not: a skill goes
// stale because nothing used it, episodes are compacted because they piled
// up, and a repeated procedure becomes visible only across turns. All three
// are therefore loops, all three are fleet SINGLETONS — two nodes compacting
// one seat's episodes would summarise the same cluster twice and pay for it
// twice — and all three are deliberately slow: their unit of change is a day,
// not a tick.

// CuratorInterval is how often the skill state machine walks the catalogue.
//
// A DAY, because the transitions it makes are measured in tens of days:
// stale after 30, archived after 90. A pass an hour would scan the whole
// catalogue 24 times to make the same zero transitions, and the one it
// eventually makes would land at most an hour earlier — against a threshold
// nobody set to the hour.
const CuratorInterval = 24 * time.Hour

// LifecycleInterval is how often each seat's episode count is checked.
//
// FAR SHORTER than the curator's, because what it watches is a COUNT rather
// than a clock: a busy seat crosses its raw-episode threshold in a burst,
// and every turn past that point pays the recall scan over rows that should
// already have been folded. An hour bounds that overshoot to one hour of one
// seat's traffic while costing a single indexed count per seat when nothing
// is due.
const LifecycleInterval = time.Hour

// Seats lists the seats a background pass walks.
//
// A FUNCTION rather than a slice: an apply changes the roster, and a pass
// holding the list it started with would keep compacting a seat the company
// removed and never touch one it added.
//
// ROLES rather than handles, because every pass needs TWO names for a seat
// and both have to come from one reading of the roster: the rows are keyed on
// the handle the seat was CREATED under ([org.Role.Origin], see the package
// doc) and every event and log line names it by the address it answers to now
// ([org.Role.Handle]). A handle list gave the passes only the second, so a
// renamed seat was compacted, clustered and announced under an address its
// memory is not filed under — and a resolver consulted per pass could answer
// about a seat an apply renamed between the listing and the lookup.
type Seats func() []*org.Role

// Background runs the passes no turn drives.
//
// ONE PER PROCESS, and what it runs follows the applied revision. The loops
// are armed once, at [Background.Start], and every config apply hands them the
// passes that revision configures through [Background.Reconfigure]. Built
// instead from whichever company the process started with, a node that booted
// with no company never ran a pass at all, one that booted without a model
// never compacted after a provider was added, and every node kept compacting
// on its boot-time models, credentials and knobs through every edit and
// rotation until it restarted. Rebuilding the loops per apply would be worse
// in the other direction: each apply would restart a daily clock, and a
// company edited more often than a day would never curate at all.
type Background struct {
	agentIDFor func(seat *org.Role) string
	seats      Seats
	publish    Announce
	claimDuty  func(ctx context.Context) (bool, error)
	now        func() time.Time

	// mu guards the passes and the wake channel.
	mu sync.Mutex

	// passes is what the loops run this tick, with the defaults applied.
	passes BackgroundPasses

	// wake is closed by every Reconfigure and replaced, which is how a
	// loop hears that its cadence may have moved without polling for it.
	wake chan struct{}

	// cancel and running are the loops' OWNED lifetime, and they are not
	// the context Start was handed. The caller detaches that context on
	// purpose — these loops outlive SIGTERM like the node's do — so the
	// context carries no stop at all and Stop is the only one there is.
	// Without it the passes keep ticking after the engine has closed the
	// store they query and the model they pay, which is the failure this
	// pair exists to make impossible.
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// BackgroundPasses is what the loops run, as one revision configures them.
//
// A value swapped WHOLE by [Background.Reconfigure], so a tick reads one
// revision's passes and never one pass from each side of an apply.
type BackgroundPasses struct {
	// Lifecycle compacts episodes; nil disables that pass.
	Lifecycle *Lifecycle

	// Skills ages the catalogue; nil disables that pass.
	Skills *Skills

	// Cluster drafts skills from the shapes a seat repeats; nil disables
	// that pass, which is what `skill_synthesis.scheduler_enabled: false`
	// — the default — resolves to.
	Cluster *Synthesizer

	// Promoter distils what several seats in a unit independently learned
	// into a knowledge-base draft; nil disables that pass, which is what a
	// company with no knowledge base resolves to.
	Promoter *Promoter

	// Policy is the disuse schedule. Zero values take the defaults.
	Policy CuratorPolicy

	// CuratorInterval, LifecycleInterval, ClusterInterval and
	// PromotionInterval override the cadences above. Zero takes each one's
	// default.
	CuratorInterval   time.Duration
	LifecycleInterval time.Duration
	ClusterInterval   time.Duration
	PromotionInterval time.Duration
}

// BackgroundOptions configures the loops.
type BackgroundOptions struct {
	// Passes is what the loops run until the first [Background.Reconfigure].
	Passes BackgroundPasses

	// AgentIDFor derives the seat's agent id, which the clustering pass
	// stamps on the skill it publishes.
	//
	// It takes the ROLE the roster already carries rather than a handle,
	// so the id and the role name on one event cannot name two seats: a
	// second lookup would read the epoch again and could answer about a
	// seat an apply renamed in between.
	//
	// Optional. Nil answers empty, which is the shape a caller with no org
	// takes — the event still carries the handle and the role, and its
	// promoted agent_id column is empty rather than wrong.
	AgentIDFor func(seat *org.Role) string

	// Seats lists the seats to walk — see [Seats]. Nil means no seats,
	// which yields loops that tick and do nothing — the correct shape for a
	// node with no active company. The clustering pass runs each seat's
	// auxiliary call on the role listed here, so it needs no resolver of
	// its own.
	Seats Seats

	// Publish announces what a pass did. Nil drops the announcements and
	// keeps the writes, which is the right trade: the pass IS the work,
	// and a broker that cannot take the announcement must not stop the
	// company forgetting what it should forget.
	Publish Announce

	// ClaimDuty gates a tick in a fleet. Nil means single-node — there is
	// nobody to be a singleton among.
	ClaimDuty func(ctx context.Context) (bool, error)

	Now func() time.Time
}

// NewBackground builds the loops.
func NewBackground(opts BackgroundOptions) *Background {
	b := &Background{
		agentIDFor: opts.AgentIDFor, seats: opts.Seats, publish: opts.Publish,
		claimDuty: opts.ClaimDuty, now: opts.Now,
		wake: make(chan struct{}),
	}
	if b.now == nil {
		b.now = func() time.Time { return time.Now().UTC() }
	}
	b.passes = b.normalise(opts.Passes)
	return b
}

// Reconfigure hands the loops the passes a new revision configures.
//
// The loops keep running, and each keeps its clock: a pass that was off and
// is now on runs at its loop's next tick, and a cadence that moved takes
// effect from now. An in-flight pass finishes on the passes it started with,
// which is the same guarantee a turn gets about its config pin.
func (b *Background) Reconfigure(p BackgroundPasses) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.passes = b.normalise(p)
	close(b.wake)
	b.wake = make(chan struct{})
}

// normalise applies the defaults.
func (b *Background) normalise(p BackgroundPasses) BackgroundPasses {
	if p.CuratorInterval <= 0 {
		p.CuratorInterval = CuratorInterval
	}
	if p.LifecycleInterval <= 0 {
		p.LifecycleInterval = LifecycleInterval
	}
	if p.ClusterInterval <= 0 {
		p.ClusterInterval = ClusterInterval
	}
	if p.PromotionInterval <= 0 {
		p.PromotionInterval = PromotionInterval
	}
	return p
}

// current is the passes this tick runs, and the channel the next Reconfigure
// closes.
func (b *Background) current() (BackgroundPasses, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.passes, b.wake
}

// pick answers one loop's cadence and, when its pass is on, the pass itself.
type pick func(BackgroundPasses) (time.Duration, func(context.Context))

// Start arms the loops, which run until [Background.Stop] or ctx is done.
//
// ALL FOUR, whether or not the revision in force turns their pass on: the
// next apply may, and a loop armed only then would start its clock at that
// apply. A loop whose pass is off ticks without claiming its duty, so an idle
// one costs a timer and never a coordination round trip.
//
// SEPARATE TICKERS rather than one at the shorter cadence with a counter:
// the passes have unrelated cadences for unrelated reasons, and a counter
// would tie the curator's schedule to the lifecycle's, so tuning one would
// silently move the other.
//
// Start derives its own cancellable context rather than ticking on the
// caller's, because the engine hands this one a DETACHED context — these
// loops are meant to outlive the signal that starts a drain — and a loop
// whose only exit is a context that can never be cancelled has no exit.
func (b *Background) Start(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	arm := func(name string, choose pick) {
		b.running.Go(func() { b.loop(loopCtx, name, choose) })
	}
	arm("episode_lifecycle", func(p BackgroundPasses) (time.Duration, func(context.Context)) {
		if p.Lifecycle == nil {
			return p.LifecycleInterval, nil
		}
		return p.LifecycleInterval, func(ctx context.Context) { b.compactPass(ctx, p.Lifecycle) }
	})
	arm("skill_curator", func(p BackgroundPasses) (time.Duration, func(context.Context)) {
		if p.Skills == nil {
			return p.CuratorInterval, nil
		}
		return p.CuratorInterval, func(ctx context.Context) { b.curatePass(ctx, p.Skills, p.Policy) }
	})
	arm("skill_clustering", func(p BackgroundPasses) (time.Duration, func(context.Context)) {
		if p.Cluster == nil {
			return p.ClusterInterval, nil
		}
		return p.ClusterInterval, func(ctx context.Context) { b.clusterPass(ctx, p.Cluster) }
	})
	arm("skill_promotion", func(p BackgroundPasses) (time.Duration, func(context.Context)) {
		if p.Promoter == nil {
			return p.PromotionInterval, nil
		}
		return p.PromotionInterval, func(ctx context.Context) { b.promotePass(ctx, p.Promoter) }
	})
}

// Stop ends the loops and waits for an in-flight pass.
//
// WAITING rather than signalling and walking away, for the reason every
// other loop in the engine waits: a pass mid-flight holds a store read and
// may be inside a paid summarisation call, and the caller's next move is to
// close that store. Returning before the pass does would run compaction
// queries against a closed database on a process that believes it stopped.
//
// Idempotent, and safe on a [Background] that was never started — the shape
// a node with no store or a company with learning off produces.
func (b *Background) Stop() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	b.running.Wait()
	b.cancel = nil
}

// promotePass drafts what each unit's seats converged on.
//
// The pass walks every unit itself, so unlike the other three this is a
// single call: a promotion is per UNIT rather than per seat, and the unit
// list is the pass's own input.
func (b *Background) promotePass(ctx context.Context, promoter *Promoter) {
	for _, payload := range promoter.Pass(ctx) {
		if b.publish != nil {
			// SOURCED to nothing in particular. A promotion has several
			// authors and no single seat, which is why its event carries a
			// unit rather than a handle — and stamping one contributor's
			// would file a team's finding under one agent.
			b.publish(ctx, "", payload)
		}
	}
}

// clusterPass drafts from the shapes each seat repeats.
//
// PER SEAT and in sequence, like the compaction pass and for the same
// reason: each seat's pass is a scan plus at most one auxiliary call, and
// running the roster concurrently would turn one tick into a company-wide
// spike against the auxiliary model for work that has a day to happen in.
func (b *Background) clusterPass(ctx context.Context, cluster *Synthesizer) {
	for _, seat := range b.roster() {
		payloads, err := cluster.ClusterPass(ctx, seat, b.agentID(seat))
		if err != nil {
			log.WarnContext(ctx, "skill_clustering_failed", "seat", seat.Handle(),
				"error", err.Error())
		}
		for _, payload := range payloads {
			if b.publish != nil {
				b.publish(ctx, seat.Handle(), payload)
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// loop ticks one pass, claiming the duty first.
//
// NO IMMEDIATE FIRST TICK. Every node in a fleet starts within seconds of a
// rolling restart, so firing on start means every node races for the duty at
// once and the winner runs a pass over a company that has not taken a turn
// yet. Waiting one interval also means a crash-looping node cannot spend the
// company's tokens compacting on every restart.
//
// A Reconfigure WAKES the loop, which re-reads its passes and its cadence;
// only a cadence that actually moved resets the clock, so an apply that
// changes nothing about this pass leaves its next tick where it was. A tick
// that races the wake runs the passes it last read, the same answer a pass
// that started a moment before the apply gets. And a pass that is off claims
// no duty: a claim is a coordination round trip, and one made for no work
// would be paid on every tick of every idle loop.
func (b *Background) loop(ctx context.Context, name string, choose pick) {
	passes, wake := b.current()
	every, _ := choose(passes)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			passes, wake = b.current()
			if next, _ := choose(passes); next != every {
				every = next
				ticker.Reset(every)
			}
		case <-ticker.C:
			_, pass := choose(passes)
			if pass == nil {
				continue
			}
			if !b.holdsDuty(ctx, name) {
				continue
			}
			pass(ctx)
		}
	}
}

// holdsDuty reports whether this node runs the pass this tick.
//
// FAILS CLOSED on an unreachable coordination store, which is the opposite
// of what the read side does and deliberately so: not knowing whether a peer
// holds the duty is the case where running anyway produces two nodes
// summarising the same cluster, at LLM prices, with both writes landing.
func (b *Background) holdsDuty(ctx context.Context, name string) bool {
	if b.claimDuty == nil {
		return true
	}
	holds, err := b.claimDuty(ctx)
	if err != nil {
		log.WarnContext(ctx, "background_duty_unknown", "pass", name, "error", err.Error(),
			"detail", "the pass is skipped this tick rather than risking a "+
				"second node running it")
		return false
	}
	return holds
}

// compactPass runs one episode-lifecycle pass per seat.
//
// PER SEAT and in sequence, because [Lifecycle.Pass] claims per handle and
// each pass is a burst of deletes plus up to a handful of summarisation
// calls. Running the roster concurrently would turn one tick into a
// company-wide spike against the auxiliary model for work that has an hour
// to happen in.
func (b *Background) compactPass(ctx context.Context, lifecycle *Lifecycle) {
	now := b.now()
	for _, seat := range b.roster() {
		// The rows by the handle the seat was created under, everything
		// said about them by the one it answers to — see [Seats].
		origin, handle := seat.Origin(), seat.Handle()
		due, ok, err := lifecycle.RawCount(ctx, origin)
		if err != nil {
			log.WarnContext(ctx, "episode_lifecycle_count_failed", "seat", handle, "error", err.Error())
			continue
		}
		if !ok {
			// Under threshold. The count is one indexed query and this is
			// the overwhelmingly common answer, which is why the pass is
			// gated on it rather than on the pass's own early return.
			continue
		}
		// ANNOUNCED BEFORE THE PASS, not after. It is the signal that a
		// seat became DUE, and it is the only one an operator gets when
		// the pass then fails or is still running: pairing it with the
		// completion below is what makes "this seat is over threshold
		// and never gets compacted" visible at all.
		if b.publish != nil {
			b.publish(ctx, handle, types.CompactionRequested{
				AgentHandle: handle, RawCount: due,
				Threshold: lifecycle.Options().Threshold,
			})
		}
		res, err := lifecycle.Pass(ctx, origin, now)
		if err != nil {
			// The partial result is still published: the deletes that
			// committed are real, and reporting nothing would claim a
			// pass removed nothing when it removed thousands of rows.
			log.WarnContext(ctx, "episode_lifecycle_pass_failed", "seat", handle,
				"raw_episodes", due, "error", err.Error())
		}
		b.announce(ctx, handle, res)
		if ctx.Err() != nil {
			return
		}
	}
}

// curatePass ages the whole catalogue in one call.
//
// ONE CALL for every seat, not one per seat: [Skills.Curate] with an empty
// handle walks the table, and its unit of work is a guarded single-row
// update rather than a model call — so there is no per-seat cost to spread
// and a per-seat loop would be N table scans instead of one.
func (b *Background) curatePass(ctx context.Context, skills *Skills, policy CuratorPolicy) {
	res, err := skills.Curate(ctx, policy, "", b.now())
	if err != nil {
		log.WarnContext(ctx, "skill_curator_pass_failed", "error", err.Error(),
			"applied", len(res.Applied), "scanned", res.Scanned)
	}
	if res.Raced > 0 {
		// Not a failure: the guard is what keeps a skill being used
		// mid-turn from being archived out from under the agent holding
		// it. Logged because a persistently high count means the pass is
		// racing the traffic it is meant to run behind.
		log.DebugContext(ctx, "skill_curator_transitions_raced", "count", res.Raced)
	}
	if len(res.Applied) == 0 {
		return
	}
	// ONE READING OF THE ROSTER for every change this pass announces. A
	// skill row holds the handle its seat was created under, and an event
	// names the seat by the address it answers to now — so a renamed seat's
	// transitions are announced under its current handle rather than one it
	// has retired.
	current := map[string]string{}
	for _, seat := range b.roster() {
		current[seat.Origin()] = seat.Handle()
	}
	for _, change := range res.Applied {
		b.announceChange(ctx, change, current)
	}
}

// roster is this tick's seats.
func (b *Background) roster() []*org.Role {
	if b.seats == nil {
		return nil
	}
	return b.seats()
}

// agentID derives the seat's agent id, or "" when no resolver was wired.
func (b *Background) agentID(seat *org.Role) string {
	if b.agentIDFor == nil {
		return ""
	}
	return b.agentIDFor(seat)
}

// Announce publishes one background pass's lifecycle event.
//
// The seat's handle rides ALONGSIDE the payload rather than being read out
// of it, because these events reach the engine's publisher which stamps a
// SOURCE — and the source is what makes a dashboard file the event under the
// seat it is about. A background pass has no turn and therefore no trace to
// inherit, which is the one place these differ from a reflection worker's.
type Announce func(ctx context.Context, handle string, payload events.Payload)

// announce reports one lifecycle pass.
//
// PUBLISHED EVEN WHEN THE PASS FAILED, carrying what it managed to do: the
// actions are independent deletes and folds, so the ones that committed are
// real, and an operator auditing a company's memory needs the counts more
// than they need the failure — which is already in the log with its error.
func (b *Background) announce(ctx context.Context, handle string, res PassResult) {
	if b.publish == nil {
		return
	}
	b.publish(ctx, handle, types.CompactionCompleted{
		AgentHandle: handle,
		// Reported together with the non-terminal drop: both are raw
		// rows this pass removed without folding them into anything,
		// which is the distinction the event's reader cares about.
		NonTerminalDropped:      res.NonTerminalDropped + res.ToolFreeDropped,
		ConsolidatedDropped:     res.ConsolidatedDropped + res.OrphansDropped,
		ClustersCompacted:       res.ClustersCompacted,
		RawReplacedByCompaction: res.RawReplaced,
		CompactedEvicted:        res.CompactedEvicted + res.ExemplarsEvicted,
	})
}

// announceChange reports one curator transition.
//
// The state on the change is the state being LEFT — see [StateChange] — so
// the destination decides the event and the snapshot supplies what it says
// about where the row came from.
//
// current maps a seat's origin to the handle it answers to now. A skill whose
// seat the roster does not hold — a seat the company removed — is announced
// under the handle it is filed under, which is the only name left for it.
func (b *Background) announceChange(ctx context.Context, c StateChange, current map[string]string) {
	if b.publish == nil {
		return
	}
	at := b.now().UTC().Format(time.RFC3339)
	lastUsed := ""
	if !c.Skill.LastUsedAt.IsZero() {
		lastUsed = c.Skill.LastUsedAt.UTC().Format(time.RFC3339)
	}
	handle := c.Skill.AgentHandle
	if now, ok := current[handle]; ok {
		handle = now
	}
	switch c.To {
	case SkillStale:
		b.publish(ctx, handle, types.SkillStaled{
			AgentHandle: handle, SkillID: c.Skill.ID,
			SkillName: c.Skill.Name, LastUsedAt: lastUsed, TransitionedAt: at,
		})
	case SkillArchived:
		b.publish(ctx, handle, types.SkillArchived{
			AgentHandle: handle, SkillID: c.Skill.ID,
			SkillName: c.Skill.Name, LastUsedAt: lastUsed, TransitionedAt: at,
		})
	case SkillActive:
		b.publish(ctx, handle, types.SkillRevived{
			AgentHandle: handle, SkillID: c.Skill.ID,
			SkillName:      c.Skill.Name,
			PriorState:     types.SkillState(c.Skill.State),
			TransitionedAt: at,
		})
	}
}
