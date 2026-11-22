package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// landingBoards is a Home board store whose every save lands one revision
// past the one named, for the walks that call a write more than once.
type landingBoards struct{}

func (landingBoards) SaveHomeBoard(_ context.Context, _ string, _ json.RawMessage, _ time.Time,
	revision uint64) (uint64, bool, error) {

	return revision + 1, true, nil
}

// failingBoards is a store whose every save fails with err.
type failingBoards struct{ err error }

func (f failingBoards) SaveHomeBoard(context.Context, string, json.RawMessage, time.Time,
	uint64) (uint64, bool, error) {

	return 0, false, f.err
}

var boardNow = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

// boardTool is the operator catalogue's save_home_board over store, decided
// by authorize (the real table when nil).
func boardTool(t *testing.T, store builtin.HomeBoardStore, authorize builtin.Authorizer) tools.Callable {
	t.Helper()
	if authorize == nil {
		authorize = builtin.Decide(seatLeads)
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		HomeBoard: builtin.HomeBoardDeps{Boards: store, Actor: builtin.PrincipalActor,
			Now: func() time.Time { return boardNow }},
		Authorize: authorize,
	}) {
		if tool.Name() == builtin.SaveHomeBoardTool {
			return tool
		}
	}
	t.Fatal("the operator catalogue serves no save_home_board with a store wired")
	return nil
}

func saveBoard(t *testing.T, tool tools.Callable, as iam.Principal, args map[string]any) (tools.Result, map[string]any) {
	t.Helper()
	res, err := tool.Call(builtin.OutsideAnyTurn(iam.WithPrincipal(context.Background(), as)), args)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("answered %q, not JSON: %v", res.Output, err)
		}
	}
	return res, out
}

func boardArgs(ifMatch int, widgets ...any) map[string]any {
	if widgets == nil {
		widgets = []any{}
	}
	return map[string]any{"board": map[string]any{"widgets": widgets}, "if_match": json.Number(strconv.Itoa(ifMatch))}
}

// A BOARD LANDS UNDER THE CALLER'S SEAT AT THE REVISION THEY READ, answering
// that seat and the new revision so the next save needs no read; a save at a
// revision that moved is refused stale_version, naming the read to make, and
// changes nothing.
//
// Mutation: save without the revision, and the second save lands.
func TestAHomeBoardLandsAtTheRevisionReadUnderTheCallersSeat(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	tool := boardTool(t, fleet, nil)
	jane := boundTo("jane")
	note := map[string]any{"id": "n-1", "kind": "note", "text": "a <b> & c"}
	res, out := saveBoard(t, tool, jane, boardArgs(0, note))
	if res.Failed || out["seat"] != "jane" || out["outcome"] != "applied" {
		t.Fatalf("the first save answered %q", res.Output)
	}
	if position, stated := out["position"]; !stated || position != nil {
		t.Errorf("the save answered position %v, want null: a board is on no log", position)
	}
	stored, revision, found, err := fleet.HomeBoard(t.Context(), "jane")
	if err != nil || !found || out["revision"] != float64(revision) ||
		string(stored.Board) != `{"widgets":[{"id":"n-1","kind":"note","text":"a <b> & c"}]}` ||
		!stored.UpdatedAt.Equal(boardNow) {
		t.Fatalf("stored %s at %d (%v, %v), answered %v", stored.Board, revision, found, err, out)
	}
	res, _ = saveBoard(t, tool, jane, boardArgs(0))
	if !res.Failed || tools.RefusalOf(res) != tools.RefusalStaleVersion ||
		!strings.Contains(res.Output, builtin.HomeBoardQuestion) {
		t.Fatalf("a save at a revision that moved answered %+v, want stale_version naming %s",
			res, builtin.HomeBoardQuestion)
	}
	if _, again, _, _ := fleet.HomeBoard(t.Context(), "jane"); again != revision {
		t.Fatalf("a refused save moved the board to %d", again)
	}
	res, out = saveBoard(t, tool, jane, boardArgs(int(revision)))
	if res.Failed || out["revision"] == float64(revision) {
		t.Fatalf("a save at the revision read answered %q", res.Output)
	}
}

