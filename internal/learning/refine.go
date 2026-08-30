package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// RefinerSource names the worker in a pass result and in its logs.
const RefinerSource = "skill_refiner"

// Refiner appends what practice taught to a skill the turn was working from.
//
// # The auto path, which was documented and absent
//
// `refine_skill` — the manual half — has always existed: a planner that finds
// a skill wrong rewrites it mid-turn. The AUTO half is what closes the loop
// without the model having to notice, and it was config only:
// `auto_refine_on_success` and `auto_refine_on_failure` validated, shipped in
// the example company, and had no reader outside internal/config. The only
// per-turn worker touching skills stamped their use counters and stopped.
//
// # One call, one bullet, one skill
//
// Not one call per offered skill. A turn is offered its whole catalogue, so
// per-skill calls would cost a completion per skill per turn for answers that
// are almost always NOOP. Instead the model is shown the skills the turn had
// and the turn itself, and picks at most ONE thing worth writing down —
// which is also the honest shape of the question: a turn rarely teaches two
// procedures something new at once.
//
// # NOOP is the expected answer
//
// A model asked "what did this turn teach that skill" will produce something
// for any turn at all, and a skill that grows a bullet per turn stops being a
// procedure and becomes a diary of the turns that read it. So the prompt says
// twice that answering nothing is correct, and the body cap is the hard stop
// behind that: past it the refinement is SKIPPED rather than truncated,
// because a clipped procedure lands mid-step.
type Refiner struct {
	models Models
	skills *Skills

	onSuccess bool
	onFailure bool

	timeout      time.Duration
	maxTokens    int
	bodyMax      int
	keepVersions int
	now          func() time.Time
}

// RefinerOptions configures the worker. Zero values take the shipped defaults,
// which are the same numbers config.DefaultSkillRefinement carries.
type RefinerOptions struct {
	// OnSuccess and OnFailure gate the two halves independently. They are
	// POINTERS to nothing here: both default to on, and a caller that wants
	// one off says so — matching the config Toggle, whose zero value is
	// "unset" rather than "off".
	OnSuccess *bool
	OnFailure *bool

	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps one refinement; zero takes DefaultRefinementTokens.
	MaxTokens int

	// MaxBodyChars is the ceiling on the refined body; zero takes
	// DefaultRefinementBodyMax. A refinement that would breach it is
	// skipped, not truncated.
	MaxBodyChars int

	// KeepVersions bounds the archived history; zero lets the store apply
	// its own default.
	KeepVersions int

	Now func() time.Time
}

// The refiner's own defaults, kept equal to config.DefaultSkillRefinement.
const (
	// DefaultRefinementTokens caps one refinement's completion. A bullet is
	// a sentence, and the budget is generous rather than tight because the
	// call also has to read the skills it is choosing between.
	DefaultRefinementTokens = 3000

	// DefaultRefinementBodyMax is the ceiling on a refined skill's body.
	DefaultRefinementBodyMax = 20000
)

// Skip reasons this worker reports.
const (
	SkipNoSkillsOffered   = "no_skills_offered"
	SkipOutcomeNotRefined = "outcome_not_refined"
)

