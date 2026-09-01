package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// SynthesizerSource names the worker in a pass result and in its logs.
const SynthesizerSource = "skill_synthesizer"

// Synthesizer drafts a reusable skill from a turn that did real work.
//
// # Why this file had to exist
//
// Everything downstream of a synthesized skill shipped and nothing produced
// one. `Skills.Insert` had no caller outside its own tests, so
// `synthesized_skills` was permanently empty and every reader of it —
// `use_skill`, the Plan-phase "Synthesized skills you've learned" block,
// `refine_skill`, the curator's ageing pass, the health rollup — ran
// correctly over nothing. The config block (`learning.skill_synthesis`)
// validated, shipped in the example company, and had no reader outside
// internal/config; `Skills.ToolSequences` was written FOR this worker's
// duplicate check and never called; `types.SkillSynthesized` was registered
// and never constructed.
//
// # What earns a skill
//
// A turn that used enough tools to have a shape worth naming. One tool call
// is not a procedure, and the threshold is the operator's
// (`min_tool_calls`). Below it a turn still feeds the clustered path, which
// looks for a pattern ACROSS turns rather than within one.
//
// # What stops one
//
// Three gates, and each exists because the failure it prevents is quiet. The
// per-seat cap (`max_skills_per_agent`) stops a catalogue growing past what a
// prompt can carry. The duplicate check compares the draft's tool sequence
// against the seat's existing ones by Jaccard similarity — the same seat
// doing the same job every week would otherwise accumulate a dozen
// paraphrases of one skill, and the prefetch would spend its budget showing
// the model the same procedure repeatedly. And a draft the model returns
// empty is dropped rather than written as a skill with no body.
type Synthesizer struct {
	models Models
	skills *Skills

	minToolCalls int
	maxPerAgent  int
	duplicateAt  float64
	timeout      time.Duration
	maxTokens    int
	now          func() time.Time

	// The clustered path's own inputs. episodes is nil for a synthesizer
	// built for the inline path alone, and ClusterPass answers empty.
	episodes      *Episodes
	clusterMin    int
	clusterAt     float64
	fetchLimit    int
	clusterWindow time.Duration
}

// SynthesizerOptions configures the worker. Zero values take the shipped
// defaults, which are the same numbers config.DefaultSkillSynthesis carries.
type SynthesizerOptions struct {
	// MinToolCalls is the single-turn trigger; zero takes
	// DefaultMinToolCalls.
	MinToolCalls int

	// MaxSkillsPerAgent is the hard per-seat cap; zero takes
	// DefaultMaxSkillsPerAgent.
	MaxSkillsPerAgent int

	// DuplicateJaccardThreshold rejects a draft this similar to one the
	// seat already has; zero takes DefaultDuplicateJaccard.
	DuplicateJaccardThreshold float64

	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps one draft; zero takes DefaultSynthesisTokens.
	MaxTokens int

	// Episodes backs the clustered pass. Nil leaves [Synthesizer.ClusterPass]
	// answering empty — the inline path needs no episode store, because a
	// completed turn carries its own tool sequence.
	Episodes *Episodes

	// ClusterMinSize is how many turns must converge before a cluster earns
	// a skill; zero takes DefaultClusterMinSize.
	ClusterMinSize int

	// ClusterJaccardThreshold is how similar two tool runs must be to pool
	// into one cluster; zero takes DefaultClusterJaccard.
	ClusterJaccardThreshold float64

	// EpisodeFetchLimit is how many recent turns one pass reads; zero
	// takes DefaultEpisodeFetchLimit.
	EpisodeFetchLimit int

	// ClusterWindow is how far back a pass looks; zero takes
	// DefaultClusterWindow. The second bound, for the quiet seat whose
	// last 200 turns span a year.
	ClusterWindow time.Duration

	Now func() time.Time
}

