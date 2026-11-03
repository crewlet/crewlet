package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// BenchmarkAChangedActivation is the reconcile of code.repos at the most
// repositories a document may declare ([config.CodeMaxRepos]), end to end on
// one booted data node as the reconciler runs it
// ([Engine.decideCodeRepos]): the fence, the walk of the rows, the batch
// read, every decision side by side, and this node's real applier committing
// what they append — in the four activations a company meets: the first that
// declares them all (a create each), one that changes nothing, one that
// declares none (a deletion each), and one that declares them all again (a
// restore each). Each phase is reported as seconds an activation: how long
// after its apply the last repository it changed has its record applied here,
// which no apply waits for ([codeReconciler]).
//
//	go test ./internal/engine -run '^$' -bench BenchmarkAChangedActivation -benchtime 1x
//
// internal/code's doc.go records what it measured, and why
// reconcileInFlight is what it is.
func BenchmarkAChangedActivation(b *testing.B) {
	const repos = config.CodeMaxRepos
	e := newSandboxNode(b, parseCompany(b, codeReposDoc("", "")))
	declaring := func(prefix string) *Company {
		var list strings.Builder
		for i := range repos {
			fmt.Fprintf(&list, "    - {name: %s-%05d, owner: {seat: swe}, protect: [{branch: main}]}\n",
				prefix, i)
		}
		c, err := NewCompany(parseCompany(b, codeReposDoc(list.String(), "")))
		if err != nil {
			b.Fatalf("build the epoch: %v", err)
		}
		return c
	}
	none, err := NewCompany(parseCompany(b, codeReposDoc("", "")))
	if err != nil {
		b.Fatalf("build the epoch: %v", err)
	}
	waitUntil(b, 20*time.Second, "the code host to serve", func() bool {
		_, err := e.router.Code().Read(b.Context(), staleRepos)
		return err == nil
	})

	activation := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	var phases [4]time.Duration
	iteration := 0
	for b.Loop() {
		iteration++
		all := declaring(fmt.Sprintf("i%d", iteration))
		for i, c := range []*Company{all, all, none, all, none} {
			activation = activation.Add(time.Minute)
			began := time.Now()
			e.decideCodeRepos(b.Context(), codeReposWanted{company: c, at: activation})
			if i < len(phases) {
				phases[i] += time.Since(began)
			}
		}
	}
	for i, name := range []string{"create", "unchanged", "delete", "restore"} {
		b.ReportMetric(phases[i].Seconds()/float64(b.N), name+"-s")
	}
}
