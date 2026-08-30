package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The learning WRITE side, wired.
//
// # Why this is one function and not three subscriptions
//
// Everything a seat learns is learned from one event — a completed turn —
// and every writer is gated on the same questions about it. Giving each
// writer its own consumer group would mean three independent redelivery
// windows over one turn, three places to discover that a company learns
// nothing, and three chances for one of them to be quietly unwired. The
// reflect dispatcher already owns that gate sequence and reports which gate
// closed, per worker, per turn.
//
// # It is the half that was missing
//
// The read side — the Plan-phase prefetch's memory, recall, profile and
// skill blocks — was wired first and looked healthy: it queried, found
// nothing, and rendered nothing, which is indistinguishable from a young
// company. Nothing wrote, so nothing was ever going to be found.

// buildReflectionWorkers assembles this epoch's learning passes.
//
// EACH IS OPTIONAL AND EACH FAILURE IS FATAL TO ITSELF ONLY: a company can
// turn any of them off, and a worker that cannot be built (no diary, no
// model registry) is reported and left out rather than taking the other two
// with it. What is NOT tolerated is silence — a worker that should have been
// built and was not is logged at warn, because a company learning nothing is
// exactly the failure nobody notices.
func (e *Engine) buildReflectionWorkers(c *Company) []learning.Worker {
	cfg := c.Config.Learning
	if !cfg.On() {
		// The company turned learning off. Not a warning: it is a
		// supported configuration, and the dispatcher's no-workers fast
		// path costs nothing.
		return nil
	}
	db := e.backends.Store
	if db == nil {
		log.Warn("learning_write_side_unavailable",
			"reason", "this node has no local store",
			"detail", "nothing will write a diary row, an episode or a "+
				"counterparty profile, and every prefetch block will stay empty")
		return nil
	}

	// EVERY WORKER RESOLVES ITS MODEL THROUGH THIS, so auxiliary spend is
	// charged against the same fleet counter a turn is. Wrapping the seam
	// rather than each call site is what makes a worker added later charge
	// without anyone remembering to — see learningbudget.go.
	models := e.meteredModelsFor(c)

	var workers []learning.Worker
	if cfg.Reflect.Enabled.Or(true) && cfg.Reflect.PersistDecider.Or(true) {
		decider, err := learning.NewPersistDecider(models, learning.NewDiary(db),
			learning.PersistOptions{MaxTokens: cfg.Reflect.BudgetTokens})
		if err != nil {
			log.Warn("persist_decider_unavailable", "error", err,
				"detail", "turns will not be classified and nothing will be "+
					"written to the diary")
		} else {
			workers = append(workers, decider)
		}
	}

	// THE EPISODIST IS NOT UNDER A TOGGLE, and that is deliberate: the
	// episode table is what the lifecycle worker compacts, what skill
	// synthesis clusters over and what recall reads. There is no
	// `episodic.enabled` in the config because the read-side knobs
	// (retrieval_limit) presuppose rows exist — a company that wants no
	// episodic memory turns learning off entirely.
	episodist, err := learning.NewEpisodist(learning.NewEpisodes(db),
		learning.EpisodistOptions{Embed: e.embedder()})
	if err != nil {
		log.Warn("episodist_unavailable", "error", err,
			"detail", "no episode will be recorded, so recall and skill "+
				"synthesis have nothing to work from")
	} else {
		workers = append(workers, episodist)
	}

	// THE USE STAMP IS NOT UNDER A TOGGLE either, and it is not really a
	// learning pass: it is what keeps the curator honest. Without it every
	// synthesized skill's last-used stamp stands still while the prefetch
	// puts it in front of a model every turn, and the catalogue ages out
	// over a quarter with nothing to show why.
	if use := learning.NewSkillUse(learning.NewSkills(db), nil); use != nil {
		workers = append(workers, use)
	}

	// THE SYNTHESIZER, which is what makes synthesized_skills a table with
	// rows in it. Everything that reads a skill — use_skill, the Plan-phase
	// catalogue, refine_skill, the curator — shipped before anything wrote
	// one, so all of it ran correctly over an empty table.
	if cfg.SkillSynthesis.Enabled.Or(true) {
		synth, err := learning.NewSynthesizer(models, learning.NewSkills(db),
			synthesizerOptions(cfg.SkillSynthesis))
		if err != nil {
			log.Warn("skill_synthesizer_unavailable", "error", err,
				"detail", "no skill will ever be drafted, so use_skill and the "+
					"Plan-phase skill catalogue stay empty")
		} else {
			workers = append(workers, synth)
		}
	}

	// THE REFINER, the auto half of what refine_skill does by hand. Both
	// were documented from the start; only the manual one existed, so
	// `auto_refine_on_success` and `auto_refine_on_failure` validated,
	// shipped in the example company and had no reader — a company whose
	// skills only ever improved when a model happened to notice.
	if cfg.SkillRefinement.Refines() {
		switch {
		case !cfg.SkillRefinement.OnSuccess() && !cfg.SkillRefinement.OnFailure():
			// Both halves off is refinement off, spelled the long way.
			// Building the worker anyway would cost a Skip per turn and
			// report a reason that reads like a bug.
			log.Info("skill_refiner_idle",
				"detail", "auto_refine_on_success and auto_refine_on_failure "+
					"are both false, so no turn's outcome is refined")
		default:
			refiner, err := learning.NewRefiner(models, learning.NewSkills(db),
				refinerOptions(cfg.SkillRefinement))
			if err != nil {
				log.Warn("skill_refiner_unavailable", "error", err,
					"detail", "no skill will be refined after a turn; only the "+
						"refine_skill tool will ever change one")
			} else {
				workers = append(workers, refiner)
			}
		}
	}

	if cfg.Counterparty.Enabled.Or(true) {
		profiler, err := learning.NewProfiler(models, learning.NewCounterparties(db),
			learning.ProfilerOptions{MaxTokens: cfg.Counterparty.BudgetTokens})
		if err != nil {
			log.Warn("counterparty_profiler_unavailable", "error", err,
				"detail", "no profile will be written, so the prefetch's "+
					"counterparty block stays empty")
		} else {
			workers = append(workers, profiler)
		}
	}
	return workers
}

