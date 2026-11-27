package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The learning WRITE side, wired: every writer runs behind one reflect
// dispatcher over the completed turn, not a consumer group each. Every writer
// is gated on the same questions about that turn, and separate groups would
// mean several redelivery windows over it and several chances for one writer
// to be quietly unwired; the dispatcher owns the gates and reports which one
// closed, per worker, per turn.

// buildReflectionWorkers assembles this epoch's learning passes.
//
// Each is optional, and one that cannot be built is left out alone, but never
// silently: a worker that should have been built and was not is logged at
// warn, because a company learning nothing is the failure nobody notices.
func (e *Engine) buildReflectionWorkers(c *Company) []learning.Worker {
	cfg := c.Config.Learning
	if !cfg.On() {
		// Learning off is a supported configuration, not a warning.
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

	// Every worker resolves its model through this seam, so auxiliary spend is
	// charged and recorded beside a turn's, a worker added later included
	// (auxiliary.go).
	models := e.auxiliaryFor(c)
	if models == nil {
		// No models is a valid company (nomodels.go): the model-calling
		// workers wait for a provider. Said once, not warned per worker.
		log.Info("learning_models_absent",
			"detail", "the company configures no providers.llm, so the persist "+
				"decider, the skill synthesizer and refiner, the counterparty "+
				"profiler and the background compaction, clustering and "+
				"promotion passes are built by the apply that adds one")
	}

	var workers []learning.Worker
	if models != nil && cfg.Reflect.Enabled.Or(true) && cfg.Reflect.PersistDecider.Or(true) {
		decider, err := learning.NewPersistDecider(models,
			learning.NewDiary(db, learning.WithEmbed(e.embedText)),
			learning.PersistOptions{})
		if err != nil {
			log.Warn("persist_decider_unavailable", "error", err,
				"detail", "turns will not be classified and nothing will be "+
					"written to the diary")
		} else {
			workers = append(workers, decider)
		}
	}

	// The episodist has no toggle: the lifecycle worker, skill synthesis and
	// recall all presuppose its rows, so a company that wants no episodic
	// memory turns learning off entirely.
	//
	// Its embedder is read at call time ([Engine.embedText]), not captured: a
	// captured one would be current only by the order of two calls in the apply.
	episodist, err := learning.NewEpisodist(learning.NewEpisodes(db),
		learning.EpisodistOptions{Embed: e.embedText})
	if err != nil {
		log.Warn("episodist_unavailable", "error", err,
			"detail", "no episode will be recorded, so recall and skill "+
				"synthesis have nothing to work from")
	} else {
		workers = append(workers, episodist)
	}

	// The use stamp has no toggle either: without it a synthesized skill's
	// last-used stamp stands still while the prefetch serves it every turn,
	// and the curator ages the catalogue out.
	if use := learning.NewSkillUse(learning.NewSkills(db), nil); use != nil {
		workers = append(workers, use)
	}

	// The synthesizer is what writes synthesized_skills; use_skill, the
	// executor's catalogue, refine_skill and the curator only read it.
	if models != nil && cfg.SkillSynthesis.Enabled.Or(true) {
		synth, err := learning.NewSynthesizer(models, learning.NewSkills(db),
			synthesizerOptions(cfg.SkillSynthesis))
		if err != nil {
			log.Warn("skill_synthesizer_unavailable", "error", err,
				"detail", "no skill will ever be drafted, so "+builtin.UseSkillTool+
					" and the skill catalogue stay empty")
		} else {
			workers = append(workers, synth)
		}
	}

	// The refiner: the automatic half of refine_skill, and the reader of
	// `auto_refine_on_success` and `auto_refine_on_failure`.
	if models != nil && cfg.SkillRefinement.Refines() {
		switch {
		case !cfg.SkillRefinement.OnSuccess() && !cfg.SkillRefinement.OnFailure():
			// Both halves off is refinement off: a worker would cost a Skip
			// per turn with a reason that reads like a bug.
			log.Info("skill_refiner_idle",
				"detail", "auto_refine_on_success and auto_refine_on_failure "+
					"are both false, so no turn's outcome is refined")
		default:
			refiner, err := learning.NewRefiner(models, learning.NewSkills(db),
				refinerOptions(cfg.SkillRefinement))
			if err != nil {
				log.Warn("skill_refiner_unavailable", "error", err,
					"detail", "no skill will be refined after a turn; only the "+
						builtin.RefineSkillTool+" tool will ever change one")
			} else {
				workers = append(workers, refiner)
			}
		}
	}

	if models != nil && cfg.Counterparty.Enabled.Or(true) {
		profiler, err := learning.NewProfiler(models, learning.NewCounterparties(db),
			learning.ProfilerOptions{})
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

// refinerOptions carries the company's refinement knobs to the worker: a
// named function rather than an inline literal, so a test can hold that no
// knob is silently dropped.
//
// The toggles are resolved to bools and passed by address: a config Toggle's
// zero is UNSET, and a nil pointer would apply the worker's own default
// instead of the company's answer.
func refinerOptions(cfg config.SkillRefinement) learning.RefinerOptions {
	onSuccess, onFailure := cfg.OnSuccess(), cfg.OnFailure()
	return learning.RefinerOptions{
		OnSuccess:    &onSuccess,
		OnFailure:    &onFailure,
		MaxBodyChars: cfg.MaxBodyChars,
		KeepVersions: cfg.MaxVersionsKept,
	}
}

// prepareReflection readies the reflect dispatcher for an apply of c and
// returns what points it at c, which the apply calls later ([Engine.Apply]).
// The dispatcher is built once and outlives every apply ([learning.Reflector]):
// at boot, or here on a node whose first company c is, pointing at c already,
// so the returned install does nothing. A failure to build fails the apply as
// it fails a boot: a company without the write side learns nothing while
// nothing says so, and the retry builds it again.
//
// On a running dispatcher it builds c's workers, swaps nothing and never fails:
// the apply installs them once nothing can refuse the revision and its
// credential bundles are restored ([Company.restoreBundles]). Swapped earlier,
// a pass could run signed out, or on a revision a later stage refused.
func (e *Engine) prepareReflection(ctx context.Context, c *Company) (install func(), err error) {
	reflector := e.reflector.Load()
	if reflector == nil {
		if err := e.attachReflection(ctx, c); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	workers, budget := e.buildReflectionWorkers(c), e.auxGate(c, "reflection")
	return func() {
		if err := reflector.Reconfigure(c.Org, workers, budget); err != nil {
			// The previous epoch's workers keep serving: a stale org beats
			// not reflecting. An error, not a warn: this is a wiring bug,
			// not the operator's config.
			log.Error("reflection_reconfigure_failed", "error", err)
			return
		}
		if len(workers) == 0 {
			log.Info("learning_write_side_idle",
				"detail", "no learning worker is wired for this revision; "+
					"nothing will be written and every prefetch block stays empty")
		}
	}, nil
}

// startReflection builds the dispatcher for the company this node booted
// with, once, before the first apply: it lives as long as the process and an
// apply only swaps what runs behind it. A node booted with no company builds
// it on its first apply instead ([Engine.prepareReflection]).
func (e *Engine) startReflection(ctx context.Context) error {
	return e.attachReflection(ctx, e.Company())
}

// attachReflection builds the dispatcher for c. It subscribes nothing: a
// turn's reflection arrives on the seat's own subject, which the node holding
// the seat attaches ([Engine.attachSeatReflection]), so it runs where the
// seat's memory is held and carried. A fleet-wide group would run most of
// them where nothing of the seat is, writing memory nobody reads.
func (e *Engine) attachReflection(_ context.Context, c *Company) error {
	if c == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	// THE NODE'S LEDGER, flushed for a turn before its pass's sentinel: the
	// pass runs here, so what its workers' calls cost is in this ledger.
	reflector, err := learning.NewReflector(c.Org, e.backends.Queue,
		e.buildReflectionWorkers(c), e.auxGate(c, "reflection"), e.auxSpend)
	if err != nil {
		return fmt.Errorf("engine: build the reflect dispatcher: %w", err)
	}
	e.reflector.Store(reflector)
	return nil
}

// attachSeatReflection attaches a seat's reflection subject on the node that
// has just acquired it, as part of the seat's preparation, so a refused attach
// refuses the seat.
//
// After the seat's memory is hydrated: a reflection reads what the seat
// knows, and the wakes retained on the subject are taken as soon as this
// returns. Keyed by the seat's ID, as its inbox is: a durable name outlives
// every record of the seat, and every node derives the id alike from the
// immutable handle.
func (e *Engine) attachSeatReflection(ctx context.Context, seat placement.Seat) error {
	subject, group := topics.AgentReflect(seat.ID), topics.AgentReflectGroup(seat.ID)
	if subject == "" || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	if err := e.backends.Queue.Subscribe(ctx, subject, group, e.reflectSeatTurn); err != nil {
		return fmt.Errorf("attaching the seat's reflection subject: %w", err)
	}
	return nil
}

// detachSeatReflection lets a released seat's reflection subject go, BEFORE the
// release's last memory flush. Detach does not wait for a reflection already
// running ([queue.EventQueue.Detach] — the fenced release must not), so one
// that finishes after the flush writes rows this node no longer carries, lost
// to the seat's next holder as whatever the flush does not reach is
// ([memoryFlushTimeout]). A seat this node never attached detaches to nothing.
func (e *Engine) detachSeatReflection(ctx context.Context, seat placement.Seat) {
	subject, group := topics.AgentReflect(seat.ID), topics.AgentReflectGroup(seat.ID)
	if subject == "" || e.backends == nil || e.backends.Queue == nil {
		return
	}
	if _, err := e.backends.Queue.Detach(ctx, subject, group); err != nil {
		log.WarnContext(ctx, "seat_reflection_detach_failed", "seat", seat.Handle,
			"agent_id", seat.ID.String(), "error", err)
	}
}

// reflectSeatTurn hands one wake to the dispatcher. Before the first company has
// built one there is nothing to reflect with, and no turn has run either, so a
// wake then is acknowledged and said.
func (e *Engine) reflectSeatTurn(ctx context.Context, ev *events.Event) queue.Result {
	reflector := e.reflector.Load()
	if reflector == nil {
		log.DebugContext(ctx, "reflection_without_dispatcher", "event", ev.ID.String())
		return queue.Ack()
	}
	return reflector.Handle(ctx, ev)
}

// ---- the fleet singletons --------------------------------------------- //

// learningDutyName is the singleton every background learning pass claims:
// BackgroundOptions takes one ClaimDuty, so all the passes run on one node,
// which learningDutyTTL is sized against. It is a coordination key two builds
// share in a rolling upgrade: renamed, a node on each name would hold "the"
// duty.
const learningDutyName = "learning"

// lifecycleOptions projects the episode-lifecycle config onto the worker's
// options, so the learning package imports nothing but the store.
func lifecycleOptions(c *config.EpisodeLifecycle, synthesis *config.SkillSynthesis) learning.Options {
	return learning.Options{
		CapAge:            time.Duration(synthesis.ClusterWindowHours) * time.Hour,
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

// startLearningBackground arms the loops of the passes no turn drives, once
// for the life of the process.
//
// Last, like the sandbox waiter and the retention sweep: every pass is a fleet
// singleton claimed under this node's incarnation, which exists only then.
//
// Armed whatever the company configures, and with no company at all: the
// loops are the process's and what they run is the revision's, so this hands
// them the boot company's passes and every apply hands them its own
// ([Engine.reconfigureLearningPasses]). A node booted unconfigured or without
// a model thus learns once a revision configures it.
func (e *Engine) startLearningBackground(ctx context.Context) {
	if e.backends.Store == nil {
		// Every pass uses this node's store, so no revision could turn
		// one on here.
		return
	}
	if !e.profile.RunsWorkers() {
		// This node runs no singleton duties: the duty gate would refuse
		// every tick, so arm nothing.
		return
	}
	e.learning = learning.NewBackground(learning.BackgroundOptions{
		Passes: e.learningPasses(ctx, e.Company()),
		// From the role the roster listed, not re-resolved by handle: a
		// second epoch read could answer about a seat an apply removed.
		AgentIDFor: e.seatAgentID,
		// Read fresh through the epoch: a captured roster would tend
		// removed seats and miss added ones. Roles, because clustering
		// needs a seat's model chain and agent id (learning.Seats).
		Seats:     e.seatRoles,
		Publish:   e.publishLearning,
		ClaimDuty: e.workerDuty(learningDutyName, learningDutyTTL),
	})
	// Detached like the node's other loops, so SIGTERM does not end these
	// alone; stopLearning does.
	e.learning.Start(context.WithoutCancel(ctx))
}

// reconfigureLearningPasses hands the background loops the passes c
// configures; the loops keep running and keep their clocks
// ([learning.Background.Reconfigure]).
//
// After the epoch swap, unlike the reflect workers a turn needs at once: the
// loops walk the current epoch's roster, so handed over earlier they would run
// this revision's models over the previous company's seats, and stay there if
// a later stage refused the revision.
func (e *Engine) reconfigureLearningPasses(ctx context.Context, c *Company) {
	if e.learning == nil {
		return
	}
	e.learning.Reconfigure(e.learningPasses(ctx, c))
}

// learningPasses builds the background passes one revision configures. No
// company, or one with learning off, gets none, and the loops tick without
// claiming anything.
func (e *Engine) learningPasses(ctx context.Context, c *Company) learning.BackgroundPasses {
	db := e.backends.Store
	if c == nil || db == nil || !c.Config.Learning.On() {
		return learning.BackgroundPasses{}
	}
	cfg := c.Config.Learning
	passes := learning.BackgroundPasses{
		Policy: learning.CuratorPolicy{
			StaleAfter:   days(cfg.SkillCurator.StaleAfterDays),
			ArchiveAfter: days(cfg.SkillCurator.ArchiveAfterDays),
		},
		CuratorInterval: hours(cfg.SkillCurator.IntervalHours),
		ClusterInterval: seconds(float64(cfg.SkillSynthesis.SchedulerIntervalSeconds)),
		// LifecycleInterval and PromotionInterval stay zero, which each
		// loop reads as its own default: no knob configures either.
	}
	if cfg.SkillCurator.Enabled.Or(true) {
		passes.Skills = learning.NewSkills(db)
	}
	if c.Models == nil {
		// The model-calling passes wait for a provider (nomodels.go); said
		// once, by learning_models_absent in [Engine.buildReflectionWorkers].
		return passes
	}
	// Compaction requires a summarizer: without one the pass could only
	// delete, the half least wanted unsupervised, so the rows stay raw.
	if summarize := e.auxSummarizer(c); summarize != nil {
		opts := lifecycleOptions(&cfg.EpisodeLifecycle, &cfg.SkillSynthesis)
		opts.Budget = e.auxGate(c, string(types.AuxEpisodeCompaction))
		passes.Lifecycle = learning.NewLifecycle(db, learning.NewSummarizer(summarize, e.episodeFit(c)), opts)
	} else {
		log.WarnContext(ctx, "episode_compaction_unavailable",
			"reason", "no auxiliary model is configured for any seat",
			"detail", "raw episodes accumulate and every recall scans all of "+
				"them; nothing is deleted")
	}
	passes.Cluster = e.clusteringPass(c)
	passes.Promoter = e.buildPromoter(c)
	return passes
}

// stopLearning ends the background passes, waiting for an in-flight one.
// Called from [Engine.Stop] before the backends close, since every pass uses
// the store; Start's context is detached, so this is the only thing that ends
// them. The field stays set: an apply still finishing reads it, and a
// Reconfigure after the loops stopped hands passes to nothing.
func (e *Engine) stopLearning() {
	if e.learning == nil {
		return
	}
	e.learning.Stop()
}

// synthesizerOptions carries the company's synthesis knobs to the worker: one
// mapping for the inline worker and the clustering pass, whose shared gates
// must not diverge (a raised `max_skills_per_agent` honoured by one path
// only), and a named function so a test can hold that no knob is dropped.
// Episodes is not set here: it is what distinguishes the clustering pass.
func synthesizerOptions(cfg config.SkillSynthesis) learning.SynthesizerOptions {
	return learning.SynthesizerOptions{
		MinToolCalls:              cfg.MinToolCalls,
		MaxSkillsPerAgent:         cfg.MaxSkillsPerAgent,
		DuplicateJaccardThreshold: cfg.DuplicateJaccardThreshold,
		ClusterMinSize:            cfg.ClusterMinSize,
		ClusterJaccardThreshold:   cfg.ClusterJaccardThreshold,
		EpisodeFetchLimit:         cfg.EpisodeFetchLimit,
		ClusterWindow:             hours(cfg.ClusterWindowHours),
	}
}

// clusteringPass builds the pass `skill_synthesis.scheduler_enabled` turns on,
// or nil when the company did not ask for it; a named function so a test can
// hold its knobs. Gated on both toggles: `scheduler_enabled` with skill
// synthesis off would draft skills for a company that said not to.
func (e *Engine) clusteringPass(c *Company) *learning.Synthesizer {
	db := e.backends.Store
	cfg := c.Config.Learning.SkillSynthesis
	if db == nil || !cfg.Enabled.Or(true) || !cfg.Clusters() {
		return nil
	}
	opts := synthesizerOptions(cfg)
	opts.Episodes = learning.NewEpisodes(db)
	opts.Budget = e.auxGate(c, string(types.AuxSkillClustering))
	built, err := learning.NewSynthesizer(e.auxiliaryFor(c), learning.NewSkills(db), opts)
	if err != nil {
		log.Warn("skill_clustering_unavailable", "error", err,
			"detail", "scheduler_enabled is on but no clustering pass could be "+
				"built, so a seat's repeated procedures are never distilled")
		return nil
	}
	return built
}

// learningDutyTTL is how long a background duty survives without a re-claim.
// Sized off the shortest cadence, since every loop claims through one helper
// and a TTL under a loop's interval hands the duty to a peer between ticks:
// lifecycle and clustering tick hourly, and three ticks is the retention
// sweep's and sandbox waiter's ratio, so one missed tick keeps the duty and a
// dead node's moves within hours. A lower `scheduler_interval_seconds` only
// makes clustering claim more often.
const learningDutyTTL = 3 * learning.LifecycleInterval

// seatRoles is the current epoch's agent seats, as the roles every background
// pass reads both of a seat's names off.
//
// The PLACED seats, resolved in the same epoch they were listed from: a seat
// the organization carries but placement does not (one no content record has
// filled yet) has no memory for a pass to tend.
func (e *Engine) seatRoles() []*org.Role {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	seats := company.Seats()
	out := make([]*org.Role, 0, len(seats))
	for _, s := range seats {
		if role := company.Org.AgentSeatByHandle(s.Handle); role != nil {
			out = append(out, role)
		}
	}
	return out
}

// seatRole resolves a seat handle against the CURRENT epoch.
//
// Read fresh, never captured: a caller holding the roster it started with
// would charge a seat's work to a model chain the company has replaced, and
// would answer nil for a seat an apply has just added.
func (e *Engine) seatRole(handle string) *org.Role {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	return company.Org.AgentSeatByHandle(handle)
}

// seatAgentID derives a resolved seat's agent id, or "" when there is none.
//
// Empty for a human seat, which has no agent id at all rather than a zero one,
// and for a node with no active company. The event still carries the handle and
// the role in that case, so a reader loses the promoted column rather than the
// attribution.
func (e *Engine) seatAgentID(seat *org.Role) string {
	company := e.Company()
	if company == nil || company.Org == nil {
		return ""
	}
	id, ok := company.Org.AgentIDFor(seat)
	if !ok {
		return ""
	}
	return id.String()
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

// auxSummarizer resolves each cluster's own seat to its auxiliary chain, per
// call, so each seat's memory is compacted by its own model rather than the
// whole company's by the first seat's. Nil when no seat has an auxiliary
// model, which disables compaction ([Engine.learningPasses] says why).
func (e *Engine) auxSummarizer(c *Company) learning.CompleteFunc {
	seam := e.auxiliaryFor(c)
	if seam == nil {
		return nil
	}
	if !anySeatHasAuxiliary(c) {
		return nil
	}
	return func(ctx context.Context, handle, system, user string) (string, error) {
		// By handle, the key the episodes are keyed on, which
		// [learning.CompleteFunc] passes and [org.Organization.Role]
		// resolves; never the display name.
		seat := c.Org.Role(handle)
		if seat == nil {
			return "", fmt.Errorf("engine: compaction for %q: this revision has "+
				"no seat with that handle", handle)
		}
		// A BACKGROUND PASS, on no turn: the lifecycle worker compacts a
		// seat's old episodes on its own schedule.
		member, err := seam.Auxiliary(seat, auxspend.Use{Stage: types.AuxStageBackground,
			Purpose: types.AuxEpisodeCompaction})
		if err != nil {
			return "", fmt.Errorf("engine: compaction for %q: %w", handle, err)
		}
		call, cancel := context.WithTimeout(ctx, learning.DefaultAuxTimeout)
		defer cancel()
		completion, err := member.Provider.Complete(call, llm.Request{
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: system},
				{Role: llm.RoleUser, Content: user},
			},
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

// episodeFit is the compaction pass's [learning.FitFunc]: the epoch's
// compactor, on the seat the cluster belongs to — named by the handle its
// episodes are keyed on, which is what [org.Organization.Role] resolves — with
// the rewrite instructions for the field it is asked about.
//
// Nil where the company has no models, which the summarizer reads as "carry
// every field whole" — the call is then larger, never wrong.
func (e *Engine) episodeFit(c *Company) learning.FitFunc {
	fitter := e.compactorFor(c)
	if fitter == nil {
		return nil
	}
	return func(ctx context.Context, handle string, field learning.EpisodeField, text string, budget int) (string, error) {
		seat := c.Org.Role(handle)
		if seat == nil {
			return "", fmt.Errorf("engine: condensing an episode for %q: this "+
				"revision has no seat with that handle", handle)
		}
		kind := compact.KindTask
		if field == learning.FieldOutcome {
			kind = compact.KindOutcome
		}
		// The background stage, as the summary it feeds is.
		res, err := fitter.For(seat, auxspend.Use{Stage: types.AuxStageBackground}).
			Fit(ctx, kind, text, budget)
		if err != nil {
			return "", err
		}
		return res.Text, nil
	}
}

// anySeatHasAuxiliary reports whether at least one seat can answer an
// auxiliary call. Checked once per revision, when its passes are built: per
// cluster, a company with none would log a failed resolution for every seat on
// every tick.
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