// NewRefiner builds the worker over a skill store.
func NewRefiner(models Models, s *Skills, opts RefinerOptions) (*Refiner, error) {
	if models == nil {
		return nil, fmt.Errorf("learning: the skill refiner needs a model registry")
	}
	if s == nil {
		return nil, fmt.Errorf("learning: the skill refiner needs a skill store to write to")
	}
	r := &Refiner{
		models: models, skills: s,
		onSuccess:    opts.OnSuccess == nil || *opts.OnSuccess,
		onFailure:    opts.OnFailure == nil || *opts.OnFailure,
		timeout:      opts.CallTimeout,
		maxTokens:    opts.MaxTokens,
		bodyMax:      opts.MaxBodyChars,
		keepVersions: opts.KeepVersions,
		now:          opts.Now,
	}
	if r.timeout <= 0 {
		r.timeout = DefaultAuxTimeout
	}
	if r.maxTokens <= 0 {
		r.maxTokens = DefaultRefinementTokens
	}
	if r.bodyMax <= 0 {
		r.bodyMax = DefaultRefinementBodyMax
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	return r, nil
}

// Name implements [Worker].
func (r *Refiner) Name() string { return RefinerSource }

// Skip implements [Worker].
//
// SETTLED ONLY, like the synthesizer and for the same reason: a self_iterate
// round is work the agent itself judged incomplete, and a lesson drawn from it
// is one the next round may contradict. Unlike the synthesizer, a `failed`
// turn is the MOST interesting case here — a counter-example is exactly what
// a skill that led an agent wrong needs.
func (r *Refiner) Skip(t Turn) string {
	if !t.Settled() {
		return SkipNotSettled
	}
	if t.Event.AgentHandle == "" {
		return SkipNoHandle
	}
	if len(t.Event.SkillsUsed) == 0 {
		return SkipNoSkillsOffered
	}
	if !r.refines(t) {
		return SkipOutcomeNotRefined
	}
	return ""
}

// refines reports whether this turn's outcome is one the operator wants
// refined.
func (r *Refiner) refines(t Turn) bool {
	if t.Event.ReviewOutcome == "done" {
		return r.onSuccess
	}
	return r.onFailure
}

// Reflect appends one bullet to one skill, or reports why it did not.
func (r *Refiner) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	handle := t.Event.AgentHandle

	// THE CANDIDATES FIRST, because the model has to choose among skills
	// rather than be told a name and asked to justify it — and because a
	// turn whose offered skills have all since been archived has nothing to
	// refine and must not cost a completion.
	candidates, err := r.candidates(ctx, t)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		log.DebugContext(ctx, "skill_refinement_skipped", "reason", "no_live_skills",
			"agent_handle", handle, "turn_id", t.Event.TurnID)
		return nil, nil
	}

	member, err := r.models.Head(t.Role, phase.Auxiliary)
	if err != nil {
		return nil, fmt.Errorf("learning: no auxiliary model for skill refinement: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: RefinementSystemPrompt},
			{Role: llm.RoleUser, Content: buildRefinementPrompt(t, candidates)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   r.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: refining a skill for %s: %w", handle, err)
	}
	choice, ok := parseRefinement(completion)
	if !ok {
		// The model declined, which is the expected answer for a turn that
		// taught its skills nothing. Not an error: asking is cheap and
		// most turns teach nothing.
		log.DebugContext(ctx, "skill_refinement_declined", "agent_handle", handle,
			"turn_id", t.Event.TurnID)
		return nil, nil
	}
	target, found := pickSkill(candidates, choice.SkillName)
	if !found {
		// A NAME THE MODEL INVENTED. Dropped rather than fuzzy-matched: a
		// bullet appended to the wrong procedure is worse than no bullet,
		// and the candidates were listed by exact name in the prompt.
		log.DebugContext(ctx, "skill_refinement_named_nothing", "agent_handle", handle,
			"named", choice.SkillName)
		return nil, nil
	}

	kind := RefineObserved
	if t.Event.ReviewOutcome != "done" {
		kind = RefineCounterExample
	}
	body := appendBullet(target.Content, kind, choice.Bullet)
	if len(body) > r.bodyMax {
		// SKIPPED, not truncated. The cap exists because a body that grows
		// a bullet per turn grows without bound, and a clip lands mid-step
		// where the model reads the remainder as the whole procedure. The
		// manual tool refuses instead, because there a person can retry.
		log.InfoContext(ctx, "skill_refinement_skipped", "reason", "body_cap",
			"agent_handle", handle, "skill", target.Name,
			"chars", len(body), "cap", r.bodyMax)
		return nil, nil
	}

	rev := target.Revision()
	rev.Content = body
	updated, err := r.skills.Update(ctx, target.ID, rev, Refinement{
		Kind:         kind,
		Note:         choice.Bullet,
		KeepVersions: r.keepVersions,
		At:           r.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("learning: appending to %s's skill %q: %w",
			handle, target.Name, err)
	}
	log.InfoContext(ctx, "skill_refined", "agent_handle", handle, "skill", updated.Name,
		"skill_id", updated.ID, "version", updated.Version, "kind", string(kind))
	return []events.Payload{types.SkillRefined{
		Agent:          t.Event.Agent,
		AgentHandle:    handle,
		RoleName:       t.Event.RoleName,
		TurnID:         t.Event.TurnID,
		SkillName:      updated.Name,
		SkillID:        updated.ID,
		SkillVersion:   updated.Version,
		RefinementKind: string(kind),
	}}, nil
}

// candidates are the skills this turn was offered that still exist.
//
// Intersected with the LIVE catalogue rather than trusted from the event: the
// turn's list is ids captured when its prompt was built, and the curator may
// have archived one since. Refining an archived skill would resurrect a body
// nobody will be shown.
func (r *Refiner) candidates(ctx context.Context, t Turn) ([]Skill, error) {
	live, err := r.skills.List(ctx, t.Event.AgentHandle, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("learning: listing %s's skills: %w", t.Event.AgentHandle, err)
	}
	offered := map[string]bool{}
	for _, id := range t.Event.SkillsUsed {
		offered[id] = true
	}
	out := make([]Skill, 0, len(offered))
	for _, sk := range live {
		if offered[sk.ID] {
			out = append(out, sk)
		}
	}
	return out, nil
}

// pickSkill resolves the model's chosen name against the candidates.
func pickSkill(candidates []Skill, name string) (Skill, bool) {
	want := strings.TrimSpace(name)
	for _, sk := range candidates {
		if strings.EqualFold(sk.Name, want) {
			return sk, true
		}
	}
	return Skill{}, false
}

// appendBullet adds one practice note to a skill's body.
//
// Under a stable heading, so successive refinements collect in one place
// instead of scattering through the steps — a reader of the skill sees the
// procedure first and what practice added to it second, which is the order
// they are useful in.
func appendBullet(body string, kind RefinementKind, bullet string) string {
	label := "Observed in practice"
	if kind == RefineCounterExample {
		label = "Counter-example"
	}
	line := fmt.Sprintf("- **%s:** %s", label, strings.TrimSpace(bullet))
	trimmed := strings.TrimRight(body, "\n")
	if strings.Contains(trimmed, refinementHeading) {
		return trimmed + "\n" + line + "\n"
	}
	return trimmed + "\n\n" + refinementHeading + "\n\n" + line + "\n"
}

// refinementHeading is where practice notes collect.
const refinementHeading = "## What practice added"

// refinementChoice is the model's answer.
type refinementChoice struct {
	SkillName string `json:"skill_name"`
	Bullet    string `json:"bullet"`
}

// parseRefinement reads the model's answer, reporting false for a decline.
func parseRefinement(completion *llm.Completion) (refinementChoice, bool) {
	if completion == nil {
		return refinementChoice{}, false
	}
	raw := strings.TrimSpace(stripFence(completion.Content))
	if raw == "" || raw == "{}" {
		return refinementChoice{}, false
	}
	var choice refinementChoice
	if err := json.Unmarshal([]byte(raw), &choice); err != nil {
		// UNPARSEABLE IS A DECLINE, not an error. The pass must not fail
		// over a model that answered in prose, and there is nothing to
		// write either way.
		log.Debug("skill_refinement_unparseable", "error", err.Error())
		return refinementChoice{}, false
	}
	if strings.TrimSpace(choice.SkillName) == "" || strings.TrimSpace(choice.Bullet) == "" {
		return refinementChoice{}, false
	}
	return choice, true
}

// RefinementSystemPrompt asks for one practice note, or nothing.
//
// "Or nothing" is stated twice for the same reason it is in the synthesis
// prompt: a model asked what a turn taught will always answer, and a skill
// that grows a bullet per turn stops being a procedure.
const RefinementSystemPrompt = `You decide whether a completed agent turn taught
one of that agent's own skills something worth writing down.

Answer with a JSON object and nothing else:
{"skill_name":"the exact name of ONE listed skill","bullet":"one sentence"}

Answer exactly {} — which is the RIGHT answer most of the time — when the turn
merely followed a skill and it worked, when nothing surprising happened, or
when what you would write is already in the skill.

Write a bullet ONLY for something the skill does not say and its next reader
would want to know: a case where following it goes wrong, a precondition it
omits, a step that turned out to matter. One sentence, general — no ticket
numbers, no names, no dates, nothing about this instance.`

// buildRefinementPrompt renders the turn and the skills it was working from.
func buildRefinementPrompt(t Turn, candidates []Skill) string {
	var b strings.Builder
	b.WriteString("The agent's skills on this turn:\n")
	for _, sk := range candidates {
		fmt.Fprintf(&b, "\n### %s\n%s\n\n%s\n", sk.Name, sk.Description, sk.Content)
	}
	b.WriteString("\nThe turn:\n- Task: ")
	b.WriteString(orElse(t.Event.TaskSummary, "(no description)"))
	b.WriteString("\n- Plan: ")
	b.WriteString(orElse(t.Event.PlanSummary, "(no plan)"))
	b.WriteString("\n- Tools called, in order: ")
	b.WriteString(strings.Join(t.Event.ToolSequence, " -> "))
	b.WriteString("\n- Outcome: ")
	b.WriteString(t.Event.ReviewOutcome)
	return b.String()
}
