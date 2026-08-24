package config

// Learning configures the agent-learning subsystem: what a seat remembers
// from a turn, what it distils into reusable skills, and what it eventually
// forgets.
//
// It needs a store and an embedding provider. With either missing the
// engine logs a disabled notice and skips registering the learning tools
// rather than failing — a company without embeddings still runs, it just
// does not recall.
//
// Every block here is a master switch plus a handful of thresholds. The
// thresholds are the interesting part: each one trades recall against cost,
// and a wrong one is not a crash but a slow drift (a synthesis threshold
// too low drafts a skill from every turn; a compaction age too aggressive
// deletes the fidelity the synthesizer needs).
type Learning struct {
	// Enabled is the master switch for the whole subsystem.
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Master switch for agent learning (default on)."`

	Episodic         Episodic         `yaml:"episodic,omitempty" json:"episodic"`
	Reflect          Reflect          `yaml:"reflect,omitempty" json:"reflect"`
	Counterparty     Counterparty     `yaml:"counterparty,omitempty" json:"counterparty"`
	SkillSynthesis   SkillSynthesis   `yaml:"skill_synthesis,omitempty" json:"skill_synthesis"`
	SkillRefinement  SkillRefinement  `yaml:"skill_refinement,omitempty" json:"skill_refinement"`
	SkillPromotion   SkillPromotion   `yaml:"skill_promotion,omitempty" json:"skill_promotion"`
	SkillCurator     SkillCurator     `yaml:"skill_curator,omitempty" json:"skill_curator"`
	PersonalMemory   PersonalMemory   `yaml:"personal_memory,omitempty" json:"personal_memory"`
	EpisodeLifecycle EpisodeLifecycle `yaml:"episode_lifecycle,omitempty" json:"episode_lifecycle"`
}

// DefaultLearning is the subsystem's shipped defaults.
func DefaultLearning() Learning {
	return Learning{
		Episodic:         Episodic{RetrievalLimit: 5},
		Reflect:          Reflect{BudgetTokens: 5000, SummarizeMaxTokens: 400},
		Counterparty:     Counterparty{BudgetTokens: 3000},
		SkillSynthesis:   DefaultSkillSynthesis(),
		SkillRefinement:  DefaultSkillRefinement(),
		SkillPromotion:   SkillPromotion{MinSiblingCount: 3, JaccardThreshold: 0.6, BudgetTokens: 4000},
		SkillCurator:     SkillCurator{IntervalHours: 24, StaleAfterDays: 30, ArchiveAfterDays: 90},
		PersonalMemory:   PersonalMemory{MaxRefreshesPerTurn: 3},
		EpisodeLifecycle: DefaultEpisodeLifecycle(),
	}
}

// On reports whether the subsystem runs, applying the true default.
func (l *Learning) On() bool { return l.Enabled.Or(true) }

func (l *Learning) validate(path string) error {
	var p problems
	p.wrap(l.Episodic.validate(at(path, "episodic")))
	p.wrap(l.Reflect.validate(at(path, "reflect")))
	p.wrap(l.Counterparty.validate(at(path, "counterparty")))
	p.wrap(l.SkillSynthesis.validate(at(path, "skill_synthesis")))
	p.wrap(l.SkillRefinement.validate(at(path, "skill_refinement")))
	p.wrap(l.SkillPromotion.validate(at(path, "skill_promotion")))
	p.wrap(l.SkillCurator.validate(at(path, "skill_curator")))
	p.wrap(l.PersonalMemory.validate(at(path, "personal_memory")))
	p.wrap(l.EpisodeLifecycle.validate(at(path, "episode_lifecycle")))
	return p.err()
}

// maxRetrievalLimit bounds one episode query's result set. Beyond this the
// hits stop being recall and start being a transcript that crowds the task
// out of the prompt.
const maxRetrievalLimit = 20

// Episodic is episodic-memory read settings.
type Episodic struct {
	// RetrievalLimit is the default number of hits an episode query
	// returns.
	RetrievalLimit int `yaml:"retrieval_limit,omitempty" json:"retrieval_limit,omitempty" js:"min=1;max=20" desc:"Episode-query hits returned (1..20)."`
}

