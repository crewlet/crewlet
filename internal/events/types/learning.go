package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// The agent-learning lifecycle. Each fires from a learning worker after one
// outcome, inheriting the trace context of the turn or scheduled tick that
// caused it, so a trace-grouped view shows them under the same card as their
// parent rather than as free-floating background work.

func init() {
	events.Register[EpisodeWritten]()
	events.Register[PersistDeciderCompleted]()
	events.Register[SkillUsed]()
	events.Register[PrefetchSummary]()
	events.Register[CounterpartyProfileUpdated]()
	events.Register[SkillSynthesized]()
	events.Register[SkillRefined]()
	events.Register[SkillPromoted]()
	events.Register[SkillStaled]()
	events.Register[SkillArchived]()
	events.Register[SkillRevived]()
	events.Register[SkillTelemetryWriteFailed]()
	events.Register[CompactionRequested]()
	events.Register[CompactionCompleted]()
	events.Register[ReflectionCompleted]()
}

// MemoryScope is whose memory a persisted reflection belongs to.
type MemoryScope string

// The two memories a reflection can land in: the writing agent's own diary, or
// the unit's shared memory, which its whole team reads back.
const (
	MemoryScopeAgent MemoryScope = "agent"
	MemoryScopeUnit  MemoryScope = "unit"
)

// PersistClassification is the decider's tier for one reflection.
type PersistClassification string

// The four tiers the decider can reach. PersistLong is a durable memory;
// PersistShort is one with a TTL; PersistDoc is a standing rule that belongs in
// a document rather than in memory; PersistNOOP is the common case — the
// decider is deliberately conservative.
const (
	PersistLong  PersistClassification = "LONG"
	PersistShort PersistClassification = "SHORT"
	PersistDoc   PersistClassification = "DOC"
	PersistNOOP  PersistClassification = "NOOP"
)

// SkillSourceKind distinguishes a skill the agent synthesized for itself from
// one an operator curated into the registry. The counters differ: a synthesized
// skill's row tracks its own use, while the registry is in-memory and the event
// is the only record a registry skill was ever loaded.
type SkillSourceKind string

// The two places a loaded skill can have come from.
const (
	SkillSourceSynthesized SkillSourceKind = "synthesized"
	SkillSourceRegistry    SkillSourceKind = "registry"
)

// SynthesisTrigger says which path drafted a skill: one turn that warranted it,
// or the scheduled pass that consolidated a cluster of them.
type SynthesisTrigger string

// The two synthesis paths. ClusterSize on SkillSynthesized is 1 for the first
// and N for the second, which is how a consolidation is told from a one-off.
const (
	SynthesisSingleTurn SynthesisTrigger = "single_turn"
	SynthesisClustered  SynthesisTrigger = "clustered"
)

// SkillState is a synthesized skill's lifecycle state. Archived rows stay in
// the table — loading refuses them and the prefetch hides them — so an operator
// can restore one by hand.
type SkillState string

// The three states, in the order the curator walks them: active until idle past
// the stale threshold, stale until idle past the archive threshold. Use moves a
// skill back to active from either.
const (
	SkillStateActive   SkillState = "active"
	SkillStateStale    SkillState = "stale"
	SkillStateArchived SkillState = "archived"
)

// CompactionSkipReason says why a lifecycle pass short-circuited. Empty means
// it actually ran.
type CompactionSkipReason string

// Why a lifecycle pass did nothing. The first two are missing wiring — nothing
// to compact into, or no auxiliary model to compact with — and the third is the
// worker's own per-agent dedup refusing an overlapping pass.
const (
	CompactionNoDatabase     CompactionSkipReason = "no_database"
	CompactionNoAuxProvider  CompactionSkipReason = "no_aux_provider"
	CompactionAlreadyRunning CompactionSkipReason = "already_running"
)

// EpisodeWritten fires after one episode row lands in the episode store,
// synchronously at turn end from inside the parent turn's span. Skipped
// entirely when no episode store is wired.
type EpisodeWritten struct {
	Agent         string `json:"agent_id"`
	AgentHandle   string `json:"agent_handle"`
	RoleName      string `json:"role"`
	TurnID        string `json:"turn_id"`
	ReviewOutcome string `json:"review_outcome"`
	DurationMS    int    `json:"duration_ms"`
	ToolCount     int    `json:"tool_count"`
}

