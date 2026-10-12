package jetstreamtest

import (
	"context"
	"fmt"
	"testing"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// EVERY CLUSTER THIS HARNESS NAMES CARRIES A NAME NO OTHER ONE DOES, and every
// member of one carries its cluster's.
//
// A member a case stops for good leaves its peers dialling its route port for
// as long as they run, and a cluster another case starts in the same binary
// can be handed that port. Under the one name every cluster here used to share,
// that member was accepted into the first cluster's mesh and the two merged —
// colliding server names, and so colliding JetStream peer ids. A name per
// cluster is what makes the server refuse that route ([newClusterName]), so
// this holds every way the harness names one against the others: each attempt
// of a bring-up and a second bring-up, through the one retry loop every
// started cluster and relay mesh passes ([withFreshPorts]), a relay mesh, and
// a direct mesh, which reserves its ports outside that loop.
//
// No member is started: what is held is the name each is handed, and that
// [memberConfig] gives every member the name it is handed, so a case that
// stood clusters up here would spend seconds proving what a value proves.
func TestEveryClusterTheHarnessNamesIsNamedApart(t *testing.T) {
	t.Parallel()
	named := map[string]string{}
	attempts := 0
	withFreshPorts(t.Context(), t, "naming probe", func(_ context.Context, name string) (*Cluster, error) {
		attempts++
		named[fmt.Sprintf("attempt %d of one bring-up", attempts)] = name
		if attempts == 1 {
			// RETRIED, as a lost port is: the second attempt is a
			// cluster of its own, whose name the first's survivors —
			// had there been any — must not share.
			return nil, fmt.Errorf("%w: the probe's first attempt", js.ErrRoutePortTaken)
		}
		return &Cluster{}, nil
	})
	withFreshPorts(t.Context(), t, "naming probe", func(_ context.Context, name string) (*Cluster, error) {
		named["another bring-up"] = name
		return &Cluster{}, nil
	})
	named["a relay mesh"] = StartRelays(t.Context(), t, 2).ClusterName()
	named["a direct mesh"] = StartDirectMesh(t.Context(), t, 2).ClusterName()

	if len(named) != 5 {
		t.Fatalf("named %d clusters, want 5: %v", len(named), named)
	}
	by := map[string]string{}
	for what, name := range named {
		if name == "" {
			t.Errorf("%s carries no cluster name", what)
			continue
		}
		if other, ok := by[name]; ok {
			t.Errorf("%s and %s are both named %q — a member either stops for "+
				"good leaves its peers dialling a port the other can be handed, "+
				"and under one name the two clusters merge", other, what, name)
		}
		by[name] = what
	}

	// AND EVERY MEMBER CARRIES THE NAME ITS CLUSTER WAS HANDED.
	name := named["another bring-up"]
	for i := range 3 {
		if got := memberConfig(js.Config{}, name, i, 3, 0, nil).ClusterName; got != name {
			t.Errorf("member %d of a cluster named %q is named %q", i, name, got)
		}
	}
}