// refinerOptions carries the company's refinement knobs to the worker.
//
// A NAMED FUNCTION rather than a struct literal inline, because every field
// here is a knob that validated and did nothing before the worker existed —
// the exact failure a literal buried in a switch arm reintroduces silently
// when a field is dropped. This is the seam a test can hold.
//
// The two toggles are resolved to concrete bools and taken by address: the
// config Toggle's zero value is UNSET rather than false, and passing it
// through as a nil pointer would leave the worker applying its own default
// instead of the company's answer.
func refinerOptions(cfg config.SkillRefinement) learning.RefinerOptions {
	onSuccess, onFailure := cfg.OnSuccess(), cfg.OnFailure()
	return learning.RefinerOptions{
		OnSuccess:    &onSuccess,
		OnFailure:    &onFailure,
		MaxTokens:    cfg.BudgetTokens,
		MaxBodyChars: cfg.MaxBodyChars,
		KeepVersions: cfg.MaxVersionsKept,
	}
}

// reconfigureReflection points the dispatcher at this epoch.
//
// The dispatcher itself is built once, at start, and outlives every apply —
// see [learning.Reflector] for why its redelivery ring must not reset.
func (e *Engine) reconfigureReflection(c *Company) {
	if e.reflector == nil {
		return
	}
	workers := e.buildReflectionWorkers(c)
	if err := e.reflector.Reconfigure(c.Org, workers, e.learningBudget(c)); err != nil {
		// The previous epoch's workers keep serving. Reflecting with a
		// stale org is a far smaller wrong than not reflecting at all,
		// and this is a bug in the wiring above rather than in the
		// operator's config — which is why it is an error, not a warn.
		log.Error("reflection_reconfigure_failed", "error", err)
		return
	}
	if len(workers) == 0 {
		log.Info("learning_write_side_idle",
			"detail", "no learning worker is wired for this revision; "+
				"nothing will be written and every prefetch block stays empty")
	}
}

// startReflection builds the dispatcher and attaches it to completed turns.
//
// Called ONCE, from start, before the first apply — so the subscription
// exists for the life of the process and an apply only ever swaps what runs
// behind it.
func (e *Engine) startReflection(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	company := e.Company()
	if company == nil {
		// No active revision. There is no org to resolve seats against
		// and no models to run a pass on; the config plane will call
		// reconfigureReflection when one arrives, and this returns then.
		return nil
	}
	reflector, err := learning.NewReflector(company.Org, e.backends.Queue,
		e.buildReflectionWorkers(company), e.learningBudget(company))
	if err != nil {
		return fmt.Errorf("engine: build the reflect dispatcher: %w", err)
	}
	if err := reflector.Start(ctx, e.backends.Queue); err != nil {
		return fmt.Errorf("engine: attach the reflect dispatcher: %w", err)
	}
	e.reflector = reflector
	return nil
}