// WHAT THE ENGINE REFUSES OF A BOARD IS REFUSED BY NAME, before the store:
// a board that is no object, widgets that are no list, a revision missing or
// negative, a board past the bound. Nothing is stored.
func TestARefusedBoardNamesWhatToChangeAndStoresNothing(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	tool := boardTool(t, fleet, nil)
	huge := strings.Repeat("x", coord.MaxHomeBoardBytes)
	for name, args := range map[string]map[string]any{
		"no board":         {"if_match": json.Number("0")},
		"a list":           {"board": []any{}, "if_match": json.Number("0")},
		"no widgets":       {"board": map[string]any{"layout": []any{}}, "if_match": json.Number("0")},
		"widgets object":   {"board": map[string]any{"widgets": map[string]any{}}, "if_match": json.Number("0")},
		"no revision":      {"board": map[string]any{"widgets": []any{}}},
		"a negative":       {"board": map[string]any{"widgets": []any{}}, "if_match": json.Number("-1")},
		"past the bound":   boardArgs(0, map[string]any{"kind": "note", "text": huge}),
		"a revision of no": {"board": map[string]any{"widgets": []any{}}, "if_match": "seven"},
	} {
		res, _ := saveBoard(t, tool, boundTo("jane"), args)
		if !res.Failed || tools.RefusalOf(res) != tools.RefusalInvalid {
			t.Errorf("%s answered %+v, want an invalid argument", name, res)
		}
	}
	if owners, _ := fleet.HomeBoardOwners(t.Context()); len(owners) != 0 {
		t.Fatalf("refused saves stored %v", owners)
	}
}

// A HOME IS A SEATED PERSON'S. The table refuses an agent and a credential
// bound to no seat before the tool runs, and the tool refuses them again for a
// surface wired without the table, since the store keys a board by any name.
//
// Mutation: drop the tool's own refusal, and the permissive surface stores a
// board under the agent's handle.
func TestOnlyASeatedPersonSavesAHomeBoard(t *testing.T) {
	t.Parallel()
	agent := iam.Principal{Kind: iam.KindSeat, Seat: "agent-cto", Login: "agent-cto", Stage: iam.StageActive}
	token := machine("ci", iam.AllGrants...)
	for _, table := range []bool{true, false} {
		fleet := coordmem.NewFleet()
		var authorize builtin.Authorizer
		if !table {
			authorize = func(context.Context, authz.Action, authz.Object) error { return nil }
		}
		tool := boardTool(t, fleet, authorize)
		for name, who := range map[string]iam.Principal{"an agent": agent, "an unbound token": token} {
			res, _ := saveBoard(t, tool, who, boardArgs(0))
			if !res.Failed {
				t.Errorf("%s saved a board (table %v): %q", name, table, res.Output)
			}
		}
		if owners, _ := fleet.HomeBoardOwners(t.Context()); len(owners) != 0 {
			t.Errorf("refused callers stored %v (table %v)", owners, table)
		}
	}
}

// A SAVE WHOSE ANSWER WAS LOST IS UNKNOWN, never done nor failed, and says to
// read before saving again; a store that could not be reached saved nothing.
func TestALostOrUnreachableSaveSaysWhatIsKnown(t *testing.T) {
	t.Parallel()
	lost := boardTool(t, failingBoards{err: errors.Join(coord.ErrUnavailable, coord.ErrWriteUnknown)}, nil)
	res, _ := saveBoard(t, lost, boundTo("jane"), boardArgs(0))
	if !res.Failed || !tools.UnknownOf(res) || !strings.Contains(res.Output, builtin.HomeBoardQuestion) {
		t.Fatalf("a lost save answered %+v, want the unknown outcome naming the read", res)
	}
	down := boardTool(t, failingBoards{err: coord.ErrUnavailable}, nil)
	res, _ = saveBoard(t, down, boundTo("jane"), boardArgs(0))
	if !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable || tools.UnknownOf(res) {
		t.Fatalf("an unreachable store answered %+v, want unavailable, not unknown", res)
	}
}
