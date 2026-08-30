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
		types.TaskCreated{}.EventType(),
		types.TaskCompleted{}.EventType(),
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
// A set like this collects defensive aliases: a fifth name, "notification",
// that nothing in the engine ever emits, outliving whatever once produced it.
// A dead name in this set is invisible: nothing refuses it,
// nothing logs it, it simply never matches, and a reader comparing the set
// against the system's behaviour finds a type that appears covered and is not.
//
// Every ledgered name must therefore be one the system can actually put on an
// inbox: a registered payload type, or one of the two A2A wakes, which are
// registered nowhere because they carry no schema. Pinning the exemption to
// exactly those two is what stops this test being satisfied by adding another.
//
// It reads LedgeredTypes rather than a list of its own. An earlier version
// iterated the four names it expected and asked whether each was ledgered,
// which is a different question: adding "notification" back to the set left it
// green, because the name it never asked about was the one that was wrong.
func TestNoLedgeredTypeIsANameNothingPublishes(t *testing.T) {
	t.Parallel()
	registered := events.RegisteredTypes()

	var unregistered []string
	for _, kind := range inbox.LedgeredTypes() {
		if !slices.Contains(registered, kind) {
			unregistered = append(unregistered, kind)
		}
	}
	want := []string{types.A2AMessageType, types.A2ARequestType}
	slices.Sort(unregistered)
	slices.Sort(want)
	if !slices.Equal(unregistered, want) {
		t.Errorf("ledgered names with no registered payload = %v, want exactly %v",
			unregistered, want)
	}
}

// TestTheLedgeredSetIsExactlyWhatItClaims pins the size.
//
// LedgeredTypes is what the guard above reads, so a name added to the map is
// only as visible as the assertions over it. This is the one that notices a
// FIFTH covered type appearing without anyone deciding it should be — the
// guard above accepts any registered type, and every trigger type is
// registered.
func TestTheLedgeredSetIsExactlyWhatItClaims(t *testing.T) {
	t.Parallel()
	want := []string{
		types.A2AMessageType,
		types.A2ARequestType,
		types.ExternalNotification{}.EventType(),
		types.TaskAssigned{}.EventType(),
	}
	slices.Sort(want)
	if got := inbox.LedgeredTypes(); !slices.Equal(got, want) {
		t.Errorf("ledgered set = %v, want %v", got, want)
	}
}
