package queries_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD NAMES EVERY VALUE A STORY CARRIES, and nothing else.
//
// A story's outcome, origin and actions are closed sets the engine owns and
// the dashboard copies into `contract/stories.ts`, because a separate build
// cannot import a Go identifier. Both directions fail silently: a value the
// engine sends that the copy lacks is a card drawn with a bare word, and one
// the copy keeps that the engine stopped sending is wording for a card that
// never comes.
func TestTheDashboardNamesEveryStoryValue(t *testing.T) {
	t.Parallel()
	for _, set := range []struct {
		name   string
		engine []string
	}{
		{"STORY_OUTCOMES", names(queries.StoryOutcomes)},
		{"STORY_ORIGINS", names(queries.StoryOrigins)},
		{"STORY_ACTIONS", names(queries.StoryActionKinds)},
	} {
		body, err := clientsource.Literal(clientsource.Tree(t), set.name)
		if err != nil {
			t.Fatal(err)
		}
		client := slices.Sorted(slices.Values(clientsource.Strings(body)))
		engine := slices.Sorted(slices.Values(set.engine))
		if !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %v and the engine sends %v — change "+
				"contract/stories.ts to the engine's set", set.name, client, engine)
		}
	}
}

// names is a closed set's values as the wire carries them.
func names[S ~string](values []S) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}
