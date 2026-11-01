package cache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE PUBLISHED CHAIN in an interleaving (ADR-0084): a repository's
// publication by this node — its maintainer, which adopts what it built — and
// by another git node, whose layer this node's installer fetches; the
// installer's passes; a store that lost or spoiled a layer's part; and the
// node's tip commit-graph over a chained copy's tail. Every publication is the
// PRODUCTION COMPACTOR's (internal/code/compact) over a cache and the world's
// store, so what is interleaved is what a maintainer runs — its plan over the
// copy's chain (a layer still at the installer waited for, a refused one
// replaced, a base the store lost replaced), its scratch of links naming
// exactly its base, its repack, its uploads and its adoption — and its
// proposal is decided and applied on the world's log by fakeCommitter, to the
// rules the code domain's decide holds a publication to.

// interleavingLayerOutcomes is every outcome of crewlet.code.cache.layers,
// each of which the seeds must reach between them.
var interleavingLayerOutcomes = []string{
	"installed", "adopted", "kept", "cut", "missing", "corrupt", "newer_git", "failed",
}

// newerGit is the git another node's publication names its layer as written
// by when it runs a newer git than this node: a release no pin reaches.
const newerGit = "9.0.0"

// errStoreDown is a store that failed to answer for a layer's part: neither
// missing nor damaged, which the installer tries again later.
var errStoreDown = errors.New("the store did not answer")

// fakeCommitter is a repository's committer as an interleaving's compactor
// reaches it: a publication decided from the log's rows as they stand when it
// is proposed and, decided, applied there as one record
// (fakeLog.publication) — its repack's outputs kept as bundles too, for a
// later merge of them (syncWorld.merged). newerGit, when set, is the git the
// record names the layer as written by: another git node's, newer than this
// node's, whose layer this node will not read.
type fakeCommitter struct {
	w        *syncWorld
	newerGit string
}

// Read refuses: an interleaving asks its compactor for one publication at a
// time (compact.Compactor.Publish), so nothing lists through its committer.
func (f fakeCommitter) Read(context.Context, code.ReadQuery) (code.ReadAnswer, error) {
	return code.ReadAnswer{}, fmt.Errorf("%w: an interleaving's committer answers proposals alone", code.ErrInvalid)
}

// Expunge refuses: an interleaving of the published chain asks its compactor
// for publications alone, so nothing expunges through its committer.
func (f fakeCommitter) Expunge(context.Context, code.ExpungeProposal) (code.ExpungeResult, error) {
	return code.ExpungeResult{}, fmt.Errorf("%w: the publication's interleavings expunge nothing", code.ErrInvalid)
}

// Compact decides p and applies it. Refused, it is stale with the rule's
// sentence, as the code domain's decide answers a plan its rows no longer
// hold.
func (f fakeCommitter) Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	if err := p.Validate(); err != nil {
		return code.CompactResult{}, err
	}
	comp := p.Compaction
	if comp.Layer == nil {
		return code.CompactResult{}, fmt.Errorf("%w: an interleaving's compactor proposes publications alone",
			code.ErrInvalid)
	}
	layer := *comp.Layer
	if f.newerGit != "" {
		layer.Git = f.newerGit
	}
	if err := f.w.keepOutputs(ctx, comp.Add); err != nil {
		return code.CompactResult{}, err
	}
	pos, refusal := f.w.log.publication(comp.RepoID, layer, comp.Retire, outputRows(comp.Add))
	if refusal != "" {
		return code.CompactResult{Refusal: code.RefusalStale, Detail: refusal}, nil
	}
	return code.CompactResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
}

