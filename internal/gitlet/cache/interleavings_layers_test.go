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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE PUBLISHED CHAIN in an interleaving (ADR-0091): publications by this node
// (adopting what it built) and by another Gitlet node (fetched by this node's
// installer), installer passes, a store that lost or spoiled a layer's part,
// and the tip commit-graph over a chained copy's tail. Every publication is the
// PRODUCTION COMPACTOR's (internal/gitlet/compact), decided and applied on the
// world's log by fakeCommitter to the code domain's rules.

// newerGit is the git a newer node's layer names as its writer: a release no
// pin reaches.
const newerGit = "9.0.0"

// errStoreDown is a store not answering for a layer's part: neither missing
// nor damaged, so the installer retries later.
var errStoreDown = errors.New("the store did not answer")

// fakeCommitter is the committer the compactor reaches: it decides and applies
// a publication as one record (fakeLog.publication), keeping repack outputs as
// bundles for a later merge (syncWorld.merged). newerGit, when set, names the
// layer's writer as a newer git, whose layer this node will not read.
type fakeCommitter struct {
	w        *syncWorld
	newerGit string
}

// Read refuses: an interleaving asks for publications one at a time
// (compact.Compactor.Publish), so nothing lists through its committer.
func (f fakeCommitter) Read(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return gitlet.ReadAnswer{}, fmt.Errorf("%w: an interleaving's committer answers proposals alone", gitlet.ErrInvalid)
}

// Expunge refuses: the published chain's interleaving only publishes.
func (f fakeCommitter) Expunge(context.Context, gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	return gitlet.ExpungeResult{}, fmt.Errorf("%w: the publication's interleavings expunge nothing", gitlet.ErrInvalid)
}

// Compact decides p and applies it; a refusal is stale with the rule's
// sentence, as the code domain's decide answers.
func (f fakeCommitter) Compact(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
	if err := p.Validate(); err != nil {
		return gitlet.CompactResult{}, err
	}
	comp := p.Compaction
	if comp.Layer == nil {
		return gitlet.CompactResult{}, fmt.Errorf("%w: an interleaving's compactor proposes publications alone",
			gitlet.ErrInvalid)
	}
	layer := *comp.Layer
	if f.newerGit != "" {
		layer.Git = f.newerGit
	}
	if err := f.w.keepOutputs(ctx, comp.Add); err != nil {
		return gitlet.CompactResult{}, err
	}
	pos, refusal := f.w.log.publication(comp.RepoID, layer, comp.Retire, outputRows(comp.Add))
	if refusal != "" {
		return gitlet.CompactResult{Refusal: gitlet.RefusalStale, Detail: refusal}, nil
	}
	return gitlet.CompactResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
}

// publication decides a publication of layer on repo by the decide's rules: it
// replaces the chain's top layers, its base the layer below; every repack input
// is live, no kept layer names a retired pack, every pack it names is live
// after, and every
// live pack is in a layer (no push comes between an interleaving's plan and its
// proposal, so none is exempt for arriving after its Seen). Applied as one
// record, the new layer is ADDED BY THIS RECORD even when it carries the id of
// the layer it repairs (the same packs write the same layer), plus the pack half
// (retireAndAdd). It answers the record's position, or the refusal's sentence.
func (f *fakeLog) publication(repo string, layer gitlet.LayerRef, retire []string,
	outputs []gitlet.PackRow) (statelog.Position, string) {

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
		if !slices.ContainsFunc(r.packs, func(p gitlet.PackRow) bool { return p.PackID == id }) {
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
	r.layers = append(slices.Clone(r.layers[:base]), gitlet.LayerRow{Seq: base + 1, Layer: layer, Added: pos})
	r.layersAt = pos
	return pos, ""
}

// baseIDOf is the id of rows' top layer, "" for none: a layer over them names
// it as its base.
func baseIDOf(rows []gitlet.LayerRow) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1].Layer.LayerID
}

// outputRows is a publication's repack outputs as the rows its apply adds.
func outputRows(add []gitlet.PackRef) []gitlet.PackRow {
	rows := make([]gitlet.PackRow, len(add))
	for i, r := range add {
		rows[i] = gitlet.PackRow{PackID: r.PackID, Part: "pack", Object: r.Object, Hash: r.Hash, Size: r.Size,
			IndexBytes: r.IndexBytes, ReverseBytes: r.ReverseBytes, GitChecksum: r.GitChecksum, Objects: r.Objects,
			Kind: gitlet.PackCompact, State: gitlet.PackLive}
	}
	return rows
}