// EventType is the "episode_written" wire type.
func (EpisodeWritten) EventType() string { return "episode_written" }

// Role is the seat whose episode was written.
func (e EpisodeWritten) Role() string { return e.RoleName }

// AgentID is the instance the episode row is keyed by.
func (e EpisodeWritten) AgentID() string { return e.Agent }

// SummaryFor names the review outcome the episode captured — the one field
// that says whether there is anything worth recalling in it.
func (e EpisodeWritten) SummaryFor(actor string) string {
	return lead(actor, "wrote episode ("+e.ReviewOutcome+")")
}

// PersistDeciderCompleted fires after one reflection pass.
//
// Persisted=false is the common case. When the decider does write, DocID and
// Scope carry the result so an operator can navigate from a turn to the
// reflection it produced. Classification is what makes the per-agent
// distribution plottable — the headline learning-write-rate signal.
type PersistDeciderCompleted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	Persisted   bool   `json:"persisted"`
	DocID       string `json:"doc_id"`
	// Scope is empty on a NOOP.
	Scope          MemoryScope           `json:"scope"`
	Classification PersistClassification `json:"classification"`
	// TTLUntil is an ISO 8601 timestamp, populated only for PersistShort.
	TTLUntil      string `json:"ttl_until"`
	ReviewOutcome string `json:"review_outcome"`
}

// EventType is the "persist_decider_completed" wire type.
func (PersistDeciderCompleted) EventType() string { return "persist_decider_completed" }

// Role is the seat whose reflection was decided on.
func (e PersistDeciderCompleted) Role() string { return e.RoleName }

// AgentID is the instance whose memory the write would land in.
func (e PersistDeciderCompleted) AgentID() string { return e.Agent }

// SummaryFor distinguishes all three outcomes rather than just persisted or
// not: a DOC classification means the decider found a standing rule and
// deliberately kept it out of memory, which reads nothing like having found
// nothing at all.
func (e PersistDeciderCompleted) SummaryFor(actor string) string {
	switch {
	case e.Persisted:
		return lead(actor, "persisted reflection ("+string(e.Classification)+")")
	case e.Classification == PersistDoc:
		return lead(actor, "observed standing rule (not memorised)")
	default:
		return lead(actor, "reviewed turn — nothing to persist")
	}
}

// SkillUsed fires when an agent loads a skill.
//
// The measurement depends on it: without this event we cannot tell whether the
// skills the synthesizer drafts are ever loaded, and the apparent benefit of
// skill induction may come from extra sampling rather than from reuse.
type SkillUsed struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	SkillName   string `json:"skill_name"`
	// SkillID is empty for a registry-loaded skill.
	SkillID    string          `json:"skill_id"`
	SourceKind SkillSourceKind `json:"source_kind"`
	// FileLoaded is a bundled file path, empty when loading the skill body.
	FileLoaded string `json:"file_loaded"`
}

// EventType is the "skill_used" wire type.
func (SkillUsed) EventType() string { return "skill_used" }

// Role is the seat that loaded the skill.
func (e SkillUsed) Role() string { return e.RoleName }

// AgentID is the instance that loaded it.
func (e SkillUsed) AgentID() string { return e.Agent }

// SummaryFor names the source kind, because a synthesized skill being reused is
// the measurement this event exists for and a registry load is not.
func (e SkillUsed) SummaryFor(actor string) string {
	return lead(actor, "used skill '"+e.SkillName+"' ("+string(e.SourceKind)+")")
}

