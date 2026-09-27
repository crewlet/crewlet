package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
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

// THE STORE PINS ONE WRITER PER DOMAIN THIS NODE RUNS, NOT PER DOMAIN THIS
// BUILD REGISTERS.
//
// Every running domain's apply loop holds one connection of the replicated
// pool for its life, and the pool is sized for exactly the pins declared. A
// satellite whose roles exclude a domain starts no applier for it, so a pin
// reserved for one is a connection nothing ever takes — a reader's connection,
// for the life of the process. And a pin short is worse: the applier that
// finds none refuses at its first round and its domain's rows never move.
//
// The rows are every role set [placement.ParseRoles] accepts a node declaring
// on its own, the empty one included, because "declared nothing" is every
// role. What each must pin is counted off the register's own predicates here
// rather than read back through [participationOf], so a pool sized from the
// whole register fails the one row where the two differ.
func TestTheStorePinsAWriterPerDomainTheRolesRun(t *testing.T) {
	t.Parallel()
	registered := len(register())
	for _, roles := range [][]string{
		nil,
		{"ingress"},
		{"seats"},
		{"workers"},
		{"seats", "workers"},
		{"ingress", "seats", "workers"},
	} {
		t.Run(fmt.Sprint(roles), func(t *testing.T) {
			t.Parallel()
			b := testBootstrap(t)
			b.Node.Roles = roles
			set, err := placement.ParseRoles(roles)
			if err != nil {
				t.Fatalf("ParseRoles(%v): %v", roles, err)
			}
			if len(set) == 0 {
				set = placement.DefaultRoles()
			}
			want := 0
			for _, entry := range register() {
				if entry.Participates(set) {
					want++
				}
			}

			opts, err := storeOptions(&b, nil)
			if err != nil {
				t.Fatalf("storeOptions: %v", err)
			}
			if opts.PinnedWriters != want {
				t.Errorf("roles %v pin %d writer(s), want %d — one per domain "+
					"these roles run, of the %d registered", roles,
					opts.PinnedWriters, want, registered)
			}
			if got := len(participationOf(set).Run); opts.PinnedWriters != got {
				t.Errorf("roles %v pin %d writer(s) and start %d apply loop(s) — "+
					"the two are one decision", roles, opts.PinnedWriters, got)
			}
		})
	}

	// THE ROW THE WHOLE TABLE EXISTS FOR, stated on its own: a seats-only
	// satellite runs every domain but the identity estate's. Without a
	// row where the count differs from the register's length, a pool
	// sized from the register passes every case above.
	b := testBootstrap(t)
	b.Node.Roles = []string{"seats"}
	opts, err := storeOptions(&b, nil)
	if err != nil {
		t.Fatalf("storeOptions: %v", err)
	}
	if opts.PinnedWriters != registered-1 {
		t.Errorf("a seats-only node pins %d writer(s) of %d registered domains, "+
			"want %d: it runs no identity estate, so a pin for one is a reader's "+
			"connection nothing takes", opts.PinnedWriters, registered, registered-1)
	}
}
