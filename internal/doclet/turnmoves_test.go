package doclet_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN MOVED THE PAGES ONLY WHERE IT WROTE MORE THAN WORDS: a turn that
// created a page or saved one moved work, one that only commented did not,
// and one that wrote nothing is not named.
func TestATurnMovedPagesOnlyWhereItWroteMoreThanWords(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(agent("maker"), doclet.NewPage{Body: "the runbook"})
	if _, _, err := r.store.Comment(t.Context(), agent("words"), page.Page.ID,
		doclet.NewComment{Body: "still current"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.store.SavePage(t.Context(), agent("saver"), page.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	got, err := r.reader.TurnsMovedPages(t.Context(), doclet.TurnsMovedQuery{
		Turns: []string{"turn-maker", "turn-words", "turn-saver", "turn-idle"}, Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"turn-maker": true, "turn-saver": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("turns that moved the pages = %v, want %v", got, want)
	}
}
