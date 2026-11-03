package harness

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE MODELS SCREEN NAMES THE ENGINE'S SIGN-INS, LIMITS AND CHECKS. Settings ›
// Models says how a model signs in, which window of a limit it sits out and
// what each line of its test measured, each in words keyed on the engine's own
// value. A value the dashboard lacks is a model whose sign-in, limit or test
// line reads as nothing; one the engine dropped is copy for a state that never
// arrives. The test lines are held in the doctor's order, which is the order a
// test answers them in.
func TestTheModelsScreenNamesTheEnginesSignIns(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name    string
		want    []string
		ordered bool
	}{
		{"ModelSignInRoute", names(SignInRoutes), false},
		{"ModelLimitWindow", names(llm.LimitWindows), false},
		{"ModelCheckName", names(checkOrder), true},
	} {
		got, err := clientsource.Union(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if !tc.ordered {
			got, want = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
		}
		if !slices.Equal(got, want) {
			t.Errorf("contract/models.ts %s is %v, want exactly the engine's %v", tc.name, got, want)
		}
	}
}

func names[T ~string](set []T) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}
