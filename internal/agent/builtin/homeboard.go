package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// SaveHomeBoardTool is the tool a person saves their Home board with, its
// authority row's action.
const SaveHomeBoardTool = string(authz.ActionHomeBoardSave)

// HomeBoardQuestion is the question a board and its revision are read with,
// which a refused save sends its caller back to.
const HomeBoardQuestion = "home_board"

// HomeBoardStore is where a person's Home board is kept: the coordination
// store's [coord.HomeBoards.SaveHomeBoard].
type HomeBoardStore interface {
	SaveHomeBoard(ctx context.Context, handle string, board json.RawMessage, at time.Time,
		revision uint64) (uint64, bool, error)
}

// HomeBoardDeps are what save_home_board saves through. The zero value omits
// the tool.
type HomeBoardDeps struct {
	// Boards is the store. Nil omits the tool.
	Boards HomeBoardStore

	// Actor is who saves, off the caller's own credential ([PrincipalActor]).
	// Required with Boards.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Now is the clock a save is stamped with; nil is the wall clock.
	Now func() time.Time
}

func (d HomeBoardDeps) serves() bool { return d.Boards != nil && d.Actor != nil }

func (d HomeBoardDeps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// saveHomeBoard replaces the caller's Home board whole, at the revision they
// read: a board states every widget at once, so a save from an older screen
// would put back what a newer one removed. The board is the dashboard's and
// the engine reads nothing inside it but that `widgets` is a list. Kept under
// the caller's seat ([iam.RecordOwner]); the table keeps it a seated person's.
type saveHomeBoard struct{ deps HomeBoardDeps }

var _ tools.SeatCallable = (*saveHomeBoard)(nil)

func (t *saveHomeBoard) Name() string { return SaveHomeBoardTool }

func (t *saveHomeBoard) Description() string {
	return "Save your own Home board on the dashboard: the widgets you placed and how you " +
		"arranged them, the whole board at once. Name the `revision` " + HomeBoardQuestion +
		" answered as `if_match` (0 when you have none stored); a board saved since you " +
		"read it is refused, so read " + HomeBoardQuestion + " again and decide from it."
}

func (t *saveHomeBoard) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"board": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"widgets": map[string]any{"type": "array"},
				},
				"required": []any{"widgets"},
				"description": fmt.Sprintf("The whole board, as the dashboard keeps it: an object "+
					"whose `widgets` is a list. Stored as given, up to %d bytes.",
					coord.MaxHomeBoardBytes),
			},
			"if_match": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "The `revision` " + HomeBoardQuestion + " answered, 0 when it " +
					"answered none: the save lands only if the board is still that one.",
			},
		},
		"required": []any{"board", "if_match"},
	}
}

func (t *saveHomeBoard) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *saveHomeBoard) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = SaveHomeBoardTool
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		return faulted(ctx, name, err, name+" could not tell who is saving, so it saved "+
			"nothing: "+faultSaid+"."), nil
	}
	// A HOME IS A SEATED PERSON'S. The table refuses everybody else; this holds
	// a surface wired without it, since the store keys a board by any handle.
	switch actor.Kind {
	case worklet.AuthorHuman:
	case worklet.AuthorAgent:
		return refused(tools.RefusalForbidden, name+" saved nothing: a Home board is a "+
			"person's, and a seat has no dashboard to arrange."), nil
	default:
		return refused(tools.RefusalForbidden, name+" saved nothing: this credential is "+
			"bound to no seat, and a Home board is kept under a person's seat. Bind it to a "+
			"human seat with `crewlet iam bind`."), nil
	}
	board, ok := args["board"].(map[string]any)
	if !ok {
		return failedArg("board", name+" needs a `board`: an object whose `widgets` is a "+
			"list, the whole board as the dashboard keeps it."), nil
	}
	if _, ok := argval.List(board["widgets"]); !ok {
		return failedArg("board", name+"'s `board` needs `widgets`, a list: empty for a "+
			"bare Home."), nil
	}
	if _, given := args["if_match"]; !given {
		return failedArg("if_match", name+" needs `if_match`: the `revision` "+
			HomeBoardQuestion+" answered, or 0 when it answered none."), nil
	}
	ifMatch := argInt(args, "if_match", -1)
	if ifMatch < 0 {
		return failedArg("if_match", "`if_match` is the `revision` "+HomeBoardQuestion+
			" answered, a whole number of zero or more."), nil
	}
	raw, err := plainjson.Marshal(board, "")
	if err == nil {
		err = coord.CheckHomeBoard(raw)
	}
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return failedArg("board", fmt.Sprintf("%s refused that board: %s. Nothing was saved.",
			name, coord.HomeBoardSentence(err))), nil
	}
	revision, landed, err := t.deps.Boards.SaveHomeBoard(ctx, actor.Handle, raw, t.deps.now(),
		uint64(ifMatch))
	switch {
	case errors.Is(err, coord.ErrInvalidHomeBoard):
		return failedArg("board", fmt.Sprintf("%s refused that board: %s. Nothing was saved.",
			name, coord.HomeBoardSentence(err))), nil
	case errors.Is(err, coord.ErrWriteUnknown):
		return failedUnknown("", false, name+": whether this landed is unknown: the "+
			"coordination store took the write and its answer was lost. Read "+
			HomeBoardQuestion+" before saving again: its `revision` says whether this board "+
			"is the one stored."), nil
	case err != nil:
		return nodeFailure(ctx, name, err, name+" could not save the board", "Nothing was saved."), nil
	case !landed:
		return refused(tools.RefusalStaleVersion, fmt.Sprintf("%s was refused: the Home board "+
			"is no longer at revision %d. Nothing is wrong with your board; it was saved "+
			"elsewhere after you read it. Call %s again and decide from the board it shows now.",
			name, ifMatch, HomeBoardQuestion)), nil
	}
	return jsonResult(ownRecordAnswer{Seat: actor.Handle, Revision: revision,
		Outcome: statelog.OutcomeApplied})
}

// ownRecordAnswer is what a save of one's own record (a board, Ask Crewlet
// instructions) that landed answers.
type ownRecordAnswer struct {
	// Seat is the handle the record is kept under.
	Seat string `json:"seat"`

	// Revision is the stored record's new revision: the `if_match` of the
	// next save, so a screen saves again without reading.
	Revision uint64 `json:"revision"`

	// Outcome is always applied, and Position always null: a save is a
	// compare-and-set in the coordination store, on no log, landed when it
	// answers ([markChatReadAnswer]).
	Outcome  statelog.Outcome `json:"outcome"`
	Position *string          `json:"position"`
}
