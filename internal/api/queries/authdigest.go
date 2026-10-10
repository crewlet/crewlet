package queries

import (
	"cmp"
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Which key table each node's API admits, as `fleet` and `access` show it.
//
// Tier A is per node, so every node serving the API judges a request by its
// own `api.auth`, and two that disagree are a way to reach more through one of
// them. Each publishes a keyed fingerprint of its table on its presence
// ([placement.AuthDigest]); these answers hold every live node's against the
// table of THE NODE ANSWERING, which is what "does this match" can mean to a
// reader standing on one of them. The fleet-wide verdict — which nodes hold
// which table — is the `auth_diverged` alarm's, evaluated once on every node.

// NodeAuth is one node's key table as an answer shows it.
type NodeAuth struct {
	// Agreement is how the table stands against the answering node's:
	// `same`, `different`, or `incomparable` — the two share no keyring key,
	// usually because one of them has no keyring at all. Never read as
	// either of the others.
	Agreement secrets.Agreement `json:"agreement"`

	// Digests are what the node published, one per keyring key id, in id
	// order — the fingerprint, never the table. Empty on a node with no
	// keyring, which is the reason it compares with nobody.
	Digests []TableDigest `json:"digests"`
}

// TableDigest is one keyring key's digest of a node's table. Two nodes with the
// same digest under the same key id hold the same table.
type TableDigest struct {
	KeyID  string `json:"key_id"`
	Digest string `json:"digest"`
}

// nodeAuth renders a node's digest against the answering node's, or nil for a
// node that serves no API — it admits nobody by a table, so there is nothing
// of its to show, which is absent rather than an empty table.
func (s Sources) nodeAuth(d placement.AuthDigest) *NodeAuth {
	if !d.Serves {
		return nil
	}
	return &NodeAuth{Agreement: s.AuthDigest.Against(d), Digests: digestsOf(d.Keys)}
}

// digestsOf is a fingerprint as the wire carries it, in key id order. ALWAYS A
// LIST: no keyring is a real posture.
func digestsOf(f secrets.Fingerprint) []TableDigest {
	out := make([]TableDigest, 0, len(f))
	for id, d := range f {
		out = append(out, TableDigest{KeyID: id, Digest: d.Digest})
	}
	slices.SortFunc(out, func(a, b TableDigest) int { return cmp.Compare(a.KeyID, b.KeyID) })
	return out
}

// AccessFleet is how the key table the answering node holds stands against
// every other live node serving the API — `access`'s half of the comparison,
// drawn beside the keys it is a fingerprint of.
type AccessFleet struct {
	// Digests are this node's own, as its presence publishes them.
	Digests []TableDigest `json:"digests"`

	// Listed says Nodes is every live node rather than what could be read
	// of the fleet: a presence listing that failed, or a node with no
	// coordination to list, leaves it false, and an empty Nodes then means
	// nothing — never "every node agrees".
	Listed bool `json:"listed"`

	// Nodes are the OTHER live nodes that serve the API, in id order, each
	// against this node's table. A node that serves no API is not among them:
	// nothing reaches the company through it.
	Nodes []AccessNode `json:"nodes"`
}

// AccessNode is one other node's table against the answering node's.
type AccessNode struct {
	ID        string            `json:"id"`
	Agreement secrets.Agreement `json:"agreement"`
	Digests   []TableDigest     `json:"digests"`
}

// accessFleet lists the live nodes and holds each one's table against this
// node's.
//
// A LISTING THAT FAILS DEGRADES rather than failing the answer: the keys and
// the people are still the answer to who can reach the company through this
// node, and refusing them because the fleet could not be read would blank the
// screen somebody opens to rotate a key. `listed` says which it was.
func (s Sources) accessFleet(ctx context.Context) AccessFleet {
	out := AccessFleet{Digests: digestsOf(s.AuthDigest.Keys), Nodes: []AccessNode{}}
	if s.Coord == nil {
		return out
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		log.WarnContext(ctx, "access_fleet_unavailable", "error", err)
		return out
	}
	out.Listed = true
	for _, lease := range leases {
		profile, ok := placement.FromLease(lease)
		if !ok || profile.ID == s.NodeID {
			continue
		}
		auth := s.nodeAuth(profile.Auth)
		if auth == nil {
			continue
		}
		out.Nodes = append(out.Nodes, AccessNode{ID: profile.ID, Agreement: auth.Agreement, Digests: auth.Digests})
	}
	slices.SortFunc(out.Nodes, func(a, b AccessNode) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