func (e *Episodic) validate(path string) error {
	var p problems
	if e.RetrievalLimit < 1 || e.RetrievalLimit > maxRetrievalLimit {
		p.add(at(path, "retrieval_limit"), ErrOutOfRange,
			"must be 1..%d, got %d", maxRetrievalLimit, e.RetrievalLimit)
	}
	return p.err()
}

// Reflect is the post-turn reflection pass: after every terminal turn it
// decides whether any durable fact from that turn should be persisted.
type Reflect struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Run reflection after each turn (default on)."`

	// PersistDecider runs the deterministic persist decision after each
	// turn.
	PersistDecider Toggle `yaml:"persist_decider,omitempty" json:"persist_decider,omitzero" desc:"Run the persist decision after each turn (default on)."`

	// BudgetTokens softly caps the reflection call. 0 disables the cap.
	BudgetTokens int `yaml:"budget_tokens,omitempty" json:"budget_tokens,omitempty" js:"min=0" desc:"Soft token cap on the reflection call; 0 = uncapped."`

	// SummarizeEpisodes passes raw episode hits through the seat's cheap
	// auxiliary model for a compact per-hit summary. Falls back to raw
	// output when no auxiliary model is configured.
	SummarizeEpisodes Toggle `yaml:"summarize_episodes,omitempty" json:"summarize_episodes,omitzero" desc:"Summarise episode hits through the auxiliary model (default on)."`

	// SummarizeMaxTokens softly caps that summary.
	SummarizeMaxTokens int `yaml:"summarize_max_tokens,omitempty" json:"summarize_max_tokens,omitempty" js:"min=0" desc:"Soft cap on a per-hit summary."`
}

// Runs reports whether reflection runs, applying the true default.
func (r *Reflect) Runs() bool { return r.Enabled.Or(true) }

// Decides reports whether the persist decision runs, applying the true
// default.
func (r *Reflect) Decides() bool { return r.PersistDecider.Or(true) }

// Summarizes reports whether episode hits are summarised, applying the true
// default.
func (r *Reflect) Summarizes() bool { return r.SummarizeEpisodes.Or(true) }

func (r *Reflect) validate(path string) error {
	var p problems
	p.wrap(nonNegative(path, "budget_tokens", r.BudgetTokens))
	p.wrap(nonNegative(path, "summarize_max_tokens", r.SummarizeMaxTokens))
	return p.err()
}

// Counterparty is per-counterparty profiling: for each turn triggered by an
// identifiable sender, update a profile of that person from this seat's
// point of view.
type Counterparty struct {
	Enabled      Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Observe counterparties (default on)."`
	BudgetTokens int    `yaml:"budget_tokens,omitempty" json:"budget_tokens,omitempty" js:"min=0" desc:"Soft token cap on the profiler call."`
}

// Observes reports whether profiling runs, applying the true default.
func (c *Counterparty) Observes() bool { return c.Enabled.Or(true) }

func (c *Counterparty) validate(path string) error {
	return nonNegative(path, "budget_tokens", c.BudgetTokens)
}