// publication decides a publication of layer on repo, retiring retire and
// adding outputs, from the rows as they stand — the decide's rules a plan read
// off a copy is held to: the layers it replaces are the chain's top and its
// base the layer below them, every input of its repack is live, no layer it
// keeps names a pack it retires, every pack it names is live once it applies,
// and every live pack is in a layer of the chain it makes (nothing pushes
// between an interleaving's plan and its proposal, so no pack is exempt for
// arriving after its Seen) — and, decided, applies it as the code domain's
// apply writes one record: the layers it replaces gone, the layer on top of
// the ones it keeps, ADDED BY THIS RECORD whatever it replaced (the same
// packs write the same layer, so a repair may carry the id of the very layer
// it replaces: what replaces it is the record), and the pack half
// (retireAndAdd). It answers the record's position, or the refusal's
// sentence.
func (f *fakeLog) publication(repo string, layer code.LayerRef, retire []string,
	outputs []code.PackRow) (statelog.Position, string) {

	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	if r == nil {
		return statelog.Position{}, "the repository is gone"
	}
	base := len(r.layers) - len(layer.Replaces)
	if base < 0 || !slices.Equal(layerIDs(r.layers[base:]), layer.Replaces) {
		return statelog.Position{}, fmt.Sprintf("it replaces %v, and the chain is %v", layer.Replaces,
			layerIDs(r.layers))
	}
	if want := baseIDOf(r.layers[:base]); layer.BaseID != want {
		return statelog.Position{}, fmt.Sprintf("it is written over %q, and the layer below the ones it "+
			"replaces is %q", layer.BaseID, want)
	}
	retiring := map[string]bool{}
	for _, id := range retire {
		if !slices.ContainsFunc(r.packs, func(p code.PackRow) bool { return p.PackID == id }) {
			return statelog.Position{}, fmt.Sprintf("its repack retires pack %s, which is not live", id)
		}
		retiring[id] = true
	}
	live := map[string]bool{}
	for _, p := range r.packs {
		if !retiring[p.PackID] {
			live[p.GitChecksum] = true
		}
	}
	for _, o := range outputs {
		live[o.GitChecksum] = true
	}
	covered := map[string]bool{}
	for _, row := range r.layers[:base] {
		for _, cs := range row.Layer.Packs {
			if !live[cs] {
				return statelog.Position{}, fmt.Sprintf("layer %s, which it keeps, names pack-%s, which it "+
					"retires", row.Layer.LayerID, cs)
			}
			covered[cs] = true
		}
	}
	for _, cs := range layer.Packs {
		if !live[cs] {
			return statelog.Position{}, fmt.Sprintf("it names pack-%s, which is not live once it applies", cs)
		}
		covered[cs] = true
	}
	for cs := range live {
		if !covered[cs] {
			return statelog.Position{}, fmt.Sprintf("pack-%s is live and in no layer of the chain it makes", cs)
		}
	}
	f.seq++
	pos := f.at()
	f.retireAndAdd(repo, pos, outputs, retire)
	layer.Replaces = nil
	r.layers = append(slices.Clone(r.layers[:base]), code.LayerRow{Seq: base + 1, Layer: layer, Added: pos})
	r.layersAt = pos
	return pos, ""
}

// baseIDOf is the id of the top layer of rows, "" for none: the base a layer
// written over them names.
func baseIDOf(rows []code.LayerRow) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1].Layer.LayerID
}

// outputRows is a publication's repack outputs as the rows its apply adds.
func outputRows(add []code.PackRef) []code.PackRow {
	rows := make([]code.PackRow, len(add))
	for i, r := range add {
		rows[i] = code.PackRow{PackID: r.PackID, Part: "pack", Object: r.Object, Hash: r.Hash, Size: r.Size,
			IndexBytes: r.IndexBytes, ReverseBytes: r.ReverseBytes, GitChecksum: r.GitChecksum, Objects: r.Objects,
			Kind: code.PackCompact, State: code.PackLive}
	}
	return rows
}

// keepOutputs keeps each of a publication's repack outputs as a bundle the
// world can merge again (merged reads the bundles it holds), read back from
// the store whole: the pack, its index and its reverse index, back to back,
// at the sizes its ref names.
func (w *syncWorld) keepOutputs(ctx context.Context, add []code.PackRef) error {
	for _, ref := range add {
		rc, err := w.store.Open(ctx, ref.Content())
		if err != nil {
			return fmt.Errorf("reading output pack-%s back: %w", ref.GitChecksum, err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("reading output pack-%s back: %w", ref.GitChecksum, err)
		}
		w.mu.Lock()
		w.bundles[ref.GitChecksum] = gittest.Bundled{Bytes: raw, Checksum: ref.GitChecksum,
			PackBytes: ref.PackBytes(), IndexBytes: ref.IndexBytes, ReverseBytes: ref.ReverseBytes, Objects: ref.Objects}
		w.mu.Unlock()
	}
	return nil
}

// nameOf is repo's name on the log.
func (f *fakeLog) nameOf(repo string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[repo].ref.Name
}

// unpublishedPacks is repo's live pack rows no layer of its published chain
// names: what a compaction may merge, since a node's chain loads only over
// every pack it names and the decide refuses a compaction that retires one —
// and none an expunge's begin left dropping, which the decide refuses as an
// input too (its commit retires them).
func (f *fakeLog) unpublishedPacks(repo string) []code.PackRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	named := map[string]bool{}
	for _, row := range r.layers {
		for _, cs := range row.Layer.Packs {
			named[cs] = true
		}
	}
	var out []code.PackRow
	for _, p := range r.packs {
		if !named[p.GitChecksum] && p.State != code.PackDropping {
			out = append(out, p)
		}
	}
	return out
}

