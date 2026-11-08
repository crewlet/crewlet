package queries

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// WHICH CHANGE WOKE A SEAT IS GITLET'S, AND A READER WITHOUT IT IS NOT TOLD.
//
// A turn a change woke names the change as its origin (`code_change:web!12`),
// which names the change and its repository to whoever reads the turn. The
// trail is everybody's overview, so the origin is withheld from a reader the
// gitlet app does not admit, as a chat message's is from a reader without
// chatlet, while the turn itself stays listed (ADR-0110: no list leaks a
// change). And a filter by such an origin is refused naming gitlet, since the
// unfiltered page answers another question. The CONTROL is a holder of the app,
// shown the origin, and a turn nobody's app owns (a schedule), shown to
// everybody. Mutation: map code_change to no kind in originObject and the
// reader without the app is shown the change.
func TestAChangesOriginIsShownOnlyToAGitletHolder(t *testing.T) {
	t.Parallel()
	turns := []store.Turn{
		{Origin: &types.TriggerOrigin{Kind: types.OriginCodeChange, ID: "web!12"}},
		{Origin: &types.TriggerOrigin{Kind: types.OriginSchedule, ID: "nightly"}},
	}
	reader := func(grants ...iam.Grant) iam.Principal {
		return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
			Stage: iam.StageActive, Grants: grants}
	}
	without := turnsFor(turns, reader(iam.GrantWorklet, iam.GrantChatlet))
	if without[0].Origin != nil {
		t.Errorf("a reader without gitlet was shown the change that woke a turn: %+v", without[0].Origin)
	}
	if without[1].Origin == nil {
		t.Error("a schedule's origin, which no app owns, was withheld")
	}
	if turns[0].Origin == nil {
		t.Error("withholding an origin edited the page it was handed")
	}
	holder := turnsFor(turns, reader(iam.GrantGitlet))
	if holder[0].Origin == nil || holder[0].Origin.ID != "web!12" {
		t.Errorf("a gitlet holder was shown %+v, want the change", holder[0].Origin)
	}
	if got := originObject(store.OriginRef(types.OriginCodeChange, "web!12")); got != authz.KindChange {
		t.Errorf("a change's origin is of kind %q, want Gitlet's change", got)
	}
}
