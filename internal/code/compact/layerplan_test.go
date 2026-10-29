package compact_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/compact"
)

// layerRow is a published layer of the chain over the packs whose row ids are
// ids, holding objects objects: what a publication's plan reads of a layer
// (its id, its packs and its objects).
func layerRow(seq int, id string, objects int64, ids ...string) code.LayerRow {
	packs := make([]string, len(ids))
	for i, p := range ids {
		packs[i] = checksumOf(p)
	}
	slices.Sort(packs)
	return code.LayerRow{Seq: seq, Layer: code.LayerRef{LayerID: checksumOf("layer " + id), Packs: packs,
		Objects: objects}}
}

// smallPack is a live pack row of objects objects, far from full.
func smallPack(id string, objects int64) code.PackRow { return row(id, objects, objects*100) }

// whole is a chain the copy holds whole: every layer installed, none refused.
func whole(rows ...code.LayerRow) cache.Chain {
	return cache.Chain{Rows: rows, Installed: len(rows), Refused: map[string]string{}}
}

// planCap and planCut are the caps a plan in these cases is held to: the
// production cut and the byte cap it gives.
var (
	planCut = code.CompactPackBytes
	planCap = compact.InputByteCap(code.CompactPackBytes)
)

// THE PLAN IS GEOMETRIC, AND IT IS GIT'S OWN: over a history of seeded
// publications — each a tail of new packs, planned and applied as its record
// would apply it, its repack's inputs replaced by one output — every chain the
// plans leave keeps every layer at least half the objects of all the layers
// above it, so the chain stays logarithmic in the history, and never holds more
// than code.MaxPublishedLayers; and on chains made by hand the plan replaces
// every layer from the first one this node refused (and nothing below it the
// split factor would keep), waits for a layer neither installed nor refused,
// absorbs a too-long chain's top regardless, repacks exactly when more than
// compact.LayerRepackPacks of its packs are not full, and is a pure function
// of what it reads.
//
// Mutations: compare the gathered objects against the layer's own times the
// factor (the factor inverted), and the seeded histories' chains break the half
// rule; drop the refused layers' replacement, and the refused row plans over
// the layer it may not; treat a pending layer as refused, and the waiting row
// plans a replacement.
func TestThePlanIsGeometric(t *testing.T) {
	t.Parallel()
	t.Run("seeded publications keep the chain geometric", func(t *testing.T) {
		t.Parallel()
		for seed := uint64(1); seed <= 8; seed++ {
			rng := rand.New(rand.NewPCG(seed, 7))
			var (
				rows    []code.LayerRow
				live    []code.PackRow
				next    int
				longest int
			)
			for step := range 60 {
				for range 1 + rng.IntN(20) {
					next++
					live = append(live, smallPack(fmt.Sprintf("s%d-p%04d", seed, next), int64(1+rng.IntN(10_000))))
				}
				plan := compact.PlanLayerOf(whole(rows...), live, planCap, planCut, 0)
				if plan.Verdict != compact.LayerDue {
					t.Fatalf("seed %d step %d: a tail of new packs planned %s, want a layer", seed, step, plan.Verdict)
				}
				rows, live = published(rows, live, plan, fmt.Sprintf("s%d-l%d", seed, step))
				longest = max(longest, len(rows))
				if len(rows) > code.MaxPublishedLayers {
					t.Fatalf("seed %d step %d: the chain holds %d layers, past the %d it may", seed, step, len(rows),
						code.MaxPublishedLayers)
				}
				for i := range rows {
					var above int64
					for _, r := range rows[i+1:] {
						above += r.Layer.Objects
					}
					if 2*rows[i].Layer.Objects < above {
						t.Fatalf("seed %d step %d: layer %d holds %d objects and the layers above it %d — less than "+
							"half of them, so the chain is no longer geometric: %v", seed, step, i,
							rows[i].Layer.Objects, above, objectsOf(rows))
					}
				}
			}
			if longest < 3 {
				t.Errorf("seed %d: sixty publications never made a chain of more than %d layers: the case never "+
					"kept two layers below a new one, so it holds nothing to", seed, longest)
			}
		}
	})

	big := layerRow(1, "root", 1_000_000, "r1", "r2")
	mid := layerRow(2, "mid", 10_000, "m1")
	top := layerRow(3, "top", 100, "t1")
	held := []code.PackRow{smallPack("r1", 500_000), smallPack("r2", 500_000), smallPack("m1", 10_000),
		smallPack("t1", 100)}
	tail := []code.PackRow{smallPack("n1", 10), smallPack("n2", 10)}
	for _, c := range []struct {
		name  string
		chain cache.Chain
		live  []code.PackRow
		want  compact.LayerPlanned
	}{
		{name: "nothing published and nothing pushed", chain: whole(),
			want: compact.LayerPlanned{Verdict: compact.LayerNothing}},
		{name: "a whole chain and no tail", chain: whole(big, mid, top), live: held,
			want: compact.LayerPlanned{Verdict: compact.LayerNothing}},
		{name: "a first publication covers every pack", chain: whole(), live: tail,
			want: compact.LayerPlanned{Verdict: compact.LayerDue, Members: checksums("n1", "n2"), Objects: 20}},
		{name: "a small tail is a layer of its own over the chain", chain: whole(big, mid, top),
			live: slices.Concat(held, tail), want: compact.LayerPlanned{Verdict: compact.LayerDue, Base: 3,
				Members: checksums("n1", "n2"), Objects: 20}},
		{name: "a tail that reaches half the top absorbs it", chain: whole(big, mid, top),
			live: slices.Concat(held, []code.PackRow{smallPack("n1", 60)}),
			want: compact.LayerPlanned{Verdict: compact.LayerDue, Base: 2, Replaces: ids(top),
				Members: checksums("n1", "t1"), Objects: 160}},
		{name: "a layer still at the installer is waited for", chain: cache.Chain{Rows: rows(big, mid, top),
			Installed: 1, Refused: map[string]string{}}, live: slices.Concat(held, tail),
			want: compact.LayerPlanned{Verdict: compact.LayerWaiting}},
		{name: "a refused layer is replaced with every layer above it", chain: cache.Chain{Rows: rows(big, mid, top),
			Installed: 1, Refused: map[string]string{mid.Layer.LayerID: "missing"}}, live: held,
			want: compact.LayerPlanned{Verdict: compact.LayerDue, Base: 1, Replaces: ids(mid, top),
				Members: checksums("t1", "m1"), Objects: 10_100}},
		{name: "a refused root is replaced whole", chain: cache.Chain{Rows: rows(big, mid, top), Installed: 0,
			Refused: map[string]string{big.Layer.LayerID: "corrupt"}}, live: slices.Concat(held, tail),
			want: compact.LayerPlanned{Verdict: compact.LayerDue, Base: 0, Replaces: ids(big, mid, top),
				Members: checksums("n1", "n2", "t1", "m1", "r1", "r2"), Objects: 1_010_120}},
		{name: "one more layer absorbed past the split's", chain: whole(big, mid, top),
			live: slices.Concat(held, tail), want: compact.LayerPlanned{Verdict: compact.LayerDue, Base: 2,
				Replaces: ids(top), Members: checksums("n1", "n2", "t1"), Objects: 120}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			absorb := 0
			if c.name == "one more layer absorbed past the split's" {
				absorb = 1
			}
			got := compact.PlanLayerOf(c.chain, c.live, planCap, planCut, absorb)
			if c.want.Verdict != compact.LayerDue {
				if got.Verdict != c.want.Verdict {
					t.Fatalf("planned %s, want %s", got.Verdict, c.want.Verdict)
				}
				return
			}
			slices.Sort(got.Members)
			slices.Sort(c.want.Members)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("planned %+v\nwant    %+v", got, c.want)
			}
		})
	}

	t.Run("a chain past the most layers absorbs its top regardless", func(t *testing.T) {
		t.Parallel()
		var chain []code.LayerRow
		var live []code.PackRow
		objects := int64(1000)
		for i := range code.MaxPublishedLayers {
			// Each layer three times the one above it, which the split
			// keeps over a new layer of one object: the chain is already as
			// long as one may be.
			id := fmt.Sprintf("deep%02d", i)
			chain = append([]code.LayerRow{layerRow(code.MaxPublishedLayers-i, id, objects, id)}, chain...)
			live = append(live, smallPack(id, 1))
			objects *= 3
		}
		live = append(live, smallPack("n1", 1))
		got := compact.PlanLayerOf(whole(chain...), live, planCap, planCut, 0)
		if got.Verdict != compact.LayerDue || got.Base+1 > code.MaxPublishedLayers ||
			got.Base != code.MaxPublishedLayers-1 {
			t.Errorf("a new layer over a chain of %d planned %s over %d layers, a chain of %d: want it over all but "+
				"the top, the most a chain holds (%d)", len(chain), got.Verdict, got.Base, got.Base+1,
				code.MaxPublishedLayers)
		}
	})

	t.Run("more than eight packs not full are repacked", func(t *testing.T) {
		t.Parallel()
		full := row("full", 1_000, code.FullPackBytes)
		for _, n := range []int{compact.LayerRepackPacks, compact.LayerRepackPacks + 1} {
			var live []code.PackRow
			var want []string
			for i := range n {
				id := fmt.Sprintf("small%02d", i)
				live = append(live, smallPack(id, int64(10+i)))
				want = append(want, id)
			}
			live = append(live, full)
			got := compact.PlanLayerOf(whole(), live, planCap, planCut, 0)
			switch {
			case n <= compact.LayerRepackPacks && got.Repack != nil:
				t.Errorf("%d packs not full beside a full one: repacked %v, want none", n, got.Repack)
			case n > compact.LayerRepackPacks && !slices.Equal(got.Repack, want):
				t.Errorf("%d packs not full beside a full one: repacked %v, want every one of them but the full: %v",
					n, got.Repack, want)
			}
		}
	})

	t.Run("a layer past what its parts hold is too large", func(t *testing.T) {
		t.Parallel()
		huge := code.MaxLayerParts * code.LayerPartBytes / 40
		got := compact.PlanLayerOf(whole(), []code.PackRow{smallPack("huge", huge)}, planCap, planCut, 0)
		if got.Verdict != compact.LayerTooLarge {
			t.Errorf("a layer of %d objects, an estimate of %d bytes against the %d its parts hold, planned %s, "+
				"want %s", huge, compact.LayerEstimate(huge), code.MaxLayerParts*code.LayerPartBytes, got.Verdict,
				compact.LayerTooLarge)
		}
	})

	t.Run("a plan is a function of what it reads", func(t *testing.T) {
		t.Parallel()
		chain := cache.Chain{Rows: rows(big, mid, top), Installed: 2,
			Refused: map[string]string{top.Layer.LayerID: "missing"}}
		live := slices.Concat(held, tail)
		first := compact.PlanLayerOf(chain, live, planCap, planCut, 0)
		for range 16 {
			if again := compact.PlanLayerOf(chain, live, planCap, planCut, 0); !reflect.DeepEqual(again, first) {
				t.Fatalf("the same chain and packs planned %+v, then %+v", first, again)
			}
		}
	})
}