// setFail sets the store's answer to an open of an object (fail), nil to open
// every one.
func (o *countingObjects) setFail(fail func(objstore.Key) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = fail
}

// maintainsAll is a node that maintains every repository: an interleaving's
// cache, and the git node that publishes beside it, are each a repository's
// maintainer when it publishes.
type maintainsAll struct{}

func (maintainsAll) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	out := make(map[string]bool, len(charges))
	for id := range charges {
		out[id] = true
	}
	return out, nil
}

func (maintainsAll) Listed() (bool, error) { return true, nil }

// publisher is a compactor over c — the production one, never started: each
// publication is asked of it — storing in the world's store and proposing to
// committer, on the interleaving's clock.
func (in *interleaving) publisher(c *cache.Cache, committer fakeCommitter) *compact.Compactor {
	in.t.Helper()
	cp, err := compact.New(compact.Options{
		Cache: c, Objects: in.w.store, Estate: committer, Maintainers: maintainsAll{}, NodeID: "git-1",
		Limits: code.DefaultLimits, Logger: in.w.logs.logger(), Now: in.clk.Now,
	})
	if err != nil {
		in.fatalf("a compactor over the interleaving's cache: %v", err)
	}
	return cp
}

// candidate is repo as a maintainer's pass hands it to a publication: its
// name, and its row's version — the floor the publication syncs at.
func (in *interleaving) candidate(repo string) code.PublicationCandidate {
	return code.PublicationCandidate{ID: repo, Name: in.w.log.nameOf(repo), Version: in.last[repo]}
}

// publishOn is one publication of repo by a compactor over c proposing to
// committer: what became of it, and a line for the trace. Anything but a
// publication applied, nothing to publish, or a plan waiting for its
// installer fails the run: in an interleaving no other build holds the
// repository, the node's memory holds every charge, and the plan is made over
// the rows the committer decides it from, so a refusal is the plan's bug.
func (in *interleaving) publishOn(c *cache.Cache, repo string, committer fakeCommitter) (compact.Outcome, string) {
	in.t.Helper()
	cp := in.publisher(c, committer)
	defer cp.Stop(context.WithoutCancel(in.t.Context()))
	out, err := cp.Publish(in.t.Context(), in.candidate(repo))
	switch out {
	case compact.OutcomeApplied:
		in.last[repo] = in.w.log.head()
		lines := in.w.logs.lines(in.t, "code_publication_applied")
		l := lines[len(lines)-1]
		repacked, _ := l["repacked"].(float64)
		if repacked > 0 {
			in.saw("a repacking publication")
		} else {
			in.saw("a layer-only publication")
		}
		return out, fmt.Sprintf("layer %v replacing %v, %v packs repacked", l["layer"], l["replaces"], repacked)
	case compact.OutcomeNotDue:
		return out, "nothing to publish"
	case compact.OutcomeWaiting:
		return out, "waits for its installer"
	}
	in.fatalf("a publication of repo-%d: %s: %v", slices.Index(in.repos, repo)+1, out, err)
	return out, ""
}

// published is the repository the run publishes: the first. The second is
// NEVER published, so every operation meets a copy in own mode too — the
// maintainer's own index and graph, which a chained copy holds none of —
// whose outcomes a run publishing both reached on no seed: every copy was
// chained within a few steps. Each operation onPublished names is handed it
// whichever repository was drawn.
func (in *interleaving) published() string { return in.repos[0] }

// onPublished is the operations of the published chain, which act on the
// published repository whichever was drawn.
var onPublished = map[string]bool{
	"publish": true, "publish elsewhere": true, "install": true, "lose a layer's part": true,
	"spoil a layer's part": true, "tip": true,
}