// PrefetchSummary fires once per turn, after the six context blocks the
// executor's prompt is built from resolve, recording hit and rendered size for
// each of them.
//
// IT IS THE ONLY VISIBILITY THIS PIPELINE HAS. Every block degrades to empty
// on failure by design — an unreachable store, an unconfigured auxiliary
// model and a filter that selected nothing all render the same nothing — so
// without this event a seat running with no memory at all looks exactly like
// a seat whose stores had nothing to say. Operators plot per-block hit rate
// per agent: a block stuck at zero is a configuration or data problem, not a
// turn problem.
type PrefetchSummary struct {
	Agent                  string `json:"agent_id"`
	AgentHandle            string `json:"agent_handle"`
	RoleName               string `json:"role"`
	TurnID                 string `json:"turn_id"`
	CounterpartyHit        bool   `json:"counterparty_hit"`
	CounterpartyBytes      int    `json:"counterparty_bytes"`
	SynthesizedSkillsHit   bool   `json:"synthesized_skills_hit"`
	SynthesizedSkillsBytes int    `json:"synthesized_skills_bytes"`
	EpisodeRecallHit       bool   `json:"episode_recall_hit"`
	EpisodeRecallBytes     int    `json:"episode_recall_bytes"`
	OnboardingHintHit      bool   `json:"onboarding_hint_hit"`
	OnboardingHintBytes    int    `json:"onboarding_hint_bytes"`
	PersonalMemoryHit      bool   `json:"personal_memory_hit"`
	PersonalMemoryBytes    int    `json:"personal_memory_bytes"`
	RelevantKnowledgeHit   bool   `json:"relevant_knowledge_hit"`
	RelevantKnowledgeBytes int    `json:"relevant_knowledge_bytes"`
	// RelevantKnowledgeSelectionCount distinguishes the two hit=true paths: a
	// non-zero count means the filter rendered real picks, while zero with
	// hit=true means it ran, found nothing relevant, and rendered the empty
	// hint. Operators investigating low effectiveness pivot on this to tell "no
	// signal" from "hint nudge only".
	RelevantKnowledgeSelectionCount int `json:"relevant_knowledge_selection_count"`
	// TriggerRequiresRecon says the trigger was a bare pointer, so the
	// personal-memory, relevant-knowledge and episode-recall prefetches skipped
	// their aux-LLM call entirely: their hit and byte counts reflect the GATE,
	// not a filter that ran and found nothing. This is the field that tells
	// "empty because gated" from "the filter selected nothing". Such a turn
	// searches later instead, with the executor's own search_knowledge call.
	TriggerRequiresRecon bool `json:"trigger_requires_recon"`
}

// EventType is the "prefetch_summary" wire type.
func (PrefetchSummary) EventType() string { return "prefetch_summary" }

// Role is the seat whose turn the prefetches ran for; per-block hit rate is
// plotted per role.
func (e PrefetchSummary) Role() string { return e.RoleName }

// AgentID is the instance whose stores were searched.
func (e PrefetchSummary) AgentID() string { return e.Agent }

// SummaryFor counts hits out of six and flags a gated turn separately: without
// the flag, a thin trigger and a genuinely empty set of stores produce the same
// "0/6" and lead an operator to look in the wrong place.
func (e PrefetchSummary) SummaryFor(actor string) string {
	hits := 0
	for _, hit := range []bool{
		e.CounterpartyHit, e.SynthesizedSkillsHit, e.EpisodeRecallHit,
		e.OnboardingHintHit, e.PersonalMemoryHit, e.RelevantKnowledgeHit,
	} {
		if hit {
			hits++
		}
	}
	gated := ""
	if e.TriggerRequiresRecon {
		gated = " (thin trigger — filters gated)"
	}
	return lead(actor, fmt.Sprintf("prefetch: %d/6 hits%s", hits, gated))
}

// CounterpartyProfileUpdated fires after one observation pass, whenever the
// turn had an identifiable counterparty — even on a no-op traits patch, because
// the interaction count still moves and the cadence should stay visible.
type CounterpartyProfileUpdated struct {
	ObserverHandle    string `json:"observer_handle"`
	RoleName          string `json:"role"`
	TurnID            string `json:"turn_id"`
	SubjectHandle     string `json:"subject_handle"`
	SubjectExternalID string `json:"subject_external_id"`
	SubjectPlatform   string `json:"subject_platform"`
	SubjectName       string `json:"subject_name"`
	TraitsPatched     int    `json:"traits_patched"`
}

