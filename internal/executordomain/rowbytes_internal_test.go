package executordomain

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT'S SESSIONS AND ITS SEARCH END A PAGE AT ITS BYTES AND RESUME AFTER THE
// LAST ROW KEPT: five sessions, each asked a long question holding the word
// searched for, under a bound two of their rows fit come back in pages of two,
// two and one through either read, every session once and in the order one
// unbounded page gives.
//
// Mutations, each red: drop the Take check in Sessions or in Search (one page
// of five); leave a cut sessions page without a cursor (paging stops after
// two); advance the search's cursor past the refused row (one skipped).
func TestASeatsSessionsPageAtTheirBytes(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	completed := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	for i := range 5 {
		turn := newTurn()
		e.capture(capturing("ana", "chat:room", turn, 1, 1, 100, budget))
		e.end(asked("ana", "chat:room", turn, "redeploy zebras "+strings.Repeat("y", 800)+
			string(rune('a'+i)), completed))
	}
	for _, read := range []struct {
		name string
		page func(ctx context.Context, cursor string) (turns []string, rows []any, next string)
	}{
		{"sessions", func(ctx context.Context, cursor string) ([]string, []any, string) {
			page, err := e.reader().Sessions(ctx, "ana", cursor, MaxPageSize, stale)
			if err != nil {
				t.Fatal(err)
			}
			var turns []string
			var rows []any
			for _, s := range page.Sessions {
				turns, rows = append(turns, s.Turn), append(rows, s)
			}
			return turns, rows, page.Next
		}},
		{"search", func(ctx context.Context, cursor string) ([]string, []any, string) {
			page, err := e.reader().Search(ctx, "ana", SessionSearch{Query: "zebras", Cursor: cursor,
				Limit: MaxPageSize}, stale)
			if err != nil {
				t.Fatal(err)
			}
			var rows []any
			for _, h := range page.Hits {
				rows = append(rows, h)
			}
			return hitTurns(page.Hits), rows, page.Next
		}},
	} {
		want, rows, next := read.page(t.Context(), "")
		if len(want) != 5 || next != "" {
			t.Fatalf("%s: the unbounded page answers %v and cursor %q, want all five", read.name, want, next)
		}
		heaviest := 0
		for _, row := range rows {
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			heaviest = max(heaviest, len(raw))
		}
		ctx := statelog.WithAnswerBytes(t.Context(), 2*heaviest+heaviest/2)
		var got []string
		var sizes []int
		cursor := ""
		for range 10 {
			turns, _, next := read.page(ctx, cursor)
			got, sizes = append(got, turns...), append(sizes, len(turns))
			if cursor = next; cursor == "" {
				break
			}
		}
		if !slices.Equal(got, want) || !slices.Equal(sizes, []int{2, 2, 1}) {
			t.Errorf("%s: paged %v in pages of %v, want %v in pages of [2 2 1]", read.name, got, sizes, want)
		}
	}
}

// THE SEATS HOLDING SESSIONS PAGE AT THEIR BYTES: five seats under a bound two
// of their handles fit come back in pages of two, two and one, each once and
// in order, as the whole listing names them.
//
// Mutations, each red: drop the Take check in HandlesPage (one page of five);
// name the refused handle as the next page's start (one handle skipped).
func TestTheSeatsHoldingSessionsPageAtTheirBytes(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	seats := []string{"ana", "bob", "cid", "dee", "eve"}
	for _, seat := range seats {
		e.capture(capturing(seat, "chat:room", newTurn(), 1, 1, 100, budget))
	}
	whole, err := e.reader().Handles(t.Context(), stale)
	if err != nil || !slices.Equal(whole, seats) {
		t.Fatalf("the whole listing names %v, %v; want %v", whole, err, seats)
	}
	ctx := statelog.WithAnswerBytes(t.Context(), 2*len(`"ana"`)+2)
	var got []string
	var sizes []int
	for after, pages := "", 0; pages < 10; pages++ {
		page, err := e.reader().HandlesPage(ctx, after, stale)
		if err != nil {
			t.Fatal(err)
		}
		got, sizes = append(got, page.Handles...), append(sizes, len(page.Handles))
		if after = page.Next; after == "" {
			break
		}
	}
	if !slices.Equal(got, seats) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("paged %v in pages of %v, want %v in pages of [2 2 1]", got, sizes, seats)
	}
}
