package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PERSON'S MARKS OVER THEIR CODE NOTICES: the operator tool
// `code_mark_inbox`, the code log's half of what `mark_inbox` is on
// Worklet's. A person's inbox is composed from every native domain's notices,
// and each domain keeps the marks over its own on its own log
// (internal/inbox): a position is a coordinate on ONE stream, so a gesture
// crossing two logs would be two decides that cannot be one. So each domain's
// marks are written by a verb of its own, and a screen's "mark all read" sends
// both.
//
// ONE GESTURE, NEVER THE LISTS — the arguments mark_inbox takes, read by the
// one parser ([gestureOf]) over the code host's reasons — decided by the code
// host on the person's own record (code.Marks).
//
// THE OPERATOR CATALOGUE'S ALONE, and a PERSON'S: a seat has no inbox to mark
// — it is woken, never shown a list — and a person's marks are their own,
// which the authority table holds (authz.ActionCodeMarkInbox: own record,
// human-only) and the code host's writer holds again, refusing a mark of
// anybody else's record whatever admitted it.

// CodeMarkInboxTool is the tool's name — its authority row's action.
const CodeMarkInboxTool = string(authz.ActionCodeMarkInbox)

// codeMarkVerb is the verb a mark's operation is named by.
const codeMarkVerb = "code-mark"

type codeMarkInbox struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeMarkInbox)(nil)

func (t *codeMarkInbox) Name() string { return CodeMarkInboxTool }

func (t *codeMarkInbox) Description() string {
	return "Move your own CODE inbox by one gesture — the notices about changes the `inbox` question lists " +
		"from the code host. Name notices by their `record_id`: `read` marks them done, `unread` brings them " +
		"back, `snooze` puts them off until a time, `unsnooze` brings a snoozed one back now. `read_through` " +
		"reads everything up to a position on the code log — the newest code notice you have seen, as " +
		"`<stream>@<generation>:<sequence>` — and never moves your position backwards. Only what you name " +
		"changes. Your work notices are marked with " + worklet.MarkInboxTool + ": each domain keeps its own marks."
}

func (t *codeMarkInbox) Parameters() map[string]any {
	ids := func(description string) map[string]any {
		return map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"description": description,
		}
	}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"read":     ids("Record ids of code notices to mark read — done."),
			"unread":   ids("Record ids of code notices to mark unread again."),
			"unsnooze": ids("Record ids of code notices whose snooze to lift now."),
			"snooze": map[string]any{
				"type":        "array",
				"description": "Code notices to put off, each until an instant.",
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
				"description": "Read every code notice at or before this position on the code log, " +
					"`<stream>@<generation>:<sequence>` — a code notice's `log_stream`, `log_generation` and " +
					"`log_seq`. A position at or behind the one you have read to changes nothing.",
			},
			"primary_reasons": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Which code reasons land in the PRIMARY half of your code notices, the rest " +
					"being context. One of: " + words.Or(code.Reasons()) + ". An empty list takes the " +
					"shipped default (" + words.And(code.DefaultPrimaryReasons) + ") rather than making " +
					"nothing primary; omit it to leave your choice as it is.",
			},
		},
	})
}

func (t *codeMarkInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeMarkInbox) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.writeCaller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	// A PERSON'S OWN, by the human seat they hold — the table admits nobody
	// else, and a seat that reached this call anyway marks nothing.
	if c.asker.Kind != code.PrincipalOperator || c.asker.Seat == "" {
		return refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, fmt.Sprintf("%s marked nothing: a code "+
			"inbox is a person's, marked by the person who holds it.", t.Name())), nil
	}
	actor, bad := t.deps.bindOperation(c.actor, t.Name(), args)
	if bad != "" {
		return failed(bad), nil
	}
	gesture, bad := gestureOf(args, code.Reasons(), words.And(code.Reasons()))
	if bad != "" {
		return failed(bad), nil
	}
	handle := c.asker.Seat
	opID := opIDFor(actor, t.Name(), codeMarkVerb, handle, args)
	res, err := t.deps.Writer.Mark(ctx, code.MarkWrite{OpID: opID, Asker: c.asker, Handle: handle,
		Gesture: code.MarkGestureOf(gesture)})
	if err != nil {
		var unserved *estate.ErrUnserved
		if errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent() {
			return unknownWrite(actor, t.Name(), fmt.Sprintf("%s's code inbox was marked", handle), opID, false,
				unknownNext(false, sameCall(actor, t.Name()), "Read your inbox again",
					"it marks the same notices again, which changes nothing")), nil
		}
		return codeWriteFailure(ctx, t.Name(), actor, "the code host", err), nil
	}
	if res.Refusal != "" {
		return markRefused(t.Name(), res), nil
	}
	if res.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, t.Name(), fmt.Sprintf("%s's code inbox was marked", handle), opID, false,
			unknownNext(false, sameCall(actor, t.Name()), "Read your inbox again",
				"it marks the same notices again, which changes nothing")), nil
	}
	answer, _ := jsonResult(withOperation(map[string]any{
		"handle": handle, "outcome": string(res.Outcome), "unchanged": res.Unchanged,
		"position": positionOf(res.Position),
	}, actor))
	return answer, nil
}

// markRefused is a mark the code host refused, in its class: a mark of
// somebody else's record FORBIDDEN, a list past its ceiling INBOX_FULL, a
// gesture the rules refuse INVALID, and a record that kept moving or a log
// that refused it UNAVAILABLE — nothing was written, and the same call is the
// same write.
func markRefused(tool string, res code.MarkResult) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	said := fmt.Sprintf("%s marked nothing: %s (%s).", tool, res.Detail, res.Refusal)
	switch res.Refusal {
	case code.MarkNotPermitted:
		return refusedBy(tools.RefusalForbidden, cause, said+" That is a decision about who is asking, so the "+
			"same call is refused again.")
	case code.MarkFull:
		return refusedBy(tools.RefusalInboxFull, cause, said)
	case code.MarkInvalid:
		return refusedBy(tools.RefusalInvalid, cause, said+" Change the arguments — the same ones are refused "+
			"again.")
	case code.MarkBusy, code.MarkUnavailable:
		return refusedBy(tools.RefusalUnavailable, cause, said+" Nothing was written; make the same call again "+
			"— it is the same write.")
	}
	return refusedBy(tools.RefusalInternalError, cause, said+" "+faultSaid+".")
}
