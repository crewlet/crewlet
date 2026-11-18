package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE DURABLE CONSUMER NAMES ARE FROZEN. The group name IS the fleet's
// position: a rename fails nowhere, it creates a fresh durable consumer that
// starts from the log's first retained record, so the fleet replays every
// change the log still holds as a wake (collapsed only within the change
// feed's claim and wake-id windows) and the log's trim stalls behind the new
// consumer's floor until it catches up. The source name is frozen too: dashboard
// filters and a company's notification rules match on it.
func TestTheNativeFeedGroupNamesNeverMove(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		got            changefeed.Source
		wantName       string
		wantGroupExact string
	}{
		"the tracker": {worklet.NewTranslator().Source(), "work", "crewlet-tracker-feed"},
		"the wiki":    {doclet.NewTranslator(nil).Source(), "page", "crewlet-pages-feed"},
		"the code":    {gitlet.NewTranslator().Source(), "code", "crewlet-code-feed"},
		"artifacts":   {artifactsdomain.NewTranslator().Source(), "artifact", "crewlet-artifacts-feed"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got.Group != tc.wantGroupExact {
				t.Errorf("group = %q, want %q — renaming it replays every record "+
					"the log still holds as a wake and holds back the log's trim "+
					"until the new consumer catches up", tc.got.Group, tc.wantGroupExact)
			}
			// The source name scopes the claim keys and is the source
			// column on every published event, so it is a stored value too.
			if tc.got.Name != tc.wantName {
				t.Errorf("source name = %q, want %q", tc.got.Name, tc.wantName)
			}
		})
	}
	groups := map[string]bool{}
	for _, src := range []changefeed.Source{worklet.NewTranslator().Source(),
		doclet.NewTranslator(nil).Source(), gitlet.NewTranslator().Source(),
		artifactsdomain.NewTranslator().Source()} {
		if groups[src.Group] {
			t.Errorf("two estates share the durable consumer %q, so each would handle "+
				"records it cannot decode", src.Group)
		}
		groups[src.Group] = true
	}
}
