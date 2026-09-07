package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// EVERY SURFACE CAN BE DISCONNECTED, including the ones with nothing to
// remove at the third-party app.
//
// A third-party app with no provisioning pass still has a BLOCK in the company
// document. Datadog's webhook is created by a person in Datadog's own UI and
// Slack's apps are made from the command line, so neither has anything this
// engine registered — but a disconnect for either still has to drop the
// block. Without a disconnector the intent sat on the fleet row for ever,
// with the screen reporting Disconnecting and no node ever finishing it,
// which is exactly what happened to Datadog.
func TestEverySurfaceHasADisconnector(t *testing.T) {
	got := (&Engine{}).disconnectors()
	for _, kind := range integration.Kinds {
		if _, ok := got[kind]; !ok {
			t.Errorf("%s has no disconnector, so a disconnect for it would never finish", kind)
		}
	}
	if len(got) != len(integration.Kinds) {
		t.Errorf("disconnectors = %d, kinds = %d", len(got), len(integration.Kinds))
	}
}