// keepOutputs reads each repack output back from the store (pack, index and
// reverse index back to back) and keeps it as a bundle merged can merge again.
func (w *syncWorld) keepOutputs(ctx context.Context, add []gitlet.PackRef) error {
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

// unpublishedPacks is repo's live packs no layer names and no expunge left
// dropping: the only ones a compaction may merge, since the decide refuses
// retiring a pack a layer names (the chain loads over it) or a dropping one.
func (f *fakeLog) unpublishedPacks(repo string) []gitlet.PackRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	named := map[string]bool{}
	for _, row := range r.layers {
		for _, cs := range row.Layer.Packs {
			named[cs] = true
		}
	}
	var out []gitlet.PackRow
	for _, p := range r.packs {
		if !named[p.GitChecksum] && p.State != gitlet.PackDropping {
			out = append(out, p)
		}
	}
	return out
}

// setFail sets the store's answer to an object's open; nil opens every one.
func (o *countingObjects) setFail(fail func(context.Context, objstore.Key) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = fail
}

// maintainsAll maintains every repository: each node that publishes in an
// interleaving is the maintainer when it does.
type maintainsAll struct{}

func (maintainsAll) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	out := make(map[string]bool, len(charges))
	for id := range charges {
		out[id] = true
	}
	return out, nil
}

func (maintainsAll) Listed() (bool, error) { return true, nil }

// publisher is the production compactor over c, never started (each
// publication is asked of it), proposing to committer on the interleaving's
// clock.
func (in *interleaving) publisher(c *cache.Cache, committer fakeCommitter) *compact.Compactor {
	in.t.Helper()
	cp, err := compact.New(compact.Options{
		Cache: c, Objects: in.w.store, Estate: committer, Maintainers: maintainsAll{}, NodeID: "git-1",
		Limits: gitlet.DefaultLimits, Logger: in.w.logs.logger(), Now: in.clk.Now,
	})
	if err != nil {
		in.fatalf("a compactor over the interleaving's cache: %v", err)
	}
	return cp
}

// candidate is repo as a maintainer's pass hands it to a publication; Version
// is the floor the publication syncs at.
func (in *interleaving) candidate(repo string) gitlet.PublicationCandidate {
	return gitlet.PublicationCandidate{ID: repo, Name: in.w.log.nameOf(repo), Version: in.last[repo]}
}

// publishOn runs one publication of repo by a compactor over c, answering its
// outcome and a trace line. Anything but Applied, NotDue or Waiting fails: no
// other build holds the repository, memory holds every charge, and the plan
// comes from the rows the committer decides by, so a refusal is the plan's bug.
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
// NEVER published, so operations also meet a copy in own mode (the maintainer's
// own index and graph, which a chained copy lacks); publishing both chains every
// copy within a few steps.
func (in *interleaving) published() string { return in.repos[0] }

// onPublished is the published chain's operations, which act on the published
// repository whichever was drawn.
var onPublished = map[string]bool{
	"publish": true, "publish elsewhere": true, "install": true, "lose a layer's part": true,
	"spoil a layer's part": true, "tip": true,
}