// SkillSynthesis drafts reusable skills from what turns actually did.
//
// Two triggers: single-turn (inline, when a turn used at least
// MinToolCalls tools and finished done) and clustered (a scheduled pass
// that groups recent successful turns by tool-sequence similarity and
// drafts for recurring patterns).
type SkillSynthesis struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Draft skills from turns (default on)."`

	// MinToolCalls is the single-turn trigger. Turns below it get no
	// inline draft, but still feed clustered synthesis.
	MinToolCalls int `yaml:"min_tool_calls,omitempty" json:"min_tool_calls,omitempty" js:"min=0" desc:"Tool calls a turn needs before an inline draft."`

	BudgetTokens int `yaml:"budget_tokens,omitempty" json:"budget_tokens,omitempty" js:"min=0" desc:"Soft token cap per draft attempt."`

	// MaxSkillsPerAgent is a hard cap; once reached the synthesizer
	// no-ops.
	MaxSkillsPerAgent int `yaml:"max_skills_per_agent,omitempty" json:"max_skills_per_agent,omitempty" js:"min=0" desc:"Hard per-seat skill cap."`

	// DuplicateJaccardThreshold rejects a draft whose tool sequence is
	// this similar to one the seat already has.
	DuplicateJaccardThreshold float64 `yaml:"duplicate_jaccard_threshold,omitempty" json:"duplicate_jaccard_threshold,omitempty" js:"min=0;max=1" desc:"Reject a draft this similar to an existing skill."`

	// SchedulerEnabled is opt-in by default, so a local run and CI do not
	// spin up a background pass nobody asked for.
	SchedulerEnabled Toggle `yaml:"scheduler_enabled,omitempty" json:"scheduler_enabled,omitzero" desc:"Background clustering pass (default off)."`

	SchedulerIntervalSeconds int     `yaml:"scheduler_interval_seconds,omitempty" json:"scheduler_interval_seconds,omitempty" js:"min=0" desc:"How often the clustering pass runs."`
	ClusterWindowHours       int     `yaml:"cluster_window_hours,omitempty" json:"cluster_window_hours,omitempty" js:"min=0" desc:"How far back the clustering pass looks."`
	ClusterMinSize           int     `yaml:"cluster_min_size,omitempty" json:"cluster_min_size,omitempty" js:"min=0" desc:"Turns a cluster needs before it earns a skill."`
	ClusterJaccardThreshold  float64 `yaml:"cluster_jaccard_threshold,omitempty" json:"cluster_jaccard_threshold,omitempty" js:"min=0;max=1" desc:"Similarity that pools two turns into one cluster."`
	EpisodeFetchLimit        int     `yaml:"episode_fetch_limit,omitempty" json:"episode_fetch_limit,omitempty" js:"min=0" desc:"Episodes pulled into one clustering pass."`
}

// DefaultSkillSynthesis is the shipped defaults.
func DefaultSkillSynthesis() SkillSynthesis {
	return SkillSynthesis{
		MinToolCalls:              5,
		BudgetTokens:              4000,
		MaxSkillsPerAgent:         50,
		DuplicateJaccardThreshold: 0.7,
		SchedulerIntervalSeconds:  3600,
		// A week: long enough that a weekly ritual clusters with itself,
		// short enough that a pattern the company has moved on from stops
		// being drafted.
		ClusterWindowHours:      168,
		ClusterMinSize:          3,
		ClusterJaccardThreshold: 0.6,
		EpisodeFetchLimit:       200,
	}
}

// Drafts reports whether synthesis runs, applying the true default.
func (s *SkillSynthesis) Drafts() bool { return s.Enabled.Or(true) }

// Clusters reports whether the background pass runs, applying the FALSE
// default — it is opt-in.
func (s *SkillSynthesis) Clusters() bool { return s.SchedulerEnabled.Or(false) }

func (s *SkillSynthesis) validate(path string) error {
	var p problems
	p.wrap(positive(path, "min_tool_calls", s.MinToolCalls))
	p.wrap(nonNegative(path, "budget_tokens", s.BudgetTokens))
	p.wrap(positive(path, "max_skills_per_agent", s.MaxSkillsPerAgent))
	p.wrap(fraction(path, "duplicate_jaccard_threshold", s.DuplicateJaccardThreshold))
	p.wrap(positive(path, "scheduler_interval_seconds", s.SchedulerIntervalSeconds))
	p.wrap(positive(path, "cluster_window_hours", s.ClusterWindowHours))
	p.wrap(positive(path, "cluster_min_size", s.ClusterMinSize))
	p.wrap(fraction(path, "cluster_jaccard_threshold", s.ClusterJaccardThreshold))
	p.wrap(positive(path, "episode_fetch_limit", s.EpisodeFetchLimit))
	return p.err()
}

// SkillRefinement appends what practice taught to a skill that was used:
// an observation on success, a counter-example on failure. Version history
// is archived automatically, so a bad refinement rolls back.
type SkillRefinement struct {
	Enabled             Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Refine skills after use (default on)."`
	AutoRefineOnSuccess Toggle `yaml:"auto_refine_on_success,omitempty" json:"auto_refine_on_success,omitzero" desc:"Append an observation when a skill's turn succeeded (default on)."`
	AutoRefineOnFailure Toggle `yaml:"auto_refine_on_failure,omitempty" json:"auto_refine_on_failure,omitzero" desc:"Append a counter-example when it failed (default on)."`
	BudgetTokens        int    `yaml:"budget_tokens,omitempty" json:"budget_tokens,omitempty" js:"min=0" desc:"Soft token cap per refinement."`
	MaxBodyChars        int    `yaml:"max_body_chars,omitempty" json:"max_body_chars,omitempty" js:"min=0" desc:"Ceiling on a refined skill's body."`
	MaxVersionsKept     int    `yaml:"max_versions_kept,omitempty" json:"max_versions_kept,omitempty" js:"min=0" desc:"Archived versions kept for rollback."`
}

