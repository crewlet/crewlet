package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/tools"
)

// A fleet over a REAL lease table: node n1 is upgraded, node n2 runs an older
// build, and each holds one seat. The gate is exercised through
// [coord.FeatureReader] rather than a fake, so what is certified is the answer
// a real heartbeat produces.
func featureFleet(t *testing.T) (*coordtest.Faulty, Fleet) {
	t.Helper()
	ctx := context.Background()
	store := coordtest.NewFaulty(memory.New())
	claim := func(resource, owner string, meta map[string]any) {
		t.Helper()
		lease, err := store.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: time.Hour, Ungated: true, Meta: meta,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim %s: %v (granted %v)", resource, err, lease != nil)
		}
	}
	upgraded := coord.NodeStatus{Features: []coord.Feature{coord.FeatureMCPStatus}}
	older := coord.NodeStatus{InFlight: 1}
	claim(coord.NodeResource("n1"), "n1:a", map[string]any{coord.StatusKey: upgraded.Meta()})
	claim(coord.NodeResource("n2"), "n2:a", map[string]any{coord.StatusKey: older.Meta()})
	claim(coord.SeatResource("ceo"), "n1:a", nil)
	claim(coord.SeatResource("pm"), "n2:a", nil)
	return store, coord.FeatureReader{Leases: store}
}

// A GESTURE THE CARRYING NODE CANNOT HONOUR IS REFUSED `peer_upgrading`, and
// one it can goes ahead. Without the gate the older node would accept the
// gesture by never hearing of it, while this node answered `pending`.
func TestAVerbTheOwnerCannotHonourRefusesPeerUpgrading(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, fleet := featureFleet(t)

	if r := seatCanCarry(ctx, fleet, "steer_turn", "ceo", coord.FeatureMCPStatus); r != nil {
		t.Errorf("the upgraded holder's seat was refused: %+v", *r)
	}
	requireRefusal(t, "the older holder's seat", tools.RefusalPeerUpgrading, "steer_turn",
		seatCanCarry(ctx, fleet, "steer_turn", "pm", coord.FeatureMCPStatus))
	// The gate asks for THE FEATURE THE GESTURE NEEDS, not for "is this
	// node upgraded": the upgraded holder still cannot carry a gesture from
	// a build newer than its own.
	requireRefusal(t, "a feature newer than the holder's build", tools.RefusalPeerUpgrading, "steer_turn",
		seatCanCarry(ctx, fleet, "steer_turn", "ceo", "from_a_newer_build"))
	// A fleet-wide gesture is refused while ANY node lacks it.
	requireRefusal(t, "a fleet with an older node", tools.RefusalPeerUpgrading, "pause_seat",
		fleetCanCarry(ctx, fleet, "pause_seat", coord.FeatureMCPStatus))
}

// AN UNREADABLE FLEET IS `unavailable`, NOT `peer_upgrading`. A store blip is
// something a retry clears; telling a person their fleet is mid-upgrade sends
// them looking for an upgrade that is not happening. And it is never a yes —
// that would accept a gesture nobody verified can be carried out.
func TestAnUnreadableFleetIsUnavailableNotRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, fleet := featureFleet(t)
	store.Break(nil)
	requireRefusal(t, "a seat gesture on a broken store", tools.RefusalUnavailable, "answer_run",
		seatCanCarry(ctx, fleet, "answer_run", "ceo", coord.FeatureMCPStatus))
	requireRefusal(t, "a fleet gesture on a broken store", tools.RefusalUnavailable, "pause_seat",
		fleetCanCarry(ctx, fleet, "pause_seat", coord.FeatureMCPStatus))

	// A surface wired with no reader at all is the same answer: a gate
	// that opened when nobody connected it would not be a gate.
	requireRefusal(t, "a surface with no fleet reader", tools.RefusalUnavailable, "steer_turn",
		seatCanCarry(ctx, nil, "steer_turn", "ceo", coord.FeatureMCPStatus))
	requireRefusal(t, "a surface with no fleet reader", tools.RefusalUnavailable, "resume_seat",
		fleetCanCarry(ctx, nil, "resume_seat", coord.FeatureMCPStatus))
}

// A HOLDER THAT HAS NOT SAID is also `unavailable`: a seat whose holder is
// mid-drain has no presence to read, which is a moment rather than a build.
func TestAHolderThatHasNotSaidIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, fleet := featureFleet(t)
	if _, err := store.TryAcquire(ctx, coord.SeatResource("cto"), coord.AcquireOptions{
		Owner: "n3:draining", TTL: time.Hour,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	requireRefusal(t, "a holder with no presence", tools.RefusalUnavailable, "answer_run",
		seatCanCarry(ctx, fleet, "answer_run", "cto", coord.FeatureMCPStatus))
}

// requireRefusal checks the class and that the sentence names the tool: a
// person's surface acts on the class, and a model reading the sentence needs
// to know which of its calls was turned away.
func requireRefusal(t *testing.T, what string, want tools.Refusal, tool string, got *tools.Result) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: went ahead, want a %s refusal", what, want)
	}
	if !got.Failed || got.Refusal != want {
		t.Fatalf("%s: answered %+v, want a %s refusal", what, *got, want)
	}
	if !strings.Contains(got.Output, tool) {
		t.Errorf("%s: the refusal does not name %s: %q", what, tool, got.Output)
	}
}
