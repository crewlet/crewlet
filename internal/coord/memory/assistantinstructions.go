package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// seatInstructions is one seat's held instructions and their revision.
type seatInstructions struct {
	held     coord.SeatInstructions
	revision uint64
}

func cloneSeatInstructions(s coord.SeatInstructions) coord.SeatInstructions {
	s.Instructions = bytes.Clone(s.Instructions)
	return s
}

// SeatInstructions implements [coord.AssistantInstructions].
func (f *Fleet) SeatInstructions(_ context.Context, handle string) (coord.SeatInstructions, uint64, bool, error) {
	if handle == "" {
		return coord.SeatInstructions{}, 0, false, errors.New("coord/memory: instructions are read per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instructions[handle]
	return cloneSeatInstructions(s.held), s.revision, ok, nil
}

// SaveSeatInstructions implements [coord.AssistantInstructions].
func (f *Fleet) SaveSeatInstructions(_ context.Context, handle string, doc json.RawMessage,
	at time.Time, revision uint64) (uint64, bool, error) {

	if handle == "" {
		return 0, false, errors.New("coord/memory: instructions are saved per seat")
	}
	if err := coord.CheckAssistantInstructions(doc); err != nil {
		return 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.instructions[handle]
	if s.revision != revision {
		return 0, false, nil
	}
	f.version++
	f.instructions[handle] = seatInstructions{
		held: coord.SeatInstructions{Instructions: bytes.Clone(doc), UpdatedAt: at.UTC(),
			AbsentSince: s.held.AbsentSince},
		revision: f.version,
	}
	return f.version, true, nil
}

// SetSeatInstructionsAbsence implements [coord.AssistantInstructions].
func (f *Fleet) SetSeatInstructionsAbsence(_ context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: an absence on instructions is stamped per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instructions[handle]
	if !ok {
		return false, nil
	}
	if next, changed := s.held.WithAbsence(since); changed {
		f.version++
		f.instructions[handle] = seatInstructions{held: next, revision: f.version}
	}
	return true, nil
}

// ForgetSeatInstructions implements [coord.AssistantInstructions].
func (f *Fleet) ForgetSeatInstructions(_ context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: instructions are forgotten per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instructions[handle]
	if !ok || !s.held.AbsentBy(absentBy) {
		return false, nil
	}
	delete(f.instructions, handle)
	return true, nil
}

// SeatInstructionsOwners implements [coord.AssistantInstructions].
func (f *Fleet) SeatInstructionsOwners(context.Context) ([]coord.SeatInstructionsOwner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owners := make([]coord.SeatInstructionsOwner, 0, len(f.instructions))
	for handle, s := range f.instructions {
		owners = append(owners, coord.SeatInstructionsOwner{Handle: handle, AbsentSince: s.held.AbsentSince})
	}
	slices.SortFunc(owners, func(a, b coord.SeatInstructionsOwner) int { return cmp.Compare(a.Handle, b.Handle) })
	return owners, nil
}
