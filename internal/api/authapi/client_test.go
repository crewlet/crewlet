package authapi_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE RESET SCREEN READS WHAT THE RESET VIEW SENDS.
//
// `ResetView` (`contract/identity.ts`) is the dashboard's type for
// `GET /auth/reset/{id}`, and ONE screen serves both links that route opens —
// a reset an administrator issued to somebody holding a password, and the
// first password link a person created on a seat is handed — told apart by
// `first` and nothing else. A key renamed on this side alone would draw every
// first password link as a reset warning of sessions it ends, with nothing
// failing.
//
// HELD BOTH WAYS over the answers to both links: every member the interface
// declares is sent by some answer and every REQUIRED one by every answer, and
// every key either answer sends is one the interface declares.
//
// Mutation: rename `first` on [resetView] and the first two directions fail;
// add a field the screen does not declare and the third does.
func TestTheResetScreenReadsWhatTheResetViewSends(t *testing.T) {
	t.Parallel()
	var answers []map[string]any
	for _, person := range []func(*estate){created, passwordOnly} {
		r, h := passwordRig(t)
		person(r.estate)
		id, secret := withResetLink(t, r.estate, nil)
		rec := viewReset(t, h, id, secret)
		if rec.Code != http.StatusOK {
			t.Fatalf("the view answered %d: %s", rec.Code, rec.Body)
		}
		var answer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, answer)
	}

	members, err := clientsource.Interface(clientsource.Tree(t), "ResetView")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the dashboard's "+
			"declaration, and skipping would certify nothing", err)
	}
	declared := map[string]bool{} // member → required
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	if len(declared) < 4 {
		t.Fatalf("ResetView declares %v, so this gate compares less than the "+
			"view carries", members)
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		sentBy := 0
		for _, answer := range answers {
			if _, ok := answer[name]; ok {
				sentBy++
			}
		}
		switch {
		case sentBy == 0:
			t.Errorf("ResetView declares %q and neither link's view sends it — "+
				"the screen reads it as undefined", name)
		case declared[name] && sentBy != len(answers):
			t.Errorf("ResetView declares %q REQUIRED and %d of %d views send "+
				"it", name, sentBy, len(answers))
		}
	}
	for _, answer := range answers {
		for _, key := range slices.Sorted(maps.Keys(answer)) {
			if _, ok := declared[key]; !ok {
				t.Errorf("the view sends %q and ResetView does not declare it, "+
					"so the screen cannot read it", key)
			}
		}
	}
}
