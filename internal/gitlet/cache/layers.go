package cache

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE INSTALLED CHAIN (package doc, "The installed chain"). Two modes per
// incarnation, never both: CHAIN mode holds an installed chain, OWN mode the
// maintainer's DerivedMidx and DerivedGraph. git refuses the mix every way (a
// whole write deletes every layer and the chain file, midx-write.c's
// clear_midx_files; a whole graph write unlinks the graph chain), so a placed
// chain drops own mode's files first (EventLayersMoved). A delta never
// downloads a layer (a root layer may be some 2.6 GB, and every fetch waiting
// on the round would wait on it): the installer does, behind the serving. THE
// CHAIN HOLDS ITS PACKS: a pack an installed layer names stays in the copy,
// its row retired or not (apply's live set, packStrays, Verify's strays),
// until the layer leaves, at most installRetryCap later; dropped at the delta,
// it would cut the chain and its bitmap until the replacement landed.

// layersFile is this file as the declarations name their writer
// (Writer.File): the writers gate holds the install writes of DerivedLayers
// and DerivedGraphLayers to this file's allowance row.
const layersFile = "internal/gitlet/cache/layers.go"

// layerOutcome is what became of one published layer on this node: the
// outcome attribute of crewlet.code.cache.layers and
// crewlet.code.cache.layer.install.
type layerOutcome string

const (
	// layerInstalled: fetched from the object store, held to its row and
	// placed.
	layerInstalled layerOutcome = "installed"
	// layerAdopted: placed from the files the repository's maintainer wrote
	// on this node (Cache.AdoptLayer), with no GET.
	layerAdopted layerOutcome = "adopted"
	// layerKept: carried into the incarnation a placement or a rebuild
	// made, the chain moving around it.
	layerKept layerOutcome = "kept"
	// layerCut: taken out of the copy — the rows no longer name it, a pack it
	// names left, its files failed their check, or a layer below it went.
	layerCut layerOutcome = "cut"
	// layerMissing: a part the object store holds no object for, while a
	// read at the log's head made after it still names the layer.
	layerMissing layerOutcome = "missing"
	// layerCorrupt: a part whose bytes are not its row's, files gitlet.CheckLayer
	// refuses, or a placement whose canary failed.
	layerCorrupt layerOutcome = "corrupt"
	// layerNewerGit: written by a git newer than this node's, which it would
	// not read.
	layerNewerGit layerOutcome = "newer_git"
	// layerFailed: the store failing to answer, this node's disk failing a
	// write, a missing part the log's head could not be read about, or a
	// placement whose canary read nothing of the chain: tried again, and
	// counted once a try.
	layerFailed layerOutcome = "failed"
)

// layerOutcomes is every layerOutcome, in the order the reference lists them.
var layerOutcomes = []layerOutcome{layerInstalled, layerAdopted, layerKept, layerCut, layerMissing, layerCorrupt,
	layerNewerGit, layerFailed}

// layerRefusal is a layer of the rows' chain this node will not try again
// while the rows name it at the record that added it (added): the same bytes
// republished under a new record (the maintainer's repair of a lost layer) are
// another try. Kept in memory only, so a restart tries once more.
type layerRefusal struct {
	outcome layerOutcome
	added   statelog.Position
}

// The installed chain's files, relative to an incarnation: its two chain
// files, and the directories every layer's files live in.
var (
	layersChainRel = filepath.Join(filepath.FromSlash(layersDir), "multi-pack-index-chain")
	graphChainRel  = filepath.Join(filepath.FromSlash(graphLayersDir), "commit-graph-chain")
)

// layerRels is where l's three files live in an incarnation: its
// multi-pack-index layer, its bitmap and — "" where it carries none — its
// commit-graph layer, as git names each (derived.go's patterns).
func layerRels(l gitlet.LayerRef) [3]string {
	out := [3]string{
		filepath.Join(filepath.FromSlash(layersDir), "multi-pack-index-"+l.LayerID+".midx"),
		filepath.Join(filepath.FromSlash(layersDir), "multi-pack-index-"+l.LayerID+".bitmap"),
	}
	if l.GraphID != "" {
		out[2] = filepath.Join(filepath.FromSlash(graphLayersDir), "graph-"+l.GraphID+".graph")
	}
	return out
}

// chainIDs is rows as the two chain files list them: every layer's id, and
// the id of every commit-graph layer they carry, root first.
func chainIDs(rows []gitlet.LayerRow) (midx, graph []string) {
	for _, r := range rows {
		midx = append(midx, r.Layer.LayerID)
		if r.Layer.GraphID != "" {
			graph = append(graph, r.Layer.GraphID)
		}
	}
	return midx, graph
}

// chainText is a chain file listing ids, as git writes one and reads one: an
// id a line.
func chainText(ids []string) []byte {
	var b bytes.Buffer
	for _, id := range ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// sameLayer reports whether a and b are one layer of one chain: the same
// multi-pack-index layer (named by its checksum, so the same bytes) and the
// same commit-graph layer over the same base. A republication under another
// record is the same layer.
func sameLayer(a, b gitlet.LayerRow) bool {
	return a.Layer.LayerID == b.Layer.LayerID && a.Layer.GraphID == b.Layer.GraphID && a.Layer.BaseID == b.Layer.BaseID
}

// sameRow reports whether a and b are one row of the rows' chain: one layer of
// one chain (sameLayer) named by the record that added it. An installed
// layer's row is the one it was installed from.
func sameRow(a, b gitlet.LayerRow) bool {
	return sameLayer(a, b) && a.Added == b.Added
}

// sameRows reports whether a and b are one chain, row for row (sameRow).
func sameRows(a, b []gitlet.LayerRow) bool {
	return slices.EqualFunc(a, b, sameRow)
}

// layerPacks is every pack checksum rows' layers name.
func layerPacks(rows []gitlet.LayerRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		for _, cs := range r.Layer.Packs {
			out[cs] = true
		}
	}
	return out
}

// prefixOf is how many layers of installed a change ev keeps, as d details it:
// THE ONE CUT both chains, the swap and every placement read, so the two chain
// files are never cut at two places. It is the longest prefix naming none of
// d's packs, with no layer in d.failing and, for EventLayersMoved, layer for
// layer d.want; nothing above the first cut layer is kept, since a layer is
// valid only over its exact base.
func prefixOf(installed []gitlet.LayerRow, ev Event, d eventDetail) int {
	for i, l := range installed {
		if d.failing[l.Layer.LayerID] {
			return i
		}
		for _, cs := range l.Layer.Packs {
			if d.packs[cs] {
				return i
			}
		}
		if ev == EventLayersMoved && (i >= len(d.want) || !sameLayer(d.want[i], l)) {
			return i
		}
	}
	return len(installed)
}

// recordInstalled makes rows what inc holds of the chain, then its records
// (recordChains): THE ONE WRITE OF AN INCARNATION'S INSTALLED CHAIN; e.mu is
// held. held is the packs the copy holds, by checksum, at their rows' figures;
// every pack rows names is among them (nothing is installed over a pack the
// copy lacks, installable), and the chain keeps those figures whatever becomes
// of the rows (incarnation.installedPacks).
func (c *Cache) recordInstalled(inc *incarnation, rows []gitlet.LayerRow, held map[string]livePack) {
	was := inc.installedPacks
	inc.installed = slices.Clone(rows)
	inc.installedPacks = map[string]livePack{}
	for cs := range layerPacks(rows) {
		inc.installedPacks[cs] = held[cs]
	}
	if !maps.EqualFunc(was, inc.installedPacks, func(livePack, livePack) bool { return true }) {
		// The tail is every live pack the chain does not name.
		recountTail(inc)
	}
	c.recordChains(inc)
}

// recordChains makes the declarations' records of inc's chain what it holds,
// its installed rows and its tip, keeping the cache's total in step
// (setDerived); e.mu is held. Each record lists its chain file's ids
// (derivedState.Layers, the graph's Tip) and counts its files' bytes from the
// rows and the tip's write, so a boot restores the chain without measuring a
// file; a declaration with no file has no entry. Where no layer carries a
// graph, the graph's record is the tip's (StepTipGraph). It carries the digest
// of the tail the tip covered (TipOver), so a boot over the same tail owes the
// tip no write.
func (c *Cache) recordChains(inc *incarnation) {
	midx, graph := chainIDs(inc.installed)
	var midxBytes, graphBytes int64
	for _, r := range inc.installed {
		midxBytes += r.Layer.MidxBytes + r.Layer.BitmapBytes
		graphBytes += r.Layer.GraphBytes
	}
	lines := graph
	graphStep := StepLayer
	if inc.tip.id != "" {
		lines = append(slices.Clone(graph), inc.tip.id)
		graphBytes += inc.tip.bytes
		if len(graph) == 0 {
			graphStep = StepTipGraph
		}
	}
	var over string
	if inc.tipWritten {
		over = inc.tipOver.String()
	}
	for _, d := range []struct {
		name      DerivedName
		ids       []string
		lines     []string
		step      Step
		bytes     int64
		tip, over string
	}{{DerivedLayers, midx, midx, StepLayer, midxBytes, "", ""},
		{DerivedGraphLayers, graph, lines, graphStep, graphBytes, inc.tip.id, over}} {
		if c.image.decl(d.name) == nil {
			continue
		}
		if len(d.lines) == 0 {
			c.setDerived(inc, d.name, derivedState{}, false)
			continue
		}
		c.setDerived(inc, d.name, derivedState{Bytes: d.bytes + int64(len(chainText(d.lines))),
			Covers: len(inc.installedPacks), Step: d.step, Layers: d.ids, Tip: d.tip, TipOver: d.over}, true)
	}
}

// setInstalled is recordInstalled for an incarnation placed in place, its
// generation moved where rows are another chain than it held, and the chained
// gauge kept in step (noteChained); e.mu is held.
func (c *Cache) setInstalled(e *entry, inc *incarnation, rows []gitlet.LayerRow, held map[string]livePack) {
	if !sameRows(inc.installed, rows) {
		inc.chainGen++
	}
	c.recordInstalled(inc, rows, held)
	c.noteChained(e)
}

// heldPacks is every pack inc holds in objects/pack, by checksum, at the
// figures its rows count: those its live set places there, and those its
// installed chain holds beyond them — what a layer may name. e.mu is held.
func heldPacks(inc *incarnation) map[string]livePack {
	out := maps.Clone(inc.installedPacks)
	if out == nil {
		out = map[string]livePack{}
	}
	for _, p := range inc.live {
		if placeOf(inc, p.checksum) == placeLive {
			out[p.checksum] = p
		}
	}
	return out
}

// packFigures is the packs snap places in objects/pack, by checksum, at their
// rows' figures: what a full round's copy holds there once installed, what a
// layer may name and an index covers. Never the dropping view.
func (s snapshot) packFigures() map[string]livePack {
	places := s.places()
	out := make(map[string]livePack, len(s.packs))
	for _, p := range s.packs {
		if places[p.GitChecksum] == placeLive {
			out[p.GitChecksum] = liveOf(p.Ref(), gitlet.PackLive)
		}
	}
	return out
}

