package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY ARGUMENT THE AUTHORITY TABLE READS IS ONE ITS TOOL DECLARES: a row
// naming a field its tool lacks reads empty on every call, and every personal
// class refuses an empty owner, so the tool is shut for all but an admin.
// Nothing else catches it: internal/authz's walks hold the rules against
// themselves, and [TestEveryRegisteredToolHasAnAuthorityRule] only the tool
// names.
//
// BOTH DIRECTIONS, so the list is exactly the rows: a stale entry would claim
// this walk covers a tool it reads nothing of.
func TestEveryObjectFieldIsOneItsToolDeclares(t *testing.T) {
	t.Parallel()
	declared := declaredArguments(t)
	for name := range declared {
		if _, governed := subjectOf[name]; !governed {
			t.Errorf("%s is listed in toolsWithArguments and has no row in "+
				"subjectOf — drop it from the list", name)
		}
	}
	for name, subj := range subjectOf {
		properties, ok := declared[name]
		if !ok {
			t.Errorf("%s has an authority row and is not a tool this build "+
				"serves — add it to toolsWithArguments, or drop the row", name)
			continue
		}
		for field, role := range map[string]string{
			subj.owner: "owner", subj.container: "container",
			subj.personal: "personal flag",
		} {
			if field == "" {
				continue
			}
			if !slices.Contains(properties, field) {
				t.Errorf("%s's authority row reads %q as its %s and the tool "+
					"declares %v — so the field is empty on every call and "+
					"the rule refuses everyone who is not an admin",
					name, field, role, properties)
			}
		}
	}
}

// AND A PERSONAL VERB WITH NO HANDLE IS ABOUT THE CALLER, or
// [authz.ClassOwnRecord] reads the empty owner as [authz.ReasonUnnamed] and
// refuses. Mutating [subject.objectFor]'s personal default to leave the owner
// empty turns this red.
func TestAPersonalVerbWithNoHandleIsAboutTheCaller(t *testing.T) {
	t.Parallel()
	caller := iam.Principal{Kind: iam.KindSeat, Seat: "ana", Stage: iam.StageActive}
	for _, name := range []string{"my_work", "mark_inbox", "set_pins", "mark_chat_read",
		"save_home_board", "save_assistant_instructions"} {
		got := subjectOf[name].objectFor(caller, nil)
		if got.Owner != "ana" {
			t.Errorf("%s with no argument is about %q, want the caller — a "+
				"verb that takes no handle can only ever be about them",
				name, got.Owner)
		}
	}
	// AND A VIEW IS NOT: omitting the owner SHARES a view, so filling one
	// in would hand every seat a personal view and hide the container's rule.
	view := subjectOf["save_work_view"].objectFor(caller,
		map[string]any{"container": "project:eng"})
	if view.Owner != "" {
		t.Errorf("a shared view came out owned by %q", view.Owner)
	}
	if view.ContainerKind != authz.KindProject || view.Container != "ENG" {
		t.Errorf("a view on project:eng decided against %s %q, want the "+
			"project relation over the canonical key",
			view.ContainerKind, view.Container)
	}

	// AND A PERSONAL ONE IS THE CALLER'S OWN RECORD: a bound person's under
	// their seat, never their login, and a caller the engine can name nothing
	// for names NOTHING (refused by every class) rather than falling back to
	// the shared reading of an owner-less view.
	for _, c := range []struct {
		name   string
		caller iam.Principal
		owner  string
		empty  bool
	}{
		{"a seat", caller, "ana", false},
		{"a bound person", iam.Principal{Kind: iam.KindPerson,
			Login: "ana.diaz", Seat: "ana"}, "ana", false},
		{"an unbound person", iam.Principal{Kind: iam.KindPerson,
			Login: "ana.diaz"}, "ana.diaz", false},
		{"nobody", iam.Principal{Kind: iam.KindPerson}, "", true},
	} {
		got := subjectOf["save_work_view"].objectFor(c.caller,
			map[string]any{"container": "project:eng", "personal": true})
		if got.Owner != c.owner {
			t.Errorf("%s's personal view is about %q, want %q", c.name,
				got.Owner, c.owner)
		}
		if c.empty && got.Container != "" {
			t.Errorf("%s's personal view kept its container %q, so it is "+
				"decided as a SHARED tab", c.name, got.Container)
		}
	}
}

// AND A GATED TOOL IS COMPARABLE: internal/engine compares registered tools
// with `==` (a seat's builtins are the applied epoch's OBJECTS), and a wrapper
// holding the [Authorizer] func by value makes that a RUNTIME PANIC the type
// system does not catch. Revert [gate] to returning values and this panics.
func TestAGatedToolIsComparable(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	for _, tool := range toolsWithArguments() {
		first, second := gate(tool, authorize, nil), gate(tool, authorize, nil)
		if first == second {
			t.Errorf("%s: two gates over one tool compared equal, so the "+
				"epoch-identity check upstream cannot tell them apart",
				tool.Name())
		}
		if same := first; same != first {
			t.Errorf("%s: a gated tool does not equal itself", tool.Name())
		}
	}
}

// declaredArguments is each tool's own JSON Schema properties.
func declaredArguments(t *testing.T) map[string][]string {
	t.Helper()
	declared := make(map[string][]string)
	for _, tool := range toolsWithArguments() {
		properties, _ := tool.Parameters()["properties"].(map[string]any)
		names := make([]string, 0, len(properties))
		for field := range properties {
			names = append(names, field)
		}
		slices.Sort(names)
		declared[tool.Name()] = names
	}
	return declared
}

// toolsWithArguments is one instance of every tool [subjectOf] governs, built
// with no dependencies since `Name()` and `Parameters()` touch none (the full
// registry's fixture lives in the external suite, which this package cannot
// import). It cannot go stale: the walk above fails on a row with no entry
// here and on an entry with no row.
func toolsWithArguments() []tools.Callable {
	return []tools.Callable{
		&getWorkItem{}, &commentOnWorkItem{},
		&getPerson{}, &workInbox{}, &setPriorities{}, &myWork{},
		&markInbox{}, &codeMarkInbox{}, &codeMarkViewed{}, &artifactMarkInbox{}, &setPins{}, &saveWorkView{}, &deleteWorkView{},
		&removeWorkItem{}, &restoreWorkItem{}, &moveWorkItem{},
		&pauseSeat{}, &resumeSeat{}, &steerTurn{}, &runSchedule{},
		&answerExecutor{}, &removeProjectFile{},
		&getArtifact{}, &renderArtifact{}, &publishArtifact{}, &updateArtifact{},
		&shareArtifact{}, &unshareArtifact{}, &uploadArtifactAsset{}, &moveArtifact{},
		&listArtifactComments{}, &commentOnArtifact{}, &resolveArtifactComment{},
		&editArtifactComment{}, &removeArtifactComment{},
		&readArtifactData{}, &queryArtifactData{}, &writeArtifactData{}, &setArtifactSchema{},
		&editMessage{}, &deleteMessage{}, &markChatRead{}, &saveHomeBoard{}, &saveInstructions{},
	}
}