// The synthesizer's own defaults, kept equal to config.DefaultSkillSynthesis.
const (
	// DefaultMinToolCalls is five: fewer than that is a step, not a
	// procedure, and drafting one produces a "skill" that says less than
	// the tool's own description already does.
	DefaultMinToolCalls = 5

	// DefaultMaxSkillsPerAgent bounds the catalogue a prompt can carry.
	DefaultMaxSkillsPerAgent = 50

	// DefaultDuplicateJaccard is how similar two tool sequences may be
	// before the second is the first again.
	DefaultDuplicateJaccard = 0.7

	// DefaultSynthesisTokens caps one draft.
	DefaultSynthesisTokens = 4000

	// DefaultClusterMinSize is three: two turns that ran the same tools is
	// a coincidence, and drafting from it teaches a seat a procedure it
	// has performed once more than by accident.
	DefaultClusterMinSize = 3

	// DefaultClusterJaccard is LOWER than the duplicate threshold on
	// purpose. Pooling asks "is this the same kind of work" and rejecting
	// a draft asks "is this the same skill" — a stricter question, so a
	// cluster that forms at 0.6 can still be found new at 0.7.
	DefaultClusterJaccard = 0.6

	// DefaultEpisodeFetchLimit is how far back one pass looks. Two hundred
	// turns is weeks of work for a busy seat and the seat's whole history
	// for a quiet one, read as one indexed scan on a daily cadence.
	DefaultEpisodeFetchLimit = 200
)

// Skip reasons this worker reports.
const (
	SkipNotSettled     = "not_settled"
	SkipTooFewTools    = "too_few_tools"
	SkipNoHandle       = "no_handle"
	SkipSkillCapV      = "skill_cap_reached"
	SkipDuplicateSkill = "duplicate_skill"
)

// NewSynthesizer builds the worker over a skill store.
func NewSynthesizer(models Models, s *Skills, opts SynthesizerOptions) (*Synthesizer, error) {
	if models == nil {
		return nil, fmt.Errorf("learning: the skill synthesizer needs a model registry")
	}
	if s == nil {
		return nil, fmt.Errorf("learning: the skill synthesizer needs a skill store to write to")
	}
	syn := &Synthesizer{
		models: models, skills: s,
		minToolCalls:  opts.MinToolCalls,
		maxPerAgent:   opts.MaxSkillsPerAgent,
		duplicateAt:   opts.DuplicateJaccardThreshold,
		timeout:       opts.CallTimeout,
		maxTokens:     opts.MaxTokens,
		now:           opts.Now,
		episodes:      opts.Episodes,
		clusterMin:    opts.ClusterMinSize,
		clusterAt:     opts.ClusterJaccardThreshold,
		fetchLimit:    opts.EpisodeFetchLimit,
		clusterWindow: opts.ClusterWindow,
	}
	if syn.minToolCalls <= 0 {
		syn.minToolCalls = DefaultMinToolCalls
	}
	if syn.maxPerAgent <= 0 {
		syn.maxPerAgent = DefaultMaxSkillsPerAgent
	}
	if syn.duplicateAt <= 0 {
		syn.duplicateAt = DefaultDuplicateJaccard
	}
	if syn.timeout <= 0 {
		syn.timeout = DefaultAuxTimeout
	}
	if syn.maxTokens <= 0 {
		syn.maxTokens = DefaultSynthesisTokens
	}
	if syn.clusterMin <= 0 {
		syn.clusterMin = DefaultClusterMinSize
	}
	if syn.clusterAt <= 0 {
		syn.clusterAt = DefaultClusterJaccard
	}
	if syn.fetchLimit <= 0 {
		syn.fetchLimit = DefaultEpisodeFetchLimit
	}
	if syn.clusterWindow <= 0 {
		syn.clusterWindow = DefaultClusterWindow
	}
	if syn.now == nil {
		syn.now = func() time.Time { return time.Now().UTC() }
	}
	return syn, nil
}

// Name implements [Worker].
func (s *Synthesizer) Name() string { return SynthesizerSource }

// Skip implements [Worker].
//
// The cap is NOT checked here, because it costs a query and Skip runs on
// every turn: a seat at its cap is the rare case, and paying for the check on
// every turn to report it a phase earlier buys nothing. Reflect reports it.
func (s *Synthesizer) Skip(t Turn) string {
	if !t.Settled() {
		// A skill drafted from work the agent itself judged incomplete
		// encodes a procedure the next round may contradict.
		return SkipNotSettled
	}
	if t.Event.AgentHandle == "" {
		// Skills are keyed (agent_handle, name). Without a handle there is
		// no row to write and no catalogue to read it back from.
		return SkipNoHandle
	}
	if len(t.Event.ToolSequence) < s.minToolCalls {
		return SkipTooFewTools
	}
	return ""
}