// ---- the fleet singletons --------------------------------------------- //

// skillCuratorDutyName is the singleton EVERY background pass claims.
//
// One name for three loops, deliberately: BackgroundOptions takes a single
// ClaimDuty, so the skill-ageing pass, the episode compaction and the skill
// clustering all run on the same node — which is also what learningDutyTTL is
// sized against. The name is the skill curator's for history rather than
// accuracy: renaming it would change the coordination key, and during a
// rolling upgrade a node on each name would both believe they held "the"
// duty.
const skillCuratorDutyName = "skill-curator"

// lifecycleOptions projects the operator's episode-lifecycle config onto the
// worker's own options.
//
// A PROJECTION rather than the config type itself, so the learning package
// keeps importing nothing but the store — the same rule its Summarizer seam
// follows.
func lifecycleOptions(c *config.EpisodeLifecycle) learning.Options {
	return learning.Options{
		Threshold:         c.MaxRawEpisodesPerAgent,
		NonTerminalMaxAge: days(c.NonTerminalMaxAgeDays),
		ToolFreeMaxAge:    days(c.ToolFreeMaxAgeDays),
		ConsolidatedGrace: days(c.ConsolidatedGraceDays),
		MinAge:            days(c.CompactionMinAgeDays),
		MinClusterSize:    c.CompactionMinClusterSize,
		JaccardThreshold:  c.CompactionJaccardThreshold,
		BatchSize:         c.CompactionBatchSize,
		ExemplarCount:     c.ExemplarCount,
		CompactedMaxAge:   days(c.CompactedMaxAgeDays),
	}
}

// days turns a config horizon into a duration. Zero stays zero, which every
// horizon reads as "never".
func days(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * 24 * time.Hour
}

// startLearningBackground arms the passes no turn drives.
//
// LAST, like the sandbox waiter and the retention sweep, because both are
// fleet singletons claimed under this node's own incarnation — which does
// not exist until the node does.
func (e *Engine) startLearningBackground(ctx context.Context) {
	db := e.backends.Store
	if db == nil {
		return
	}
	if !e.profile.RunsWorkers() {
		// The operator said this node runs no singleton duties. The duty
		// gate would refuse every tick anyway; not arming the loops at
		// all is the same answer without two goroutines waking hourly to
		// be told no.
		return
	}
	company := e.Company()
	if company == nil || !company.Config.Learning.On() {
		return
	}
	cfg := company.Config.Learning

	var lifecycle *learning.Lifecycle
	// A SUMMARIZER IS REQUIRED for compaction: the pass folds a cluster by
	// asking a model to describe what its members had in common, and one
	// without a model can only delete. Deleting is the half an operator
	// least wants unsupervised, so no summarizer means no pass at all —
	// the rows stay raw and readable rather than being dropped unfolded.
	if summarize := e.auxSummarizer(company); summarize != nil {
		lifecycle = learning.NewLifecycle(db, learning.NewSummarizer(summarize),
			lifecycleOptions(&cfg.EpisodeLifecycle))
	} else {
		log.WarnContext(ctx, "episode_compaction_unavailable",
			"reason", "no auxiliary model is configured for any seat",
			"detail", "raw episodes accumulate and every recall scans all of "+
				"them; nothing is deleted")
	}

	var skills *learning.Skills
	if cfg.SkillCurator.Enabled.Or(true) {
		skills = learning.NewSkills(db)
	}

	cluster := e.clusteringPass(company)
	promoter := e.buildPromoter(company)

	if lifecycle == nil && skills == nil && cluster == nil && promoter == nil {
		return
	}

	e.learning = learning.NewBackground(learning.BackgroundOptions{
		Lifecycle: lifecycle,
		Skills:    skills,
		Cluster:   cluster,
		Promoter:  promoter,
		// FRESH through the epoch, like Seats below: a captured resolver
		// would keep answering with the roles of the revision this call
		// saw, and charge a renamed seat's clustering to a chain the
		// company has replaced.
		RoleFor: e.seatRole,
		Policy: learning.CuratorPolicy{
			StaleAfter:   days(cfg.SkillCurator.StaleAfterDays),
			ArchiveAfter: days(cfg.SkillCurator.ArchiveAfterDays),
		},
		// READ FRESH through the epoch, never bound to the company this
		// call sees: an apply replaces the roster, and a captured list
		// would keep compacting a seat the revision removed and never
		// touch one it added.
		Seats:             func() []string { return e.seatHandles() },
		Publish:           e.publishLearning,
		CuratorInterval:   hours(cfg.SkillCurator.IntervalHours),
		ClusterInterval:   seconds(float64(cfg.SkillSynthesis.SchedulerIntervalSeconds)),
		PromotionInterval: 0, // learning.PromotionInterval; no knob configures it
		ClaimDuty:         e.workerDuty(skillCuratorDutyName, learningDutyTTL),
		LifecycleInterval: 0,
	})
	// Detached, for the same reason the node's loops are: a loop bound to
	// a signal context stops at SIGTERM, which would make its lifetime
	// differ from every other loop's for no reason a reader could find.
	e.learning.Start(context.WithoutCancel(ctx))
}

