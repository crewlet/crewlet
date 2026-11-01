package modelnote_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/modelnote"
)

// A NOTE IS HEARD ONLY BY THE CALLER THAT EXPECTED ONE: a route telling a
// request nobody expects a note for (a browser's, a script's) records nothing
// anywhere, and the caller that did expect one hears the last sentence told.
func TestANoteIsHeardOnlyByTheCallerThatExpectedIt(t *testing.T) {
	t.Parallel()
	modelnote.Tell(t.Context(), "create_work_item needs a `title`.")
	ctx, note := modelnote.Expect(t.Context())
	if note.Said() != "" {
		t.Errorf("a fresh note says %q", note.Said())
	}
	modelnote.Tell(ctx, "create_work_item needs a `title`.")
	if got := note.Said(); got != "create_work_item needs a `title`." {
		t.Errorf("the caller heard %q", got)
	}
	var none *modelnote.Note
	if none.Said() != "" {
		t.Error("no note said something")
	}
}
