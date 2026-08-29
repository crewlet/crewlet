package learning

import (
	"context"
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

// Seats lists the handles a background pass walks.
//
// A FUNCTION rather than a slice: an apply changes the roster, and a pass
// holding the list it started with would keep compacting a seat the company
// removed and never touch one it added.
type Seats func() []string

// Background runs the passes no turn drives.
type Background struct {
	lifecycle *Lifecycle
	skills    *Skills
	cluster   *Synthesizer
	roleFor   func(handle string) *org.Role
	policy    CuratorPolicy
	seats     Seats
	publish   Announce

	curatorEvery   time.Duration
	lifecycleEvery time.Duration
	clusterEvery   time.Duration
	claimDuty      func(ctx context.Context) (bool, error)
	now            func() time.Time
}

// BackgroundOptions configures the loops.
type BackgroundOptions struct {
	// Lifecycle compacts episodes; nil disables that pass.
	Lifecycle *Lifecycle

	// Skills ages the catalogue; nil disables that pass.
	Skills *Skills

	// Cluster drafts skills from the shapes a seat repeats; nil disables
	// that pass, which is what `skill_synthesis.scheduler_enabled: false`
	// — the default — resolves to.
	Cluster *Synthesizer

	// RoleFor resolves a seat handle to the role whose auxiliary model the
	// clustering pass runs on. Required alongside Cluster: a pass with no
	// role cannot resolve a model, and answering with the first role in
	// the org would charge one seat's work to another's chain.
	RoleFor func(handle string) *org.Role

	// Policy is the disuse schedule. Zero values take the defaults.
	Policy CuratorPolicy

	// Seats lists the handles to walk. Nil means no seats, which yields
	// loops that tick and do nothing — the correct shape for a node with
	// no active company.
	Seats Seats

	// Publish announces what a pass did. Nil drops the announcements and
	// keeps the writes, which is the right trade: the pass IS the work,
	// and a broker that cannot take the announcement must not stop the
	// company forgetting what it should forget.
	Publish Announce

	// CuratorInterval, LifecycleInterval and ClusterInterval override the
	// cadences above.
	CuratorInterval   time.Duration
	LifecycleInterval time.Duration
	ClusterInterval   time.Duration

	// ClaimDuty gates a tick in a fleet. Nil means single-node — there is
	// nobody to be a singleton among.
	ClaimDuty func(ctx context.Context) (bool, error)

	Now func() time.Time
}

// NewBackground builds the loops.
func NewBackground(opts BackgroundOptions) *Background {
	b := &Background{
		lifecycle: opts.Lifecycle, skills: opts.Skills,
		cluster: opts.Cluster, roleFor: opts.RoleFor,
		policy: opts.Policy, seats: opts.Seats, publish: opts.Publish,
		curatorEvery: opts.CuratorInterval, lifecycleEvery: opts.LifecycleInterval,
		clusterEvery: opts.ClusterInterval,
		claimDuty:    opts.ClaimDuty, now: opts.Now,
	}
	if b.roleFor == nil {
		// A clustering pass without one resolves no model and would fail
		// per seat, per tick, forever. Refusing the pass is the honest
		// answer and it is logged where NewBackground's caller sees it.
		b.cluster = nil
	}
	if b.curatorEvery <= 0 {
		b.curatorEvery = CuratorInterval
	}
	if b.lifecycleEvery <= 0 {
		b.lifecycleEvery = LifecycleInterval
	}
	if b.clusterEvery <= 0 {
		b.clusterEvery = ClusterInterval
	}
	if b.now == nil {
		b.now = func() time.Time { return time.Now().UTC() }
	}
	return b
}

// Start arms both loops, which run until ctx is done.
//
// SEPARATE TICKERS rather than one at the shorter cadence with a counter:
// the two passes have unrelated cadences for unrelated reasons, and a
// counter would tie the curator's schedule to the lifecycle's — so tuning
// one would silently move the other.
func (b *Background) Start(ctx context.Context) {
	if b.lifecycle != nil {
		go b.loop(ctx, "episode_lifecycle", b.lifecycleEvery, b.compactPass)
	}
	if b.skills != nil {
		go b.loop(ctx, "skill_curator", b.curatorEvery, b.curatePass)
	}
	if b.cluster != nil {
		go b.loop(ctx, "skill_clustering", b.clusterEvery, b.clusterPass)
	}
}

// clusterPass drafts from the shapes each seat repeats.
//
// PER SEAT and in sequence, like the compaction pass and for the same
// reason: each seat's pass is a scan plus at most one auxiliary call, and
// running the roster concurrently would turn one tick into a company-wide
// spike against the auxiliary model for work that has a day to happen in.
func (b *Background) clusterPass(ctx context.Context) {
	for _, handle := range b.handles() {
		role := b.roleFor(handle)
		if role == nil {
			// A seat in the roster the epoch cannot resolve — mid-apply,
			// or a handle the org no longer carries. Skipped rather than
			// run with no role, which would charge its auxiliary call to
			// whichever chain answered.
			log.Debug("skill_clustering_skipped", "reason", "unknown_seat",
				"agent_handle", handle)
			continue
		}
		payloads, err := b.cluster.ClusterPass(ctx, role, handle)
		if err != nil {
			log.Warn("skill_clustering_failed", "seat", handle, "error", err.Error())
		}
		for _, payload := range payloads {
			if b.publish != nil {
				b.publish(ctx, handle, payload)
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
func (b *Background) loop(ctx context.Context, name string, every time.Duration,
	pass func(context.Context),
) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
		log.Warn("background_duty_unknown", "pass", name, "error", err.Error(),
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
func (b *Background) compactPass(ctx context.Context) {
	now := b.now()
	for _, handle := range b.handles() {
		due, ok, err := b.lifecycle.RawCount(ctx, handle)
		if err != nil {
			log.Warn("episode_lifecycle_count_failed", "seat", handle, "error", err.Error())
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
				Threshold: b.lifecycle.Options().Threshold,
			})
		}
		res, err := b.lifecycle.Pass(ctx, handle, now)
		if err != nil {
			// The partial result is still published: the deletes that
			// committed are real, and reporting nothing would claim a
			// pass removed nothing when it removed thousands of rows.
			log.Warn("episode_lifecycle_pass_failed", "seat", handle,
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
func (b *Background) curatePass(ctx context.Context) {
	res, err := b.skills.Curate(ctx, b.policy, "", b.now())
	if err != nil {
		log.Warn("skill_curator_pass_failed", "error", err.Error(),
			"applied", len(res.Applied), "scanned", res.Scanned)
	}
	if res.Raced > 0 {
		// Not a failure: the guard is what keeps a skill being used
		// mid-turn from being archived out from under the agent holding
		// it. Logged because a persistently high count means the pass is
		// racing the traffic it is meant to run behind.
		log.Debug("skill_curator_transitions_raced", "count", res.Raced)
	}
	for _, change := range res.Applied {
		b.announceChange(ctx, change)
	}
}

// handles is this tick's roster.
func (b *Background) handles() []string {
	if b.seats == nil {
		return nil
	}
	return b.seats()
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
		AgentHandle:             handle,
		NonTerminalDropped:      res.NonTerminalDropped,
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
func (b *Background) announceChange(ctx context.Context, c StateChange) {
	if b.publish == nil {
		return
	}
	at := b.now().UTC().Format(time.RFC3339)
	lastUsed := ""
	if !c.Skill.LastUsedAt.IsZero() {
		lastUsed = c.Skill.LastUsedAt.UTC().Format(time.RFC3339)
	}
	switch c.To {
	case SkillStale:
		b.publish(ctx, c.Skill.AgentHandle, types.SkillStaled{
			AgentHandle: c.Skill.AgentHandle, SkillID: c.Skill.ID,
			SkillName: c.Skill.Name, LastUsedAt: lastUsed, TransitionedAt: at,
		})
	case SkillArchived:
		b.publish(ctx, c.Skill.AgentHandle, types.SkillArchived{
			AgentHandle: c.Skill.AgentHandle, SkillID: c.Skill.ID,
			SkillName: c.Skill.Name, LastUsedAt: lastUsed, TransitionedAt: at,
		})
	case SkillActive:
		b.publish(ctx, c.Skill.AgentHandle, types.SkillRevived{
			AgentHandle: c.Skill.AgentHandle, SkillID: c.Skill.ID,
			SkillName:      c.Skill.Name,
			PriorState:     types.SkillState(c.Skill.State),
			TransitionedAt: at,
		})
	}
}
