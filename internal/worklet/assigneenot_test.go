package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// `assignee=!<handle>` IS THE WORK THAT HANDLE DOES NOT HOLD, spelled as
// `status=` spells its negation.
//
// A person's list of what they follow or were brought onto is asked as the
// work list narrowed to them, and without the negation it repeated every task
// they hold, which their own queue already is: the engine's own My work blocks
// leave those out, and the list a reader filters has to ask the same question.
// `!none` leaves out exactly what `none` admits.
func TestAnAssigneeCanBeLeftOut(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	holders := func(kv map[string]any) []string {
		var out []string
		for _, row := range r.ask(kv).Rows {
			out = append(out, row.Assignee)
		}
		slices.Sort(out)
		return out
	}
	base := map[string]any{"container": "project:ENG", "show_closed": "true"}
	with := func(value string) map[string]any {
		kv := map[string]any{"assignee": value}
		for k, v := range base {
			kv[k] = v
		}
		return kv
	}
	if got := holders(with("!ana")); !slices.Equal(got, []string{"", "bob", "bob"}) {
		t.Fatalf("assignee=!ana answered %q, want the work ana does not hold", got)
	}
	if got := holders(with("!none")); !slices.Equal(got, []string{"ana", "ana", "bob", "bob"}) {
		t.Fatalf("assignee=!none answered %q, want the work somebody holds", got)
	}
	if got := holders(with("bob,!bob")); len(got) != 0 {
		t.Fatalf("assignee=bob,!bob answered %q, want nothing: both hold", got)
	}
	if _, err := worklet.ParseQuery(worklet.MapParams(with("!")), time.Now(), time.UTC); err == nil {
		t.Fatal("assignee=! names nobody to leave out and was parsed")
	}
}
