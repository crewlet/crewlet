package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE DURABLE CONSUMER NAMES ARE FROZEN, and this is the assertion that keeps
// them so.
//
// The group name IS the fleet's position. A rename does not fail, log, or
// error: it creates a second consumer at the current head, so every change
// the first had not yet handled is abandoned silently, and the only symptom
// is notifications nobody ever received.
//
// The tracker's group changed ONCE, when its estate did — a bucket feed and a
// log feed are two different consumers over two different things, and the old
// name would have been a position on an estate that no longer exists. The
// SOURCE name did not change with it, deliberately: it appears in every event's
// source column and every dashboard filter, and a company's existing
// notification rules go on meaning what they meant.
func TestTheNativeFeedGroupNamesNeverMove(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		got            changefeed.Source
		wantName       string
		wantGroupExact string
	}{
		"the tracker": {tracker.NewTranslator().Source(), "work", "crewlet-tracker-feed"},
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
	if tracker.NewTranslator().Source().Group == pages.NewTranslator(nil).Source().Group {
		t.Error("two estates share a durable consumer, so each would handle " +
			"records it cannot decode")
	}
}