// EventType is the "counterparty_profile_updated" wire type.
func (CounterpartyProfileUpdated) EventType() string { return "counterparty_profile_updated" }

// Role is the OBSERVER's seat, never the subject's: the profile is one seat's
// view of a counterparty, and attributing it to the person observed would file
// it under the wrong party. There is no AgentID — the profile is keyed by
// observer handle, which the payload carries in its own field.
func (e CounterpartyProfileUpdated) Role() string { return e.RoleName }

// SummaryFor falls all the way through to "(unknown)" rather than rendering a
// blank subject: an observation of someone the engine could not name still
// happened, and a line ending mid-sentence would read as a bug.
func (e CounterpartyProfileUpdated) SummaryFor(actor string) string {
	subject := e.SubjectName
	if subject == "" {
		subject = e.SubjectHandle
	}
	if subject == "" {
		subject = "(unknown)"
	}
	if e.TraitsPatched > 0 {
		return lead(actor, fmt.Sprintf("updated %d trait(s) on %s", e.TraitsPatched, subject))
	}
	return lead(actor, "observed "+subject)
}

// SkillSynthesized fires when a new agent-scope skill is written, from either
// the single-turn path or the scheduled clustered one.
type SkillSynthesized struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	// TurnID is empty for a clustered synthesis: there is no single turn that
	// triggered it.
	TurnID    string `json:"turn_id"`
	SkillName string `json:"skill_name"`
	SkillID   string `json:"skill_id"`
	// Trigger here is the synthesis path, NOT the turn-trigger descriptor.
	Trigger SynthesisTrigger `json:"trigger"`
	// ClusterSize is 1 for a single-turn synthesis, N for a clustered one.
	ClusterSize int `json:"cluster_size"`
	ToolCount   int `json:"tool_count"`
}

// EventType is the "skill_synthesized" wire type.
func (SkillSynthesized) EventType() string { return "skill_synthesized" }

// Role is the seat the new skill belongs to.
func (e SkillSynthesized) Role() string { return e.RoleName }

// AgentID is the instance that owns the synthesized row.
func (e SkillSynthesized) AgentID() string { return e.Agent }

// SummaryFor names the synthesis path, which is the difference between one turn
// that warranted a skill and a scheduled pass that consolidated a cluster.
func (e SkillSynthesized) SummaryFor(actor string) string {
	return lead(actor, "synthesised skill '"+e.SkillName+"' ("+string(e.Trigger)+")")
}

// SkillRefined fires when a synthesized skill picks up a new observation or
// counter-example. Most turns do not: it fires only when the refiner actually
// appended a note and bumped the version.
type SkillRefined struct {
	Agent        string `json:"agent_id"`
	AgentHandle  string `json:"agent_handle"`
	RoleName     string `json:"role"`
	TurnID       string `json:"turn_id"`
	SkillName    string `json:"skill_name"`
	SkillID      string `json:"skill_id"`
	SkillVersion int    `json:"skill_version"`
	// RefinementKind captures the intent (observed_in_practice,
	// counter_example, …) so a dashboard can tell a success annotation from a
	// failure one. Open: a refiner may name a kind this build has not seen.
	RefinementKind string `json:"refinement_kind"`
}

// EventType is the "skill_refined" wire type.
func (SkillRefined) EventType() string { return "skill_refined" }

// Role is the seat whose skill was refined.
func (e SkillRefined) Role() string { return e.RoleName }

// AgentID is the instance that owns the refined row.
func (e SkillRefined) AgentID() string { return e.Agent }

// SummaryFor carries the version and the refinement kind: the version is what
// makes successive refinements distinguishable, and the kind tells a success
// annotation from a counter-example.
func (e SkillRefined) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("refined '%s' v%d (%s)",
		e.SkillName, e.SkillVersion, e.RefinementKind))
}

