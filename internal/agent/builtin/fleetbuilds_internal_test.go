package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CODE TOOLS TELL THE FLEET'S TWO BUILDS AS WHAT THEY ARE, NOT AS A WAIT.
// Where no Gitlet or data node's build can read a request whole, the router
// answers estate.ErrUnknownKeys over the walk nobody served. A retry meets the
// same refusal until an administrator finishes the upgrade, so each of the
// three answers names the two builds and says trying again won't clear it,
// over either walk.
//
// Mutation: drop the ErrUnknownKeys arm from codeFailure, codeWriteFailure or
// unjudged, and that helper's rows are told to try again.
func TestTheCodeToolsTellTheFleetsTwoBuildsAsWhatTheyAre(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for walk, unserved := range map[string]*estate.ErrUnserved{
		"the git walk":  {Detail: "git-b: its build does not declare args.tree.went_on", Git: true},
		"the data walk": {Detail: "data-b: its build does not declare args.tree.went_on"},
	} {
		refused := fmt.Errorf("estate: code.git_read: %w", &estate.ErrUnknownKeys{Op: "code.git_read",
			Keys: []string{"args.tree.went_on"}, Nodes: []string{"git-b"}, Unserved: unserved})
		for helper, got := range map[string]tools.Result{
			"a code read":   codeFailure(ctx, CodeReadTool, "app", refused),
			"a code write":  codeWriteFailure(ctx, CodeCommitFilesTool, Actor{}, "app", refused),
			"a code status": unjudged(ctx, CodeStatusTool, refused),
		} {
			if tools.RefusalOf(got) != tools.RefusalUnavailable {
				t.Errorf("%s over %s is %q, want %q", helper, walk, tools.RefusalOf(got), tools.RefusalUnavailable)
			}
			if !strings.Contains(got.Output, "two different builds") || !strings.Contains(got.Output, "won't clear") {
				t.Errorf("%s over %s is told %q, want the two builds and that trying again won't clear it",
					helper, walk, got.Output)
			}
			for _, wrong := range []string{"try again shortly", "same call again", "right now", "went_on", "git-b"} {
				if strings.Contains(got.Output, wrong) {
					t.Errorf("%s over %s is told %q, with %q in it", helper, walk, got.Output, wrong)
				}
			}
		}
	}
}
