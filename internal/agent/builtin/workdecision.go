package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A DECISION IS AN ARGUMENT OF TWO TOOLS, stated once.
//
// `comment_on_work_item` asks a question on an item that exists, and
// `create_work_item` files an item AS a question — the "ask an agent"
// gesture, one record rather than a create and a comment. Both take the same
// `decision` object with the same schema, the same parse and the same checks
// against the world, because two copies of a structure a model composes from a
// schema are two chances to describe it differently, and the model reads
// whichever it met last.

// decisionSchema is the `decision` argument's JSON Schema.
//
// EVERY CAP IS IN A DESCRIPTION, read off the tracker's own constants, so the
// number a model is told is the number it is refused at.
func decisionSchema() map[string]any {
	option := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "A short slug the answer names in `choice`: 1 " +
					"to 32 of a-z, 0-9, `_` and `-`.",
			},
			"label": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The option as a button would "+
					"say it, at most %d bytes.", tracker.MaxOptionLabel),
			},
			"detail": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What choosing it means — its "+
					"cost, its risk, what happens next. At most %d bytes.",
					tracker.MaxOptionDetail),
			},
		},
		"required": []any{"id", "label"},
	}
	evidence := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []any{
					string(tracker.EvidenceTask), string(tracker.EvidencePage),
					string(tracker.EvidenceTurn), string(tracker.EvidenceRun),
					string(tracker.EvidenceURL),
				},
			},
			"ref": map[string]any{
				"type": "string",
				"description": "A work item's key or id, a page's id or " +
					"CONTAINER/Title, a turn or run id, or an https:// URL.",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "What it is, as a link would say it.",
			},
		},
		"required": []any{"kind", "ref"},
	}
	return map[string]any{
		"type": "object",
		"description": "Structure the question when somebody has to CHOOSE: " +
			"the options, the one you recommend and why, and what you looked " +
			"at. Only with `ask`. The person answers by naming an option in " +
			"`choice`, and you are woken with their choice.",
		"properties": map[string]any{
			"question": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What is being decided, in one "+
					"sentence of at most %d bytes. The context goes in `body`.",
					tracker.MaxDecisionQuestion),
			},
			"options": map[string]any{
				"type": "array",
				"description": fmt.Sprintf("%d to %d choices — two to four "+
					"is what a person weighs well.", tracker.MinDecisionOptions,
					tracker.MaxDecisionOptions),
				"items": option,
			},
			"recommended": map[string]any{
				"type":        "string",
				"description": "The id of the option you would choose.",
			},
			"rationale": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Why you recommend it, at most %d "+
					"bytes.", tracker.MaxDecisionRationale),
			},
			"evidence": map[string]any{
				"type": "array",
				"description": fmt.Sprintf("What you looked at, at most %d. A "+
					"work item or page must exist.", tracker.MaxDecisionEvidence),
				"items": evidence,
			},
			"role": map[string]any{
				"type": "string",
				"enum": []any{string(tracker.RoleApprover), string(tracker.RoleContributor)},
				"description": "`approver` when their answer IS the decision; " +
					"`contributor` when it is an input to one somebody else makes.",
			},
			"inform": map[string]any{
				"type": "object",
				"description": "The chat channel you will report the outcome in " +
					"once it is decided.",
				"properties": map[string]any{
					"surface": map[string]any{
						"type": "string",
						"enum": []any{string(tracker.InformMattermost), string(tracker.InformSlack)},
					},
					"channel": map[string]any{"type": "string"},
				},
				"required": []any{"surface", "channel"},
			},
		},
		"required": []any{"question", "options", "role"},
	}
}

// readDecision parses and checks a `decision` argument, or returns the refusal.
//
// STRICT ABOUT KEYS, because every one a model misspells is a part of the
// question the person deciding never sees: `recommendation` for `recommended`
// would store a decision with no recommendation and report it applied. So an
// unknown key is refused by name rather than dropped.
//
// THE WORLD IS CHECKED HERE, before anything is published: a task the
// evidence names is resolved to its id and a page must exist
// ([tracker.ValidateDecision]). A lookup that FAILS is the node's condition,
// and is answered as one.
func (d WorkDeps) readDecision(ctx context.Context, tool string, raw any,
	pageReader PageReader) (*tracker.Decision, *tools.Result) {

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, refusalOf(failed(fmt.Sprintf("%s: `decision` is not a JSON "+
			"object (%v).", tool, err)))
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decision tracker.Decision
	if err = decoder.Decode(&decision); err != nil {
		return nil, refusalOf(failed(fmt.Sprintf("%s: `decision` does not have "+
			"the shape its schema describes (%v). It takes question, options "+
			"[{id, label, detail}], recommended, rationale, evidence [{kind, ref, "+
			"label}], role and inform {surface, channel}.", tool, err)))
	}
	checked, err := tracker.ValidateDecision(ctx, decision, evidenceLookup{
		work: d, pages: pageReader,
	})
	switch {
	case errors.Is(err, tracker.ErrInvalid):
		return nil, refusalOf(failed(fmt.Sprintf("%s was refused: %v. Nothing "+
			"was posted.", tool, err)))
	case err != nil:
		return nil, refusalOf(readFailure(tool, err))
	}
	return &checked, nil
}

// evidenceLookup is the world a decision's evidence is checked against: this
// surface's own tracker reader and knowledge-base reader.
type evidenceLookup struct {
	work  WorkDeps
	pages PageReader
}

var _ tracker.EvidenceLookup = evidenceLookup{}

// TaskID resolves a key or an id through the same point read every other
// reference in these tools takes.
func (l evidenceLookup) TaskID(ctx context.Context, ref string) (string, error) {
	if l.work.Reader == nil {
		return "", errors.New("this surface has no tracker reader to check " +
			"a work item against")
	}
	got, err := l.work.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
	if err != nil {
		return "", err
	}
	return got.Task.ID, nil
}

// PageID resolves a page by id or by address to a live page's id.
//
// A COMPANY WITH NO NATIVE KNOWLEDGE BASE CANNOT CITE A PAGE by kind: there is
// nothing to check it against, and a reference nothing checked is the dead
// link the check exists to prevent. That is the caller's to change — cite it
// as a url — so it is refused as invalid rather than reported as a failure.
// A trashed page is not live: it is where nobody deciding can read it.
func (l evidenceLookup) PageID(ctx context.Context, ref string) (string, bool, error) {
	if l.pages == nil {
		return "", false, fmt.Errorf("%w: this company has no native knowledge "+
			"base, so a `page` cannot be checked — cite it as a `url`",
			tracker.ErrInvalid)
	}
	detail, err := l.pages.Get(ctx, strings.TrimSpace(ref), seatRead)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	case detail.Page.Status == pages.StatusTrashed:
		return "", false, nil
	}
	return detail.Page.ID, true, nil
}
