package compact_test

import (
	"bufio"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// splitLists is how many seeded lists of packs TestTheRollIsGitsGeometricSplit
// holds the split to, and splitMaxPacks and splitMaxObjects the most packs a
// list holds and the most objects a pack does.
//
// UP TO 4 096 OBJECTS A PACK, not the millions a repository's packs hold:
// git's split compares weights only against factor times another weight or a
// sum of them, so it is the same at every scale, and what a list exercises is
// the ORDER of its weights — 2 to 40 packs, each count drawn log-uniformly so
// a list mixes packs a thousand times apart, which is where the progression
// breaks and the roll-up swallows. Sixty-four lists of up to forty such packs
// are a few hundred thousand blobs, a few seconds of fast-import.
const (
	splitLists      = 64
	splitMaxPacks   = 40
	splitMaxObjects = 4_096
)

// THE ROLL IS GIT'S OWN GEOMETRIC SPLIT: for every seeded list of packs, the
// packs `git repack --geometric=2 -d` replaces — built as real packs of those
// object counts, on the node pin's git — are exactly the lightest the plan's
// split rolls up, so a compaction merges what git's own maintenance would.
// Every count in a list is distinct, so which packs are the lightest is never
// a tie git's sort may break either way.
//
// Mutation: drop the split's second pass (the roll-up of packs the new pack
// would put out of progression), and lists that need it disagree with git.
func TestTheRollIsGitsGeometricSplit(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	client := gittest.NewClient(t)
	for seed := range uint64(splitLists) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			weights := drawWeights(rand.New(rand.NewPCG(seed, 1)))
			repo := gittest.Empty(t)
			// EVERY CHECKPOINT A PACK: fast-import loosens a pack of fewer
			// objects than its unpack limit (transfer.unpackLimit's 100,
			// unset), and a list's light packs would be no packs at all.
			client.Run(t, repo, "config", "fastimport.unpackLimit", "0")
			gittest.FastImport(t, client, repo, packsOf(weights))
			before := packCounts(t, repo)
			if len(before) != len(weights) {
				t.Fatalf("fast-import wrote %d packs for %d checkpoints", len(before), len(weights))
			}
			client.Run(t, repo, "repack", "--geometric=2", "-d", "--no-write-bitmap-index", "-q")
			after := packCounts(t, repo)
			var rolled []int64
			for cs, n := range before {
				if _, kept := after[cs]; !kept {
					rolled = append(rolled, n)
				}
			}
			slices.Sort(rolled)
			sorted := slices.Sorted(slices.Values(weights))
			split := compact.GeometricSplit(sorted, code.CompactFactor)
			want := sorted[:split]
			if split < 2 {
				// ONE PACK IS NOTHING TO MERGE: git leaves a roll of one
				// pack as it is, as the plan calls it not due.
				want = nil
			}
			if !slices.Equal(rolled, want) {
				t.Errorf("weights %v: git rolled up %v, the split %v (%d of the lightest)", sorted, rolled,
					sorted[:split], split)
			}
		})
	}
}

// drawWeights is one list of distinct object counts: 2 to splitMaxPacks of
// them, each drawn log-uniformly in 1..splitMaxObjects.
func drawWeights(rng *rand.Rand) []int64 {
	n := 2 + rng.IntN(splitMaxPacks-1)
	seen := map[int64]bool{}
	out := make([]int64, 0, n)
	for len(out) < n {
		w := int64(math.Exp(rng.Float64() * math.Log(splitMaxObjects)))
		if w < 1 || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// packsOf is a fast-import stream writing one pack of weights[i] distinct blobs
// for each i, in the list's order: fast-import's checkpoint closes the pack it
// is writing, so each run of blobs between two is a pack of its own.
func packsOf(weights []int64) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		for i, n := range weights {
			for j := range n {
				body := fmt.Sprintf("pack %d blob %d\n", i, j)
				if _, err := fmt.Fprintf(w, "blob\ndata %d\n%s\n", len(body), body); err != nil {
					return err
				}
			}
			if _, err := w.WriteString("checkpoint\n\n"); err != nil {
				return err
			}
		}
		return nil
	}
}

// packCounts is every pack in repo by checksum, with the objects its index
// counts.
func packCounts(t *testing.T, repo string) map[string]int64 {
	t.Helper()
	dir := filepath.Join(repo, "objects", "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "pack-") || !strings.HasSuffix(name, ".idx") {
			continue
		}
		n, f, err := code.OpenIdx(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		out[strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".idx")] = n
	}
	return out
}