// noteChained keeps crewlet.code.cache.chained's running total in step with
// e: whether its current incarnation serves from an installed chain; e.mu is
// held. Called wherever the current incarnation or its chain changes, so the
// gauge is two loads, never a walk of every repository.
func (c *Cache) noteChained(e *entry) {
	now := !e.dead && e.current != nil && len(e.current.installed) > 0
	if now == e.chained {
		return
	}
	e.chained = now
	if now {
		c.chainedRepos.Add(1)
	} else {
		c.chainedRepos.Add(-1)
	}
}

// recordedIDs is the chain inc's records name — the ids each chain file
// lists, the graph's ending in the node's tip where it holds one — from its
// installed rows and its tip, or, before a round of this process has read the
// chain (a boot's copy), from the declarations' records the state carried.
// e.mu is held.
func recordedIDs(inc *incarnation) (midx, graph []string) {
	if len(inc.installed) > 0 || inc.chainRead {
		midx, graph = chainIDs(inc.installed)
	} else {
		midx, graph = inc.derived[DerivedLayers].Layers, slices.Clone(inc.derived[DerivedGraphLayers].Layers)
	}
	if tip := recordedTip(inc); tip != "" {
		graph = append(graph, tip)
	}
	return midx, graph
}

// recordedTip is the node's tip inc holds, "" for none: its own, or, before a
// round of this process has read the chain (a boot's copy), the one the
// graph's record names — which that round keeps only where the copy kept
// every layer it was written over and holds its file sound (planChain).
// e.mu is held.
func recordedTip(inc *incarnation) string {
	if len(inc.installed) > 0 || inc.chainRead {
		return inc.tip.id
	}
	return inc.derived[DerivedGraphLayers].Tip
}

// chainWants is what each chain file of inc holds when it is sound — the ids
// its record names, a line each — by its path relative to the incarnation:
// what the check, the scrub and Verify hold a chain file to (CheckLayers),
// since it carries no trailer of its own. e.mu is held.
func chainWants(inc *incarnation) map[string][]byte {
	midx, graph := recordedIDs(inc)
	return map[string][]byte{layersChainRel: chainText(midx), graphChainRel: chainText(graph)}
}

// tipRel is where the tip layer id lives in an incarnation, as git names a
// split commit-graph layer (derived.go's graphLayerName).
func tipRel(id string) string {
	return filepath.Join(filepath.FromSlash(graphLayersDir), "graph-"+id+".graph")
}

// dropTip takes inc's tip off its record: none held and none written over any
// tail, so the maintainer writes one at once (GRAPH FIRST). Its file and chain
// line are the caller's to take out, and its record to write again
// (recordChains). e.mu is held.
func dropTip(inc *incarnation) {
	inc.tip, inc.tipOver, inc.tipWritten = tipLayer{}, tailDigest{}, false
}

// tipGoes reports whether a change ev that keeps k of installed's layers takes
// the node's tip: every change but a retirement keeping the whole chain. A cut
// layer is a base gone, a moved chain a base the tip was not written over, a
// removed pack or an expunge may take commits the tip names, and a failing
// file is cut with the tip above it; a retirement merges packs and drops no
// commit, and the tip names commits by id (X04).
func tipGoes(installed []gitlet.LayerRow, k int, ev Event) bool {
	return k < len(installed) || ev != EventPackRetired
}

// chainMark is what a reader of inc's chain files reads beside what its record
// names of them (chainWants), under the same lock: the count of in-place
// writes of them begun and ended, and whether one is in flight.
type chainMark struct {
	seq     uint64
	writing bool
}

// markChain is inc's chainMark now; e.mu is held.
func markChain(inc *incarnation) chainMark {
	return chainMark{seq: inc.chainSeq, writing: inc.chainWriting > 0}
}

// chainStill reports whether inc's chain files stood still from m to now: no
// in-place write in flight at m, and none begun or ended since. Otherwise a
// chain file's verdict is none, the file and its expectation perhaps of two
// moments. e.mu is held.
func chainStill(inc *incarnation, m chainMark) bool {
	return !m.writing && inc.chainSeq == m.seq
}

// chainWrite marks an in-place write of inc's chain files — the tip's run, a
// cut — as begun, and its answer marks it ended (chainStill). e.mu is held
// for each.
func chainWrite(inc *incarnation) func() {
	inc.chainSeq++
	inc.chainWriting++
	return func() {
		inc.chainSeq++
		inc.chainWriting--
	}
}

// bootInstalled is the chain inc's record names, read against the rows' chain:
// the longest prefix of chain whose every layer is, in turn, the next id each
// record lists (the graph's only where the layer carries one). A boot's copy is known by ids alone; a layer the rows no
// longer name is nobody's, and the round's placement removes its files
// (cutChain). e.mu is held.
func bootInstalled(inc *incarnation, chain []gitlet.LayerRow) []gitlet.LayerRow {
	midx, graph := inc.derived[DerivedLayers].Layers, inc.derived[DerivedGraphLayers].Layers
	var out []gitlet.LayerRow
	g := 0
	for i, r := range chain {
		if i >= len(midx) || midx[i] != r.Layer.LayerID {
			break
		}
		if r.Layer.GraphID != "" {
			if g >= len(graph) || graph[g] != r.Layer.GraphID {
				break
			}
			g++
		}
		out = append(out, r)
	}
	return out
}

// setChain records chain as inc's rows' chain, as a snapshot served it
// (gitlet.RepoSnapshot.LayersServed), and drops every refusal the rows no
// longer name at its record and, where the chain moved, the node's refusal to
// publish: a publication landed, so this node's next try is judged again. e.mu
// is held. The generation moves only for another chain than inc read: a round
// serving the very chain an installer's plan was made over is no move, and
// counting it as one throws away what the plan fetched.
func setChain(inc *incarnation, chain []gitlet.LayerRow) {
	if !inc.chainRead || !sameRows(inc.chain, chain) {
		inc.chainGen++
	}
	if inc.chainRead && !sameChain(inc.chain, chain) {
		inc.publishRefused = 0
	}
	inc.chain, inc.chainRead = slices.Clone(chain), true
	for id, r := range inc.refused {
		i := slices.IndexFunc(chain, func(row gitlet.LayerRow) bool { return row.Layer.LayerID == id })
		if i < 0 || chain[i].Added != r.added {
			delete(inc.refused, id)
		}
	}
}

// refuse records that this node will not install row while the rows name it
// at its record — logged code_layer_refused once an incarnation a layer —
// and counts it; e.mu is held.
func (c *Cache) refuse(e *entry, inc *incarnation, row gitlet.LayerRow, outcome layerOutcome, detail string) {
	if r, ok := inc.refused[row.Layer.LayerID]; ok && r == (layerRefusal{outcome: outcome, added: row.Added}) {
		return
	}
	if inc.refused == nil {
		inc.refused = map[string]layerRefusal{}
	}
	inc.refused[row.Layer.LayerID] = layerRefusal{outcome: outcome, added: row.Added}
	c.countLayers(outcome, 1)
	c.log.Warn("code_layer_refused", "repo", e.id, "layer", row.Layer.LayerID, "seq", row.Seq,
		"outcome", string(outcome), "detail", detail)
}

// countLayers counts n layers under outcome.
func (c *Cache) countLayers(outcome layerOutcome, n int) {
	if c.metrics != nil && n > 0 {
		c.metrics.Add(metrics.CodeCacheLayers, uint64(n), metrics.Attrs{"outcome": string(outcome)})
	}
}

// observeInstall records how long one layer's fetch, check and placement
// took, by what became of it.
func (c *Cache) observeInstall(outcome layerOutcome, took time.Duration) {
	if c.metrics != nil {
		c.metrics.Observe(metrics.CodeCacheLayerInstall, took, metrics.Attrs{"outcome": string(outcome)})
	}
}

// chainPlan is what a round or the installer decided of an incarnation's chain
// before a byte moves: what it holds (installed), the longest prefix of the
// rows' chain it holds whole and sound (kept), and the layers to fetch above
// it in chain order (want), stopping at the first refused, from a newer git,
// or over a pack the copy lacks.
type chainPlan struct {
	installed, kept, want []gitlet.LayerRow
	// failing is the layers with a file a check or a scrub found failing that
	// is still the file found so, by id: cut, and fetched again.
	failing map[string]bool
	// rewrite: a chain file is not the record's (failed its check, absent under
	// installed layers, or present with none), so both are written again even
	// where nothing else moves. dropTip: the tip the copy holds is no file to
	// serve (failing, gone, or not the one checked), and goes likewise.
	rewrite, dropTip bool
	// held is the packs the copy holds once the round's install has run, by
	// checksum, at the figures their rows count: the snapshot's. tips is the
	// object ids the snapshot's refs name, each once: what a placement in
	// place, before the round writes those refs, walks its canary from.
	held map[string]livePack
	tips []string
	// at is when the plan was made, a time.Now() reading: how long each
	// layer's install took is measured from it.
	at time.Time
}

// base is the commit-graph layers below want[i] once the chain is placed,
// root first: what CheckLayer holds its graph layer's base to.
func (p chainPlan) base(i int) []string {
	return baseOf(slices.Concat(p.kept, p.want), len(p.kept)+i)
}

// rows is the chain a placement of p installs, given the layers fetched:
// kept, then the fetched prefix of want.
func (p chainPlan) rows(fetched []*stagedLayer) []gitlet.LayerRow {
	out := slices.Clone(p.kept)
	for _, st := range fetched {
		out = append(out, st.row)
	}
	return out
}

// moves reports whether placing rows, fetched among them, changes what the
// incarnation holds of its chain: another chain, a layer's files fetched
// again over ones that failed, chain files that are not the record's, or a
// tip that goes.
func (p chainPlan) moves(rows []gitlet.LayerRow, fetched []*stagedLayer) bool {
	return p.rewrite || p.dropTip || len(fetched) > 0 || !sameChain(rows, p.installed)
}

