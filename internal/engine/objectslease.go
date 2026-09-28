package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
)

// The object store's membership lease: `objects:{node}` (coord.ClassObjects),
// kept by the membership-lease loop both placement maps share
// (memberlease.go).
//
// # Why the store claims it, and not the seat host
//
// Membership in the object store used to be read off the node's PRESENCE, and
// presence is the seat host's: a shutdown drain gives it back at its very first
// step, while this node is still serving every chunk it holds. A drain longer
// than the placement map's grace therefore moved the node's whole share of the
// company's files to the other members — and moved it back when the node
// returned — for a node that never stopped answering. So the store claims its
// own lease, renews it on its own loop, and gives it back only once its chunk
// server has been withdrawn ([Engine.stopObjects]).
//
// # What it carries, and why every beat
//
// Presence carried a weight the node was CONFIGURED with and nothing about
// whether the volume under it still worked, so a data node whose objects
// directory had failed stayed placed on for ever and every write sent it a
// copy it could not keep. The lease carries the store's own account of its
// health, measured on every beat (disk.Store.Probe), and what its passes last
// found — the repair the map's split waits on, the scrub, the strays an
// operator waits on before stopping it — so the maintainer can take a failed
// store out of the map and an operator can see why. Re-sent on EVERY renew,
// as presence's profile is, because it describes the live process.

// startObjectsLease claims this node's object-store membership at once and
// renews it until stopped, saying what meta says on every beat.
func startObjectsLease(ctx context.Context, leases coord.Backend, node, owner string,
	ttl time.Duration, meta func() objstore.ObjectsMeta) *memberLease {

	return startMemberLease(ctx, leases, memberLeaseSpec{
		resource: coord.ObjectsResource(node), node: node, owner: owner, ttl: ttl,
		what: "object-store membership", event: "objects",
		// NEVER UNSAID: every field an objects lease carries is either
		// measured on the beat or absent until there is something to say,
		// so there is always an account to write.
		meta: func() (map[string]any, error) { return meta().Encode(), nil },
	})
}
