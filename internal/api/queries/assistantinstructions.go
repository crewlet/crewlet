package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// InstructionsStore is where each person's own Ask Crewlet instructions are
// kept, as this surface reads them: the read half of
// [coord.AssistantInstructions]. A save is the operator catalogue's
// `save_assistant_instructions`.
type InstructionsStore interface {
	SeatInstructions(ctx context.Context, handle string) (coord.SeatInstructions, uint64, bool, error)
}

// instructionsAnswer is `assistant_instructions`: both layers of how Ask
// Crewlet writes to the caller, the company's beside their own.
type instructionsAnswer struct {
	// Seat is the handle their own are kept under, "" for a credential that
	// holds no human seat, which keeps none.
	Seat string `json:"seat"`

	// Company is the company's layer, which every member reads: it shapes
	// their answers, though only an administrator changes it.
	Company config.AssistantInstructions `json:"company"`

	// Mine is the caller's own layer, and Revision its stored revision, zero
	// for none: the `if_match` of the next save.
	Mine     config.AssistantInstructions `json:"mine"`
	Revision uint64                       `json:"revision"`
}

// assistantInstructions answers the caller's instructions and the company's,
// ABOUT THE CALLER AND NOBODY ELSE, under the seat a save writes under, as
// [Sources.homeBoard] does.
func (s Sources) assistantInstructions(ctx context.Context, _ Params) (any, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "assistant_instructions")
	}
	var answer instructionsAnswer
	if s.Company != nil {
		if c, _ := s.Company(); c != nil {
			answer.Company = c.Assistant.Instructions
		}
	}
	actor := iam.ActorFor(principal)
	if actor.Kind != iam.ActorHuman {
		return answer, nil
	}
	answer.Seat = actor.Name
	held, revision, found, err := s.Instructions.SeatInstructions(ctx, actor.Name)
	if err != nil {
		return nil, err
	}
	if found {
		answer.Mine, answer.Revision = config.ReadAssistantInstructions(held.Instructions), revision
	}
	return answer, nil
}
