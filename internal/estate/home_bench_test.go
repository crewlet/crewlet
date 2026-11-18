package estate

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/memory"
)

// BenchmarkHomeOf is what a Gitlet node pays to judge every repository the
// home's backstop pass lists against ONE reading of who serves git
// ([Router.HomeOf]): 10000 repos over 8 or 16 Gitlet nodes, this node one of
// them, reporting ns/repo (the placement read included) and home, the
// repositories it is home to. Every Gitlet node lists the same rows and only a
// repository's home works them (internal/gitlet/home's Backstop), so this is
// what each repository it is NOT home to costs a pass.
func BenchmarkHomeOf(b *testing.B) {
	for _, n := range []int{8, 16} {
		b.Run(fmt.Sprintf("repos=10000/gitlet-nodes=%d", n), func(b *testing.B) {
			nodes := make([]string, n)
			for i := range nodes {
				nodes[i] = fmt.Sprintf("git-%02d", i)
			}
			q := memory.NewBroker().Client()
			if err := q.Start(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = q.Stop(context.Background()) })
			r, err := NewRouter(RouterOptions{Queue: q, Self: nodes[0], Placement: &fakePlacement{},
				Git: &fakePlacement{nodes: nodes}, LocalGit: gitHalf{node: &fakeGit{name: nodes[0]}},
				Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
			if err != nil {
				b.Fatal(err)
			}
			repos := make([]string, 10_000)
			for i := range repos {
				repos[i] = repoID(i)
			}
			var home int
			for b.Loop() {
				own, err := r.HomeOf(repos)
				if err != nil {
					b.Fatal(err)
				}
				home = len(own)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(repos)), "ns/repo")
			b.ReportMetric(float64(home), "home")
		})
	}
}
