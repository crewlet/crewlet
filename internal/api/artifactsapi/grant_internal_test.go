package artifactsapi

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// A PREVIEW OF A PAGE THAT KEEPS STATE IS GRANTED A NEWCOMER'S STATE, and
// still no viewer and no room: the dashboard's preview host answers the state
// as a first visit holds it, so a page that remembers a viewer's choice draws
// in the gallery rather than standing as its kind's picture. An open is
// granted everything its version declares.
func TestAPreviewIsGrantedStateButNoViewerAndNoRoom(t *testing.T) {
	t.Parallel()
	declared := []artifacts.Capability{
		artifacts.CapabilityDB, artifacts.CapabilityIdentity, artifacts.CapabilityState,
		artifacts.CapabilityPresence, artifacts.CapabilityAssets,
	}
	got := grant(declared, artifacts.TicketPurposePreview)
	want := []artifacts.Capability{artifacts.CapabilityDB, artifacts.CapabilityState, artifacts.CapabilityAssets}
	if !slices.Equal(got, want) {
		t.Errorf("a preview was granted %v, want %v", got, want)
	}
	if got := grant(declared, ""); !slices.Equal(got, declared) {
		t.Errorf("an open was granted %v, want everything declared %v", got, declared)
	}
	// A PAGE THAT DECLARES NO STATE IS GRANTED NONE: the preview's grant is
	// a subset of the declaration, never an addition to it.
	if got := grant([]artifacts.Capability{artifacts.CapabilityDB}, artifacts.TicketPurposePreview); !slices.Equal(got, []artifacts.Capability{artifacts.CapabilityDB}) {
		t.Errorf("a preview of a page declaring only its datastore was granted %v", got)
	}
}