// OnSuccess reports whether a successful use appends an observation,
// applying the true default.
func (s *SkillRefinement) OnSuccess() bool { return s.AutoRefineOnSuccess.Or(true) }

// OnFailure reports whether a failed use appends a counter-example,
// applying the true default.
func (s *SkillRefinement) OnFailure() bool { return s.AutoRefineOnFailure.Or(true) }

// DefaultSkillRefinement is the shipped defaults.
func DefaultSkillRefinement() SkillRefinement {
	return SkillRefinement{BudgetTokens: 3000, MaxBodyChars: 20000, MaxVersionsKept: 10}
}

// Refines reports whether refinement runs, applying the true default.
func (s *SkillRefinement) Refines() bool { return s.Enabled.Or(true) }

func (s *SkillRefinement) validate(path string) error {
	var p problems
	p.wrap(nonNegative(path, "budget_tokens", s.BudgetTokens))
	p.wrap(positive(path, "max_body_chars", s.MaxBodyChars))
	p.wrap(positive(path, "max_versions_kept", s.MaxVersionsKept))
	return p.err()
}

// SkillPromotion lifts a pattern several siblings independently learned to
// a unit-scope skill, so the next seat in that unit starts with it.
type SkillPromotion struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Promote sibling skills to unit scope (default on)."`

	// MinSiblingCount is how many distinct seats must share a cluster
	// before it is worth promoting. Below three it is a coincidence.
	MinSiblingCount  int     `yaml:"min_sibling_count,omitempty" json:"min_sibling_count,omitempty" js:"min=0" desc:"Distinct seats a cluster needs before promotion."`
	JaccardThreshold float64 `yaml:"jaccard_threshold,omitempty" json:"jaccard_threshold,omitempty" js:"min=0;max=1" desc:"Similarity that pools two seats' skills."`
	BudgetTokens     int     `yaml:"budget_tokens,omitempty" json:"budget_tokens,omitempty" js:"min=0" desc:"Soft token cap per promotion draft."`
}

// Promotes reports whether promotion runs, applying the true default.
func (s *SkillPromotion) Promotes() bool { return s.Enabled.Or(true) }

func (s *SkillPromotion) validate(path string) error {
	var p problems
	p.wrap(positive(path, "min_sibling_count", s.MinSiblingCount))
	p.wrap(fraction(path, "jaccard_threshold", s.JaccardThreshold))
	p.wrap(nonNegative(path, "budget_tokens", s.BudgetTokens))
	return p.err()
}

// SkillCurator ages unused skills out: active, then stale, then archived.
// Pinned skills are exempt, and an archived skill is still in the table —
// restoring one is a state change, not a re-synthesis.
type SkillCurator struct {
	Enabled          Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Age unused skills out (default on)."`
	IntervalHours    int    `yaml:"interval_hours,omitempty" json:"interval_hours,omitempty" js:"min=0" desc:"How often the curator walks the skill table."`
	StaleAfterDays   int    `yaml:"stale_after_days,omitempty" json:"stale_after_days,omitempty" js:"min=0" desc:"Unused days before a skill goes stale."`
	ArchiveAfterDays int    `yaml:"archive_after_days,omitempty" json:"archive_after_days,omitempty" js:"min=0" desc:"Unused days before a skill is archived."`
}

// Curates reports whether the curator runs, applying the true default.
func (s *SkillCurator) Curates() bool { return s.Enabled.Or(true) }

func (s *SkillCurator) validate(path string) error {
	var p problems
	p.wrap(positive(path, "interval_hours", s.IntervalHours))
	p.wrap(positive(path, "stale_after_days", s.StaleAfterDays))
	p.wrap(positive(path, "archive_after_days", s.ArchiveAfterDays))
	if s.ArchiveAfterDays < s.StaleAfterDays {
		// Archiving before staling collapses the two states into one, so
		// the intermediate state an operator configured never happens.
		p.add(at(path, "archive_after_days"), ErrOutOfRange,
			"must be at least stale_after_days (%d), got %d",
			s.StaleAfterDays, s.ArchiveAfterDays)
	}
	return p.err()
}

