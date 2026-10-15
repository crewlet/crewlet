package codecluster_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
)

// Not t.Parallel(): two three-member clusters at once on one runner is the
// contention internal/solo exists to remove.

// TestTheCodeHostPassesTheFleetSuiteOnTheBroker runs every multi-node case
// with the broker's own object store: one bucket, reached by each member
// through its own broker client, as a fleet's data nodes reach it.
func TestTheCodeHostPassesTheFleetSuiteOnTheBroker(t *testing.T) {
	codetest.RunFleet(t, func(t *testing.T) codetest.Cluster { return startCluster(t, "nats") })
}

// TestTheCodeHostPassesTheFleetSuiteOnS3 runs every multi-node case with S3,
// through an in-process bucket speaking its wire — the production store,
// whose puts go in parts and whose reads go in ranges.
func TestTheCodeHostPassesTheFleetSuiteOnS3(t *testing.T) {
	codetest.RunFleet(t, func(t *testing.T) codetest.Cluster { return startCluster(t, "s3") })
}

// EVERY PUSH OF A REPOSITORY IS APPENDED BY ITS COMMITTER, WHICHEVER MEMBER
// RECEIVED IT: for each of sixteen repositories a push through every member,
// and only the repository's committer — the first of its rendezvous order over
// the running members — appends anything. A router that asked its own member
// first would have each receiver commit its own pushes, and a burst to one
// repository would contend at the broker between three committers.
func TestEveryMemberRoutesARepositoryToOneCommitter(t *testing.T) {
	const repos = 16
	c := startCluster(t, "nats")
	declared := make([]code.Declared, repos)
	for i := range declared {
		declared[i] = code.Declared{Name: fmt.Sprintf("route-%02d", i), Settings: codetest.Settings()}
	}
	c.Declare(t, declared...)
	if err := c.Settle(t.Context()); err != nil {
		t.Fatalf("settle the fleet: %v", err)
	}
	p := codetest.NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	members := c.DataNodes()
	for _, d := range declared {
		rows, err := c.Rows(t.Context(), d.Name, codetest.Linearizable)
		if err != nil {
			t.Fatalf("the rows of %s: %v", d.Name, err)
		}
		committer := c.Committer(rows.Repo.ID)
		before := map[string]int{}
		for _, m := range members {
			before[m] = c.Appends(m)
		}
		results := make([]codetest.Pushed, len(members))
		var wg sync.WaitGroup
		for i, m := range members {
			url := urlOf(t, c, m, d.Name)
			wg.Go(func() { results[i] = p.Push(t.Context(), work, url, fmt.Sprintf("main:refs/heads/via-%s", m)) })
		}
		wg.Wait()
		for i, res := range results {
			if res.Err != nil {
				t.Errorf("%s: the push through %s: %s", d.Name, members[i], res)
			}
		}
		for _, m := range members {
			grew := c.Appends(m) - before[m]
			switch {
			case m == committer && grew == 0:
				t.Errorf("%s: its committer %s appended none of the pushes received by %v", d.Name, m, members)
			case m != committer && grew != 0:
				t.Errorf("%s: %s appended %d records, and the repository's committer is %s", d.Name, m, grew,
					committer)
			}
		}
	}
}

// urlOf is the repository called name's URL through member node.
func urlOf(t *testing.T, c *cluster, node, name string) string {
	t.Helper()
	for _, n := range c.Nodes() {
		if n.ID() == node {
			return n.URL() + "/git/" + name
		}
	}
	t.Fatalf("member %s serves no git", node)
	return ""
}
