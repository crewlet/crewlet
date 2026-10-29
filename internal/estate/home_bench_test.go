package estate

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/memory"
)

// BenchmarkHomeOf is what a git node pays to judge every repository the
// backstop pass of the home's change work lists against ONE reading of who
// serves git ([Router.HomeOf]): repos=10000 ids over a fleet of
// git-nodes={8,16}, this node one of them and home to its share — ns/repo, the
// one read of the placement included, and home, the repositories it is home
// to. It is what each listed repository this node is NOT home to costs a
// pass, since every git node lists the same rows and only a repository's home
// works them (internal/code/home's Backstop).
func BenchmarkHomeOf(b *testing.B) {
	for _, n := range []int{8, 16} {
		b.Run(fmt.Sprintf("repos=10000/git-nodes=%d", n), func(b *testing.B) {
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