// PersonalMemory is the read side of a seat's own memory: the Plan-phase
// digest and the mid-turn refresh tool. Writes are governed by the reflect
// block, not by this.
type PersonalMemory struct {
	// MaxRefreshesPerTurn caps distinct mid-turn refreshes. Repeats with
	// the same hint are served from an idempotency cache and do not count.
	// A call at the cap returns an error rather than silently no-opping,
	// so the planner notices and stops trying.
	MaxRefreshesPerTurn int `yaml:"max_refreshes_per_turn,omitempty" json:"max_refreshes_per_turn,omitempty" js:"min=0" desc:"Distinct memory refreshes one turn may make."`
}

func (m *PersonalMemory) validate(path string) error {
	return positive(path, "max_refreshes_per_turn", m.MaxRefreshesPerTurn)
}

// EpisodeLifecycle drains the episode table: it drops low-value rows and
// compacts clusters of similar routine turns into single summaries.
//
// The trigger is write-side — a write publishes a compaction request once a
// seat's raw count crosses the threshold — so there is no daily cron and no
// latency on the read path.
type EpisodeLifecycle struct {
	// MaxRawEpisodesPerAgent is the count that fires a pass. The worker
	// compacts the oldest batch, bringing the count down toward but not
	// necessarily under the threshold — it drifts down as routine work
	// rotates through.
	MaxRawEpisodesPerAgent int `yaml:"max_raw_episodes_per_agent,omitempty" json:"max_raw_episodes_per_agent,omitempty" js:"min=0" desc:"Raw episodes per seat that trigger a lifecycle pass."`

	// WriteCheckEveryN is how often the write path actually counts rather
	// than incrementing a counter. Higher is cheaper and blunter.
	WriteCheckEveryN int `yaml:"write_check_every_n,omitempty" json:"write_check_every_n,omitempty" js:"min=0" desc:"Writes between real threshold checks."`

	// NonTerminalMaxAgeDays drops mid-state rows. They never feed skill
	// synthesis (its terminal-outcome gate filters them out), so the only
	// consumer is recall, where they are noise.
	NonTerminalMaxAgeDays int `yaml:"non_terminal_max_age_days,omitempty" json:"non_terminal_max_age_days,omitempty" js:"min=0" desc:"Age at which mid-state episodes are dropped."`

	// ConsolidatedGraceDays drops raw rows a skill already consolidated.
	// The grace is an audit window: it is the only chance to spot a bad
	// consolidation before its source disappears.
	ConsolidatedGraceDays int `yaml:"consolidated_grace_days,omitempty" json:"consolidated_grace_days,omitempty" js:"min=0" desc:"Audit grace before a consolidated raw row is dropped."`

	// CompactionMinAgeDays keeps recent rows raw so the synthesizer and
	// recall still see them at full fidelity inside the active learning
	// window.
	CompactionMinAgeDays int `yaml:"compaction_min_age_days,omitempty" json:"compaction_min_age_days,omitempty" js:"min=0" desc:"Only compact rows older than this."`

	// CompactionMinClusterSize leaves singletons and pairs alone: if a
	// pattern occurred three times it is worth summarising, and below that
	// the per-turn detail is worth more than the aggregate.
	CompactionMinClusterSize int `yaml:"compaction_min_cluster_size,omitempty" json:"compaction_min_cluster_size,omitempty" js:"min=0" desc:"Smallest cluster worth compacting."`

	CompactionJaccardThreshold float64 `yaml:"compaction_jaccard_threshold,omitempty" json:"compaction_jaccard_threshold,omitempty" js:"min=0;max=1" desc:"Similarity that pools two episodes into one cluster."`
	CompactionBatchSize        int     `yaml:"compaction_batch_size,omitempty" json:"compaction_batch_size,omitempty" js:"min=0" desc:"Raw episodes pulled into one pass."`
	CompactionBudgetTokens     int     `yaml:"compaction_budget_tokens,omitempty" json:"compaction_budget_tokens,omitempty" js:"min=0" desc:"Soft token cap on summarising one cluster."`

	// ExemplarCount is how many originals stay raw after a cluster is
	// compacted, so an operator can drill into typical examples without
	// losing fidelity entirely.
	ExemplarCount int `yaml:"exemplar_count,omitempty" json:"exemplar_count,omitempty" js:"min=0" desc:"Originals kept raw after a cluster is compacted."`

	// CompactedMaxAgeDays is an optional very-long-tail eviction of the
	// summaries themselves. 0 keeps them indefinitely, which is the
	// default: they are small, and losing them loses long-horizon signal.
	CompactedMaxAgeDays int `yaml:"compacted_max_age_days,omitempty" json:"compacted_max_age_days,omitempty" js:"min=0" desc:"Age at which compacted summaries are dropped; 0 = never."`
}