// SkillPromoted fires when sibling agent-scope skills are distilled into a
// knowledge-base draft under the unit's auto-drafted parent, so a unit lead can
// review what the agents worked out for themselves.
type SkillPromoted struct {
	// Role is a representative role for the unit rather than one author.
	RoleName  string `json:"role"`
	UnitID    string `json:"unit_id"`
	SkillName string `json:"skill_name"`
	PageID    string `json:"page_id"`
	PageTitle string `json:"page_title"`
	// ContainerKey is the unit's configured space or project the draft landed
	// in — the backend-neutral half of the link.
	ContainerKey string `json:"container_key"`
	// SiblingCount is the contributing agent-scope skills, DistinctAgents the
	// distinct owners across the cluster. One agent repeating itself and five
	// agents converging are different findings.
	SiblingCount   int `json:"sibling_count"`
	DistinctAgents int `json:"distinct_agents"`
}

// EventType is the "skill_promoted" wire type.
func (SkillPromoted) EventType() string { return "skill_promoted" }

// Role is a REPRESENTATIVE role for the unit, not the author: a promotion
// distils several agents' skills, so no single seat authored it.
func (e SkillPromoted) Role() string { return e.RoleName }

// Summary is unit-led rather than actor-led, matching what the event is about,
// and reports the distinct-agent count: one agent repeating itself and five
// agents converging are different findings for whoever reviews the draft.
func (e SkillPromoted) Summary() string {
	return fmt.Sprintf(
		"unit '%s' drafted '%s' in the team knowledge base from %d agent(s)",
		e.UnitID, e.SkillName, e.DistinctAgents)
}

// SkillStaled fires when an active synthesized skill goes idle past its stale
// threshold. Operators watching this stream see which skills are ageing out and
// can pin the ones they want kept.
type SkillStaled struct {
	AgentHandle string `json:"agent_handle"`
	SkillID     string `json:"skill_id"`
	SkillName   string `json:"skill_name"`
	// LastUsedAt is ISO 8601, empty when the skill was never used.
	LastUsedAt     string `json:"last_used_at"`
	TransitionedAt string `json:"transitioned_at"`
}

// EventType is the "skill_staled" wire type. No summary method: the envelope's
// type-derived fallback ("Skill Staled") is the whole line, and the skill name
// belongs to the operator view that lists these, not to a feed.
func (SkillStaled) EventType() string { return "skill_staled" }

// SkillArchived fires when a stale skill goes idle past its archive threshold.
// Archived rows stay in the table: loading refuses them and the prefetch hides
// them, so restoring one is an operator edit rather than a re-synthesis.
type SkillArchived struct {
	AgentHandle    string `json:"agent_handle"`
	SkillID        string `json:"skill_id"`
	SkillName      string `json:"skill_name"`
	LastUsedAt     string `json:"last_used_at"`
	TransitionedAt string `json:"transitioned_at"`
}

// EventType is the "skill_archived" wire type.
func (SkillArchived) EventType() string { return "skill_archived" }

// SkillRevived fires when a previously stale skill is used again. Distinct from
// the two transitions above so the bidirectional churn is visible: a skill that
// keeps staling and reviving is a threshold that is set wrong.
type SkillRevived struct {
	AgentHandle string `json:"agent_handle"`
	SkillID     string `json:"skill_id"`
	SkillName   string `json:"skill_name"`
	// PriorState is SkillStateStale, or SkillStateArchived when an operator
	// restored the row by hand.
	PriorState     SkillState `json:"prior_state"`
	TransitionedAt string     `json:"transitioned_at"`
}

// EventType is the "skill_revived" wire type.
func (SkillRevived) EventType() string { return "skill_revived" }

// SkillTelemetryWriteFailed fires when bumping a skill's use counters failed
// twice. A persistent failure means the last-used stamp is stale and the
// curator may archive a hot skill; operators watching this can act before it
// does.
type SkillTelemetryWriteFailed struct {
	SkillID     string `json:"skill_id"`
	SkillName   string `json:"skill_name"`
	AgentHandle string `json:"agent_handle"`
	// Kind is the write that failed: mark_used, transition_state, … Open, since
	// it names an internal operation rather than a contract.
	Kind  string `json:"kind"`
	Error string `json:"error"`
}

