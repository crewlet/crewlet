package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// What a node's BUILD can honour, and how a gesture asks before it is offered.
//
// # Why a gesture has to ask
//
// A rolling upgrade runs two builds on one fleet for as long as it takes, and
// some gestures are carried out by a node OTHER than the one that accepted
// them: a person pauses a seat on the node serving their dashboard, and the
// node that holds the seat is the one that has to stop taking its mail. An
// older build there does not refuse the gesture — it has never heard of it —
// so it would be accepted, answered `pending`, and silently never happen.
// Refusing it `peer_upgrading` until the fleet can carry it is the only
// honest answer, and it needs each node to say what it can do.
//
// # Why on the presence heartbeat
//
// Because that is already where a node says what it IS ([NodeStatus]), every
// peer already reads it, and its freshness is the heartbeat interval — a node
// that finishes upgrading is believed within one beat, with no registry to
// keep and no second record that could disagree with the lease table.
//
// # Why three-valued
//
// "Every node honours it", "one does not" and "the store could not say" call
// for three different answers to a person: carry it out, `peer_upgrading`,
// and `unavailable` (try again). Collapsing the last into the second would
// tell somebody a fleet mid-blip is mid-upgrade; collapsing it into the first
// would accept a gesture nobody verified can be carried out. See CLAUDE.md's
// rule for a three-valued answer.

// Feature names one gesture a build can carry out on behalf of a peer.
//
// A named string with [Feature.Valid], because the value travels: a newer peer
// advertises features this build has no constant for, and those must arrive
// as values nobody asks about rather than as a decode error.
type Feature string

const (
	// FeatureMCPStatus — the node publishes [NodeStatus.MCP], so an absent
	// list there means "started no MCP server" rather than "did not say".
	FeatureMCPStatus Feature = "mcp_status"
)

// Features is every feature THIS build honours, which is exactly what a node
// running it advertises.
//
// A CONSTANT IS ADDED IN THE SAME CHANGE AS THE CODE THAT HONOURS IT. The list
// is a claim a peer acts on — a gesture it gates is accepted the moment every
// node carries the name — so a name listed ahead of its implementation is a
// fleet told it can do something it cannot.
var Features = []Feature{FeatureMCPStatus}

// Valid reports whether this build knows the feature.
func (f Feature) Valid() bool { return slices.Contains(Features, f) }

// ErrFeatureUnknown is a feature read that could not conclude: the store
// answered, but the node that would have to carry the gesture has not said
// what it can do — its heartbeat carried no status (the status hook overran
// its budget, or the node is mid-drain and dropped its presence) or no node is
// live at all. It is RETRYABLE, which is precisely what separates it from a
// definite "no": that one is the tool refusal class `peer_upgrading`, and this
// is `unavailable`.
var ErrFeatureUnknown = errors.New("coord: the node that would carry this has not said what it can do")

// FeatureReader answers feature questions from the lease table. It is the
// concrete type a consumer's own two-method interface is satisfied by.
type FeatureReader struct {
	Leases Backend
}

// SeatFeature reports whether the node HOLDING a seat honours a feature — the
// question for a gesture only the holder carries out, such as a note into its
// running turn.
//
// A SEAT NOBODY HOLDS is answered for the whole fleet, because whichever live
// node claims it next is the one that will carry the gesture out, and nothing
// says which that will be.
//
// The holder is found by OWNER, not by parsing a node id out of the owner
// string: a seat lease and its node's presence lease carry the same process
// incarnation, and an incarnation that has since restarted is a different
// process whose presence says nothing about the old one's build.
func (r FeatureReader) SeatFeature(ctx context.Context, handle string, feature Feature) (bool, error) {
	lease, err := r.Leases.Get(ctx, SeatResource(handle))
	if err != nil {
		return false, fmt.Errorf("coord: read the holder of seat %q: %w", handle, err)
	}
	if lease == nil {
		return r.AllLiveHave(ctx, feature)
	}
	nodes, err := r.Leases.ListLive(ctx, ClassNode)
	if err != nil {
		return false, fmt.Errorf("coord: read the fleet's presence: %w", err)
	}
	for _, node := range nodes {
		if node.Owner != lease.Owner {
			continue
		}
		status, ok := StatusFromMeta(node.Meta)
		if !ok {
			return false, fmt.Errorf("%w: %s published no status on its last heartbeat",
				ErrFeatureUnknown, node.Resource)
		}
		return slices.Contains(status.Features, feature), nil
	}
	return false, fmt.Errorf("%w: seat %q is held by %s, which has no presence lease "+
		"(it is draining or its heartbeat lapsed)", ErrFeatureUnknown, handle, lease.Owner)
}

// AllLiveHave reports whether EVERY live node honours a feature — the question
// for a gesture any node may end up carrying out, such as a pause every future
// holder of the seat has to respect.
//
// A node that definitively lacks it answers false even when another node could
// not be read: one "no" already decides the gesture, and reporting it as
// unknown would send a person to retry what cannot succeed until the upgrade
// finishes.
func (r FeatureReader) AllLiveHave(ctx context.Context, feature Feature) (bool, error) {
	nodes, err := r.Leases.ListLive(ctx, ClassNode)
	if err != nil {
		return false, fmt.Errorf("coord: read the fleet's presence: %w", err)
	}
	if len(nodes) == 0 {
		// Not vacuously true: the node asking is itself live whenever the
		// fleet is healthy, so an empty membership is a read that says
		// nothing about who would carry the gesture out.
		return false, fmt.Errorf("%w: no node is live", ErrFeatureUnknown)
	}
	var unknown error
	for _, node := range nodes {
		status, ok := StatusFromMeta(node.Meta)
		if !ok {
			// The FIRST such node is the one reported, so two identical
			// reads name the same reason.
			if unknown == nil {
				unknown = fmt.Errorf("%w: %s published no status on its last heartbeat",
					ErrFeatureUnknown, node.Resource)
			}
			continue
		}
		if !slices.Contains(status.Features, feature) {
			return false, nil
		}
	}
	if unknown != nil {
		return false, unknown
	}
	return true, nil
}
