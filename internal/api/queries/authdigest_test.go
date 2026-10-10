package queries_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Which key table each node's API admits, held against the answering node's:
// Tier A is per node, and a key that is a member's on one node and an admin's
// on the next is a way to reach more through whichever node a load balancer
// picks.

// tableDigest is a key table's fingerprint under the one keyring key every
// node of these cases holds.
func tableDigest(t *testing.T, table string) placement.AuthDigest {
	t.Helper()
	ring := secrets.Keyring{Keys: map[string][]byte{"k1": bytes.Repeat([]byte{3}, 32)}}
	f, err := ring.Fingerprint(config.AuthDigestDomain, []byte(table))
	if err != nil {
		t.Fatal(err)
	}
	return placement.AuthDigest{Serves: true, Keys: f}
}

// presenceOf is a lease table holding one presence lease per profile, each
// carrying the profile as a node's heartbeat does.
func presenceOf(t *testing.T, nodes ...placement.NodeProfile) *coordmemory.Backend {
	t.Helper()
	backend := coordmemory.New()
	for _, n := range nodes {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(n.ID), coord.AcquireOptions{
			Owner: n.ID + ":1", TTL: time.Minute, Meta: n.Meta(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return backend
}

// keyFleet is the fleet these cases read: this node (whose own row is the
// table it held before a restart), a peer with its table, a peer with another,
// a peer serving the API with no keyring, and a peer serving no API at all.
func keyFleet(t *testing.T) *coordmemory.Backend {
	return presenceOf(t,
		placement.NodeProfile{ID: "core-1", Roles: placement.DefaultRoles(), Auth: tableDigest(t, "before the restart")},
		placement.NodeProfile{ID: "core-2", Roles: placement.DefaultRoles(), Auth: tableDigest(t, "table")},
		placement.NodeProfile{ID: "core-3", Roles: placement.DefaultRoles(), Auth: tableDigest(t, "another table")},
		placement.NodeProfile{ID: "core-4", Roles: placement.DefaultRoles(), Auth: placement.AuthDigest{Serves: true}},
		placement.NodeProfile{ID: "worker-1", Roles: placement.Roles(placement.RoleSeats)},
	)
}

// EVERY NODE ROW SAYS HOW ITS KEY TABLE STANDS AGAINST THIS NODE'S — `same`,
// `different`, or `incomparable` for a node with no keyring — and a node that
// serves no API carries no table at all. This node's own row is the table THIS
// PROCESS admits, not its presence row, which for a lease TTL after a restart
// is still the previous process's.
func TestEveryNodeRowSaysWhetherItAdmitsThisNodesKeys(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Coord: keyFleet(t), NodeID: "core-1", AuthDigest: tableDigest(t, "table"),
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))
	got := map[string]any{}
	for _, row := range rowsOf(t, body["nodes"]) {
		auth, ok := row["auth"].(map[string]any)
		if !ok {
			got[row["id"].(string)] = nil
			continue
		}
		got[row["id"].(string)] = auth["agreement"]
	}
	want := map[string]any{
		"core-1": "same", "core-2": "same", "core-3": "different",
		"core-4": "incomparable", "worker-1": nil,
	}
	for id, agreement := range want {
		if got[id] != agreement {
			t.Errorf("%s: agreement %v, want %v (rows: %v)", id, got[id], agreement, got)
		}
	}
}

// ACCESS SAYS WHETHER THE KEYS IT LISTS ARE EVERY OTHER NODE'S: each other
// live node serving the API against this node's table, this node's own row and
// a node serving no API left out, in id order.
func TestAccessSaysWhetherEveryOtherNodeAdmitsTheseKeys(t *testing.T) {
	t.Parallel()
	s := accessSources(t, queries.AccessPosture{Keys: keys("founder")})
	s.Coord, s.NodeID, s.AuthDigest = keyFleet(t), "core-1", tableDigest(t, "table")
	got := askAccess(t, s).Fleet

	if !got.Listed {
		t.Fatal("a fleet that was listed says it was not")
	}
	if len(got.Digests) != 1 || got.Digests[0].KeyID != "k1" || got.Digests[0].Digest == "" {
		t.Errorf("this node's digests = %+v, want its one key's", got.Digests)
	}
	var seen []string
	for _, n := range got.Nodes {
		seen = append(seen, n.ID+"="+string(n.Agreement))
	}
	want := []string{"core-2=same", "core-3=different", "core-4=incomparable"}
	if !slices.Equal(seen, want) {
		t.Errorf("nodes = %v, want %v", seen, want)
	}
}

// A FLEET NOBODY COULD LIST IS NOT A FLEET THAT AGREES. With no coordination,
// or a listing that failed, the keys and people are still answered — they are
// who reaches the company through this node — and `listed` is false, so an
// empty list of nodes is never read as every node holding these keys.
func TestAccessWithNoFleetToReadSaysSo(t *testing.T) {
	t.Parallel()
	for name, backend := range map[string]coord.Backend{
		"no coordination":   nil,
		"a listing refused": refusingLister{coordmemory.New()},
	} {
		s := accessSources(t, queries.AccessPosture{Keys: keys("founder")})
		s.Coord, s.NodeID, s.AuthDigest = backend, "core-1", tableDigest(t, "table")
		got := askAccess(t, s)
		if got.Fleet.Listed || len(got.Fleet.Nodes) != 0 || got.Fleet.Nodes == nil {
			t.Errorf("%s: fleet = %+v, want unlisted with an empty list", name, got.Fleet)
		}
		if len(got.Tokens) != 1 {
			t.Errorf("%s: the keys were not answered: %+v", name, got.Tokens)
		}
	}
}

// refusingLister is a lease table whose listing cannot be read.
type refusingLister struct{ *coordmemory.Backend }

func (refusingLister) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.Join(coord.ErrUnavailable, errors.New("the store is unreachable"))
}