// DefaultEpisodeLifecycle is the shipped defaults.
func DefaultEpisodeLifecycle() EpisodeLifecycle {
	return EpisodeLifecycle{
		MaxRawEpisodesPerAgent:   500,
		WriteCheckEveryN:         10,
		NonTerminalMaxAgeDays:    14,
		ConsolidatedGraceDays:    30,
		CompactionMinAgeDays:     30,
		CompactionMinClusterSize: 3,
		// Matched to the clustering scheduler's threshold so the two
		// consolidation paths pool the same turns; two different answers
		// to "are these the same work" would compact what synthesis had
		// treated as distinct.
		CompactionJaccardThreshold: 0.6,
		CompactionBatchSize:        200,
		CompactionBudgetTokens:     4000,
		ExemplarCount:              2,
	}
}

func (e *EpisodeLifecycle) validate(path string) error {
	var p problems
	p.wrap(positive(path, "max_raw_episodes_per_agent", e.MaxRawEpisodesPerAgent))
	p.wrap(positive(path, "write_check_every_n", e.WriteCheckEveryN))
	p.wrap(positive(path, "non_terminal_max_age_days", e.NonTerminalMaxAgeDays))
	p.wrap(positive(path, "consolidated_grace_days", e.ConsolidatedGraceDays))
	p.wrap(positive(path, "compaction_min_age_days", e.CompactionMinAgeDays))
	p.wrap(positive(path, "compaction_min_cluster_size", e.CompactionMinClusterSize))
	p.wrap(fraction(path, "compaction_jaccard_threshold", e.CompactionJaccardThreshold))
	p.wrap(positive(path, "compaction_batch_size", e.CompactionBatchSize))
	p.wrap(nonNegative(path, "compaction_budget_tokens", e.CompactionBudgetTokens))
	p.wrap(nonNegative(path, "exemplar_count", e.ExemplarCount))
	p.wrap(nonNegative(path, "compacted_max_age_days", e.CompactedMaxAgeDays))

	if e.ExemplarCount >= e.CompactionMinClusterSize && e.CompactionMinClusterSize > 0 {
		// Keeping as many exemplars as the cluster had makes compaction a
		// pure cost: every row survives AND a summary is written, so the
		// table grows on the pass that exists to drain it.
		p.add(at(path, "exemplar_count"), ErrConflict,
			"%d exemplars out of a minimum cluster of %d keeps every row, so "+
				"compaction only adds a summary. Keep it below "+
				"compaction_min_cluster_size", e.ExemplarCount, e.CompactionMinClusterSize)
	}
	return p.err()
}

// ---- shared numeric checks ------------------------------------------- //
//
// Every learning knob is one of three shapes: a count that must be at least
// one, a budget that may be zero to mean uncapped, or a similarity
// threshold in (0, 1]. Writing the check once per shape keeps the message
// consistent across forty fields — an operator reading two of these errors
// should not have to work out whether they mean different things.

func positive(path, field string, value int) error {
	if value < 1 {
		return fault(at(path, field), ErrOutOfRange, "must be at least 1, got %d", value)
	}
	return nil
}

func nonNegative(path, field string, value int) error {
	if value < 0 {
		return fault(at(path, field), ErrOutOfRange, "must not be negative, got %d", value)
	}
	return nil
}

func fraction(path, field string, value float64) error {
	if value <= 0 || value > 1 {
		return fault(at(path, field), ErrOutOfRange,
			"must be a similarity in (0, 1], got %v", value)
	}
	return nil
}
