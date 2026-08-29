package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/tools"
)

// RefineSkillTool is the tool's wire name.
const RefineSkillTool = "refine_skill"

// RefinableSkills is what refine_skill needs from the skill store.
type RefinableSkills interface {
	Get(ctx context.Context, handle, name string) (learning.Skill, bool, error)
	Update(ctx context.Context, skillID string, rev learning.Revision, r learning.Refinement) (learning.Skill, error)
}

// refineSkill lets an agent correct one of its own skills.
//
// A WHOLE-BODY REPLACEMENT, not a patch. A model asked to produce a diff
// produces something diff-shaped that does not apply, and a partial edit that
// half-applied would leave a procedure that is neither the old one nor the new
// one — with nothing to compare against, because the prior body is already
// archived by then. Handing back the full text is also what makes the archived
// version meaningful: two complete bodies a person can read side by side.
//
// The prior body is kept (learning.RefineTool), which is what makes this safe
// to offer an agent at all: a refinement that made a skill worse is one
// rollback away rather than a loss.
type refineSkill struct {
	skills RefinableSkills

	// bodyMax is learning.skill_refinement.max_body_chars: the ceiling on
	// the whole procedure after the edit. It was documented as a runaway
	// guard and enforced nowhere — refine.go clipped only the note.
	bodyMax int

	// versions is learning.skill_refinement.max_versions_kept, passed
	// through to the store's prune. Zero lets the store apply its own.
	versions int

	events Telemetry
}

var _ tools.SeatCallable = (*refineSkill)(nil)

func (t *refineSkill) Name() string { return RefineSkillTool }

func (t *refineSkill) Description() string {
	return "Correct one of your own synthesized skills, when following it " +
		"led you wrong or you learned something it should have said. Give " +
		"the FULL corrected procedure, not a description of the change — " +
		"the new text replaces the old one. The previous version is kept, " +
		"so a correction that turns out worse can be undone."
}

func (t *refineSkill) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill_name": map[string]any{
				"type":        "string",
				"description": "Exact name of one of your synthesized skills",
			},
			"content": map[string]any{
				"type": "string",
				"description": "The full corrected procedure, replacing the " +
					"current body in its entirety",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why it needed correcting — read by a person later",
			},
		},
		"required": []any{"skill_name", "content"},
	}
}

func (t *refineSkill) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *refineSkill) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle := turn.Handle()
	if handle == "" {
		return failed("refine_skill can only be called during a turn, on behalf of a seat."), nil
	}
	if t.skills == nil {
		return failed("Skill synthesis is not configured on this deployment."), nil
	}

	name := strings.TrimSpace(argString(args, "skill_name"))
	content := strings.TrimSpace(argString(args, "content"))
	switch {
	case name == "":
		return failed("refine_skill needs a `skill_name`."), nil
	case content == "":
		return failed("refine_skill needs `content`: the FULL corrected " +
			"procedure. It replaces the current body, so a partial edit " +
			"would delete the rest of the skill."), nil
	case t.bodyMax > 0 && len(content) > t.bodyMax:
		// REFUSED, not truncated. A skill is a procedure the seat will
		// follow, and half a procedure is worse than the one it already
		// has — a clip lands mid-step and the model reads the remainder
		// as the whole. The cap exists because a body that grows an
		// annotation per turn grows without bound, so the answer is to
		// tighten the text, which is something the model can do.
		return failed(fmt.Sprintf(
			"That body is %d characters and this company caps a skill at %d "+
				"(learning.skill_refinement.max_body_chars). Tighten it: drop "+
				"what practice has superseded rather than appending to it.",
			len(content), t.bodyMax)), nil
	}

	// This seat's own skills only. Editing a colleague's would let one
	// agent rewrite another's learned procedure with nothing in the way.
	sk, found, err := t.skills.Get(ctx, handle, name)
	if err != nil {
		return failed(fmt.Sprintf("Could not load %q: %v", clip(name), err)), nil
	}
	if !found {
		return failed(fmt.Sprintf(
			"You have no synthesized skill called %q, so there is nothing to "+
				"refine. Skills are distilled from your own completed turns.",
			clip(name))), nil
	}

	rev := sk.Revision()
	rev.Content = content
	if reason := strings.TrimSpace(argString(args, "reason")); reason != "" {
		// Recorded on the REFINEMENT rather than folded into the body: it
		// explains the change, and a body carrying its own changelog grows
		// one entry per edit in every prompt that loads it.
		rev.Description = sk.Description
	}
	updated, err := t.skills.Update(ctx, sk.ID, rev, learning.Refinement{
		Kind:         learning.RefineTool,
		Note:         clipTo(argString(args, "reason"), diaryNoteMax),
		KeepVersions: t.versions,
		At:           time.Now().UTC(),
	})
	if err != nil {
		return failed(fmt.Sprintf("Could not refine %q: %v", clip(name), err)), nil
	}
	// The VERSION is what makes successive refinements distinguishable, and
	// the kind tells a success annotation from a counter-example. Published
	// after the write, because an event for a refinement that failed to
	// store would put a version in the feed that no rollback can reach.
	if agentID, why := seatAgentID(turn); why == "" {
		note(ctx, t.events, turn, types.SkillRefined{
			Agent: agentID, AgentHandle: handle, RoleName: turn.Role(),
			TurnID: turn.ID, SkillName: updated.Name, SkillID: updated.ID,
			SkillVersion:   updated.Version,
			RefinementKind: string(learning.RefineTool),
		})
	}
	return tools.Result{Output: fmt.Sprintf(
		"Refined %q (now version %d). The previous version is kept.",
		updated.Name, updated.Version)}, nil
}
