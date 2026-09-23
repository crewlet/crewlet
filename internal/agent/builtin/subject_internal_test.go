package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY ARGUMENT THE AUTHORITY TABLE READS IS ONE ITS TOOL DECLARES.
//
// This is the walk that had to exist. [subjectOf] used to hold closures —
// `ownerFrom("handle")` — and four rows named a field their tool does not
// have: `my_work`, `mark_inbox` and `set_pins` take no handle at all, and
// `save_work_view` spells its own `owner`. Every one of them read empty on
// every call, every personal class refuses an empty owner, and so four tools
// a seat uses each turn were gated shut for everybody but an admin — with the
// only symptom a refusal naming a grant the caller already held.
//
// NOTHING ELSE CATCHES IT. The completeness walks in internal/authz hold the
// rules table against itself, and [TestEveryToolHasAnAuthorityRule] holds it
// against the tool NAMES. A field name inside a row is below all of them.
func TestEveryObjectFieldIsOneItsToolDeclares(t *testing.T) {
	t.Parallel()
	declared := declaredArguments(t)
	for name, subj := range subjectOf {
		properties, ok := declared[name]
		if !ok {
			t.Errorf("%s has an authority row and is not a tool this build "+
				"serves — add it to toolsWithArguments, or drop the row", name)
			continue
		}
		for field, role := range map[string]string{
			subj.owner: "owner", subj.container: "container",
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

// AND A PERSONAL VERB WITH NO HANDLE IS ABOUT THE CALLER.
//
// The other half of the same bug: the three that take no argument must still
// reach their class with an owner, or [authz.ClassOwnRecord] reads the empty
// one as [authz.ReasonUnnamed] and refuses. Mutating [subject.objectFor]'s
// personal default to leave the owner empty turns this red.
func TestAPersonalVerbWithNoHandleIsAboutTheCaller(t *testing.T) {
	t.Parallel()
	caller := iam.Principal{Kind: iam.KindSeat, Seat: "ana", Stage: iam.StageActive}
	for _, name := range []string{"my_work", "mark_inbox", "set_pins"} {
		got := subjectOf[name].objectFor(caller, nil)
		if got.Owner != "ana" {
			t.Errorf("%s with no argument is about %q, want the caller — a "+
				"verb that takes no handle can only ever be about them",
				name, got.Owner)
		}
	}
	// AND A VIEW IS NOT, which is why it has a kind of its own: omitting
	// the owner SHARES a view rather than making it mine, so filling one
	// in here would hand every seat a personal view it never asked for
	// and hide the container's own rule.
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
}

// AND A GATED TOOL IS COMPARABLE.
//
// A registered tool is compared with `==` — internal/engine asserts a seat's
// builtins are the applied epoch's OBJECTS and not merely its names — and a
// wrapper that embedded the [Authorizer] func by value made that comparison a
// RUNTIME PANIC. Nothing in the type system says so, which is why it is said
// here: revert [gate] to returning values and this test panics.
func TestAGatedToolIsComparable(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	for _, tool := range toolsWithArguments() {
		first, second := gate(tool, authorize), gate(tool, authorize)
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

// toolsWithArguments is one instance of every tool [subjectOf] governs.
//
// CONSTRUCTED WITH NO DEPENDENCIES, because what is read is `Name()` and
// `Parameters()` and neither touches one — and because the alternative,
// standing up the full registry, needs a fixture of forty seams that the
// external suite already owns and this package cannot import.
//
// HAND-WRITTEN AND UNABLE TO GO STALE: the walk above fails on a row with no
// entry here, so a new row cannot be added without one.
func toolsWithArguments() []tools.Callable {
	return []tools.Callable{
		&createWorkItem{}, &updateWorkItem{}, &commentOnWorkItem{},
		&mergeWorkItem{}, &a2aAsk{}, &writePage{}, &savePage{},
		&commentOnPage{}, &writeProject{},
		&getPerson{}, &workInbox{}, &setPriorities{}, &myWork{},
		&markInbox{}, &setPins{}, &saveWorkView{},
		&removeWorkItem{}, &restoreWorkItem{},
	}
}
