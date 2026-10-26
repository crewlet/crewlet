package configplane_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/store"
)

// A WITHHELD OFFLINE CHANGE IS ANSWERED WITH THE CHANGE, never with its whole
// document put up as the company, which would undo what the fleet moved on to.
// The diff it names is against what the change was built on, or — for one
// built on no company — against the fleet's revision.
func TestAWithheldOfflineChangeIsReadAgainstWhatItWasBuiltOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, builtOn, against string
	}{
		{"built on a revision", "01JBASE", "against=01JBASE"},
		{"built on no company", "", "against=active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remedy := configplane.WithheldOfferRemedy("01JOFFER", tc.builtOn)
			for _, want := range []string{
				"/config/revisions/01JOFFER/diff?" + tc.against,
				"would undo what the fleet changed",
				"/config/revisions/01JOFFER/revert",
			} {
				if !strings.Contains(remedy, want) {
					t.Errorf("%q does not say %q", remedy, want)
				}
			}
		})
	}
}

// AN OFFER AN EARLIER START SENT IS NEVER SAID TO BE UNPUBLISHED: that start
// may have landed it before the fleet moved on, and an operator told otherwise
// redoes a change the fleet already took. One nobody sent is said plainly.
func TestAWithheldOfferThatWasSentIsNotCalledUnpublished(t *testing.T) {
	t.Parallel()
	never := configplane.WithheldOfferReason(store.Offer{Base: "01JBASE"})
	sent := configplane.WithheldOfferReason(store.Offer{Base: "01JBASE",
		PlannedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)})
	if strings.Contains(never, "published it") {
		t.Errorf("an offer no start sent reads %q, want no word of a publish", never)
	}
	if !strings.Contains(sent, "published it, or tried to") ||
		!strings.Contains(sent, "not recorded here") {
		t.Errorf("an offer an earlier start sent reads %q, want it to say the publish may have landed", sent)
	}
}