// placeChain is a FULL round's placement of its plan (planChain) once its
// install fetched what it could (layers, in chain order): the kept prefix and
// every layer up to the first not fetched (a layer is valid only over its
// exact base, so whatever was fetched above a failure goes), or nothing where
// the copy holds that already. Chain files or a tip alone are written again IN
// PLACE, even under a reader, one rename each (cutChain); any other change to
// a copy a reader may hold is a SWAP's layer step, returned for the round's
// swap; a copy nobody holds is placed in place (placeInPlace). A layer it
// could not fetch is roundLayerFailed's. It answers the swap's step, or
// whether it wrote into the copy. e.mu is not held; the round's write lock is.
func (c *Cache) placeChain(ctx context.Context, e *entry, inc *incarnation, p chainPlan,
	layers []layerFetch) (*layerStep, bool, error) {

	var fetched []*stagedLayer
	stopped := false
	for _, f := range layers {
		if !stopped && f.staged != nil {
			fetched = append(fetched, f.staged)
			continue
		}
		if f.staged != nil {
			f.staged.remove(c)
			continue
		}
		if !stopped {
			c.roundLayerFailed(e, inc, f)
		}
		stopped = true
	}
	rows := p.rows(fetched)
	if !p.moves(rows, fetched) {
		return nil, false, nil
	}
	e.mu.Lock()
	held := inc.converged || inc.pins > 0
	e.mu.Unlock()
	switch {
	case len(fetched) == 0 && sameChain(rows, p.installed):
		return nil, true, c.cutChain(e, inc, len(rows), EventLayersMoved)
	case held:
		return &layerStep{rows: rows, fetched: fetched}, false, nil
	}
	objects := make(map[string]int64, len(p.held))
	for cs, f := range p.held {
		objects[cs] = f.objects
	}
	_, err := c.placeInPlace(ctx, e, inc, rows, fetched, p.held, tailObjects(rows, objects), p.tips)
	if err != nil {
		for _, st := range fetched {
			st.remove(c)
		}
	}
	return nil, true, err
}

// roundLayerFailed is a full round's layer it could not fetch (placeChain),
// logged code_layer_install_failed: bytes not its row's are refused at once;
// anything else is the installer's, which the round's end kicks (installDue),
// a missing part asked of the log's head by its try (refuseFailed), since the
// round holds the repository a head read would queue behind.
func (c *Cache) roundLayerFailed(e *entry, inc *incarnation, f layerFetch) {
	if f.err == nil {
		return
	}
	c.log.Warn("code_layer_install_failed", "repo", e.id, "layer", f.row.Layer.LayerID, "seq", f.row.Seq,
		"outcome", string(f.outcome), "error", f.err)
	c.observeInstall(f.outcome, f.took)
	switch f.outcome {
	case layerCorrupt:
		e.mu.Lock()
		c.refuse(e, inc, f.row, layerCorrupt, f.err.Error())
		e.mu.Unlock()
	case layerFailed:
		c.countLayers(layerFailed, 1)
	}
}

// planChain is a FULL round's plan for inc's chain, the snapshot's chain made
// inc's (setChain). kept is the longest prefix inc holds, every file present
// at its row's size, vouched by the round's check (which hashes every file
// this process has not vouched), and named failing by no finding still at the
// identity it failed at (found: the round's check's and every scrub's waiting
// on the repair mark); want is installable above it. A boot's copy holds its
// record's chain (bootInstalled), so the placement cuts what lies past it.
// e.mu is NOT held.
func (c *Cache) planChain(e *entry, inc *incarnation, snap snapshot, found []checkedFile) chainPlan {
	p := chainPlan{failing: map[string]bool{}, held: snap.packFigures(), at: time.Now()}
	tips := map[string]bool{}
	for _, r := range snap.refs {
		if r.OID != "" && !tips[r.OID] {
			tips[r.OID] = true
			p.tips = append(p.tips, r.OID)
		}
	}
	// Read before the lock: findings still at the identity they failed at, and
	// every file of the rows' chain.
	failingFiles := map[string]bool{}
	for _, f := range found {
		if i, _ := c.image.owner(f.rel); i >= 0 && c.image.decls[i].chained() &&
			lstatID(filepath.Join(inc.dir, f.rel)) == f.id {
			failingFiles[f.rel] = true
		}
	}
	ids := map[string]fileID{}
	for _, r := range snap.layers {
		for _, rel := range layerRels(r.Layer) {
			if rel != "" {
				ids[rel] = lstatID(filepath.Join(inc.dir, rel))
			}
		}
	}
	// The tip the copy holds (a boot's, as its record names it) and its file now.
	e.mu.Lock()
	tipID := recordedTip(inc)
	e.mu.Unlock()
	var tipFile fileID
	if tipID != "" {
		tipFile = lstatID(filepath.Join(inc.dir, tipRel(tipID)))
	}
	holds := func(r gitlet.LayerRow) bool {
		return !slices.ContainsFunc(r.Layer.Packs, func(cs string) bool { _, ok := p.held[cs]; return !ok })
	}
	e.mu.Lock()
	first := !inc.chainRead
	setChain(inc, snap.layers)
	if first && len(inc.installed) == 0 {
		// A BOOT'S COPY holds its record's chain as far as the rows' chain agrees and
		// the copy holds each layer's packs: a layer is valid only over its exact base
		// and packs.
		boot := bootInstalled(inc, inc.chain)
		if i := slices.IndexFunc(boot, func(r gitlet.LayerRow) bool { return !holds(r) }); i >= 0 {
			boot = boot[:i]
		}
		// And its record's tip, where the copy keeps every layer it was written over
		// and holds its file: a tip is valid only over its exact base. The round's
		// check holds it like any file a previous process left (dropped below where it
		// failed); TipOver comes with it, so an unchanged tail owes the tip nothing.
		_, graph := chainIDs(boot)
		rec := inc.derived[DerivedGraphLayers]
		if len(boot) == len(inc.derived[DerivedLayers].Layers) && slices.Equal(graph, rec.Layers) &&
			(tipID == "" || tipFile.present) {
			if tipID != "" {
				inc.tip = tipLayer{id: tipID, bytes: tipFile.size}
			}
			// The state's check refused a record whose TipOver is no digest.
			if over, err := parseTailDigest(rec.TipOver); rec.TipOver != "" && err == nil {
				inc.tipOver, inc.tipWritten = over, true
			}
		}
		c.setInstalled(e, inc, boot, p.held)
	}
	// THE NODE'S TIP stays only as a file this process wrote or its round's check
	// found sound, still at that identity and failing no finding; else it goes,
	// the chain files are written again without it, and the maintainer writes it
	// again at once.
	if inc.tip.id != "" {
		rel := tipRel(inc.tip.id)
		p.dropTip = failingFiles[rel] || !vouchedAt(inc, rel, tipFile)
	}
	p.installed = slices.Clone(inc.installed)
	for _, row := range p.installed {
		for _, rel := range layerRels(row.Layer) {
			if failingFiles[rel] {
				p.failing[row.Layer.LayerID] = true
			}
		}
	}
	k := prefixOf(p.installed, EventLayersMoved, eventDetail{want: inc.chain, failing: p.failing})
	if i := slices.IndexFunc(inc.chain[:k], func(r gitlet.LayerRow) bool {
		return !layerSound(inc, r, ids) || !holds(r)
	}); i >= 0 {
		k = i
	}
	p.kept = slices.Clone(inc.chain[:k])
	p.want = c.installable(e, inc, inc.chain[k:], func(cs string) bool { _, ok := p.held[cs]; return ok })
	wants := chainWants(inc)
	e.mu.Unlock()
	p.rewrite = failingFiles[layersChainRel] || failingFiles[graphChainRel] || !chainFilesAre(inc.dir, wants)
	return p
}

// layerSound reports whether inc holds every file of row at its row's size,
// vouched at the identity it has now (ids, read before e.mu was taken). e.mu
// is held.
func layerSound(inc *incarnation, row gitlet.LayerRow, ids map[string]fileID) bool {
	sizes := row.Layer.Sections()
	for i, rel := range layerRels(row.Layer) {
		if rel == "" {
			continue
		}
		id := ids[rel]
		if !id.present || id.size != sizes[i] || !vouchedAt(inc, rel, id) {
			return false
		}
	}
	return true
}

// chainFilesAre reports whether the incarnation at dir holds exactly wants'
// chain files: each present holding what wants says, and absent where it says
// nothing. A read that fails reads as not.
func chainFilesAre(dir string, wants map[string][]byte) bool {
	for rel, want := range wants {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if len(want) > 0 {
				return false
			}
		case err != nil, len(want) == 0, !bytes.Equal(got, want):
			// A chain file where the record lists no layer is one too many,
			// whatever it holds.
			return false
		}
	}
	return true
}

// installable is the layers of above (the rows' chain past what inc keeps)
// this node may fetch, in chain order, up to the first refused at the same
// record, written by a git newer than this node's (refused newer_git before a
// byte moves), or naming a pack holds says the copy lacks. e.mu is held.
func (c *Cache) installable(e *entry, inc *incarnation, above []gitlet.LayerRow, holds func(cs string) bool) []gitlet.LayerRow {
	var out []gitlet.LayerRow
	for _, row := range above {
		if r, ok := inc.refused[row.Layer.LayerID]; ok && r.added == row.Added {
			break
		}
		v, err := gitproc.ParseRelease(row.Layer.Git)
		if err != nil || !c.git.Version().AtLeast(v) {
			c.refuse(e, inc, row, layerNewerGit, fmt.Sprintf("written by git %s, and this node runs %s",
				row.Layer.Git, c.git.Version()))
			break
		}
		if !slices.ContainsFunc(row.Layer.Packs, func(cs string) bool { return !holds(cs) }) {
			out = append(out, row)
			continue
		}
		break
	}
	return out
}

// stagedLayer is one layer's files in tmp/layer-<id>-<v7>/{midx,bitmap,graph},
// fetched and held to their row or linked from the maintainer's own
// (AdoptLayer), until a placement renames them in; whatever no placement takes
// is removed and counted out of tmp/'s bytes.
type stagedLayer struct {
	row gitlet.LayerRow
	dir string
	// paths is the three files, in the order of gitlet.LayerRef.Sections; the
	// graph's "" where the layer carries none.
	paths   [3]string
	adopted bool
	// base is the commit-graph layers the layer was checked over, root first.
	base []string
	// at is when it was staged, on Options.Now's clock: an adoption no round's
	// chain names within objstore.RecordWithin is discarded. began is the same
	// moment by time.Now(), what crewlet.code.cache.layer.install measures from.
	at, began time.Time
}

// installedLayers counts and logs every layer of placed a placement put into
// an incarnation: installed from the object store, or adopted from its
// maintainer's files, with how long since it was staged.
func (c *Cache) installedLayers(id string, placed []*stagedLayer) {
	for _, st := range placed {
		outcome := layerInstalled
		if st.adopted {
			outcome = layerAdopted
		}
		took := time.Since(st.began)
		c.countLayers(outcome, 1)
		c.observeInstall(outcome, took)
		c.log.Info("code_layer_installed", "repo", id, "layer", st.row.Layer.LayerID, "seq", st.row.Seq,
			"bytes", st.row.Layer.Size(), "took", took, "adopted", st.adopted)
	}
}

