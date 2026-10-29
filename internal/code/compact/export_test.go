package compact

import (
	"context"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// ScaleBounds holds every build c makes to a cut of cut and every output's
// bundle to maxBundle, in place of code.CompactPackBytes and code.MaxPackBytes:
// a suite builds over mebibytes what production builds over gibibytes, and the
// plan's full rule and its byte cap follow the cut as they do in production.
func ScaleBounds(c *Compactor, cut, maxBundle int64) {
	c.bounds.cut, c.bounds.maxBundle = cut, maxBundle
}

// MergeFrom has c's plans merge n packs at the least below the target, and its
// listing name the repositories holding n or more, in place of
// code.CompactMinMerge: what BenchmarkCompact's min-merge arms vary.
func MergeFrom(c *Compactor, n int) { c.bounds.minMerge = n }

// BuildWith has c run every git process of a build through run — a merge's
// pack-objects, a layer's multi-pack-index and commit-graph writes — in place
// of the admission's reserved slot: a builder a case stands in, one whose
// outputs a cut cannot bound, or that records what it was asked.
func BuildWith(c *Compactor, run func(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) (
	gitproc.Result, error)) {
	c.runBuild = run
}

// RunInSlot is the production builder BuildWith replaces: cmd run in the
// admission's reserved slot.
func RunInSlot(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) (gitproc.Result, error) {
	return runInSlot(ctx, adm, cmd)
}

// Planned is one plan as a case reads it: whether it is due, and every
// input's row ids, lightest first.
type Planned struct {
	Due    bool
	Inputs [][]string
}

// PlanOf is the plan of a repository whose live rows are live, its roll capped
// at byteCap pack bytes, for outputs cut at cut.
func PlanOf(live []code.PackRow, byteCap, cut int64) Planned {
	return PlanOver(live, nil, byteCap, cut)
}

// PlanOver is PlanOf of a repository whose published layers name the pack
// checksums in published.
func PlanOver(live []code.PackRow, published map[string]bool, byteCap, cut int64) Planned {
	p := plan(live, published, byteCap, bounds{cut: cut, maxBundle: production.maxBundle,
		minMerge: production.minMerge})
	out := Planned{Due: p.due}
	for _, in := range p.inputs {
		out.Inputs = append(out.Inputs, in.ids)
	}
	return out
}

// LayerPlanned is one publication's plan as a case reads it: what the plan
// found, how many layers of the chain the new layer is written over, the
// layers it replaces, every pack checksum it covers before its repack, the
// pack row ids its repack merges (none where it repacks nothing) and the
// objects it holds.
type LayerPlanned struct {
	Verdict  string
	Base     int
	Replaces []string
	Members  []string
	Repack   []string
	Objects  int64
}

// The verdicts a [LayerPlanned] names.
const (
	LayerDue      = "due"
	LayerNothing  = "nothing"
	LayerWaiting  = "waiting"
	LayerTooLarge = "too_large"
)

// PlanLayerOf is the publication's plan over chain and live, its repack capped
// at byteCap pack bytes for outputs cut at cut, absorbing absorb layers more
// than the geometric rule does.
func PlanLayerOf(chain cache.Chain, live []code.PackRow, byteCap, cut int64, absorb int) LayerPlanned {
	p, verdict := planLayer(chain, live, byteCap, bounds{cut: cut, maxBundle: production.maxBundle,
		minMerge: production.minMerge}, absorb)
	out := LayerPlanned{Verdict: map[layerVerdict]string{layerDue: LayerDue, layerNothing: LayerNothing,
		layerWaiting: LayerWaiting, layerTooLarge: LayerTooLarge}[verdict], Base: p.base, Replaces: p.replaces,
		Objects: p.objects}
	for _, m := range p.members {
		out.Members = append(out.Members, m.checksum)
	}
	if p.repack.due {
		out.Repack = p.repack.ids()
	}
	return out
}

// LayerEstimate is the upper bound a plan holds a layer of objects objects to.
func LayerEstimate(objects int64) int64 { return layerEstimate(objects) }

// GeometricSplit is git's split of weights, sorted ascending, at factor: how
// many of the lightest a geometric repack rolls up.
func GeometricSplit(weights []int64, factor int64) int { return geometricSplit(weights, factor) }

// CapOf is the pack bytes c's next plan of repoID is capped at.
func CapOf(c *Compactor, repoID string) int64 { return c.capFor(repoID) }

// HeldBase is how many of rows[:base], from the root, c's object store still
// holds every part of, or the store's error (Compactor.heldBase).
func HeldBase(ctx context.Context, c *Compactor, rows []code.LayerRow, base int) (int, error) {
	return c.heldBase(ctx, rows, base)
}

// RememberNotDue has c remember cand as a plan found not due at the version the
// listing names and at its repository's cap now — what a pass over a
// repository that is not due leaves behind — so BenchmarkCompact's poll arms
// price the memory's steady state without planning every repository first.
func RememberNotDue(c *Compactor, cand code.CompactionCandidate) { c.remember(cand, c.capFor(cand.ID)) }
