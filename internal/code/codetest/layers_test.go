package codetest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
)

// publishedFleet is a one-git-node fleet holding RepoName, its pushes
// published in turn — one publication after each group of that many files —
// and its node, its repository's id and its node's copy's directory, the copy
// held by the honest check first.
func publishedFleet(t *testing.T, groups ...int) (codetest.Node, string, string) {
	t.Helper()
	f := codetest.SoloNode(t, codetest.SoloOptions{})
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(t, f, "ceo")
	work := p.Init()
	n := f.Nodes()[0]
	url := n.URL() + "/git/" + codetest.RepoName
	var repoID string
	for g, files := range groups {
		tree := map[string]string{}
		for i := range files {
			tree[fmt.Sprintf("g%d/f%03d", g, i)] = strings.Repeat(fmt.Sprintf("%d.%d ", g, i), 40)
		}
		p.Commit(work, tree, fmt.Sprintf("group %d", g))
		if res := p.Push(t.Context(), work, url, "main"); res.Err != nil {
			t.Fatalf("push group %d: %s", g, res)
		}
		a, err := n.Read(t.Context(), code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: codetest.Linearizable})
		if err != nil || len(a.Publish) != 1 {
			t.Fatalf("the publication due listing after group %d: %+v, %v", g, a.Publish, err)
		}
		repoID = a.Publish[0].ID
		deadline := time.Now().Add(time.Minute)
		for {
			out, err := n.Compactor().Publish(t.Context(), a.Publish[0])
			if out == compact.OutcomeApplied {
				break
			}
			if (out != compact.OutcomeBusy && out != compact.OutcomeWaiting) || time.Now().After(deadline) {
				t.Fatalf("publish group %d: %s, %v", g, out, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := codetest.CopyHoldsItsLayers(t.Context(), n, repoID); err != nil {
		t.Fatalf("the honest copy of a published repository: %v", err)
	}
	s, err := n.Cache().Sync(t.Context(), repoID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	return n, repoID, s.Dir
}

// A suite whose check passes a liar certifies nothing: the layer half of every
// case's convergence check (CopyHoldsItsLayers) must catch a copy whose
// installed layer names a pack the copy does not hold — a layer git would read
// over a pack it cannot open, which the install's own check of the packs keeps
// any honest node from placing — and pass the honest copy it was before.
func TestTheSuiteCatchesALayerOverAPackTheNodeLacks(t *testing.T) {
	n, repoID, dir := publishedFleet(t, 3)
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("the copy's packs: %v, %v", packs, err)
	}
	if err := os.Remove(packs[0]); err != nil {
		t.Fatal(err)
	}
	err = codetest.CopyHoldsItsLayers(t.Context(), n, repoID)
	if err == nil || !strings.Contains(err.Error(), "the copy holds no "+filepath.Base(packs[0])) {
		t.Errorf("CopyHoldsItsLayers over a copy whose installed layer names a pack it lacks = %v, want the "+
			"pack named", err)
	}
}

// A suite whose check passes a liar certifies nothing: the layer half of every
// case's convergence check must catch a copy whose chain file lists its layers
// in another order than the rows' — a layer over the wrong base, which git's
// own verify passes and the install's canary read refuses — and pass the
// honest copy it was before.
func TestTheSuiteCatchesAChainOverTheWrongBase(t *testing.T) {
	n, repoID, dir := publishedFleet(t, 60, 2)
	chain := filepath.Join(dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-chain")
	raw, err := os.ReadFile(chain)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) != 2 {
		t.Fatalf("the copy's chain lists %v, want the two layers the case published", lines)
	}
	slices.Reverse(lines)
	if err := os.WriteFile(chain, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = codetest.CopyHoldsItsLayers(t.Context(), n, repoID)
	if err == nil || !strings.Contains(err.Error(), "multi-pack-index-chain lists") {
		t.Errorf("CopyHoldsItsLayers over a copy whose chain file lists its layers reversed = %v, want the chain "+
			"named", err)
	}
}