// publish is this node's publication of the published repository — what its
// maintainer's pass runs — adopted here once its record applies, so this node
// installs its own layer from the files it wrote; a third of the time after a
// burst of pushes past compact.LayerRepackPacks, whose plan repacks the
// burst's packs before it writes the layer. A plan that meets a layer still
// at this node's installer waits for it; one over a layer this node refused
// replaces it, a REPAIR (cache.Cache.Damaged is where a pass finds them); and
// one over a base the store lost replaces that, holding its base to the
// store.
func (in *interleaving) publish(repo string) string {
	burst := ""
	if in.rng.IntN(3) == 0 {
		for range compact.LayerRepackPacks + 1 {
			in.push(repo)
		}
		burst = fmt.Sprintf("after %d pushes, ", compact.LayerRepackPacks+1)
	}
	repair := slices.ContainsFunc(in.c.Damaged(), func(r code.RepoRef) bool { return r.ID == repo })
	lost := len(in.w.logs.lines(in.t, "code_publication_base_lost"))
	out, detail := in.publishOn(in.c, repo, fakeCommitter{w: in.w})
	switch out {
	case compact.OutcomeApplied:
		if repair {
			in.saw("a repair")
			detail += ", a repair"
		}
		if len(in.w.logs.lines(in.t, "code_publication_base_lost")) > lost {
			in.saw("a publication over a base the store lost")
			detail += ", over a base the store lost"
		}
	case compact.OutcomeWaiting:
		in.saw("a publication waited for its installer")
	}
	return burst + detail
}

// publishElsewhere is another git node's publication of the published
// repository: a node of its own over the same log and store, which syncs the
// repository from nothing — installing the chain as every cold round does,
// its installer settling whatever the round left it (a missing part is asked
// about at the log's head once, from there) — publishes and leaves, so this
// node's copy meets a layer it did not write, its installer's to fetch. A
// third of the time the other node runs a newer git than this one, whose
// layer this node refuses unread until its own next publication replaces it;
// and a third of the time this node's request syncs at once and its
// installer's pass behind it meets a store that fails to answer for the new
// layer's first part, which the installer counts failed and tries again a
// minute later — a fetch drawn on its own met a layer still to fetch on no
// seed.
func (in *interleaving) publishElsewhere(repo string) string {
	committer := fakeCommitter{w: in.w}
	draw := in.rng.IntN(3)
	if draw == 0 {
		committer.newerGit = newerGit
	}
	other, stop := in.w.fresh(in.t)
	defer stop()
	settledSync(in.t, other, repo, nil).Release()
	out, detail := in.publishOn(other, repo, committer)
	if out != compact.OutcomeApplied {
		return detail
	}
	in.saw("a publication by another git node")
	switch draw {
	case 0:
		in.saw("a publication by a newer git")
		detail = "git " + newerGit + ", " + detail
	case 1:
		in.withStoreDown(func() { detail += "; " + in.install(repo) })
		detail += ", the store failing its first part"
	}
	return detail
}

// withStoreDown runs f with the store failing to answer for the first layer
// part this node's cache asks it for, and answering every other.
func (in *interleaving) withStoreDown(f func()) {
	parts := in.layerParts()
	var failed atomic.Bool
	in.w.objects.setFail(func(k objstore.Key) error {
		if parts[k] && failed.CompareAndSwap(false, true) {
			return errStoreDown
		}
		return nil
	})
	defer in.w.objects.setFail(nil)
	f()
}

// install is a request's sync of the published repository — whose delta hands
// every layer the rows name past the copy's chain to the installer, and
// answers — and then a pass of this node's installer, what its goroutine runs
// behind the serving, with the clock moved to the end of every wait a failed
// try left, so every repository queued is taken up.
func (in *interleaving) install(repo string) string {
	in.syncAt(repo, linearizable).Release()
	in.waitOutInstaller()
	took := cache.InstallPass(in.c)
	return fmt.Sprintf("%d repositories taken up", len(took))
}

// waitOutInstaller moves the clock to the end of the longest wait the
// installer holds a repository to after a failed try.
func (in *interleaving) waitOutInstaller() {
	for _, repo := range in.repos {
		if at, _ := cache.InstallWait(in.c, repo); at.After(in.clk.Now()) {
			in.clk.Set(at)
		}
	}
}

// layerParts is every part of every layer the rows' chains name.
func (in *interleaving) layerParts() map[objstore.Key]bool {
	out := map[objstore.Key]bool{}
	for _, repo := range in.repos {
		for _, row := range in.w.log.chainOf(repo) {
			for _, p := range row.Layer.Parts {
				out[p.Object] = true
			}
		}
	}
	return out
}