// stageLayer makes row's three files in tmp/, open for its parts to be
// written into, and counts its bytes into tmp/'s.
func (c *Cache) stageLayer(row gitlet.LayerRow) (*stagedLayer, [3]*os.File, error) {
	var files [3]*os.File
	dir := filepath.Join(c.dir, tmpDir, "layer-"+row.Layer.LayerID+"-"+newV7())
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		return nil, files, fmt.Errorf("cache: staging layer %s: %w", row.Layer.LayerID, err)
	}
	st := &stagedLayer{row: row, dir: dir, at: c.now(), began: time.Now()}
	c.tmpBytes.Add(row.Layer.Size())
	for i, name := range []string{"midx", "bitmap", "graph"} {
		if row.Layer.Sections()[i] == 0 {
			continue
		}
		st.paths[i] = filepath.Join(dir, name)
		f, err := os.OpenFile(st.paths[i], os.O_CREATE|os.O_EXCL|os.O_WRONLY, indexMode)
		if err != nil {
			closeAll(files)
			st.remove(c)
			return nil, files, fmt.Errorf("cache: staging layer %s: %w", row.Layer.LayerID, err)
		}
		files[i] = f
	}
	return st, files, nil
}

// closeAll closes every file of files that is open: files a fetch wrote, or
// gave up on, whose bytes are judged by what reads them next.
func closeAll(files [3]*os.File) {
	for _, f := range files {
		if f != nil {
			// What a failed close lost is found by the sync before it, or by
			// the check after it.
			_ = f.Close()
		}
	}
}

// remove takes st's directory out of tmp/ and its bytes out of tmp/'s count:
// once, whatever is left of it. Once Stop has begun, tmp/ is the next Open's
// to empty.
func (st *stagedLayer) remove(c *Cache) {
	if st.dir == "" {
		return
	}
	c.tmpBytes.Add(-st.row.Layer.Size())
	if c.enter() {
		if err := os.RemoveAll(st.dir); err != nil {
			c.log.Warn("code_cache_trash_failed", "path", st.dir, "error", err)
		}
		c.leave()
	}
	st.dir = ""
}

// fetchParts reads every part of st's layer from the object store into its
// files (gitlet.FetchLayerPart, through the store's verified read) ALL AT
// ONCE, a goroutine a part, each holding its GET's slot from slot only while
// its read runs, and answers the first failure, which ends the rest.
func (c *Cache) fetchParts(ctx context.Context, st *stagedLayer, files [3]*os.File,
	slot func(ctx context.Context) ([]byte, func(), error)) error {

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var writers [3]io.WriterAt
	for i, f := range files {
		if f != nil {
			writers[i] = f
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, len(st.row.Layer.Parts))
	for i := range st.row.Layer.Parts {
		wg.Go(func() {
			buf, give, err := slot(ctx)
			if err != nil {
				errs[i] = err
				cancel(err)
				return
			}
			defer give()
			if err = gitlet.FetchLayerPart(ctx, c.objects, st.row.Layer, i, writers, buf); err != nil {
				errs[i] = err
				cancel(err)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return errors.Join(errs...)
}

// finishLayer makes st's files durable (every sync at once, as syncStaged does
// a pack's), closes them, and holds them to the row over base
// (gitlet.CheckLayer): the one rule adoption and every install share, hashed a
// piece at a time in a read-lane slot hold takes (a round's,
// gitproc.Pool.HoldRead, or the installer's, installHold), stopped with ctx.
// Fault point InstallAfterFetch: fetched and checked, nothing placed.
func (c *Cache) finishLayer(ctx context.Context, st *stagedLayer, files [3]*os.File, base []string,
	hold func(context.Context) (func(), error)) error {

	syncFile := (*os.File).Sync
	if c.knobs != nil && c.knobs.fileSync != nil {
		syncFile = c.knobs.fileSync
	}
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		if f != nil {
			wg.Go(func() { errs[i] = syncFile(f) })
		}
	}
	wg.Wait()
	for i, f := range files {
		if f != nil {
			if cerr := f.Close(); errs[i] == nil {
				errs[i] = cerr
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cache: layer %s: making its files durable on this node: %w", st.row.Layer.LayerID, err)
	}
	if err := checkStaged(ctx, st, base, c.pieces(hold).layer()); err != nil {
		return err
	}
	st.base = base
	if c.faults != nil {
		if err := c.faults.Hit(ctx, gitletfault.InstallAfterFetch, 0); err != nil {
			return fmt.Errorf("cache: layer %s: %w", st.row.Layer.LayerID, err)
		}
	}
	return nil
}

// checkStaged holds st's files to its row over base (gitlet.CheckLayer), each
// trailer hashed as p says and stopped with ctx: a refused hold and an ended
// ctx are answered as they came, never as gitlet.ErrBadLayer.
func checkStaged(ctx context.Context, st *stagedLayer, base []string, p gitlet.Pieces) error {
	var readers [3]io.ReaderAt
	for i, path := range st.paths {
		if path == "" {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cache: layer %s: %w", st.row.Layer.LayerID, err)
		}
		defer func() { _ = f.Close() }() // read-only: what was read is the answer
		readers[i] = f
	}
	if err := gitlet.CheckLayer(ctx, st.row.Layer, readers[0], readers[1], readers[2], base, p); err != nil {
		return fmt.Errorf("cache: layer %s: %w", st.row.Layer.LayerID, err)
	}
	return nil
}

// baseOf is the commit-graph layers below chain[i], root first: what layer
// i's commit-graph layer names as its BASE (gitlet.CheckLayer).
func baseOf(chain []gitlet.LayerRow, i int) []string {
	_, graph := chainIDs(chain[:i])
	return graph
}

// layerFailure classifies a fetch's failure: missing (no object for a part)
// and corrupt (bytes not the row's, or files CheckLayer refuses) are definite;
// failed (the store not answering, this node's disk) is tried again.
func layerFailure(err error) layerOutcome {
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return layerMissing
	case errors.Is(err, objstore.ErrCorrupt), errors.Is(err, gitlet.ErrBadLayer):
		return layerCorrupt
	}
	return layerFailed
}

// layerFetch is one layer a round or the installer fetched: staged and
// checked, or the failure that stopped the chain at it.
type layerFetch struct {
	row     gitlet.LayerRow
	staged  *stagedLayer
	err     error
	outcome layerOutcome
	// took is how long the fetch ran before it failed: what
	// crewlet.code.cache.layer.install observes of a layer no placement took.
	took time.Duration
}

// placeInPlace makes rows inc's chain IN PLACE, only on an incarnation nobody
// holds (a boot's first round, a rebuild's product): a pack taken out under a
// reader would be met half way. Own mode's files go and the chain is cut below
// the first layer rows does not keep as is, refetched ones included (dropFor,
// before anything new is visible); fetched files are renamed in; both chain
// files are written; and where a layer was added the CANARY reads the chain
// before any reader is handed it: on its verdict every added layer is refused
// corrupt, where it read nothing they are the installer's, and either way the
// chain is put back below them. It answers the chain placed. held is as
// recordInstalled's. e.mu is not held; the round's write lock is.
func (c *Cache) placeInPlace(ctx context.Context, e *entry, inc *incarnation, rows []gitlet.LayerRow,
	fetched []*stagedLayer, held map[string]livePack, tail int64, tips []string) ([]gitlet.LayerRow, error) {

	e.mu.Lock()
	installed := slices.Clone(inc.installed)
	e.mu.Unlock()
	replaced := map[string]bool{}
	for _, st := range fetched {
		replaced[st.row.Layer.LayerID] = true
	}
	if _, err := c.dropFor(e, inc, EventLayersMoved, eventDetail{want: rows, failing: replaced}); err != nil {
		return installed, err
	}
	e.mu.Lock()
	kept := slices.Clone(inc.installed)
	e.mu.Unlock()
	var moved []string
	for _, st := range fetched {
		rels, err := c.moveLayerIn(st, inc.dir)
		moved = append(moved, rels...)
		if err != nil {
			c.trashRels(inc.dir, moved)
			return kept, err
		}
	}
	if err := c.writeChains(inc.dir, rows, "", syncDir); err != nil {
		c.trashRels(inc.dir, moved)
		return kept, err
	}
	added := (&layerStep{rows: rows}).adds(installed)
	if len(added) > 0 {
		if err := c.canary(ctx, e, inc, inc.dir, tail, tips); err != nil {
			// Every layer from the first added goes, each valid only over its base:
			// refused corrupt on the canary's verdict, else the installer's to retry
			// (installDue kicks it at the round's end).
			first := slices.IndexFunc(rows, func(r gitlet.LayerRow) bool { return sameLayer(r, added[0]) })
			outcome := layerFailed
			if errors.Is(err, errChainUnsound) {
				outcome = layerCorrupt
				e.mu.Lock()
				for _, r := range added {
					c.refuse(e, inc, r, layerCorrupt, err.Error())
				}
				e.mu.Unlock()
			} else {
				for _, r := range added {
					c.log.Warn("code_layer_install_failed", "repo", e.id, "layer", r.Layer.LayerID, "seq", r.Seq,
						"outcome", string(layerFailed), "error", err)
				}
				c.countLayers(layerFailed, len(added))
			}
			for _, st := range fetched {
				if slices.ContainsFunc(added, func(r gitlet.LayerRow) bool { return sameLayer(r, st.row) }) {
					c.observeInstall(outcome, time.Since(st.began))
				}
			}
			var gone []string
			for _, r := range rows[first:] {
				for _, rel := range layerRels(r.Layer) {
					if rel != "" && slices.Contains(moved, rel) {
						gone = append(gone, rel)
					}
				}
			}
			if werr := c.writeChains(inc.dir, rows[:first], "", syncDir); werr != nil {
				return kept, errors.Join(err, werr)
			}
			c.trashRels(inc.dir, gone)
			c.placed(e, inc, rows[:first], fetched, held)
			return rows[:first], nil
		}
	}
	c.placed(e, inc, rows, fetched, held)
	return rows, nil
}

// placed records rows as inc's chain once placed in place: every file of rows
// and the chain files vouched at their identity now (written here from files
// held to their rows, or checked by its round), the record set
// (setInstalled), and fetched's staging gone from tmp/. e.mu is not held.
func (c *Cache) placed(e *entry, inc *incarnation, rows []gitlet.LayerRow, fetched []*stagedLayer,
	held map[string]livePack) {

	var rels []string
	for _, r := range rows {
		for _, rel := range layerRels(r.Layer) {
			if rel != "" {
				rels = append(rels, rel)
			}
		}
	}
	if len(rows) > 0 {
		rels = append(rels, layersChainRel)
		if _, graph := chainIDs(rows); len(graph) > 0 {
			rels = append(rels, graphChainRel)
		}
	}
	c.vouchFiles(e, inc, rels...)
	e.mu.Lock()
	c.setInstalled(e, inc, rows, held)
	c.wrote(inc, c.image.set(DerivedLayers, DerivedGraphLayers))
	e.mu.Unlock()
	var in []*stagedLayer
	for _, st := range fetched {
		if slices.ContainsFunc(rows, func(r gitlet.LayerRow) bool { return sameLayer(r, st.row) }) {
			in = append(in, st)
		}
		st.remove(c)
	}
	c.installedLayers(e.id, in)
}

// moveLayerIn renames st's files into the incarnation (or staged one) at dir
// under git's names and answers the paths placed, relative to dir. Each
// directory is synced before anything names what landed: by the chain files'
// write in place, or with a swap's staging (syncStaged).
func (c *Cache) moveLayerIn(st *stagedLayer, dir string) ([]string, error) {
	var placed []string
	for i, rel := range layerRels(st.row.Layer) {
		if rel == "" {
			continue
		}
		to := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(to), hostbox.DirMode); err != nil {
			return placed, fmt.Errorf("cache: placing layer %s: %w", st.row.Layer.LayerID, err)
		}
		if err := os.Rename(st.paths[i], to); err != nil {
			return placed, fmt.Errorf("cache: placing layer %s: renaming %s over %s: %w", st.row.Layer.LayerID,
				st.paths[i], to, err)
		}
		placed = append(placed, rel)
	}
	return placed, nil
}

// linkLayerIn hard-links row's files from the incarnation at from into the one
// at to — a swap's kept layer, the same inodes — and answers what it linked,
// relative to them.
func (c *Cache) linkLayerIn(row gitlet.LayerRow, from, to string) ([]string, error) {
	var linked []string
	for _, rel := range layerRels(row.Layer) {
		if rel == "" {
			continue
		}
		dst := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(dst), hostbox.DirMode); err != nil {
			return linked, err
		}
		if err := c.link(filepath.Join(from, rel), dst); err != nil {
			return linked, err
		}
		linked = append(linked, rel)
	}
	return linked, nil
}

// trashRels moves the files at rels under dir into trash/: what an unfinished
// placement renamed in. A file already gone is no failure; one that will not
// move is logged, and the next full round's placement clears it.
func (c *Cache) trashRels(dir string, rels []string) {
	for _, rel := range rels {
		path := filepath.Join(dir, rel)
		if err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path)); err != nil &&
			!errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", path, "error", err)
		}
	}
}

