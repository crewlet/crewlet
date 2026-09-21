package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
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
