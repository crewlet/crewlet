package queries

import (
	"context"
	"encoding/json"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// HomeBoards is where each person's Home board is kept, as this surface reads
// it: the read half of [coord.HomeBoards]. A save is the operator catalogue's
// `save_home_board`.
type HomeBoards interface {
	HomeBoard(ctx context.Context, handle string) (coord.HomeBoard, uint64, bool, error)
}

// homeBoardAnswer is `home_board`: the caller's board, kept under their seat.
type homeBoardAnswer struct {
	// Seat is the handle the board is kept under, "" for a credential that
	// holds no human seat, which has no stored Home.
	Seat string `json:"seat"`

	// Board is the board exactly as saved, null when none is stored.
	Board json.RawMessage `json:"board"`

	// Revision is the stored board's, zero when none: the `if_match` of the
	// next save.
	Revision uint64 `json:"revision"`
}

// homeBoard answers the caller's own Home board. ABOUT THE CALLER AND NOBODY
// ELSE: it takes no handle, and reads under the seat [iam.ActorFor] names a
// person by, the name a save writes under ([iam.RecordOwner]). A credential
// bound to no seat, or a seat that is no person's, has none, which is an
// answer rather than a refusal: the dashboard then keeps that sign-in's board
// in the browser.
func (s Sources) homeBoard(ctx context.Context, _ Params) (any, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "home_board")
	}
	actor := iam.ActorFor(principal)
	if actor.Kind != iam.ActorHuman {
		return homeBoardAnswer{}, nil
	}
	board, revision, found, err := s.HomeBoards.HomeBoard(ctx, actor.Name)
	if err != nil {
		return nil, err
	}
	answer := homeBoardAnswer{Seat: actor.Name}
	if found {
		answer.Board, answer.Revision = board.Board, revision
	}
	return answer, nil
}