// writeChains writes both chain files of the incarnation (or staged one) at
// dir over rows by writeChain, the graph's ending in tip where one is named (a
// swap carrying it, maintain.go).
func (c *Cache) writeChains(dir string, rows []gitlet.LayerRow, tip string, sync func(dir string) error) error {
	midx, graph := chainIDs(rows)
	if c.knobs != nil && c.knobs.chainLines != nil {
		midx = c.knobs.chainLines(midx)
	}
	if tip != "" {
		graph = append(graph, tip)
	}
	if err := c.writeChain(dir, layersChainRel, midx, sync); err != nil {
		return err
	}
	return c.writeChain(dir, graphChainRel, graph, sync)
}

// writeChain writes the chain file at rel under dir listing ids, staged in
// tmp/, durable and renamed in; for no ids it removes it, and its directory if
// left empty, since a chainless incarnation holds no chain file. sync makes
// the directory durable before the rename (so every layer file renamed in
// before is durable before a chain file names it) and after it. It is syncDir for
// an incarnation a reader may open, and a no-op for a swap's staging
// (syncStaged syncs it once) and for the tip's chain over the installed layers
// (maintain.go's writeTip), which names nothing new.
func (c *Cache) writeChain(dir, rel string, ids []string, sync func(dir string) error) error {
	path := filepath.Join(dir, rel)
	if len(ids) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cache: removing %s: %w", path, err)
		}
		// Left only where something else is still in it.
		if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, fs.ErrNotExist) &&
			!errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("cache: removing %s: %w", filepath.Dir(path), err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
		return fmt.Errorf("cache: making %s: %w", filepath.Dir(path), err)
	}
	text := chainText(ids)
	staged, err := c.stage(hostbox.FileMode, func(w *os.File) error {
		_, werr := w.Write(text)
		return werr
	})
	if err != nil {
		return err
	}
	// What a file names is durable before it is renamed in (durable.go): this
	// sync covers the layer files renamed in before.
	if err = sync(filepath.Dir(path)); err != nil {
		return removeStaged(staged, err)
	}
	if err = c.place(staged, path); err != nil {
		return err
	}
	return sync(filepath.Dir(path))
}

