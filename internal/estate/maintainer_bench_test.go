package estate

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/memory"
)

// BenchmarkMaintainerOf is what a Gitlet node pays to judge every repository a
// maintainer's pass lists against ONE listing of who serves git and what each
// node advertises ([Router.MaintainerOf]): 10000 repos over 8 or 16 Gitlet
// nodes, each advertising a budget that holds the charge, this node one of
// them, reporting ns/repo (the listing's read included) and maintains. Only a
// repository's maintainer remembers a plan found not due, so this is what each
// repository it does NOT maintain costs a pass (internal/gitlet/compact's poll
// arms price the rest).
func BenchmarkMaintainerOf(b *testing.B) {
	const charge = int64(1) << 30
	for _, n := range []int{8, 16} {
		b.Run(fmt.Sprintf("repos=10000/gitlet-nodes=%d", n), func(b *testing.B) {
			nodes := make([]string, n)
			said := make(map[string]int64, n)
			for i := range nodes {
				nodes[i] = fmt.Sprintf("git-%02d", i)
				said[nodes[i]] = 8 * charge
			}
			q := memory.NewBroker().Client()
			if err := q.Start(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = q.Stop(context.Background()) })
			r, err := NewRouter(RouterOptions{Queue: q, Self: nodes[0], Placement: &fakePlacement{},
				Git: &fakePlacement{nodes: nodes, said: said}, LocalGit: gitHalf{node: &fakeGit{name: nodes[0]}},
				Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
			if err != nil {
				b.Fatal(err)
			}
			charges := make(map[string]int64, 10_000)
			for i := range 10_000 {
				charges[repoID(i)] = charge
			}
			var maintains int
			for b.Loop() {
				own, err := r.MaintainerOf(charges)
				if err != nil {
					b.Fatal(err)
				}
				maintains = len(own)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(charges)), "ns/repo")
			b.ReportMetric(float64(maintains), "maintains")
		})
	}
}
