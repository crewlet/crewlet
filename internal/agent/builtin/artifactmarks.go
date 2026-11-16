package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PERSON'S MARKS OVER THEIR COMMENT NOTICES: the operator tool
// `artifact_mark_inbox`, the artifacts log's half of what `mark_inbox` is on
// Worklet's and `code_mark_inbox` on Gitlet's, one gesture read by the one
// parser ([gestureOf]) and decided on the person's own reader subject
// (artifactsdomain's MarkInbox), the caller's `seen` floor travelling with it.
// A person's alone: a seat is woken, never shown an inbox.

// ArtifactMarkInboxTool is the tool's name, its authority row's action.
const ArtifactMarkInboxTool = string(authz.ActionArtifactMarkInbox)

type artifactMarkInbox struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*artifactMarkInbox)(nil)

func (t *artifactMarkInbox) Name() string { return ArtifactMarkInboxTool }

func (t *artifactMarkInbox) Description() string {
	return "Move your own ARTIFACTS inbox by one gesture: the notices about comments on artifacts the " +
		"`inbox` question lists. Name notices by their `record_id`: `read` marks them done, `unread` brings " +
		"them back, `snooze` puts them off until a time, `unsnooze` brings a snoozed one back now. " +
		"`read_through` reads everything up to a position on the artifacts log and never moves your " +
		"position backwards. Pass `seen`, the newest artifacts-log position you read, so the mark waits for " +
		"the notices you were shown. Your work notices are marked with " + worklet.MarkInboxTool +
		" and your code notices with " + CodeMarkInboxTool + ": each domain keeps its own marks."
}

func (t *artifactMarkInbox) Parameters() map[string]any {
	ids := func(description string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": description}
	}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"read":     ids("Record ids of comment notices to mark read: done."),
			"unread":   ids("Record ids of comment notices to mark unread again."),
			"unsnooze": ids("Record ids of comment notices whose snooze to lift now."),
			"snooze": map[string]any{
				"type":        "array",
				"description": "Comment notices to put off, each until an instant.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"record_id": map[string]any{"type": "string"},
						"until": map[string]any{
							"type": "string",
							"description": fmt.Sprintf("RFC3339; when it comes back. In the future, and at "+
								"most %d days away.", int(inbox.MaxSnoozeAhead.Hours()/24)),
						},
					},
					"required": []string{"record_id", "until"},
				},
			},
			"read_through": map[string]any{
				"type": "string",
				"description": "Read every comment notice at or before this position on the artifacts log, " +
					"`<stream>@<generation>:<sequence>`. A position at or behind the one you have read to " +
					"changes nothing.",
			},
			"seen": map[string]any{
				"type": "string",
				"description": "How far you have read the artifacts log, `<stream>@<generation>:<sequence>`: " +
					"the `position` of the newest comment notice you were shown. Omit it to wait for nothing.",
			},
			"primary_reasons": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Which comment reasons land in the PRIMARY half of your comment notices. One " +
					"of: " + words.Or(artifactsdomain.NotifyReasons) + ". An empty list takes the shipped " +
					"default (" + words.And(artifactsdomain.DefaultPrimaryReasons) + "); omit it to leave " +
					"your choice as it is.",
			},
		},
	})
}

func (t *artifactMarkInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *artifactMarkInbox) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	// A PERSON'S OWN, by the human seat they hold: the table admits nobody
	// else, and the writer refuses anybody else again.
	if c.actor.Kind != iam.ActorHuman || c.principal.Seat == "" {
		return refusedBy(tools.RefusalForbidden, artifactsdomain.ErrInvalid, fmt.Sprintf("%s marked nothing: "+
			"an artifacts inbox is a person's, marked by the person who holds it.", t.Name())), nil
	}
	gesture, bad := gestureOf(args, artifactsdomain.NotifyReasons, words.And(artifactsdomain.NotifyReasons))
	if bad != "" {
		return failed(bad), nil
	}
	seen, bad := markFloor(args, topics.ArtifactsLogStream, "artifacts log", "comment")
	if bad != "" {
		return failed(bad), nil
	}
	written, err := t.deps.Writer.MarkInbox(ctx, t.deps.call(c, "", false), artifactsdomain.MarkInput{
		Gesture: artifactsdomain.MarkGestureOf(gesture), Seen: seen})
	switch {
	case errors.Is(err, inbox.ErrFull):
		return refusedBy(tools.RefusalInboxFull, err, fmt.Sprintf("%s marked nothing: %s.", t.Name(),
			inbox.Sentence(err))), nil
	case errors.Is(err, artifactsdomain.ErrInvalid):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s marked nothing: %s. Change the "+
			"arguments; the same ones are refused again.", t.Name(), artifactsdomain.Sentence(err))), nil
	case err != nil:
		return publishFailure(ctx, t.Name(), c, err), nil
	case written.Result.Outcome == statelog.OutcomeUnknown:
		return artifactUnknown(t.Name(), c, written.Result, "Read your inbox again: a mark sent twice "+
			"changes nothing."), nil
	}
	answer, _ := jsonResult(map[string]any{"handle": c.principal.Seat, "outcome": string(written.Result.Outcome),
		"unchanged": written.Result.Position.IsZero(), "position": positionOf(written.Result.Position)})
	return answer, nil
}