// published is rows and live once plan's publication applied: the layers it
// replaces gone and its layer above the ones it builds on, and its repack's
// inputs replaced by one output holding their objects.
func published(rows []code.LayerRow, live []code.PackRow, plan compact.LayerPlanned, id string) ([]code.LayerRow,
	[]code.PackRow) {

	out := slices.Clone(rows[:plan.Base])
	members := slices.Clone(plan.Members)
	if len(plan.Repack) > 0 {
		var objects int64
		live = slices.DeleteFunc(slices.Clone(live), func(r code.PackRow) bool {
			if slices.Contains(plan.Repack, r.PackID) {
				objects += r.Objects
				members = slices.DeleteFunc(members, func(cs string) bool { return cs == r.GitChecksum })
				return true
			}
			return false
		})
		output := smallPack(id+"-out", objects)
		live = append(live, output)
		members = append(members, output.GitChecksum)
	}
	slices.Sort(members)
	return append(out, code.LayerRow{Seq: len(out) + 1, Layer: code.LayerRef{LayerID: checksumOf("layer " + id),
		Packs: members, Objects: plan.Objects}}), live
}

// rows is the layer rows given, as a chain lists them.
func rows(r ...code.LayerRow) []code.LayerRow { return r }

// ids is every layer's id, in chain order.
func ids(r ...code.LayerRow) []string {
	out := make([]string, len(r))
	for i, l := range r {
		out[i] = l.Layer.LayerID
	}
	return out
}

// checksums is the git checksum of every pack id given.
func checksums(packIDs ...string) []string {
	out := make([]string, len(packIDs))
	for i, id := range packIDs {
		out[i] = checksumOf(id)
	}
	return out
}

// objectsOf is every layer's objects, root first, for a failure to quote.
func objectsOf(rows []code.LayerRow) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.Layer.Objects
	}
	return out
}