// Reflect drafts one skill, or reports why it did not.
func (s *Synthesizer) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	handle := t.Event.AgentHandle

	// THE CAP FIRST, because it is a cheap count and the alternative is
	// paying for an auxiliary call whose result is thrown away.
	count, err := s.skills.Count(ctx, handle, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("learning: counting %s's skills: %w", handle, err)
	}
	if count >= s.maxPerAgent {
		log.DebugContext(ctx, "skill_synthesis_skipped", "reason", SkipSkillCapV,
			"agent_handle", handle, "skills", count, "cap", s.maxPerAgent)
		return nil, nil
	}

	// THE DUPLICATE CHECK ALSO BEFORE THE CALL, for the same reason. The
	// seat's existing sequences are what Skills.ToolSequences was written
	// to serve.
	existing, err := s.skills.ToolSequences(ctx, handle, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("learning: reading %s's tool sequences: %w", handle, err)
	}
	if name, dup := mostSimilar(t.Event.ToolSequence, existing, s.duplicateAt); dup {
		log.DebugContext(ctx, "skill_synthesis_skipped", "reason", SkipDuplicateSkill,
			"agent_handle", handle, "similar_to", name)
		return nil, nil
	}

	member, err := s.models.Head(t.Role, phase.Auxiliary)
	if err != nil {
		return nil, fmt.Errorf("learning: no auxiliary model for skill synthesis: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: SynthesisSystemPrompt},
			{Role: llm.RoleUser, Content: buildSynthesisPrompt(t)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   s.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: drafting a skill for %s: %w", handle, err)
	}
	draft, ok := parseSkillDraft(completion)
	if !ok {
		// The model declined, which is the expected answer for a turn whose
		// tool run had no reusable shape. Not an error: asking is cheap and
		// most turns are not procedures.
		log.DebugContext(ctx, "skill_synthesis_declined", "agent_handle", handle,
			"turn_id", t.Event.TurnID)
		return nil, nil
	}

	at := s.now()
	skill := Skill{
		ID:               uuid.NewString(),
		AgentHandle:      handle,
		Name:             draft.Name,
		Description:      draft.Description,
		Content:          draft.Content,
		ToolSequence:     t.Event.ToolSequence,
		SourceEpisodeIDs: []string{t.Event.TurnID},
		Version:          1,
		CreatedAt:        at,
		UpdatedAt:        at,
		State:            SkillActive,
	}
	if err := s.skills.Insert(ctx, skill); err != nil {
		// ErrSkillExists is a race with a peer that drafted the same name,
		// not a failure: the row that won is as good as the one that lost.
		if errors.Is(err, ErrSkillExists) {
			log.DebugContext(ctx, "skill_synthesis_lost_race", "agent_handle", handle,
				"skill", draft.Name)
			return nil, nil
		}
		return nil, fmt.Errorf("learning: writing %s's skill %q: %w", handle, draft.Name, err)
	}

	log.InfoContext(ctx, "skill_synthesized", "agent_handle", handle, "skill", draft.Name,
		"skill_id", skill.ID, "tools", len(t.Event.ToolSequence))
	return []events.Payload{types.SkillSynthesized{
		Agent:       t.Event.Agent,
		AgentHandle: handle,
		RoleName:    t.Event.RoleName,
		TurnID:      t.Event.TurnID,
		SkillName:   draft.Name,
		SkillID:     skill.ID,
		Trigger:     types.SynthesisSingleTurn,
		ClusterSize: 1,
		ToolCount:   len(t.Event.ToolSequence),
	}}, nil
}

// skillDraft is what the model is asked for.
type skillDraft struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// parseSkillDraft reads the model's answer, reporting whether it drafted one.
//
// A draft missing any of its three parts is DROPPED rather than written with
// the gap: a skill with no body is a catalogue entry that costs prompt budget
// and teaches nothing, and one with no name cannot be addressed by `use_skill`.
func parseSkillDraft(c *llm.Completion) (skillDraft, bool) {
	if c == nil {
		return skillDraft{}, false
	}
	body := strings.TrimSpace(c.Content)
	if body == "" || body == "{}" {
		return skillDraft{}, false
	}
	body = stripFence(body)

	var draft skillDraft
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		log.Warn("skill_draft_undecodable", "error", err.Error())
		return skillDraft{}, false
	}
	draft.Name = strings.TrimSpace(draft.Name)
	draft.Description = strings.TrimSpace(draft.Description)
	draft.Content = strings.TrimSpace(draft.Content)
	if draft.Name == "" || draft.Content == "" || draft.Description == "" {
		return skillDraft{}, false
	}
	return draft, true
}