// pickPart picks one part, not yet lost or spoiled, of the topmost layer of
// repo's chain that has one — the layer a node is likeliest not to have
// installed yet, whose fetch then meets the damage — and false when no layer
// is left whole.
func (in *interleaving) pickPart(repo string) (code.LayerRef, objstore.Key, bool) {
	chain := in.w.log.chainOf(repo)
	for i := len(chain) - 1; i >= 0; i-- {
		var whole []objstore.Key
		for _, p := range chain[i].Layer.Parts {
			if !in.damaged[p.Object] {
				whole = append(whole, p.Object)
			}
		}
		if len(whole) > 0 {
			return chain[i].Layer, whole[in.rng.IntN(len(whole))], true
		}
	}
	return code.LayerRef{}, objstore.Key{}, false
}

// loseLayer is a store that lost one part of one of the published layers —
// what a restore leaves of every layer, since no backup carries one: a copy
// that installed the layer before keeps it, and every copy that fetches it
// after refuses it missing, until the maintainer's next publication replaces
// it.
func (in *interleaving) loseLayer(repo string) string {
	layer, key, ok := in.pickPart(repo)
	if !ok {
		return "no published layer"
	}
	if err := in.w.backend.Delete(in.t.Context(), key.Name()); err != nil {
		in.fatalf("losing a part of layer %s: %v", layer.LayerID, err)
	}
	in.damaged[key] = true
	in.saw("a layer's part lost")
	return fmt.Sprintf("a part of layer %s lost", layer.LayerID)
}

// spoilLayer is a store that hands back other bytes than it took for one
// part of one of the published layers, which its hash refuses: a copy
// that installed the layer before keeps it, and every copy that fetches it
// after refuses it corrupt, until the maintainer's next publication replaces
// it.
func (in *interleaving) spoilLayer(repo string) string {
	layer, key, ok := in.pickPart(repo)
	if !ok {
		return "no published layer"
	}
	// A read of the store carries a deadline, as every reader of it must.
	ctx, cancel := context.WithTimeout(in.t.Context(), time.Minute)
	defer cancel()
	r, err := in.w.backend.Get(ctx, key.Name(), 0, -1)
	if err != nil {
		in.fatalf("reading a part of layer %s: %v", layer.LayerID, err)
	}
	raw, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		in.fatalf("reading a part of layer %s: %v", layer.LayerID, err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := in.w.backend.Delete(ctx, key.Name()); err != nil {
		in.fatalf("spoiling a part of layer %s: %v", layer.LayerID, err)
	}
	if err := in.w.backend.Put(ctx, key.Name(), bytes.NewReader(raw), objstore.PutMeta{}); err != nil {
		in.fatalf("spoiling a part of layer %s: %v", layer.LayerID, err)
	}
	in.damaged[key] = true
	in.saw("a layer's part spoiled")
	return fmt.Sprintf("a part of layer %s spoiled", layer.LayerID)
}

// tip is a push into the published repository's tail and the maintainer's
// pass over it, with the clock past its interval: over a chained copy its
// plan is the node's tip commit-graph over the tail alone — the one step that
// walks less than the history.
func (in *interleaving) tip(repo string) string {
	ref := in.push(repo)
	return ref + "; " + in.maintain(repo)
}

// noteLayers records the outcomes the layers reached on this node — every
// outcome of crewlet.code.cache.layers it counted — and the node's tip
// commit-graph written over a chained copy's tail.
func (in *interleaving) noteLayers() {
	for _, o := range interleavingLayerOutcomes {
		if counter(in.w.rec, metrics.CodeCacheLayers, "outcome", o) > 0 {
			in.saw("layer " + o)
		}
	}
	for _, l := range in.w.logs.lines(in.t, "code_cache_maintained") {
		if l["tip"] == true {
			in.saw("a tip written")
			break
		}
	}
}

// checkLayersAsTheyStand holds repo's copy at dir as it stands, before the
// check lets its installer catch up: the layers it holds installed — whatever
// the rows name now, a delta's new layers still at the installer — are sound,
// its multi-pack-index-chain listing exactly them, each over packs it holds
// and passing code.CheckLayer (codetest.LayersHold).
func (in *interleaving) checkLayersAsTheyStand(step int, repo, dir string) {
	installed := cache.Installed(in.c, repo)
	held := cache.Chain{Rows: installed, Installed: len(installed)}
	if err := codetest.LayersHold(in.t.Context(), dir, held); err != nil {
		in.t.Fatalf("after step %d, repo-%d's installed chain as it stands, %v: %v", step,
			slices.Index(in.repos, repo)+1, layerIDs(installed), err)
	}
}

// settledSync is c's copy of repo synced linearizable once its installer has
// had its say — what a running node's installer does behind every request:
// a request's sync, whose round hands the installer what is due, and while a
// layer the rows name past the copy's installed prefix is neither installed
// nor refused, or one the rows no longer name is still installed, a pass of
// the installer, waitOut (nil for a cache on the wall clock, which waits out
// nothing in a case) first moving the cache's clock past what a failed try
// waits. A copy with no chain settles at its first sync. The caller releases
// the Synced.
func settledSync(t testing.TB, c *cache.Cache, repo string, waitOut func()) cache.Synced {
	t.Helper()
	for range 8 {
		s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			t.Fatalf("a linearizable sync of %s: %v", repo, err)
		}
		if installSettled(c.Chain(s), cache.Installed(c, repo)) {
			return s
		}
		s.Release()
		if waitOut != nil {
			waitOut()
		}
		cache.InstallPass(c)
	}
	at, failures := cache.InstallWait(c, repo)
	t.Fatalf("%s's copy holds a layer its installer has not settled after eight passes: queued %v, waiting until "+
		"%v after %d failures", repo, cache.InstallQueued(c), at, failures)
	return cache.Synced{}
}

