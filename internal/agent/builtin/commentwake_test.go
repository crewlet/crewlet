package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// THE COMMENT TOOL SAYS WHOM A COMMENT WAKES.
//
// A comment starts a colleague's turn only when it names them (an @-mention,
// an `ask`, or a reply in their thread); otherwise it is an inbox row for the
// assignee and reporter and starts no turn (worklet's routing). An agent told
// "everyone following the item is told" would leave a hand-off unaddressed and
// wait for an answer no turn was started to give.
//
// Mutation: say followers are told, and every assertion below fails.
func TestTheCommentToolSaysACommentWakesOnlyWhomItNames(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Seats: askable})
	entry, ok := reg.Lookup(builtin.CommentOnWorkTool)
	if !ok {
		t.Fatalf("%s is not registered", builtin.CommentOnWorkTool)
	}
	text := entry.Tool.Description()
	for _, want := range []string{
		"wakes a colleague to act on it only when it asks them",
		"an @-mention with `asks: true`",
		"a question put to them with `ask`",
		"in a thread where they asked or were asked something still open",
		"is a reference and wakes nobody",
		"wakes nobody, not even the item's assignee or its reporter",
		"name whoever you need to act",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the comment tool's description does not say %q:\n%s", want, text)
		}
	}
}
