package types

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// Auxiliary spend: what the cheap model a seat's `llm_auxiliary` names cost,
// for the work that is not a phase of a turn — the turn-start memory filter,
// knowledge query and episode summary, every rewrite a compaction makes, the
// reflection workers after a turn, the background learning passes, and a
// person's question answered from the knowledge base.
//
// # Why a type of its own
//
// Every one of those calls was charged to the token counters and reached no
// event, so every spend figure built from events — the usage domain, the Spend
// screen, the live rollup, the turn list, a turn's page and a task's spend —
// understated what the counters had charged, by exactly this. The readers were
// written for an `agent_phase_completed` with `phase: auxiliary` that nothing
// ever published, and publishing one would have been wrong: that type DRIVES
// the live seat state, so a reflection call stamped after its turn ended
// reopened the turn on every node (older ones included) and cleared a held
// provider stop. A record of spend must move no seat, so it is a type no state
// machine reads.
//
// # Coalesced, never one per call
//
// A record is the calls of ONE key — stage, seat or person, turn, purpose,
// model, provider entry and company day — that a node made inside one flush of
// its ledger (internal/auxspend), so a compaction that made seventy rewrites
// is one row rather than seventy. ACCOUNTING, NOT A TRANSCRIPT: no prompt and
// no response is on it. What the memory filter was shown is a debugging
// question with its own answer, and carrying a compaction's chunks would
// dwarf the event log for a number.
//
// # No `failed` key, deliberately
//
// A failed call is COUNTED (FailedCalls), never flagged: the store promotes a
// top-level `failed: true` into the tag that marks a whole turn failed in the
// turn list, and the live projection derives a turn's failure from it too — so
// a memory filter that timed out would have painted a turn that succeeded red.

func init() {
	events.Register[AuxiliarySpend]()
}

// AuxStage is which part of the work an auxiliary call served: whose cost it
// is, and when it was spent.
//
// A NAMED STRING with a Valid method, so a stage a newer build publishes is a
// value rather than a panic.
type AuxStage string

const (
	// AuxStageTurn is spent INSIDE a turn, for that turn's work: its
	// turn-start context, the rewrites its ledgers and its judge's
	// evidence needed, a worker's answer condensed for it, its card.
	// It is part of the turn's cost and is charged to the work item the
	// turn is on (ADR-0022).
	AuxStageTurn AuxStage = "turn"

	// AuxStageReflection is spent AFTER a turn, on the seat's behalf:
	// the reflection workers, and the conversation ledger's account of
	// the turn. Drawn beside the turn and counted on the seat's day, and
	// never in the turn's own total or on its work item — it is how much
	// the seat had to remember, not what the work cost.
	AuxStageReflection AuxStage = "reflection"

	// AuxStageBackground is spent on no turn: the episode compaction,
	// skill clustering and skill promotion passes.
	AuxStageBackground AuxStage = "background"

	// AuxStageOperator is spent for a PERSON: a question answered from the
	// knowledge base on the operator surface. A person has no seat budget,
	// so it is charged to the company's windows alone.
	AuxStageOperator AuxStage = "operator"
)

// AuxStages is every stage this build publishes.
var AuxStages = []AuxStage{AuxStageTurn, AuxStageReflection, AuxStageBackground, AuxStageOperator}

// Valid reports whether this build knows the stage.
func (s AuxStage) Valid() bool { return slices.Contains(AuxStages, s) }

// AuxPurpose is what one auxiliary call was FOR — one value per caller, so a
// reader can say which piece of machinery spent the tokens.
//
// A NAMED STRING with a Valid method, for [AuxStage]'s reason.
type AuxPurpose string

// The callers. Each is the one site that makes the call, so a purpose names a
// line of code a reader can find.
const (
	// AuxMemoryFilter picks which stored memories reach a turn's prompt,
	// at turn start and again on `refresh_memory`.
	AuxMemoryFilter AuxPurpose = "memory_filter"
	// AuxKnowledgeQuery writes the turn-start knowledge search's query.
	AuxKnowledgeQuery AuxPurpose = "knowledge_query"
	// AuxEpisodeSummary summarises the recalled episodes for a turn.
	AuxEpisodeSummary AuxPurpose = "episode_summary"

	// AuxPersistDecider classifies a finished turn for the diary.
	AuxPersistDecider AuxPurpose = "persist_decider"
	// AuxCounterpartyProfiler updates what a seat knows about a party.
	AuxCounterpartyProfiler AuxPurpose = "counterparty_profiler"
	// AuxSkillSynthesizer drafts a skill from one turn.
	AuxSkillSynthesizer AuxPurpose = "skill_synthesizer"
	// AuxSkillRefiner refines a skill a turn used.
	AuxSkillRefiner AuxPurpose = "skill_refiner"

	// AuxEpisodeCompaction summarises a cluster of a seat's old episodes.
	AuxEpisodeCompaction AuxPurpose = "episode_compaction"
	// AuxSkillClustering drafts a skill from a cluster of episodes.
	AuxSkillClustering AuxPurpose = "skill_clustering"
	// AuxSkillPromotion promotes sibling skills to a unit's skill.
	AuxSkillPromotion AuxPurpose = "skill_promotion"

	// AuxAnswerKnowledge answers a person's question from the company's
	// pages and work items.
	AuxAnswerKnowledge AuxPurpose = "answer_knowledge"
)

// auxCondensePrefix is what every compaction's purpose begins with.
const auxCondensePrefix = "condense_"

// auxCondenseKinds are the kinds of text internal/compact rewrites, as its
// [github.com/crewlet/crewlet/internal/compact.Kind] values spell them. A COPY, because this package must not
// import that one; internal/compact's own suite holds the two lists equal in
// both directions.
var auxCondenseKinds = []string{
	"conversation", "thread", "argument", "tool_error", "produced",
	"source", "task", "answer", "outcome", "report",
}