// installSettled reports whether a copy holding installed, its chain ch,
// leaves its installer nothing to do: every layer of the rows' chain
// installed up to the first it refuses, and none past them.
func installSettled(ch cache.Chain, installed []code.LayerRow) bool {
	if len(installed) != ch.Installed {
		return false
	}
	if ch.Installed == len(ch.Rows) {
		return true
	}
	_, refused := ch.Refused[ch.Rows[ch.Installed].Layer.LayerID]
	return refused
}

// checkLayersSettled holds repo's copy once its installer has had its say
// (settledSync: installed up to the first layer it refuses, and nothing past
// it), synced at the log's head — ch its chain, dir its directory: the chain
// it read is the rows' and the layers it holds installed are sound; and
// fresh, the chain of a cache rebuilt from nothing at the same position over
// the same store, its installer settled too, is installed to the same layer —
// but for a layer this copy installed before the store lost or spoiled a part
// of it, which a copy fetching it now refuses.
func (in *interleaving) checkLayersSettled(step int, repo, dir string, ch, fresh cache.Chain) {
	t := in.t
	n := slices.Index(in.repos, repo) + 1
	rows := in.w.log.chainOf(repo)
	if !slices.Equal(layerIDs(ch.Rows), layerIDs(rows)) {
		t.Fatalf("after step %d, repo-%d's copy read the chain %v at the log's head, and the rows hold %v", step,
			n, layerIDs(ch.Rows), layerIDs(rows))
	}
	if err := codetest.LayersHold(t.Context(), dir, ch); err != nil {
		t.Fatalf("after step %d, repo-%d's installed chain: %v", step, n, err)
	}
	switch {
	case fresh.Installed == ch.Installed:
	case fresh.Installed < ch.Installed && in.damagedLayer(ch.Rows[fresh.Installed].Layer) &&
		slices.Contains([]string{"missing", "corrupt"}, fresh.Refused[ch.Rows[fresh.Installed].Layer.LayerID]):
		in.saw("a copy keeps a layer the store lost since")
	default:
		t.Fatalf("after step %d, repo-%d's copy holds %d layers of %v installed, refusing %v, and a fresh cache's "+
			"%d, refusing %v; the layers' packs against the live rows:\n%s", step, n, ch.Installed, layerIDs(rows),
			ch.Refused, fresh.Installed, fresh.Refused, in.w.log.packsNamed(repo))
	}
}

// packsNamed is every pack each layer of repo's chain names, each marked
// whether a live row holds it: what a copy installs a layer over.
func (f *fakeLog) packsNamed(repo string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	live := map[string]bool{}
	for _, p := range r.packs {
		live[p.GitChecksum] = true
	}
	var b strings.Builder
	for _, row := range r.layers {
		fmt.Fprintf(&b, "\tlayer %s (added %s):", row.Layer.LayerID, row.Added)
		for _, cs := range row.Layer.Packs {
			fmt.Fprintf(&b, " %.12s live=%v", cs, live[cs])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// damagedLayer reports whether the run lost or spoiled a part of l.
func (in *interleaving) damagedLayer(l code.LayerRef) bool {
	return slices.ContainsFunc(l.Parts, func(p code.LayerPart) bool { return in.damaged[p.Object] })
}