// cutChain cuts inc's installed chain to its first k layers IN PLACE, dropFor's
// one cut: the chain files written over what is kept first, so no reader
// opening them names what goes, then every file of the two chain directories
// no kept layer owns moved into trash/ (layers above the cut, the tip,
// leftovers no record accounts for). The caller holds the write lock and has
// stopped any maintenance run writing the chain; e.mu is not held. THE TIP
// GOES WITH EVERY CUT (tipGoes): in a repair it is the one file worth nothing
// to keep, rewritten at once over the tail. THE CUT IS MARKED IN FLIGHT
// (chainWrite) until its record names what the files list: a check, scrub or
// Verify reads the record and hashes the files without its lock, and a file of
// one moment held to a record of another would read as damage, whose repair
// rewrites what was sound.
func (c *Cache) cutChain(e *entry, inc *incarnation, k int, why Event) error {
	e.mu.Lock()
	installed := slices.Clone(inc.installed)
	ended := chainWrite(inc)
	e.mu.Unlock()
	// From the write on, the files name no tip and no tail a tip covers, however
	// the cut ends; the mark ends under the record's lock, so no reader takes the
	// record without it.
	written := false
	defer func() {
		e.mu.Lock()
		if written && (inc.tip.id != "" || inc.tipWritten) {
			dropTip(inc)
			c.recordChains(inc)
		}
		ended()
		e.mu.Unlock()
	}()
	kept := installed[:min(k, len(installed))]
	if err := c.writeChains(inc.dir, kept, "", syncDir); err != nil {
		return err
	}
	written = true
	keep := map[string]bool{layersChainRel: len(kept) > 0}
	for _, r := range kept {
		for _, rel := range layerRels(r.Layer) {
			keep[rel] = true
		}
	}
	if _, graph := chainIDs(kept); len(graph) > 0 {
		keep[graphChainRel] = true
	}
	var moved []string
	for _, rel := range []string{filepath.FromSlash(layersDir), filepath.FromSlash(graphLayersDir)} {
		list, err := os.ReadDir(filepath.Join(inc.dir, rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, d := range list {
			name := filepath.Join(rel, d.Name())
			if i, _ := c.image.owner(name); i < 0 || keep[name] || !c.image.decls[i].chained() {
				continue
			}
			path := filepath.Join(inc.dir, name)
			if err = toTrash(c.dir, path, newV7()+"-"+d.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			moved = append(moved, name)
		}
		if len(moved) > 0 {
			if err = syncDir(filepath.Join(inc.dir, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	c.vouchFiles(e, inc, layersChainRel, graphChainRel)
	e.mu.Lock()
	unvouch(inc, moved...)
	if len(moved) > 0 {
		c.wrote(inc, c.image.set(DerivedLayers, DerivedGraphLayers))
		repaired(inc)
	}
	dropTip(inc)
	c.setInstalled(e, inc, kept, inc.installedPacks)
	e.mu.Unlock()
	if cut := len(installed) - len(kept); cut > 0 {
		c.countLayers(layerCut, cut)
		c.log.Info("code_layers_cut", "repo", e.id, "incarnation", inc.n, "kept", len(kept), "cut", cut,
			"why", string(why))
	}
	return nil
}

// canary reads through the chain's bitmap at dir (inc's, or a swap's staged
// one the observer routes to inc, swapAt) before any reader is handed it: one
// bitmapped `rev-list --count` in the read lane, bounded by the read deadline
// plus a walk of the tail, from every ref or, for a full round that has not
// written the refs it read, from tips. A layer over the wrong base passes
// `multi-pack-index verify` and CheckLayer and fails here ("invalid MIDX
// object position, MIDX is likely corrupt"). ITS VERDICT IS GIT'S ALONE:
// errChainUnsound for an exit status or reported damage; anything else (no
// slot within SlotWait, the deadline) is no verdict, and the added layers are
// retried, never refused, or a busy node's layers would count as damage. It
// runs as a reader of inc (gitproc.Cmd.Repairer) whose reports it consumes:
// the placement is their answer.
func (c *Cache) canary(ctx context.Context, e *entry, inc *incarnation, dir string, tail int64,
	tips []string) error {

	e.mu.Lock()
	by := c.reader(inc)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		c.consume(inc, by)
		delete(inc.held, by)
		e.mu.Unlock()
	}()
	if c.knobs != nil && c.knobs.canaryRunning != nil {
		defer c.knobs.canaryRunning(dir)()
	}
	// Each argument list a literal at its run, for the writers gate. Through the
	// dropping view (heldView), as every reader the chain serves: a ref a push
	// revived onto a dropping object reaches one, and without it the layer would
	// be refused for an object the copy holds.
	deadline := gitproc.ReadDeadline + gitproc.HistoryWalk(tail)
	view := heldView(dir)
	var res gitproc.Result
	var err error
	if len(tips) > 0 {
		res, err = c.git.Run(ctx, gitproc.Cmd{
			Dir: dir, Dropping: view, Config: c.ServeConfig(), Class: gitproc.ClassRead, Repairer: by,
			Deadline: deadline, Args: []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--stdin"},
			Stdin: strings.NewReader(strings.Join(tips, "\n") + "\n"),
		})
	} else {
		res, err = c.git.Run(ctx, gitproc.Cmd{
			Dir: dir, Dropping: view, Config: c.ServeConfig(), Class: gitproc.ClassRead, Repairer: by,
			Deadline: deadline, Args: []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--all"},
		})
	}
	var exit *gitproc.ExitError
	switch {
	case errors.As(err, &exit):
		return fmt.Errorf("%w: %w", errChainUnsound, err)
	case err != nil:
		return fmt.Errorf("cache: the chain's canary read nothing of it: %w", err)
	case res.Damage != nil:
		return fmt.Errorf("%w: git read damage: %w", errChainUnsound, res.Damage)
	}
	return nil
}

// errChainUnsound is a canary's verdict that the chain it read is not sound:
// what refuses every layer its placement added as corrupt (canary).
var errChainUnsound = errors.New("cache: the chain's canary found it unsound")

// tailObjects is the objects of packs no layer of rows names: what a walk of
// the copy reads outside the chain's bitmap. live is the live packs, by
// checksum, with the objects each row counts.
func tailObjects(rows []gitlet.LayerRow, live map[string]int64) int64 {
	in := layerPacks(rows)
	var n int64
	for cs, objects := range live {
		if !in[cs] {
			n += objects
		}
	}
	return n
}

// Chain is a repository's published chain as one copy of it holds it: the
// rows' chain the copy's last round read, root first; how many of them, from
// the root, the copy has installed; and the layers of it this node refused,
// each with what refused it.
type Chain struct {
	// Rows is the rows' chain as of the copy's last round — the layers this
	// node may not have installed among them.
	Rows []gitlet.LayerRow
	// Installed is how many of Rows, from the root, the copy holds over exactly
	// the base below each: the prefix a layer may be written over
	// (Cache.NewLayerScratch). Until the installer places a publication's layer,
	// the copy holds the replaced one, so the prefix ends below it.
	Installed int
	// Refused is the layers of Rows this node will not try again while the rows
	// name them at the records that added them, by id, with the outcome:
	// `missing`, `corrupt` or `newer_git`. A layer above a refused one is neither
	// installed nor refused.
	Refused map[string]string
}

// Chain is the published chain the incarnation s pins holds ([Chain]), read as
// ONE reading under its entry's lock, so a publication planned over it never
// pairs an installation with a refusal of another moment; the maintainer plans
// over it beside [Cache.Packs]. The zero Chain for a Synced this cache did not
// hand out.
func (c *Cache) Chain(s Synced) Chain {
	if c.handedOut(s, "a read of the published chain") != nil {
		return Chain{}
	}
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	return chainOf(s.inc)
}

// chainOf is inc's [Chain]; e.mu is held.
func chainOf(inc *incarnation) Chain {
	out := Chain{Rows: slices.Clone(inc.chain), Refused: map[string]string{}}
	for out.Installed < min(len(inc.installed), len(inc.chain)) &&
		sameLayer(inc.installed[out.Installed], inc.chain[out.Installed]) {
		out.Installed++
	}
	for _, row := range inc.chain {
		if r, ok := inc.refused[row.Layer.LayerID]; ok && r.added == row.Added {
			out.Refused[row.Layer.LayerID] = string(r.outcome)
		}
	}
	return out
}

// Damaged is every repository whose CURRENT copy here holds a refused layer of
// its rows' chain (missing, corrupt, or from a newer git), from memory, as the
// row the copy's last round read: what a maintainer REPAIRS by publishing over
// the base it holds, pushes or none (internal/gitlet/compact). No due listing
// names an idle repository whose layer the store lost, so its next maintainer
// finds it here. Sorted by id.
func (c *Cache) Damaged() []gitlet.RepoRef {
	var out []gitlet.RepoRef
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; !e.dead && inc != nil && !inc.retired && len(chainOf(inc).Refused) > 0 {
			out = append(out, inc.repo)
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b gitlet.RepoRef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// layerInstaller is THE INSTALLER (package doc, "The installed chain"): one
// goroutine a cache, started by Start, installing behind the serving what a
// delta named and the copy lacks. It holds each repository ONCE: DUE in order,
// WAITING out a backoff (a heap by its end) or RUNNING, so a pass costs what
// it moves and takes up, never what it holds: a burst or a store outage holds
// thousands, and a pass runs at every kick under the lock every round's kick
// waits on. It fetches a round's share of parts at once ACROSS repositories
// through the node's fetch slots (NodeFetches), never one repository at a
// time: most layers are one part of a few MiB, and 10 000 in series at a first
// byte of 100-200 ms on S3 take 17 to 33 minutes. One task installs a
// repository, its layers fetched in chain order and placed by one swap under
// its write lock (installRepo); its read-lane work is installReadSlots.
type layerInstaller struct {
	// share is the parts in flight across every repository, and tasks the
	// repositories installed at once: both a round's share, since a task fetches
	// at least a part. checks and canaries are its read-lane shares
	// (installReadSlots): its checks' pieces, and its placements' canaries.
	share    *semaphore.Weighted
	tasks    int
	checks   *semaphore.Weighted
	canaries *semaphore.Weighted
	kicks    chan struct{}

	mu sync.Mutex
	// repos is every repository held, each with its place and backoff: due in
	// order, waiting by when each wait ends, running counted. staged is the layers
	// staged in tmp/ for a placement to take instead of a GET, by repository and
	// layer id: the maintainer's (AdoptLayer) and the installer's own fetches a
	// moved placement kept (holdFetched), each over the base it was held to, until
	// placed, dropped (dropFetched) or expired (expireStagedLayers).
	repos   map[string]*installEntry
	due     installQueue
	waiting installWaits
	running int
	staged  map[string]map[string]*stagedLayer
	// handled counts the repositories passes have taken in hand — moved among
	// the due, or taken up — what the suite holds a pass's cost to.
	handled uint64
}

// installPlace is where a repository the installer holds is.
type installPlace uint8

const (
	// placeDue: among the due, taken up in order while a task is free.
	placeDue installPlace = iota + 1
	// placeWaiting: among the waiting until its backoff ends.
	placeWaiting
	// placeRunning: a task is installing it.
	placeRunning
)

// installEntry is one repository the installer holds: its place, its index
// among the waiting while it waits (-1 elsewhere), whether a kick came while a
// task ran it — which holds it once more as the task ends, fresh as the
// freshest such kick said — and its backoff.
type installEntry struct {
	id                string
	place             installPlace
	index             int
	again, againFresh bool
	wait              installWait
}

// installQueue is the due, in the order they became so: a slice taken from
// its head, what was taken dropped once it is half the slice, so a repository
// costs O(1) to queue and to take up, and the slice is never more than twice
// what it holds.
type installQueue struct {
	entries []*installEntry
	head    int
}

func (q *installQueue) push(e *installEntry) { q.entries = append(q.entries, e) }

// pop takes the first of the due, or nil when none is.
func (q *installQueue) pop() *installEntry {
	if q.head == len(q.entries) {
		return nil
	}
	e := q.entries[q.head]
	q.entries[q.head] = nil
	q.head++
	if q.head*2 >= len(q.entries) {
		n := copy(q.entries, q.entries[q.head:])
		clear(q.entries[n:])
		q.entries, q.head = q.entries[:n], 0
	}
	return e
}

// installWaits is the waiting, the earliest end first: a heap
// (container/heap) on when each one's wait ends, each entry knowing its index,
// so a pass takes what is due and a fresh kick lifts one out in O(log n).
type installWaits []*installEntry

func (h installWaits) Len() int           { return len(h) }
func (h installWaits) Less(i, j int) bool { return h[i].wait.retryAt.Before(h[j].wait.retryAt) }

func (h installWaits) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *installWaits) Push(x any) {
	e := x.(*installEntry)
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *installWaits) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h, e.index = old[:len(old)-1], -1
	return e
}

// hold puts e among the due, or among the waiting while its backoff holds at
// now; in.mu is held.
func (in *layerInstaller) hold(e *installEntry, now time.Time) {
	if now.Before(e.wait.retryAt) {
		e.place = placeWaiting
		heap.Push(&in.waiting, e)
		return
	}
	e.place = placeDue
	in.due.push(e)
}

// installWait is one repository's backoff: failures in a row, when the next
// try may be made (Options.Now's clock), and each installed layer the rows no
// longer named at its last try, with since when it has stood in for a
// replacement not placed. A STALE LAYER'S CLOCK IS ITS OWN, keyed by its row
// (sameRow), never the repository's: an episode can end unseen by the
// installer (a full round placing the replacement, a cut, the rows naming the
// layer again), and a repository's clock would outlive it and cut the next
// publication's stale layer at its first failed fetch, a root replacement
// dropping the copy to own mode. A clock no installed layer answers to is none.
type installWait struct {
	failures int
	retryAt  time.Time
	stale    []staleLayer
}

// staleLayer is one installed layer the rows no longer name — the row it was
// installed from — and since when the installer has seen it so.
type staleLayer struct {
	row   gitlet.LayerRow
	since time.Time
}

// staleSince is when the oldest of stale has stood in since by w's clocks —
// each layer of stale w recorded at an earlier try, the rest now — and stale's
// clocks as w keeps them from now on: one for each layer of stale, and none
// for a layer no longer stale.
func (w *installWait) staleSince(stale []gitlet.LayerRow, now time.Time) (time.Time, []staleLayer) {
	var oldest time.Time
	clocks := make([]staleLayer, 0, len(stale))
	for _, row := range stale {
		since := now
		if i := slices.IndexFunc(w.stale, func(s staleLayer) bool { return sameRow(s.row, row) }); i >= 0 {
			since = w.stale[i].since
		}
		clocks = append(clocks, staleLayer{row: row, since: since})
		if oldest.IsZero() || since.Before(oldest) {
			oldest = since
		}
	}
	return oldest, clocks
}

// installRetryBase is the installer's first wait after a try that failed for a
// reason that may pass, doubled per failure in a row to installRetryCap. A
// MINUTE: a missing layer costs a copy speed, never correctness, so asking a
// failing store sooner gains nothing, and a store back within a minute costs
// one wait.
const installRetryBase = time.Minute

// installRetryCap is the installer's longest wait between tries of a
// repository, and how long a layer the rows no longer name may serve while its
// replacement cannot be placed: THIRTY MINUTES, a decision rather than a
// measurement. A refused layer's replacement arrives as new rows through a
// round, which ends the wait (kickInstall's fresh), so this retry serves only
// a store that failed to answer: at most one GET a repository a node every
// half hour, about 5.6 a second at 10 000 repositories. A retired pack a stale
// layer holds stays at most this long.
const installRetryCap = 30 * time.Minute

// installWindow is the wait after n tries in a row that failed:
// installRetryBase doubled for each after the first, never past
// installRetryCap.
func installWindow(n int) time.Duration {
	w := installRetryBase
	for i := 1; i < n && w < installRetryCap; i++ {
		w *= 2
	}
	return min(w, installRetryCap)
}

// installReadSlots is how many read-lane slots each kind of the installer's
// own work holds at once across all repositories: its checks' pieces
// (CheckPieceBytes in a gitproc.Pool.HoldRead) and its canaries. ONE each, as
// the scrub holds one: the lane is a core a slot for work requests wait on,
// and the tasks keep GETs in flight rather than hash; bounded by tasks alone,
// a mass install held up to sixteen slots, each request behind a piece of up
// to 0.54 s read cold. One slot hashes about a GB/s (BenchmarkVerify), about
// what sixteen GETs bring (FetchConcurrency), so installs pace as downloads
// do. A SLOT FOR EACH KIND: a canary runs under the repository's write lock,
// which every round waits for, and behind checks it waited up to seconds. A
// judgment call: a many-core node with a far wider link would install faster
// with more, at its requests' cost.
const installReadSlots = 1

// newLayerInstaller is an installer of nothing yet, fetching at most share
// parts at once: a round's share (Cache.fetchConcurrency), so the rounds keep
// the node's other half.
func newLayerInstaller(share int) *layerInstaller {
	return &layerInstaller{
		share: semaphore.NewWeighted(int64(share)), tasks: share, kicks: make(chan struct{}, 1),
		checks: semaphore.NewWeighted(installReadSlots), canaries: semaphore.NewWeighted(installReadSlots),
		repos: map[string]*installEntry{}, staged: map[string]map[string]*stagedLayer{},
	}
}

// takeInstallRead takes the installer's read-lane share for its work about to
// run there (canaries' where canary, else checks', installReadSlots), in turn
// with every other task's work of the kind, and answers its idempotent
// release. It holds no lane slot itself (a piece's hold takes one after it,
// installHold; a canary's git is admitted as it starts), so the installer
// never holds a slot while it waits for its share.
func (c *Cache) takeInstallRead(ctx context.Context, canary bool) (func(), error) {
	in, kind := c.installer, c.installer.checks
	if canary {
		kind = in.canaries
	}
	if err := kind.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	if c.knobs != nil && c.knobs.installReading != nil {
		c.knobs.installReading(canary, true)
	}
	return sync.OnceFunc(func() {
		if c.knobs != nil && c.knobs.installReading != nil {
			c.knobs.installReading(canary, false)
		}
		kind.Release(1)
	}), nil
}

// installHold is the hold the installer's check of a layer hashes each piece
// in (gitlet.Pieces.Hold): its checks' share of the read lane (takeInstallRead),
// then a slot of the lane (gitproc.Pool.HoldRead), both given back once the
// piece is read.
func (c *Cache) installHold(ctx context.Context) (func(), error) {
	share, err := c.takeInstallRead(ctx, false)
	if err != nil {
		return nil, err
	}
	release, err := c.git.HoldRead(ctx)
	if err != nil {
		share()
		return nil, err
	}
	return sync.OnceFunc(func() {
		release()
		share()
	}), nil
}

// kickInstall holds id for the installer and wakes it: due at once where not
// held, left where it is (waiting or due), or held once more as a running
// task ends. fresh says a round served its chain, which ends its wait (a new
// chain is a new try). O(log n) at most. Its lock is a leaf: callable with an
// entry's mu held or not.
func (c *Cache) kickInstall(id string, fresh bool) {
	in := c.installer
	in.mu.Lock()
	switch e := in.repos[id]; {
	case e == nil:
		e = &installEntry{id: id, index: -1}
		in.repos[id] = e
		in.hold(e, time.Time{})
	case e.place == placeRunning:
		e.again, e.againFresh = true, e.againFresh || fresh
	case fresh:
		e.wait.failures, e.wait.retryAt = 0, time.Time{}
		if e.place == placeWaiting {
			heap.Remove(&in.waiting, e.index)
			in.hold(e, time.Time{})
		}
	}
	in.mu.Unlock()
	select {
	case in.kicks <- struct{}{}:
	default:
	}
}

// installDue reports whether inc has installer work: a converged copy holding
// a layer the rows' chain no longer names there, or whose next layer above
// what it holds is not refused at that record. A chain stopping at a refused
// layer has none until the rows move. e.mu is held.
func installDue(inc *incarnation) bool {
	if inc == nil || inc.retired || !inc.converged || !inc.chainRead {
		return false
	}
	k := prefixOf(inc.installed, EventLayersMoved, eventDetail{want: inc.chain})
	switch {
	case k < len(inc.installed):
		return true
	case k == len(inc.chain):
		return false
	}
	r, ok := inc.refused[inc.chain[k].Layer.LayerID]
	return !ok || r.added != inc.chain[k].Added
}

// runInstaller is the installer's loop, started by Start: a pass at every
// kick and whenever the earliest wait ends, until ctx ends or the cache
// stops. Each pass takes up the due in order while fewer than its tasks run,
// and waits for none of them — each kicks it as it ends.
func (c *Cache) runInstaller(ctx context.Context) {
	// Every task's fetches and placement end with Start's ctx or the cache's
	// lifetime, whichever ends first: Stop waits for the tasks (enter).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // c.life only cancels ctx, which stays Start's: Stop must end every task however long Start's ctx lives.
	defer context.AfterFunc(c.life, cancel)()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		if !c.enter() {
			return
		}
		select {
		case <-c.installer.kicks:
		default:
		}
		next, _, _ := c.installPass(ctx)
		c.leave()
		var wake <-chan time.Time
		switch {
		case next.IsZero():
		case c.knobs != nil && c.knobs.installWake != nil:
			wake = c.knobs.installWake(next)
		default:
			timer.Reset(max(next.Sub(c.now()), 0))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-c.life.Done():
			return
		case <-c.installer.kicks:
		case <-wake:
		}
	}
}

// installPass moves every ended wait among the due, starts a task
// (installRepo, then settleInstall) for each due in order while fewer than
// tasks run, and answers when the earliest remaining wait ends (zero for
// none), the repositories taken up and their tasks. It touches only what it
// moves and takes up. Tasks are registered as the cache's work (enter), so
// Stop waits for them; the loop does not.
func (c *Cache) installPass(ctx context.Context) (next time.Time, started []string, tasks *sync.WaitGroup) {
	in, now := c.installer, c.now()
	tasks = &sync.WaitGroup{}
	in.mu.Lock()
	for len(in.waiting) > 0 && !now.Before(in.waiting[0].wait.retryAt) {
		e := heap.Pop(&in.waiting).(*installEntry)
		e.place = placeDue
		in.due.push(e)
		in.handled++
	}
	for in.running < in.tasks {
		e := in.due.pop()
		if e == nil {
			break
		}
		in.handled++
		e.place = placeRunning
		in.running++
		started = append(started, e.id)
	}
	if len(in.waiting) > 0 {
		next = in.waiting[0].wait.retryAt
	}
	in.mu.Unlock()
	for _, id := range started {
		if !c.enter() {
			// The cache is stopping: the try is let go, nothing of it begun.
			c.settleInstall(id, installEnd{forget: true})
			continue
		}
		tasks.Go(func() {
			defer c.leave()
			end := c.installRepo(ctx, id)
			if c.knobs != nil && c.knobs.installSettling != nil {
				c.knobs.installSettling(id)
			}
			c.settleInstall(id, end)
			select {
			case in.kicks <- struct{}{}:
			default:
			}
		})
	}
	return next, started, tasks
}

// installEnd is how one try of a repository ended, as its task settles it
// (settleInstall).
type installEnd struct {
	// forget: the copy was gone or had nothing due, an episode over — the
	// repository's wait ends with the try.
	forget bool
	// moved: the placement found the copy or its chain moved since the plan
	// — due again at once, its wait as it was.
	moved bool
	// stop is the fetch that stopped the chain, nil for none; undecided the
	// failed read at the log's head that left a missing part undecided;
	// swapFailed a placement whose swap failed at a step (a disk write, or a
	// canary that read nothing); stale the installed layers the rows no longer
	// name still standing in.
	stop       *layerFetch
	undecided  error
	swapFailed bool
	stale      []gitlet.LayerRow
}

// installRepo is one repository's try: THE PLAN over the current copy, pinned
// (as no use, like the maintainer's pin): the prefix the rows still name,
// kept, and what is installable above it; EACH LAYER fetched in chain order
// holding no lock, or claimed from staging (claimStagedLayer), stopping at the
// first failure; THE PLACEMENT by swap under the write lock, or the fetches
// staged for the next plan (holdFetched) where the copy or its chain moved;
// and THE FAILURE decided (refuseFailed). A stale layer that stood in
// installRetryCap with nothing fetched is cut. A copy gone or with nothing due
// ends the repository's wait: whatever it waited out was an episode that ended
// unseen (a full round's placement, a cut, an eviction), and a wait kept would
// be charged to the next episode's first try.
func (c *Cache) installRepo(ctx context.Context, id string) installEnd {
	c.mu.Lock()
	e := c.entries[id]
	c.mu.Unlock()
	if e == nil {
		c.dropFetched(id, nil, nil)
		return installEnd{forget: true}
	}
	e.mu.Lock()
	inc := e.current
	if e.dead || !installDue(inc) {
		e.mu.Unlock()
		c.dropFetched(id, nil, nil)
		return installEnd{forget: true}
	}
	chain := slices.Clone(inc.chain)
	k := prefixOf(inc.installed, EventLayersMoved, eventDetail{want: chain})
	kept := chain[:k]
	want := c.installable(e, inc, chain[k:], func(cs string) bool {
		_, chained := inc.installedPacks[cs]
		// In objects/pack, where a layer's index finds its packs: no layer
		// names a pack in the dropping view.
		return placeOf(inc, cs) == placeLive || chained
	})
	stale := slices.Clone(inc.installed[k:])
	gen := inc.chainGen
	inc.pins++
	e.mu.Unlock()
	bases := make([][]string, len(want))
	for i := range want {
		bases[i] = baseOf(chain, k+i)
	}
	c.dropFetched(id, want, bases)
	defer func() {
		e.mu.Lock()
		gone := c.unpin(e, inc)
		e.mu.Unlock()
		c.trash(gone)
	}()

	var fetched []*stagedLayer
	var stop *layerFetch
	defer func() {
		for _, st := range fetched {
			st.remove(c)
		}
	}()
	for i, row := range want {
		f := c.fetchForInstall(ctx, id, row, bases[i])
		if f.err != nil {
			stop = &f
			break
		}
		fetched = append(fetched, f.staged)
	}

	// With nothing fetched, the stale layers are cut alone once the oldest has
	// stood in installRetryCap.
	cut := len(stale) > 0 && len(fetched) == 0 && c.stuckFor(id, stale, c.now()) >= installRetryCap
	swapFailed := false
	if len(fetched) > 0 || cut {
		rows := slices.Clone(kept)
		for _, st := range fetched {
			rows = append(rows, st.row)
		}
		switch c.placeBySwap(ctx, e, inc, gen, rows, fetched) {
		case placementMoved:
			// Planned over a copy or chain gone: plan again, the fetches staged for it.
			c.holdFetched(id, fetched)
			fetched = nil
			return installEnd{moved: true}
		case placementMade:
			// Counted by the swap that placed them (swapped).
			fetched = nil
			stale = nil
		case placementRefused:
			// Definite: the canary refused layers the copy did not hold, and the next
			// plan stops below them.
		default:
			// A swap step failed (a disk write, or a canary that read nothing: no
			// verdict), logged code_cache_swap_failed and counted failed there: retried
			// behind the wait.
			swapFailed = true
		}
	}

	// A definite failure is decided here; one that may pass, a failed placement
	// or a stale layer still standing in is settleInstall's to queue.
	end := installEnd{stop: stop, swapFailed: swapFailed, stale: stale}
	if stop != nil && stop.outcome != layerFailed {
		end.undecided = c.refuseFailed(ctx, e, *stop)
	}
	return end
}

// stuckFor is how long the oldest of stale (id's installed layers the rows no
// longer name) has stood in for a replacement, by each layer's own clock
// (installWait.staleSince), which starts at the try that first saw it stale:
// zero for a layer no earlier try saw so.
func (c *Cache) stuckFor(id string, stale []gitlet.LayerRow, now time.Time) time.Duration {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	e := in.repos[id]
	if e == nil || len(stale) == 0 {
		return 0
	}
	since, _ := e.wait.staleSince(stale, now)
	return now.Sub(since)
}

// settleInstall settles how id's try ended: let go, its wait forgotten, or
// held again, due or behind a wait; held again at once whatever else where a
// kick came while it ran (again), its wait ended if that kick was fresh.
// Moved (end.moved) is due again, its wait as it was. Forgotten or done
// (nothing stopped, no swap failed, no stale layer) is let go. A failure that
// may pass waits installWindow (settleWait): the store or disk, an unfinished
// swap, or a missing part the head could not decide, which retried at once
// would cost a GET, a staging and a failed round every pass while the head is
// unreadable. A definite refusal is due again at once, the refused layer now
// blocking. Stale layers wait until the oldest has stood installRetryCap,
// when their cut is due.
func (c *Cache) settleInstall(id string, end installEnd) {
	in, now := c.installer, c.now()
	in.mu.Lock()
	defer in.mu.Unlock()
	e := in.repos[id]
	in.running--
	w := &e.wait
	stop, stale := end.stop, end.stale
	switch {
	case end.moved:
	case end.forget || (stop == nil && !end.swapFailed && len(stale) == 0):
		*w = installWait{}
		if !e.again {
			delete(in.repos, id)
			return
		}
	default:
		c.settleWait(id, w, end, now)
	}
	if e.againFresh {
		w.failures, w.retryAt = 0, time.Time{}
	}
	e.again, e.againFresh = false, false
	in.hold(e, now)
}

// settleWait is the wait a try that did not end the repository's install
// leaves w with (settleInstall's last three cases); the installer's mu is
// held.
func (c *Cache) settleWait(id string, w *installWait, end installEnd, now time.Time) {
	stop, undecided, stale := end.stop, end.undecided, end.stale
	switch {
	case end.swapFailed || undecided != nil || (stop != nil && stop.outcome == layerFailed):
		w.failures++
		w.retryAt = now.Add(installWindow(w.failures))
		if stop != nil {
			err := stop.err
			if undecided != nil {
				err = fmt.Errorf("%w; and the log's head, which decides whether the rows still name the layer, "+
					"could not be read: %w", stop.err, undecided)
			}
			c.countLayers(layerFailed, 1)
			c.log.Warn("code_layer_install_failed", "repo", id, "layer", stop.row.Layer.LayerID, "seq", stop.row.Seq,
				"outcome", string(stop.outcome), "error", err, "retry_in", installWindow(w.failures))
		}
	default:
		w.failures, w.retryAt = 0, time.Time{}
	}
	var since time.Time
	since, w.stale = w.staleSince(stale, now)
	if len(w.stale) > 0 {
		if at := since.Add(installRetryCap); w.retryAt.IsZero() || at.Before(w.retryAt) {
			w.retryAt = at
		}
	}
}

// fetchForInstall is one layer of the installer's: the staging claimed for
// this very layer, else every part fetched at once, each holding a unit of
// the installer's share and a node fetch slot, and held to its row over base.
func (c *Cache) fetchForInstall(ctx context.Context, id string, row gitlet.LayerRow, base []string) layerFetch {
	if st := c.claimStagedLayer(id, row, base); st != nil {
		return layerFetch{row: row, staged: st}
	}
	if err := c.fetches.hold(ctx); err != nil {
		return layerFetch{row: row, err: err, outcome: layerFailed}
	}
	defer c.fetches.letGo()
	st, files, err := c.stageLayer(row)
	if err != nil {
		return layerFetch{row: row, err: err, outcome: layerFailed}
	}
	share := c.installer.share
	err = c.fetchParts(ctx, st, files, func(ctx context.Context) (_ []byte, _ func(), err error) {
		if err = share.Acquire(ctx, 1); err != nil {
			return nil, nil, err
		}
		buf, err := c.fetches.take(ctx)
		if err != nil {
			share.Release(1)
			return nil, nil, err
		}
		return buf, func() {
			c.fetches.give(buf)
			share.Release(1)
		}, nil
	})
	if err == nil {
		err = c.finishLayer(ctx, st, files, base, c.installHold)
	} else {
		closeAll(files)
	}
	if err != nil {
		st.remove(c)
		f := layerFetch{row: row, err: err, outcome: layerFailure(err), took: time.Since(st.began)}
		c.observeInstall(f.outcome, f.took)
		return f
	}
	return layerFetch{row: row, staged: st}
}

// refuseFailed decides a definite failure: a missing part is asked about once
// at the log's head (a linearizable sync) and refused `missing` on the then
// current copy only while its rows still name the layer at the same record (a
// publication may have replaced it and the store collected its parts); bytes
// not the row's are refused `corrupt` while the rows name it. It answers nil
// once decided, or the head read's failure, leaving the miss neither refused
// nor cleared: a try that may pass (settleInstall).
func (c *Cache) refuseFailed(ctx context.Context, e *entry, f layerFetch) error {
	if f.outcome == layerMissing {
		s, err := c.Sync(ctx, e.id, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			return err
		}
		s.Release()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	inc := e.current
	if e.dead || inc == nil || !slices.ContainsFunc(inc.chain, func(r gitlet.LayerRow) bool {
		return r.Layer.LayerID == f.row.Layer.LayerID && r.Added == f.row.Added
	}) {
		return nil
	}
	c.refuse(e, inc, f.row, f.outcome, f.err.Error())
	return nil
}

// chainPlacement is what became of the installer's placement of a chain.
type chainPlacement int

const (
	// placementFailed: the swap failed — a step of it, or the write lock not
	// had before the cache stopped — and inc serves as it did: tried again.
	placementFailed chainPlacement = iota
	// placementMade: the next incarnation holds the chain planned.
	placementMade
	// placementMoved: planned over a copy no longer current, or a chain no
	// longer the rows' as planned: nothing placed, planned again at once.
	placementMoved
	// placementRefused: the canary refused the layers the placement added.
	placementRefused
)

// placeBySwap places rows as inc's chain by a swap into its next incarnation
// (swap.go's layer step) under the repository's write lock, and answers what
// became of it; placementMoved where the copy or its chain moved since the
// plan at gen, leaving the downloads unplaced. The lock is waited for under
// ctx, the installer's, which Stop ends (runInstaller): a wait outliving it
// would hold a placement no swap could make.
func (c *Cache) placeBySwap(ctx context.Context, e *entry, inc *incarnation, gen uint64, rows []gitlet.LayerRow,
	fetched []*stagedLayer) chainPlacement {

	if err := c.acquireWrite(ctx, e); err != nil {
		return placementFailed
	}
	defer c.releaseWrite(e, writeExclusive)
	e.mu.Lock()
	if e.dead || e.current != inc || inc.retired || inc.chainGen != gen {
		e.mu.Unlock()
		return placementMoved
	}
	e.mu.Unlock()
	next := c.swapLeaving(ctx, e, inc, snapshot{}, &layerStep{rows: rows, fetched: fetched, installer: true})
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case next != inc && sameChain(next.installed, rows):
		return placementMade
	case slices.ContainsFunc(fetched, func(st *stagedLayer) bool {
		r, ok := inc.refused[st.row.Layer.LayerID]
		return ok && r.added == st.row.Added
	}):
		return placementRefused
	}
	return placementFailed
}

// sameChain reports whether a and b are one chain, layer for layer.
func sameChain(a, b []gitlet.LayerRow) bool {
	return slices.EqualFunc(a, b, sameLayer)
}

// layerStep is a placement a swap makes (swap.go): the next incarnation's
// chain, root first (kept layers linked, fetched ones renamed in from tmp/),
// and whether it is the installer's, whose canary takes the installer's
// canary share (takeInstallRead) rather than running as a round's.
type layerStep struct {
	rows      []gitlet.LayerRow
	fetched   []*stagedLayer
	installer bool
}

// discard takes every staged layer of s that no swap placed out of tmp/.
func (s *layerStep) discard(c *Cache) {
	for _, st := range s.fetched {
		st.remove(c)
	}
}

// adds is the layers of s that are none of inc's installed chain: what its
// canary is for.
func (s *layerStep) adds(installed []gitlet.LayerRow) []gitlet.LayerRow {
	var out []gitlet.LayerRow
	for _, r := range s.rows {
		if !slices.ContainsFunc(installed, func(i gitlet.LayerRow) bool { return sameLayer(i, r) }) {
			out = append(out, r)
		}
	}
	return out
}

// claimStagedLayer takes the layer staged for row (this repository, this
// layer, this base: an adoption, or a fetch a moved placement kept) for an
// install to place instead of a GET; nil for none.
func (c *Cache) claimStagedLayer(id string, row gitlet.LayerRow, base []string) *stagedLayer {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	st := in.staged[id][row.Layer.LayerID]
	if st == nil || !sameLayer(st.row, row) || !slices.Equal(st.base, base) {
		return nil
	}
	delete(in.staged[id], row.Layer.LayerID)
	if len(in.staged[id]) == 0 {
		delete(in.staged, id)
	}
	st.row = row
	return st
}

// holdFetched stages the layers id's moved placement did not place for the
// next plan (a try's or a full round's install), which claims each it still
// wants over the same base (claimStagedLayer) rather than downloading it
// again: a root layer may be some 2.6 GB, and a busy repository's compactions
// move its copy every few tens of seconds. Where the same layer over the same
// base is staged already (an adoption that came meanwhile), that one stays and
// the fetch goes; any other staging under its id is older and goes.
func (c *Cache) holdFetched(id string, fetched []*stagedLayer) {
	in := c.installer
	var gone []*stagedLayer
	in.mu.Lock()
	for _, st := range fetched {
		if in.staged[id] == nil {
			in.staged[id] = map[string]*stagedLayer{}
		}
		lid := st.row.Layer.LayerID
		if old := in.staged[id][lid]; old != nil {
			if sameLayer(old.row, st.row) && slices.Equal(old.base, st.base) {
				gone = append(gone, st)
				continue
			}
			gone = append(gone, old)
		}
		in.staged[id][lid] = st
	}
	in.mu.Unlock()
	for _, st := range gone {
		st.remove(c)
	}
}

// dropFetched removes every installer fetch staged for id (holdFetched) that
// the plan made now, want over bases, does not install. Adoptions stay: a
// copy's chain may not have read their rows yet, and the janitor discards one
// no round named in time (expireStagedLayers).
func (c *Cache) dropFetched(id string, want []gitlet.LayerRow, bases [][]string) {
	wanted := func(st *stagedLayer) bool {
		for i, row := range want {
			if sameLayer(row, st.row) && slices.Equal(bases[i], st.base) {
				return true
			}
		}
		return false
	}
	in := c.installer
	var gone []*stagedLayer
	in.mu.Lock()
	for lid, st := range in.staged[id] {
		if st.adopted || wanted(st) {
			continue
		}
		gone = append(gone, st)
		delete(in.staged[id], lid)
	}
	if len(in.staged[id]) == 0 {
		delete(in.staged, id)
	}
	in.mu.Unlock()
	for _, st := range gone {
		st.remove(c)
	}
}

// expireStagedLayers, run by the janitor every pass, discards every staged
// layer not placed within objstore.RecordWithin of its staging: an adoption
// no round named by then is of a record that never applied (its parts the
// collector's), and a held fetch that old has outlived every plan.
func (c *Cache) expireStagedLayers() {
	in, now := c.installer, c.now()
	in.mu.Lock()
	var gone []*stagedLayer
	for id, layers := range in.staged {
		for lid, st := range layers {
			if now.Sub(st.at) >= objstore.RecordWithin {
				gone = append(gone, st)
				delete(layers, lid)
			}
		}
		if len(layers) == 0 {
			delete(in.staged, id)
		}
	}
	in.mu.Unlock()
	for _, st := range gone {
		st.remove(c)
	}
}
