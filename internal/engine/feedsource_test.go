package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/work"
)

// THE DURABLE CONSUMER NAMES ARE FROZEN, and this is the assertion that keeps
// them so.
//
// The group name IS the fleet's position. A rename does not fail, log, or
// error: it creates a second consumer at the current head, so every change
// the first had not yet handled is abandoned silently, and the only symptom
// is notifications nobody ever received. The refactor that moved this package
// off [coord.Family] passed the name through [changefeed.Group] for exactly
// this reason, and these two literals are what "byte-identical" meant.
func TestTheNativeFeedGroupNamesNeverMove(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		got            changefeed.Source
		wantName       string
		wantGroupExact string
	}{
		"the tracker": {work.NewTranslator().Source(), "work", "crewlet-work-feed"},
		"the wiki":    {pages.NewTranslator(nil).Source(), "page", "crewlet-pages-feed"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got.Group != tc.wantGroupExact {
				t.Errorf("group = %q, want %q — renaming it abandons every change "+
					"the fleet had not yet handled, silently", tc.got.Group, tc.wantGroupExact)
			}
			// The source name scopes the claim keys and is the source
			// column on every published event, so it is a stored value too.
			if tc.got.Name != tc.wantName {
				t.Errorf("source name = %q, want %q", tc.got.Name, tc.wantName)
			}
		})
	}
	if work.NewTranslator().Source().Group == pages.NewTranslator(nil).Source().Group {
		t.Error("two estates share a durable consumer, so each would handle " +
			"records it cannot decode")
	}
}