// synthesizerOptions carries the company's synthesis knobs to the worker.
//
// ONE MAPPING for both paths — the inline worker and the clustering pass —
// because the four gates they share must not diverge: a company that raised
// `max_skills_per_agent` would otherwise find one path honouring it and the
// other capped at the shipped default, with nothing failing.
//
// A named function rather than a struct literal at each call site for the
// same reason refinerOptions is one: every field here is a knob that
// validated and did nothing until its worker existed, and a literal buried in
// a constructor call is where one silently goes missing again. Episodes is
// NOT set here — it is what distinguishes the two paths, and the inline
// worker needs none.
func synthesizerOptions(cfg config.SkillSynthesis) learning.SynthesizerOptions {
	return learning.SynthesizerOptions{
		MinToolCalls:              cfg.MinToolCalls,
		MaxSkillsPerAgent:         cfg.MaxSkillsPerAgent,
		DuplicateJaccardThreshold: cfg.DuplicateJaccardThreshold,
		MaxTokens:                 cfg.BudgetTokens,
		ClusterMinSize:            cfg.ClusterMinSize,
		ClusterJaccardThreshold:   cfg.ClusterJaccardThreshold,
		EpisodeFetchLimit:         cfg.EpisodeFetchLimit,
		ClusterWindow:             hours(cfg.ClusterWindowHours),
	}
}

// clusteringPass builds the pass `skill_synthesis.scheduler_enabled` turns on,
// or nil when the company did not ask for it.
//
// A NAMED FUNCTION rather than a block inside the wiring, because every field
// it reads is a knob that validated and did nothing before the pass existed —
// `cluster_min_size`, `cluster_jaccard_threshold`, `episode_fetch_limit` — and
// a literal buried in a constructor call is where one silently goes missing
// again. This is the seam a test can hold.
//
// Gated on BOTH toggles: `scheduler_enabled` under a company that turned
// skill synthesis off would draft skills for a company that said not to.
func (e *Engine) clusteringPass(c *Company) *learning.Synthesizer {
	db := e.backends.Store
	cfg := c.Config.Learning.SkillSynthesis
	if db == nil || !cfg.Enabled.Or(true) || !cfg.Clusters() {
		return nil
	}
	opts := synthesizerOptions(cfg)
	opts.Episodes = learning.NewEpisodes(db)
	built, err := learning.NewSynthesizer(e.meteredModelsFor(c), learning.NewSkills(db), opts)
	if err != nil {
		log.Warn("skill_clustering_unavailable", "error", err,
			"detail", "scheduler_enabled is on but no clustering pass could be "+
				"built, so a seat's repeated procedures are never distilled")
		return nil
	}
	return built
}

// learningDutyTTL is how long a background duty survives without a re-claim.
//
// Sized off the SHORTEST of the three cadences, because every loop claims
// through the same helper and a TTL shorter than a loop's interval would hand
// the duty to a peer between every tick. Lifecycle and clustering both tick
// hourly, so three of those matches the ratio the retention sweep and the
// sandbox waiter use: one missed tick must not move the duty, and a dead
// node's is picked up within a few hours.
//
// A company that lowers `scheduler_interval_seconds` below this does not
// break it — the clustering loop simply claims more often than it needs to,
// which is what a shared duty lease is for.
const learningDutyTTL = 3 * learning.LifecycleInterval

// seatHandles is the current epoch's agent seats.
func (e *Engine) seatHandles() []string {
	company := e.Company()
	if company == nil {
		return nil
	}
	seats := company.Seats()
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Handle)
	}
	return out
}

