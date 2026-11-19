package inbox_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

func TestTheTypesThatRunATurnAreLedgered(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{
		types.TaskAssigned{}.EventType(),
		types.ExternalNotification{}.EventType(),
		types.A2ARequestType,
		types.A2AMessageType,
	} {
		if !inbox.Ledgered(kind) {
			t.Errorf("%q runs a turn but is not ledgered, so every "+
				"redelivery runs it again", kind)
		}
	}
}

func TestTypesThatRunNoTurnAreNotLedgered(t *testing.T) {
	t.Parallel()
	// The counterfactual. Ledgering everything would be the easy way to
	// pass the test above, and it would spend a durable write per
	// observability event to record that nothing outward-facing happened.
	for _, kind := range []string{
		types.OrgStarted{}.EventType(),
		types.ConfigRevisionApplied{}.EventType(),
		types.TurnTriggerSkipped{}.EventType(),
		types.A2AMessageSent{}.EventType(),
		"",
		"a_type_nothing_publishes",
	} {
		if inbox.Ledgered(kind) {
			t.Errorf("%q runs no turn but is ledgered", kind)
		}
	}
}

// TestNoLedgeredTypeIsANameNothingPublishes is the guard that earns its keep.
//
// A set like this collects defensive aliases, such as a "notification" nothing
// emits; a dead name is invisible, never matching while the type appears
// covered. Every ledgered name must be a REGISTERED PAYLOAD TYPE, with no
// exemptions (a free-form bag let the ask travel under a key nothing read, so a
// woken seat got "(a2a_request)" instead of the question).
//
// It reads LedgeredTypes rather than a list of its own: iterating expected
// names asks whether each is ledgered, a different question, and leaves a wrong
// name it never asked about green.
func TestNoLedgeredTypeIsANameNothingPublishes(t *testing.T) {
	t.Parallel()
	registered := events.RegisteredTypes()

	var unregistered []string
	for _, kind := range inbox.LedgeredTypes() {
		if !slices.Contains(registered, kind) {
			unregistered = append(unregistered, kind)
		}
	}
	if len(unregistered) > 0 {
		slices.Sort(unregistered)
		t.Errorf("ledgered names with no registered payload: %v — a type that runs a "+
			"turn must carry a typed payload, or its ask has nowhere to travel",
			unregistered)
	}
}

// TestTheLedgeredSetIsExactlyWhatItClaims pins the size.
//
// LedgeredTypes is what the guard above reads, so a name added to the map is
// only as visible as the assertions over it. This is the one that notices a
// SIXTH covered type appearing without anyone deciding it should be: the
// guard above accepts any registered type, and every trigger type is
// registered.
func TestTheLedgeredSetIsExactlyWhatItClaims(t *testing.T) {
	t.Parallel()
	want := []string{
		types.A2AMessageType,
		types.A2ARequestType,
		types.ExternalNotification{}.EventType(),
		types.TaskAssigned{}.EventType(),
		types.WorkPickupType,
		types.DelegatedReviewType,
	}
	slices.Sort(want)
	if got := inbox.LedgeredTypes(); !slices.Equal(got, want) {
		t.Errorf("ledgered set = %v, want %v", got, want)
	}
}