// AuxCondense is the purpose of a compaction of one kind of text:
// `condense_<kind>`, so a rewrite of a chat thread and one of a coding run's
// report are told apart wherever they are spent.
func AuxCondense(kind string) AuxPurpose { return AuxPurpose(auxCondensePrefix + kind) }

// AuxCondenseKinds is every compaction kind this build names a purpose for.
func AuxCondenseKinds() []string { return slices.Clone(auxCondenseKinds) }

// auxPurposes is every purpose this build publishes that is not a compaction.
var auxPurposes = []AuxPurpose{
	AuxMemoryFilter, AuxKnowledgeQuery, AuxEpisodeSummary,
	AuxPersistDecider, AuxCounterpartyProfiler, AuxSkillSynthesizer, AuxSkillRefiner,
	AuxEpisodeCompaction, AuxSkillClustering, AuxSkillPromotion,
	AuxAnswerKnowledge,
}

// AuxPurposes is every purpose this build publishes, compactions included.
func AuxPurposes() []AuxPurpose {
	out := slices.Clone(auxPurposes)
	for _, kind := range auxCondenseKinds {
		out = append(out, AuxCondense(kind))
	}
	return out
}

// Valid reports whether this build knows the purpose.
func (p AuxPurpose) Valid() bool {
	if kind, ok := strings.CutPrefix(string(p), auxCondensePrefix); ok {
		return slices.Contains(auxCondenseKinds, kind)
	}
	return slices.Contains(auxPurposes, p)
}

// AuxiliarySpend is what one key's auxiliary calls cost over one flush of a
// node's ledger: one stage, one seat or person, one turn where there is one,
// one purpose, one model, one provider entry, one company day.
//
// THE DAY IS ON THE RECORD, and the envelope's timestamp is the bucket's LAST
// CALL rather than the flush: the counters charged each call in the windows
// current when it returned, so a bucket closes at midnight rather than
// straddling it, and the row lands in the company day it was charged in
// however late its flush ran.
type AuxiliarySpend struct {
	// Agent, AgentHandle and RoleName are the AGENT seat the calls were
	// made for — set on every record but a person's.
	Agent       string `json:"agent_id,omitempty"`
	AgentHandle string `json:"agent_handle,omitempty"`
	RoleName    string `json:"role,omitempty"`

	// ActorSeat and ActorRole are the PERSON the calls were made for,
	// set instead of the three above: on [AuxStageOperator], the seat
	// handle a person's credential is bound to; on [AuxStageBackground],
	// the human seat leading a unit whose pass ran on its chain. Each with
	// its role. NOT `role`: a person is not an agent seat, and the live
	// projection keys a seat's state on `role`.
	ActorSeat string `json:"actor_seat,omitempty"`
	ActorRole string `json:"actor_role,omitempty"`

	Stage   AuxStage   `json:"stage"`
	Purpose AuxPurpose `json:"purpose"`

	// TurnID is the RUN the calls were made in or for, and WorkKey the
	// unit of work behind it (ADR-0017). Set on [AuxStageTurn] and
	// [AuxStageReflection]; empty on the other two, which serve no turn.
	TurnID  string `json:"turn_id,omitempty"`
	WorkKey string `json:"work_key,omitempty"`

	// Model is the model the calls reported, and ProviderKey the
	// configured entry that served them (`providers.llm.<key>`).
	Model       string `json:"model"`
	ProviderKey string `json:"provider_key"`

	// Day is the company day the calls were charged in (`2026-09-23`).
	Day string `json:"day"`

	// Calls is how many provider calls this record covers, and
	// FailedCalls how many of them failed. A failed call is still a call:
	// it may have been billed, and a figure that dropped it would hide a
	// model that times out.
	Calls       int `json:"calls"`
	FailedCalls int `json:"failed_calls"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CacheReadTokens and CacheWriteTokens are a BREAKDOWN of InputTokens,
	// never an addition to it — see [AgentPhaseCompleted.CacheReadTokens].
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	// StartedAt is when the first call began and EndedAt when the last one
	// returned; DurationMS is the calls' own time, summed — less than the
	// span between the two wherever the calls did not run back to back.
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int       `json:"duration_ms"`
}

// EventType is the "auxiliary_spend" wire type.
func (AuxiliarySpend) EventType() string { return "auxiliary_spend" }

// Role is the seat the spend belongs to; empty for a person's.
func (e AuxiliarySpend) Role() string { return e.RoleName }

// AgentID is the seat's derived id.
func (e AuxiliarySpend) AgentID() string { return e.Agent }

// Actor is the person the calls were made for, and empty on an agent seat's
// record — which hands the envelope on to the seat's role.
func (e AuxiliarySpend) Actor() string { return e.ActorSeat }

// SummaryFor says what was spent, on what, in how many calls.
func (e AuxiliarySpend) SummaryFor(actor string) string {
	calls := fmt.Sprintf("%d calls", e.Calls)
	if e.Calls == 1 {
		calls = "1 call"
	}
	if e.FailedCalls > 0 {
		calls += fmt.Sprintf(", %d failed", e.FailedCalls)
	}
	model := e.Model
	if model == "" {
		model = e.ProviderKey
	}
	purpose := strings.ReplaceAll(string(e.Purpose), "_", " ")
	if purpose == "" {
		purpose = "auxiliary work"
	}
	return lead(actor, fmt.Sprintf("spent %d tokens on %s (%s, %s)",
		e.TotalTokens, purpose, calls, model))
}