// seatRole resolves a seat handle against the CURRENT epoch.
//
// Read fresh, never captured: a background pass holding the roster it started
// with would charge a renamed seat's auxiliary call to a chain the company
// has replaced, and would answer nil for a seat an apply has just added.
func (e *Engine) seatRole(handle string) *org.Role {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	return company.Org.AgentSeatByHandle(handle)
}

// publishLearning announces one background pass, best effort.
//
// SOURCED to the seat the pass was about, so a dashboard files it under that
// seat rather than as free-floating background work. No trace context: a
// background pass has no turn that caused it, and inventing one would nest
// a company-wide sweep under whichever turn happened to run last.
func (e *Engine) publishLearning(ctx context.Context, handle string, payload events.Payload) {
	ev := events.NewFrom(payload, events.TraceContext{})
	if ev == nil || e.backends == nil || e.backends.Queue == nil {
		return
	}
	if company := e.Company(); company != nil {
		if seat := company.Org.AgentSeatByHandle(handle); seat != nil {
			ev.Source = seat.Name
		}
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		// Swallowed: the pass's writes already landed. Losing the
		// announcement costs a dashboard row; propagating the failure
		// would cost the housekeeping it announces.
		log.WarnContext(ctx, "learning_background_publish_failed", "type", ev.Type,
			"seat", handle, "error", err)
	}
}

// hours turns a config cadence into a duration. Zero stays zero, which the
// worker reads as "take the default".
func hours(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Hour
}

// auxSummarizer resolves each cluster's own seat to its auxiliary chain.
//
// PER CALL rather than once, because the role varies: the lifecycle worker
// walks every seat, and a company whose seats run on different models must
// have each one's memory compacted by the model that seat is configured
// with. Resolving once would compact the whole company on whichever seat
// happened to be first.
//
// Returns nil when NO seat has an auxiliary model, which is what disables
// compaction entirely — see the caller for why a delete-only pass is worse
// than no pass.
func (e *Engine) auxSummarizer(c *Company) learning.CompleteFunc {
	if c.Models == nil {
		return nil
	}
	if !anySeatHasAuxiliary(c) {
		return nil
	}
	return func(ctx context.Context, role, system, user string) (string, error) {
		seat := c.Org.Role(role)
		if seat == nil {
			return "", fmt.Errorf("engine: compaction for %q: this revision has no such role", role)
		}
		member, err := e.meteredModelsFor(c).Head(seat, phase.Auxiliary)
		if err != nil {
			return "", fmt.Errorf("engine: compaction for %q: %w", role, err)
		}
		call, cancel := context.WithTimeout(ctx, learning.DefaultAuxTimeout)
		defer cancel()
		completion, err := member.Provider.Complete(call, llm.Request{
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: system},
				{Role: llm.RoleUser, Content: user},
			},
			Temperature: llm.Temp(compactionTemperature),
			MaxTokens:   e.compactionTokens(c),
		})
		if err != nil {
			return "", fmt.Errorf("engine: compaction on %s: %w", member.Key, err)
		}
		if completion == nil {
			return "", nil
		}
		return completion.Content, nil
	}
}

// compactionTemperature keeps a cluster summary reproducible.
//
// The same 0.2 the other auxiliary passes use, and for the same reason: a
// summary is a description of rows that already exist, so there is nothing
// for sampling to explore — but zero makes some providers degenerate into
// repeating the input.
const compactionTemperature = 0.2

// compactionTokens caps one cluster summary.
func (e *Engine) compactionTokens(c *Company) int {
	if n := c.Config.Learning.EpisodeLifecycle.CompactionBudgetTokens; n > 0 {
		return n
	}
	return learning.DefaultAuxTokens
}

// anySeatHasAuxiliary reports whether at least one seat can answer an
// auxiliary call.
//
// Checked ONCE at start rather than per call, because the answer is a
// property of the revision: a company with no auxiliary model anywhere gets
// no compaction, and discovering that per cluster would mean a failed model
// resolution logged for every seat on every tick, forever.
func anySeatHasAuxiliary(c *Company) bool {
	for _, seat := range c.Seats() {
		role := c.Org.AgentSeatByHandle(seat.Handle)
		if role == nil {
			continue
		}
		if _, err := c.Models.Head(role, phase.Auxiliary); err == nil {
			return true
		}
	}
	return false
}