// publish is this node's maintainer publishing the published repository and
// adopting its own layer once the record applies; a third of the time after a
// burst past compact.LayerRepackPacks, so the plan repacks first. A plan waits
// for a layer still at the installer, replaces one this node refused (a REPAIR,
// found by cache.Cache.Damaged), and replaces a base the store lost.
func (in *interleaving) publish(repo string) string {
	burst := ""
	if in.rng.IntN(3) == 0 {
		for range compact.LayerRepackPacks + 1 {
			in.push(repo)
		}
		burst = fmt.Sprintf("after %d pushes, ", compact.LayerRepackPacks+1)
	}
	repair := slices.ContainsFunc(in.c.Damaged(), func(r gitlet.RepoRef) bool { return r.ID == repo })
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

// publishElsewhere is another Gitlet node over the same log and store syncing
// the repository cold (its installer settled), publishing and leaving, so this
// node's installer must fetch a layer it did not write. A third of the time the
// other node runs a newer git, whose layer this node refuses unread until its
// own next publication replaces it. A third of the time this node installs at
// once against a store failing the new layer's first part, which the installer
// counts failed and retries a minute later: an install drawn on its own found a
// layer still to fetch on no seed.
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
	in.saw("a publication by another Gitlet node")
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

// withStoreDown runs f with the store failing the first layer part this
// node's cache asks for, and answering every other.
func (in *interleaving) withStoreDown(f func()) {
	parts := in.layerParts()
	var failed atomic.Bool
	in.w.objects.setFail(func(_ context.Context, k objstore.Key) error {
		if parts[k] && failed.CompareAndSwap(false, true) {
			return errStoreDown
		}
		return nil
	})
	defer in.w.objects.setFail(nil)
	f()
}

// install is a request's sync, which queues every new layer for the installer,
// then one installer pass, as its goroutine runs behind serving, with the clock
// past every failed try's wait so every queued repository is taken up.
func (in *interleaving) install(repo string) string {
	in.syncAt(repo, linearizable).Release()
	in.waitOutInstaller()
	took := cache.InstallPass(in.c)
	return fmt.Sprintf("%d repositories taken up", len(took))
}

// waitOutInstaller moves the clock past the installer's longest retry wait.
func (in *interleaving) waitOutInstaller() {
	for _, repo := range in.repos {
		//fixedwait:stated the retry instant is the installer's c.now() plus its window, on in.clk, compared with in.clk.Now()
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

// pickPart picks an undamaged part of the topmost layer that has one, the
// likeliest not yet installed so a fetch meets the damage; false if none.
func (in *interleaving) pickPart(repo string) (gitlet.LayerRef, objstore.Key, bool) {
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
	return gitlet.LayerRef{}, objstore.Key{}, false
}

// loseLayer deletes one part of a published layer, as a restore does (no backup
// carries layers): a copy that installed it keeps it, and a later fetch refuses
// it missing until the next publication replaces it.
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

// spoilLayer flips a byte of one part of a published layer: a copy that
// installed it keeps it, and a later fetch refuses it corrupt by its hash until
// the next publication replaces it.
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

// tip is a push and a maintainer's pass: over a chained copy the plan is the
// tip commit-graph over the tail alone, the one step walking less than the
// history.
func (in *interleaving) tip(repo string) string {
	ref := in.push(repo)
	return ref + "; " + in.maintain(repo)
}

// noteLayers records every crewlet.code.cache.layers outcome this node counted
// (the seeds must reach each between them), and a tip commit-graph written.
func (in *interleaving) noteLayers() {
	for _, o := range cache.LayerOutcomes() {
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

// checkLayersAsTheyStand holds repo's copy at dir before its installer catches
// up: the installed layers, whatever the rows name now, are sound
// (gitlettest.LayersHold).
func (in *interleaving) checkLayersAsTheyStand(step int, repo, dir string) {
	installed := cache.Installed(in.c, repo)
	held := cache.Chain{Rows: installed, Installed: len(installed)}
	if err := gitlettest.LayersHold(in.t.Context(), dir, held); err != nil {
		in.t.Fatalf("after step %d, repo-%d's installed chain as it stands, %v: %v", step,
			slices.Index(in.repos, repo)+1, layerIDs(installed), err)
	}
}

// settledSync syncs c's copy of repo linearizable, then runs installer passes
// until installSettled, as a running node's installer does behind requests.
// waitOut first moves the clock past a failed try's wait (nil for a cache on
// the wall clock). The caller releases the Synced.
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

// installSettled reports whether the installer has nothing left to do: every
// layer of ch installed up to the first it refuses, and none past ch.
func installSettled(ch cache.Chain, installed []gitlet.LayerRow) bool {
	if len(installed) != ch.Installed {
		return false
	}
	if ch.Installed == len(ch.Rows) {
		return true
	}
	_, refused := ch.Refused[ch.Rows[ch.Installed].Layer.LayerID]
	return refused
}

// checkLayersSettled holds repo's settled copy at the log's head (chain ch, at
// dir): it read the rows' chain, its installed layers are sound, and fresh, a
// settled cache rebuilt from nothing at the same position, is installed to the
// same layer, unless the store has since damaged one this copy installed.
func (in *interleaving) checkLayersSettled(step int, repo, dir string, ch, fresh cache.Chain) {
	t := in.t
	n := slices.Index(in.repos, repo) + 1
	rows := in.w.log.chainOf(repo)
	if !slices.Equal(layerIDs(ch.Rows), layerIDs(rows)) {
		t.Fatalf("after step %d, repo-%d's copy read the chain %v at the log's head, and the rows hold %v", step,
			n, layerIDs(ch.Rows), layerIDs(rows))
	}
	if err := gitlettest.LayersHold(t.Context(), dir, ch); err != nil {
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

// packsNamed lists each layer's packs, marked whether a live row holds them.
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
func (in *interleaving) damagedLayer(l gitlet.LayerRef) bool {
	return slices.ContainsFunc(l.Parts, func(p gitlet.LayerPart) bool { return in.damaged[p.Object] })
}
