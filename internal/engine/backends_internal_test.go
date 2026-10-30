package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/store"
)

// ttlBackend is a coord.Backend that reports a lease TTL, which is the shape
// the KV one has and the in-process one does not.
type ttlBackend struct {
	coord.Backend
	ttl time.Duration
}

func (b ttlBackend) TTL() time.Duration { return b.ttl }

// THE TTL IN FORCE IS WHAT THIS NODE ACQUIRES WITH, not the one its own file
// asks for.
//
// The KV backend ADOPTS the lease bucket rather than rewriting it, so on a
// fleet the TTL is whichever member created it first — and validateTTL refuses
// a claim longer than the bucket's own age. A node that kept acquiring at its
// configured value would, when configured LONGER, have every acquire refused:
// it holds no seats at all while answering every health check, which is the
// worst shape this failure has. Configured SHORTER is quieter and still wrong
// — the heartbeat and the release budget are fractions of this number, so the
// node would renew on a cadence the bucket outlives.
func TestTheLeaseTTLInForceBeatsThisNodesOwnConfig(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Coordination.LeaseTTLSeconds = 90
	configured := 90 * time.Second

	// A PEER GOT THERE FIRST, with a shorter bucket.
	live := 45 * time.Second
	if got := effectiveLeaseTTL(&b, ttlBackend{ttl: live}); got != live {
		t.Errorf("this node would acquire with %v against a bucket of %v — "+
			"validateTTL refuses that, so the node holds nothing", got, live)
	}

	// A LONGER BUCKET is taken too: the arbiter is the bucket either way,
	// and claiming less than it would lapse a lease early for no reason.
	longer := 5 * time.Minute
	if got := effectiveLeaseTTL(&b, ttlBackend{ttl: longer}); got != longer {
		t.Errorf("a bucket of %v resolved to %v", longer, got)
	}

	// A BACKEND WITH NO CEILING TO REPORT leaves the configured value
	// alone: the in-process one is this node's own and nobody else's.
	if got := effectiveLeaseTTL(&b, coordmem.New()); got != configured {
		t.Errorf("the in-process backend resolved to %v, want the configured %v",
			got, configured)
	}

	// AND A BACKEND REPORTING NOTHING falls back rather than minting a zero
	// TTL, which is a lease that has already lapsed.
	if got := effectiveLeaseTTL(&b, ttlBackend{ttl: 0}); got != configured {
		t.Errorf("a backend reporting no TTL resolved to %v, want %v", got, configured)
	}
}

// THE DEFAULT LEASE TTL IS ONE A DEPLOYMENT COULD HAVE WRITTEN.
//
// An unset coordination.lease_ttl_seconds takes the seat layer's measured
// constant here ([leaseTTL]), because config must not import it, and Tier A
// refuses anything set below [config.MinLeaseTTL] for the watchdog's sake. The
// two live in packages that cannot see each other, so a default lowered past
// the floor would boot every deployment that set nothing on a TTL the floor
// exists to refuse — the one place the rule would not reach.
func TestTheDefaultLeaseTTLIsNotBelowTheFloor(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Coordination.LeaseTTLSeconds = 0
	if got := leaseTTL(&b); got < config.MinLeaseTTL {
		t.Fatalf("an unset lease TTL takes %v, below the %v floor Tier A holds a set "+
			"one to", got, config.MinLeaseTTL)
	}
}

// A DATA NODE HOLDS THE REPLICATED ESTATE SIZED FOR THE WHOLE REGISTER, AND A
// NODE WITHOUT `data` HOLDS NONE.
//
// Every domain's apply loop pins one connection of the replicated pool for its
// life, and the pool is sized for exactly the logs it was opened for: a pin
// short and the applier that finds none refuses at its first round, so its
// domain's rows never move. Every DATA node runs every domain, so [holdEstate]
// opens the estate for the whole register — and the store refuses opening it
// again for any other count, so opening it for the register's count after
// [holdEstate] is the check. Its own file pins nothing, since no apply loop
// writes it ([storeOptions]).
//
// A node without data holds no estate at all: a scratch store, discarded at
// every boot, that answers [store.ErrNoEstate] to every reach for one rather
// than an empty database that reads as a company with nothing in it.
//
// Mutation: size holdEstate's open from anything but the register and the
// second open refuses; drop its role check and the stateless node holds one.
func TestADataNodeHoldsTheEstateForTheWholeRegister(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		roles []string
		holds bool
	}{
		{"every role", nil, true},
		{"data alone", []string{string(placement.RoleData)}, true},
		{"seats without data", []string{string(placement.RoleSeats)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := testBootstrap(t)
			b.Node.Roles = c.roles
			b.Store.Path = filepath.Join(t.TempDir(), "node.db")
			b.Store.Scratch = !c.holds
			opts := storeOptions(&b, nil)
			if opts.Scratch == c.holds {
				t.Errorf("roles %v open the node's own file with scratch=%v, want %v: "+
					"only a node that holds no data may discard its store at boot",
					c.roles, opts.Scratch, !c.holds)
			}
			db, err := store.OpenNode(t.Context(), b.Store.Path, opts)
			if err != nil {
				t.Fatalf("open the node's store: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := holdEstate(t.Context(), db, &b); err != nil {
				t.Fatalf("hold the estate: %v", err)
			}
			if !c.holds {
				if _, err := db.ReplicatedDB(); !errors.Is(err, store.ErrNoEstate) {
					t.Errorf("roles %v hold a replicated estate (%v), want none: a "+
						"node without data asks a data node for everything", c.roles, err)
				}
				return
			}
			if _, err := db.ReplicatedDB(); err != nil {
				t.Fatalf("roles %v hold no replicated estate after holdEstate: %v",
					c.roles, err)
			}
			if _, err := db.OpenReplicated(t.Context(), len(register())); err != nil {
				t.Errorf("roles %v opened the estate for some count other than the "+
					"register's %d, so an apply loop comes up short of a pin: %v",
					c.roles, len(register()), err)
			}
		})
	}
}

// THE FLOOR'S ERROR SENDS THE OPERATOR WHERE THE BUCKET IS: stream.store_dir
// only where this node's own broker keeps it, the external cluster on
// stream.type nats, and the data members on a leaf.
func TestTheRunsBucketIsNamedWhereItLives(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stream config.Stream
		want   string
	}{
		"embedded": {stream: config.Stream{Type: config.StreamEmbedded}, want: "stream.store_dir"},
		"external": {stream: config.Stream{Type: config.StreamNATS}, want: "stream.url"},
		"leaf": {stream: config.Stream{Type: config.StreamEmbedded,
			Leaf: config.StreamLeaf{URLs: []string{"nats-leaf://member-a.example.com:7422"}}}, want: "stream.leaf.urls"},
	} {
		got := runsBucketHome(&config.Bootstrap{Stream: tc.stream})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not name %s", name, got, tc.want)
		}
		if tc.want != "stream.store_dir" && strings.Contains(got, "stream.store_dir") {
			t.Errorf("%s: %q names stream.store_dir, which holds none of this bucket", name, got)
		}
	}
}
