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

// ---- the people's Home boards ------------------------------------------ //

// homeBoard is one seat's board and the revision it was written at, drawn
// from the store-wide counter so a board forgotten and saved again never
// hands back a revision its predecessor used ([Fleet.version]).
type homeBoard struct {
	board    coord.HomeBoard
	revision uint64
}

// cloneHomeBoard is b with a board of its own, so no caller edits the twin's.
func cloneHomeBoard(b coord.HomeBoard) coord.HomeBoard {
	b.Board = bytes.Clone(b.Board)
	return b
}

// HomeBoard is a seat's board and its revision, false where none is stored.
func (f *Fleet) HomeBoard(_ context.Context, handle string) (coord.HomeBoard, uint64, bool, error) {
	if handle == "" {
		return coord.HomeBoard{}, 0, false, errors.New("coord/memory: a Home board is read per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.homeBoards[handle]
	return cloneHomeBoard(held.board), held.revision, ok, nil
}

// SaveHomeBoard replaces a seat's board if it is still at revision, keeping
// its absence stamp.
func (f *Fleet) SaveHomeBoard(_ context.Context, handle string, board json.RawMessage,
	at time.Time, revision uint64) (uint64, bool, error) {

	if handle == "" {
		return 0, false, errors.New("coord/memory: a Home board is saved per seat")
	}
	if err := coord.CheckHomeBoard(board); err != nil {
		return 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.homeBoards[handle]
	if held.revision != revision {
		return 0, false, nil
	}
	f.version++
	f.homeBoards[handle] = homeBoard{
		board: coord.HomeBoard{Board: bytes.Clone(board), UpdatedAt: at.UTC(),
			AbsentSince: held.board.AbsentSince},
		revision: f.version,
	}
	return f.version, true, nil
}

// SetHomeBoardAbsence stamps or clears a board's seat's absence, reporting
// whether the seat has a board; one without is left without.
func (f *Fleet) SetHomeBoardAbsence(_ context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: a Home board's absence is stamped per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.homeBoards[handle]
	if !ok {
		return false, nil
	}
	if next, changed := held.board.WithAbsence(since); changed {
		f.version++
		f.homeBoards[handle] = homeBoard{board: next, revision: f.version}
	}
	return true, nil
}

// ForgetHomeBoard removes a seat's board if it was stamped absent at or before
// absentBy, reporting whether it did.
func (f *Fleet) ForgetHomeBoard(_ context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: a Home board is forgotten per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.homeBoards[handle]
	if !ok || !held.board.AbsentBy(absentBy) {
		return false, nil
	}
	delete(f.homeBoards, handle)
	return true, nil
}

// HomeBoardOwners is every seat holding a board, by handle, with its stamp.
func (f *Fleet) HomeBoardOwners(context.Context) ([]coord.HomeBoardOwner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owners := make([]coord.HomeBoardOwner, 0, len(f.homeBoards))
	for handle, held := range f.homeBoards {
		owners = append(owners, coord.HomeBoardOwner{Handle: handle, AbsentSince: held.board.AbsentSince})
	}
	slices.SortFunc(owners, func(a, b coord.HomeBoardOwner) int { return cmp.Compare(a.Handle, b.Handle) })
	return owners, nil
}
