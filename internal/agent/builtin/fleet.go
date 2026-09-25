package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/tools"
)

// The FEATURE GATE: a gesture another node carries out is refused until that
// node's build can carry it.
//
// A person's pause, note or answer is accepted by whichever node serves their
// request and carried out by the node holding the seat — and mid-upgrade that
// node may be running a build that has never heard of the gesture. It would
// not refuse: it would simply never do it, while this node answered `pending`.
// So before such a tool writes anything it asks the fleet, and refuses in the
// class a person's surface acts on:
//
//   - `peer_upgrading` when the node that would carry it out definitively
//     cannot — nothing to retry until the upgrade finishes;
//   - `unavailable` when the fleet could not be read or has not said — a
//     store blip, a heartbeat that overran, a holder mid-drain — which a retry
//     may well clear. Answering peer_upgrading there would tell somebody a
//     healthy fleet was mid-upgrade; answering yes would accept a gesture
//     nobody verified could happen. See [coord.FeatureReader] for how each
//     answer is reached.

// Fleet answers whether the node or nodes that would carry a gesture out can.
// Both answers are three-valued: an error is "could not tell", never "no".
type Fleet interface {
	// SeatFeature is about the node holding one seat, for a gesture only
	// the holder carries out.
	SeatFeature(ctx context.Context, handle string, feature coord.Feature) (bool, error)

	// AllLiveHave is about every live node, for a gesture any future
	// holder has to respect.
	AllLiveHave(ctx context.Context, feature coord.Feature) (bool, error)
}

// seatCanCarry gates a gesture on the node holding handle's seat. Nil means
// go ahead; otherwise it is the refusal to answer with, naming the tool.
func seatCanCarry(ctx context.Context, fleet Fleet, tool, handle string, feature coord.Feature) *tools.Result {
	if fleet == nil {
		return refusalOf(fleetUnread(tool, errNoFleet))
	}
	ok, err := fleet.SeatFeature(ctx, handle, feature)
	return featureVerdict(tool, fmt.Sprintf("the node running %s", handle), ok, err)
}

// fleetCanCarry gates a gesture on every live node.
func fleetCanCarry(ctx context.Context, fleet Fleet, tool string, feature coord.Feature) *tools.Result {
	if fleet == nil {
		return refusalOf(fleetUnread(tool, errNoFleet))
	}
	ok, err := fleet.AllLiveHave(ctx, feature)
	return featureVerdict(tool, "a node in this fleet", ok, err)
}

// errNoFleet is a surface wired without a fleet reader. It refuses as
// unavailable rather than letting the gesture through, because a gate that
// opens when nobody connected it is not a gate.
var errNoFleet = errors.New("this surface was built without a fleet reader")

func featureVerdict(tool, who string, ok bool, err error) *tools.Result {
	switch {
	case err != nil:
		return refusalOf(fleetUnread(tool, err))
	case !ok:
		return refusalOf(refused(tools.RefusalPeerUpgrading, fmt.Sprintf(
			"%s is refused: %s runs an older build that cannot carry it out. "+
				"It will be accepted once the upgrade reaches every node.", tool, who)))
	default:
		return nil
	}
}

func fleetUnread(tool string, err error) tools.Result {
	return refused(tools.RefusalUnavailable, fmt.Sprintf(
		"%s is unavailable: could not confirm the fleet can carry it out (%v). "+
			"Try again shortly.", tool, err))
}