// EventType is the "skill_telemetry_write_failed" wire type. Deliberately NOT
// in FailureEventTypes: the counter write failed, the turn that used the skill
// did not.
func (SkillTelemetryWriteFailed) EventType() string { return "skill_telemetry_write_failed" }

// CompactionRequested is published when an agent's raw episode count crosses
// its threshold, and consumed by the episode lifecycle worker.
//
// The trigger is write-side and demand-proportional: a busy agent fires it as
// it accumulates episodes, an idle one never does. The worker dedups concurrent
// requests per agent, so a burst of writes cannot fan out overlapping passes.
type CompactionRequested struct {
	AgentHandle string `json:"agent_handle"`
	// RawCount is approximate, for telemetry; Threshold is the configured cap
	// that was crossed.
	RawCount  int `json:"raw_count"`
	Threshold int `json:"threshold"`
}

// EventType is the "compaction_requested" wire type.
func (CompactionRequested) EventType() string { return "compaction_requested" }

// SummaryFor states the count and the threshold it crossed, so the line says
// why the request fired rather than only that it did.
func (e CompactionRequested) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("requested episode compaction (raw_count=%d > threshold=%d)",
		e.RawCount, e.Threshold))
}

// CompactionCompleted is published after one lifecycle pass finishes or
// short-circuits, carrying per-action counts so an operator can audit what
// happened without reading logs.
type CompactionCompleted struct {
	AgentHandle             string `json:"agent_handle"`
	NonTerminalDropped      int    `json:"non_terminal_dropped"`
	ConsolidatedDropped     int    `json:"consolidated_dropped"`
	ClustersCompacted       int    `json:"clusters_compacted"`
	RawReplacedByCompaction int    `json:"raw_replaced_by_compaction"`
	CompactedEvicted        int    `json:"compacted_evicted"`
	// SkippedReason is empty when the pass actually ran.
	SkippedReason CompactionSkipReason `json:"skipped_reason"`
}

// EventType is the "compaction_completed" wire type.
func (CompactionCompleted) EventType() string { return "compaction_completed" }

// SummaryFor reports the skip reason when there is one and the per-action counts
// otherwise. A pass that short-circuited and a pass that ran and found nothing
// to do both end with zeroes, and only the reason separates them.
func (e CompactionCompleted) SummaryFor(actor string) string {
	if e.SkippedReason != "" {
		return lead(actor, "compaction skipped ("+string(e.SkippedReason)+")")
	}
	return lead(actor, fmt.Sprintf(
		"compacted episodes: -%d non-terminal, -%d consolidated, %d clusters "+
			"(%d raw → compacted), -%d ancient compacted",
		e.NonTerminalDropped, e.ConsolidatedDropped, e.ClustersCompacted,
		e.RawReplacedByCompaction, e.CompactedEvicted))
}

// ReflectionCompleted fires at the end of every reflection pass that actually
// dispatched workers. It carries no per-worker outcome — those have their own
// events — because its job is to be a state-derivation sentinel: the aux
// workers' phase events keep the agent showing as working during learning, and
// this is what flips it back to idle when the whole pass is done.
type ReflectionCompleted struct {
	Agent         string `json:"agent_id"`
	AgentHandle   string `json:"agent_handle"`
	RoleName      string `json:"role"`
	TurnID        string `json:"turn_id"`
	WorkersRun    int    `json:"workers_run"`
	ReviewOutcome string `json:"review_outcome"`
}

// EventType is the "reflection_completed" wire type.
func (ReflectionCompleted) EventType() string { return "reflection_completed" }

// Role is the seat the pass reflected for. This is the event that flips that
// seat back to idle, so the projection needs the role to know whose.
func (e ReflectionCompleted) Role() string { return e.RoleName }

// AgentID is the instance whose learning workers have finished.
func (e ReflectionCompleted) AgentID() string { return e.Agent }

// SummaryFor counts the workers that ran and nothing else: each worker's own
// outcome has its own event, and duplicating them here would give two places to
// read the same result from.
func (e ReflectionCompleted) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("finished reflecting (%d worker(s))", e.WorkersRun))
}
