package maintenance

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE ASK CREWLET INSTRUCTIONS OF A SEAT THAT LEFT, judged by the personal
// records' reconcile (seatrecords.go), at the Home board's grace so a seat
// restored inside it gets both back together.

// instructions is how the instructions' job is named and said.
var instructions = seatRecordKind{
	job:    "assistant_instructions",
	record: "Ask Crewlet instructions",
	kept:   "own instructions on how Ask Crewlet writes",
	lost:   "gets the company's instructions alone",
}

// InstructionsRecords is the slice of the coordination store's instructions
// this job calls; [coord.AssistantInstructions] satisfies it.
type InstructionsRecords interface {
	SeatInstructionsOwners(ctx context.Context) ([]coord.SeatInstructionsOwner, error)
	SetSeatInstructionsAbsence(ctx context.Context, handle string, since time.Time) (bool, error)
	ForgetSeatInstructions(ctx context.Context, handle string, absentBy time.Time) (bool, error)
}

// InstructionsJobs is the instructions' reconcile, for [Options.Jobs]
// ([seatRecordJobs]).
func InstructionsJobs(records InstructionsRecords, roster HumanSeatRoster) []Job {
	if records == nil {
		return nil
	}
	return seatRecordJobs(instructions, instructionsSeats{records}, roster, HomeBoardForgetGrace)
}

type instructionsSeats struct{ records InstructionsRecords }

func (s instructionsSeats) holders(ctx context.Context) ([]seatHolder, error) {
	owners, err := s.records.SeatInstructionsOwners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]seatHolder, 0, len(owners))
	for _, o := range owners {
		out = append(out, seatHolder{handle: o.Handle, absentSince: o.AbsentSince})
	}
	return out, nil
}

func (s instructionsSeats) setAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	return s.records.SetSeatInstructionsAbsence(ctx, handle, since)
}

func (s instructionsSeats) forget(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	return s.records.ForgetSeatInstructions(ctx, handle, absentBy)
}
