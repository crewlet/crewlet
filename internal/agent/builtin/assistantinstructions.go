package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// SaveInstructionsTool is the tool a person saves their own Ask Crewlet
// instructions with, its authority row's action.
const SaveInstructionsTool = string(authz.ActionAssistantInstructionsSave)

// InstructionsQuestion is the question a person's instructions and their
// revision are read with, which a refused save sends its caller back to.
const InstructionsQuestion = "assistant_instructions"

// InstructionsStore is where a person's instructions are kept: the
// coordination store's [coord.AssistantInstructions.SaveSeatInstructions].
type InstructionsStore interface {
	SaveSeatInstructions(ctx context.Context, handle string, doc json.RawMessage, at time.Time,
		revision uint64) (uint64, bool, error)
}

// InstructionsDeps are what save_assistant_instructions saves through, as
// [HomeBoardDeps] are for a board. The zero value omits the tool.
type InstructionsDeps struct {
	Store InstructionsStore
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
	Now   func() time.Time
}

func (d InstructionsDeps) serves() bool { return d.Store != nil && d.Actor != nil }

// saveInstructions replaces the caller's own instructions whole, at the
// revision they read, under their seat ([iam.RecordOwner]), as a Home board is
// saved. The document is held to [config.AssistantInstructions.Validate], the
// company layer's own rules, so a credential is refused before it is stored.
type saveInstructions struct{ deps InstructionsDeps }

var _ tools.SeatCallable = (*saveInstructions)(nil)

func (t *saveInstructions) Name() string { return SaveInstructionsTool }

func (t *saveInstructions) Description() string {
	return "Save your own instructions on how Ask Crewlet writes to you: answer length, " +
		"layout and free text, all at once, applied on top of the company's. Name the " +
		"`revision` " + InstructionsQuestion + " answered as `if_match` (0 when you have none " +
		"stored); instructions saved since you read them are refused."
}

func (t *saveInstructions) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"instructions": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"length": map[string]any{"type": "string", "enum": config.AnswerLengths,
						"description": "How long answers run; leave it out for the default."},
					"format": map[string]any{"type": "string", "enum": config.AnswerFormats,
						"description": "How answers are laid out; leave it out for the default."},
					"text": map[string]any{"type": "string",
						"description": fmt.Sprintf("Anything else about how Ask Crewlet "+
							"writes, in the person's own words, at most %d characters. Never "+
							"a credential.", config.AssistantInstructionsMaxRunes)},
				},
				"description": "The whole of the person's instructions; an empty object clears them.",
			},
			"if_match": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "The `revision` " + InstructionsQuestion + " answered, 0 when it " +
					"answered none: the save lands only if the instructions are still those.",
			},
		},
		"required": []any{"instructions", "if_match"},
	}
}

func (t *saveInstructions) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *saveInstructions) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = SaveInstructionsTool
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		return faulted(ctx, name, err, name+" could not tell who is saving, so it saved "+
			"nothing: "+faultSaid+"."), nil
	}
	switch actor.Kind {
	case worklet.AuthorHuman:
	case worklet.AuthorAgent:
		return refused(tools.RefusalForbidden, name+" saved nothing: Ask Crewlet's "+
			"instructions are a person's, and a seat does not ask it."), nil
	default:
		return refused(tools.RefusalForbidden, name+" saved nothing: this credential is "+
			"bound to no seat, and instructions are kept under a person's seat. Bind it to a "+
			"human seat with `crewlet iam bind`."), nil
	}
	in, err := decodeInstructions(args["instructions"])
	if err == nil {
		err = in.Validate(config.Path{"instructions"})
	}
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return failedArg("instructions", fmt.Sprintf("%s refused those instructions: %s. "+
			"Nothing was saved.", name, err)), nil
	}
	if _, given := args["if_match"]; !given {
		return failedArg("if_match", name+" needs `if_match`: the `revision` "+
			InstructionsQuestion+" answered, or 0 when it answered none."), nil
	}
	ifMatch := argInt(args, "if_match", -1)
	if ifMatch < 0 {
		return failedArg("if_match", "`if_match` is the `revision` "+InstructionsQuestion+
			" answered, a whole number of zero or more."), nil
	}
	raw, err := plainjson.Marshal(in, "")
	if err != nil {
		return faulted(ctx, name, err, name+" could not encode the instructions: "+faultSaid+"."), nil
	}
	now := time.Now
	if t.deps.Now != nil {
		now = t.deps.Now
	}
	revision, landed, err := t.deps.Store.SaveSeatInstructions(ctx, actor.Handle, raw, now().UTC(),
		uint64(ifMatch))
	switch {
	case errors.Is(err, coord.ErrInvalidAssistantInstructions):
		return failedArg("instructions", fmt.Sprintf("%s refused those instructions: %s. "+
			"Nothing was saved.", name, coord.AssistantInstructionsSentence(err))), nil
	case errors.Is(err, coord.ErrWriteUnknown):
		return failedUnknown("", false, name+": whether this landed is unknown: the "+
			"coordination store took the write and its answer was lost. Read "+
			InstructionsQuestion+" before saving again."), nil
	case err != nil:
		return nodeFailure(ctx, name, err, name+" could not save the instructions", "Nothing was saved."), nil
	case !landed:
		return refused(tools.RefusalStaleVersion, fmt.Sprintf("%s was refused: the instructions "+
			"are no longer at revision %d; they were saved elsewhere after you read them. Read %s "+
			"again and decide from what it shows now.", name, ifMatch, InstructionsQuestion)), nil
	}
	return jsonResult(ownRecordAnswer{Seat: actor.Handle, Revision: revision,
		Outcome: statelog.OutcomeApplied})
}

// decodeInstructions reads the tool's `instructions` argument, refusing a
// member the document does not have.
func decodeInstructions(arg any) (config.AssistantInstructions, error) {
	var in config.AssistantInstructions
	if _, ok := arg.(map[string]any); !ok {
		return in, errors.New("`instructions` is an object of length, format and text")
	}
	raw, err := json.Marshal(arg)
	if err != nil {
		return in, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, errors.New("`instructions` holds only length, format and text, each a string")
	}
	return in, nil
}