// stripFence unwraps the code fence a model puts around JSON it was told to
// answer bare.
//
// Shared by every parser here rather than repeated per worker: a model told
// "JSON only" fences it anyway often enough that a worker whose parser forgot
// the case silently declines every fenced answer, which looks exactly like a
// model that never has anything to say. Only the OUTER fence, and only when
// both ends are present — a lone "```" inside a body is content, and a
// procedure that legitimately quotes a shell block would lose it.
func stripFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return trimmed
	}
	// Past the opening fence and its language tag, which is whatever the
	// model wrote on the rest of that first line.
	rest := trimmed[len("```"):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	} else {
		// A one-line fence: "```{}```" has no body line to skip past.
		rest = strings.TrimPrefix(rest, "json")
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "```"))
}

// mostSimilar reports the closest existing sequence past the threshold.
//
// Jaccard over the SET of tools, not the ordered run: two turns that call the
// same four tools in a different order are the same procedure, and treating
// order as identity is how a seat ends up with a skill per permutation.
func mostSimilar(seq []string, existing [][]string, threshold float64) (string, bool) {
	best, bestAt := "", 0.0
	for _, other := range existing {
		if score := toolJaccard(seq, other); score > bestAt {
			best, bestAt = strings.Join(other, ","), score
		}
	}
	return best, bestAt >= threshold
}

// SynthesisSystemPrompt asks for a reusable procedure or nothing.
//
// "Or nothing" is load-bearing and stated twice. A model asked to draft a
// skill will draft one for any turn at all, and a catalogue of turns
// restated as skills is worse than an empty one: it costs prompt budget on
// every future turn to show the model things it already knew.
const SynthesisSystemPrompt = `You distil a completed agent turn into a
REUSABLE skill — a short procedure the same agent could follow next time it
faces this kind of task.

Answer with a JSON object and nothing else:
{"name":"kebab-case-name","description":"one line, what this is for",
 "content":"markdown steps"}

Draft a skill ONLY when the turn shows a procedure worth repeating: an
ordered use of tools that solved a recognisable KIND of problem, not this
one instance of it.

Do NOT draft one for: a turn that just answered a question, a one-off lookup,
anything specific to this particular ticket or person, or a run whose tools
were incidental. If the turn has no reusable shape, answer exactly {}.

The content is instructions to your future self. Name the tools in order,
say what to check between them, and say when the procedure does NOT apply.
Leave out what happened this time — no ticket numbers, no names, no dates.`

// buildSynthesisPrompt renders the one turn the draft is distilled from.
func buildSynthesisPrompt(t Turn) string {
	var b strings.Builder
	b.WriteString("Turn summary:\n- Task: ")
	b.WriteString(orElse(t.Event.TaskSummary, "(no description)"))
	b.WriteString("\n- Plan: ")
	b.WriteString(orElse(t.Event.PlanSummary, "(no plan)"))
	b.WriteString("\n- Tools called, in order: ")
	b.WriteString(strings.Join(t.Event.ToolSequence, " -> "))
	b.WriteString("\n- Outcome: ")
	b.WriteString(t.Event.ReviewOutcome)
	if t.Role != nil {
		b.WriteString("\n- The agent's role: ")
		b.WriteString(t.Role.Name)
	}
	return b.String()
}
