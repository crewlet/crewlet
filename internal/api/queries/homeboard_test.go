package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// homeBoardOf asks `home_board` as ctx and answers it as the wire carries it.
func homeBoardOf(ctx context.Context, t *testing.T, s queries.Sources) map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	got, err := r.Answer(ctx, "home_board", nil)
	if err != nil {
		t.Fatalf("home_board: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A PERSON'S HOME BOARD IS READ UNDER THEIR SEAT, exactly as saved, with the
// revision the next save names; before any save it is null at revision zero.
//
// Mutation: read under the login rather than the seat, and pm's board reads
// as none.
func TestAPersonReadsTheirOwnHomeBoardUnderTheirSeat(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	s := queries.Sources{HomeBoards: fleet}
	pm := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "pm.doe",
		Kind: iam.KindPerson, Seat: "pm", Stage: iam.StageActive})
	if got := homeBoardOf(pm, t, s); got["seat"] != "pm" || got["board"] != nil || got["revision"] != 0.0 {
		t.Fatalf("before any save home_board answered %v, want pm's seat, a null board, revision 0", got)
	}
	const board = `{"widgets":[{"id":"n-1","kind":"note","text":"ship it"}]}`
	revision, ok, err := fleet.SaveHomeBoard(t.Context(), "pm", json.RawMessage(board), time.Now(), 0)
	if err != nil || !ok {
		t.Fatalf("SaveHomeBoard: %v, %v", ok, err)
	}
	r := queries.NewRegistry()
	queries.Register(r, s)
	got, err := r.Answer(pm, "home_board", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	want := `{"seat":"pm","board":` + board + `,"revision":` + jsonNumber(revision) + `}`
	if string(raw) != want {
		t.Fatalf("home_board encodes as %s, want %s", raw, want)
	}
}

// A CREDENTIAL HOLDING NO HUMAN SEAT HAS NO STORED HOME, which is an answer
// rather than a refusal, and reads nobody's board: a token, and an agent seat.
func TestACredentialWithNoHumanSeatHasNoStoredHome(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	for _, handle := range []string{"token:ci", "agent-cto"} {
		if _, _, err := fleet.SaveHomeBoard(t.Context(), handle, json.RawMessage(`{"widgets":[]}`),
			time.Now(), 0); err != nil {
			t.Fatal(err)
		}
	}
	s := queries.Sources{HomeBoards: fleet}
	for name, ctx := range map[string]context.Context{
		"a token": asGrants(t, "ci", iam.AllGrants...),
		"an agent": iam.WithPrincipal(t.Context(), iam.Principal{Kind: iam.KindSeat, Seat: "agent-cto",
			Login: "agent-cto", Stage: iam.StageActive}),
	} {
		if got := homeBoardOf(ctx, t, s); got["seat"] != "" || got["board"] != nil || got["revision"] != 0.0 {
			t.Errorf("%s read %v, want no seat, a null board and revision 0", name, got)
		}
	}
}

// AN UNREADABLE STORE IS NO EMPTY BOARD: a node that cannot reach it says so,
// and the question is registered only where a store is wired.
func TestAnUnreadableHomeBoardIsUnavailableNotEmpty(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{HomeBoards: unreadableBoards{}})
	_, err := r.Answer(asSeat(t, "pm"), "home_board", nil)
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("an unreadable store answered %v, want ErrUnavailable", err)
	}
	bare := queries.NewRegistry()
	queries.Register(bare, queries.Sources{})
	if slices.Contains(bare.Names(), "home_board") {
		t.Fatal("home_board is registered on a node with no store to read it from")
	}
}

type unreadableBoards struct{}

func (unreadableBoards) HomeBoard(context.Context, string) (coord.HomeBoard, uint64, bool, error) {
	return coord.HomeBoard{}, 0, false, coord.ErrUnavailable
}

func jsonNumber(n uint64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
