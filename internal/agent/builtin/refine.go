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
// A WHOLE-BODY REPLACEMENT, not a patch: a model's diff often does not apply,
// and a half-applied edit leaves a procedure that is neither old nor new. Two
// complete bodies are also what a person compares side by side.
//
// The prior body is kept (learning.RefineTool), which is what makes this safe
// to offer an agent: a refinement that made a skill worse is one rollback away.
type refineSkill struct {
	skills RefinableSkills

	// bodyMax is learning.skill_refinement.max_body_chars: the ceiling on
	// the whole procedure after the edit.
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
				"type": "string",
				"description": fmt.Sprintf(
					"Why it needed correcting — read by a person later. At most "+
						"%d characters — longer is refused, not shortened.", diaryNoteMax),
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
		return refused(tools.RefusalForbidden, RefineSkillTool+" can only be called during a turn, on behalf of a seat."), nil
	}
	if t.skills == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(), "Skill synthesis is not configured on this deployment."), nil
	}

	name := strings.TrimSpace(argString(args, "skill_name"))
	content := strings.TrimSpace(argString(args, "content"))
	reason := strings.TrimSpace(argString(args, "reason"))
	switch {
	case name == "":
		return failed(RefineSkillTool + " needs a `skill_name`."), nil
	case content == "":
		return failed(RefineSkillTool + " needs `content`: the FULL corrected " +
			"procedure. It replaces the current body, so a partial edit " +
			"would delete the rest of the skill."), nil
	case len(reason) > diaryNoteMax:
		// REFUSED, not clipped, as the body cap below is, and before any
		// store work so the model fixes it in the same round.
		return failed(fmt.Sprintf(
			"That `reason` is %d characters and is capped at %d. Say what "+
				"practice superseded the old text, in a sentence.",
			len(reason), diaryNoteMax)), nil
	case t.bodyMax > 0 && len(content) > t.bodyMax:
		// REFUSED, not truncated: a clip lands mid-step and the seat follows
		// the remainder as the whole procedure. The cap stops a body that
		// grows an annotation per turn; the remedy is tighter text.
		return failed(fmt.Sprintf(
			"That body is %d characters and this company caps a skill at %d "+
				"(learning.skill_refinement.max_body_chars). Tighten it: drop "+
				"what practice has superseded rather than appending to it.",
			len(content), t.bodyMax)), nil
	}

	// This seat's own skills only: no agent rewrites another's procedure.
	sk, found, err := t.skills.Get(ctx, handle, name)
	if err != nil {
		return nodeFailure(ctx, RefineSkillTool, err, fmt.Sprintf("Could not load %q", clip(name)), ""), nil
	}
	if !found {
		return refused(tools.RefusalNotFound, fmt.Sprintf(
			"You have no synthesized skill called %q, so there is nothing to "+
				"refine. Skills are distilled from your own completed turns.",
			clip(name))), nil
	}

	rev := sk.Revision()
	rev.Content = content
	if reason != "" {
		// The reason goes on the REFINEMENT, not into the body: a body
		// carrying its changelog grows per edit in every prompt that loads it.
		rev.Description = sk.Description
	}
	updated, err := t.skills.Update(ctx, sk.ID, rev, learning.Refinement{
		Kind:         learning.RefineTool,
		Note:         reason,
		KeepVersions: t.versions,
		At:           time.Now().UTC(),
	})
	if err != nil {
		return nodeFailure(ctx, RefineSkillTool, err, fmt.Sprintf("Could not refine %q", clip(name)),
			"The skill is as it was."), nil
	}
	// The VERSION tells successive refinements apart, the kind a success
	// annotation from a counter-example. Published after the write, so the
	// feed never names a version no rollback can reach.
	if agentID, why := seatAgentID(turn); why == "" {
		note(ctx, t.events, turn, types.SkillRefined{
			Agent: agentID, AgentHandle: handle, RoleName: turn.Role(),
			TurnID: turn.RunID, WorkKey: turn.WorkKey,
			SkillName: updated.Name, SkillID: updated.ID,
			SkillVersion:   updated.Version,
			RefinementKind: string(learning.RefineTool),
		})
	}
	return tools.Result{Output: fmt.Sprintf(
		"Refined %q (now version %d). The previous version is kept.",
		updated.Name, updated.Version)}, nil
}
