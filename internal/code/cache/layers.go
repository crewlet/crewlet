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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE INSTALLED CHAIN. A repository's MAINTAINER — one git node — writes its
// multi-pack-index as a CHAIN OF PUBLISHED LAYERS, each git's own incremental
// layer with its bitmap and the commit-graph layer over the same packs,
// stores each layer's bytes in the object store and names it on the code log
// (code.LayerRef, ADR-0051's Derived standing); every git node INSTALLS that
// chain: each layer fetched, held to its row (code.CheckLayer), and placed
// byte for byte as the maintainer's git wrote it, both chain files written by
// this cache from the rows' order. No node writes a multi-pack-index layer of
// its own: on the node pin a bitmapped layer costs the walk of the WHOLE
// history however few objects it adds (1.52–1.89 s and 127–132 MiB over a
// 220 000-object history against 1.43 s and 168 MiB for the whole index), and
// a layer without its bitmap turns off every bitmap beneath it — so a chain of
// node-local tips would be the whole rewrite paid more often. So the packs
// that arrived since the last publication stay OUTSIDE the chain, the TAIL,
// which git reads beside it (0.03–0.35 s of walk at 1 to 16 packs outside
// it, nothing a clone measures). Their commits are the one thing the node
// writes over the chain: its own TIP commit-graph layer (maintain.go), git's
// split write of just the commits the installed layers lack, over exactly
// them, so a walk into the tail goes by generation too — a commit-graph
// layer, unlike a multi-pack-index layer, costs its new commits alone and
// turns nothing beneath it off.
//
// TWO MODES, PER INCARNATION, NEVER BOTH. An incarnation holding a non-empty
// installed chain is in CHAIN mode; one holding none is in OWN mode — the
// maintainer's whole index and graph of derived.go's DerivedMidx and
// DerivedGraph. git refuses the mix every way: it reads a whole
// multi-pack-index before a chain, a whole write deletes every layer and the
// chain file (midx-write.c's clear_midx_files), it loads a whole commit-graph
// before the chain, and a whole graph write unlinks the chain and expires its
// layers. So a chain placed drops own mode's files first (EventLayersMoved),
// the maintainer plans no own-mode write while a chain is installed — only
// the tip over it (maintain.go) — a round writes no index of its own beside
// one (indexRound's indexed), and a chain cut to nothing leaves the copy in
// own mode, its index the maintainer's to write. The tip goes with every
// change to the chain beneath it and with every pack removed, and stays
// across a retirement that keeps the whole chain (tipGoes).
//
// A FULL ROUND INSTALLS THE CHAIN BEFORE IT ANSWERS: it keeps the longest
// prefix of the chain the copy holds whole and sound — every file there at its
// row's sizes, held to its Check (CheckLayers) by the round's check, which a
// previous process's files meet at the copy's first round — and fetches the
// rest, every part of every layer queued in the round's ONE download queue
// behind its packs (install.go), so a cold node's first answer serves with a
// bitmap at the cost of the layers' bytes overlapped with the packs', never a
// first byte a layer in series. A DELTA NEVER DOWNLOADS A LAYER: a root layer
// of the largest repository the defaults admit is some 2.6 GB, and inside a
// delta it would hold every fetch waiting on that round on every git node; it
// hands the new rows to the INSTALLER, one goroutine a cache started by
// Start, and answers at once. Correctness never waits on a layer: without one
// git reads every pack's own index.
//
// THE CHAIN HOLDS ITS PACKS. A publication's repack retires packs its own new
// layer replaces, so between the delta that brings it and the installer's
// placement every pack an INSTALLED layer names stays in the copy, whether or
// not a live row still names it — a round never takes one out (apply's live
// set, packStrays, Verify's strays) — and leaves with the layer that held it.
// Dropped at the delta, they would cut the chain, and the bitmap with it,
// until the replacement was fetched. The longest such a pack stays is the
// installer's retry ceiling, installRetryCap, past which the stale layer is
// cut.
//
// A PLACEMENT IS A SWAP on an incarnation a reader may hold (swap.go's layer
// step: the kept layers linked, the fetched renamed in, the chain files
// written, every live pack and every pack the new chain names linked), and in
// place on one nobody holds (a boot's first round, a rebuild's product); and
// before the chain is visible a CANARY reads through its bitmap. A layer file
// does not name its base — the chain's order does — so a layer placed over
// the wrong base passes `multi-pack-index verify` and fails the first read
// that uses the bitmap ("invalid MIDX object position, MIDX is likely
// corrupt"), which CheckLayer, reading each file alone, cannot see; the
// canary is one bitmapped walk, 0.004 s with no tail and 0.35 s with sixteen
// packs outside the chain. Its verdict is git's alone: a canary that read
// nothing of the chain — the read lane full past SlotWait, its deadline —
// refuses nothing, and what it was to judge is tried again.
//
// A LAYER MISSING, CORRUPT OR FROM A NEWER GIT IS REFUSED, never an error: the
// copy serves from the prefix below it, or in own mode, and the layer and every
// one above it wait for the maintainer's next publication, which replaces
// what its own cache refused (code_layer_refused, crewlet.code.cache.layers).
// A missing part is asked about once at the log's head first — a publication
// may have replaced the layer since the rows were read, and the store
// collected its parts — and a refusal is kept in memory while the rows name
// that very record, so a restart tries once more.

// layersFile is this file as the declarations name their writer
// (Writer.File): the writers gate holds the install writes of DerivedLayers
// and DerivedGraphLayers to this file's allowance row.
const layersFile = "internal/code/cache/layers.go"

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
	// layerCorrupt: a part whose bytes are not its row's, files code.CheckLayer
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

// layerRefusal is one layer of the rows' chain this node will not try to
// install again while the rows name it at the record that added it: what
// refused it, and that record's position — a publication of the same bytes
// again, under a new record and new parts (the maintainer's repair of a layer
// the store lost), is another try.
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
func layerRels(l code.LayerRef) [3]string {
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
func chainIDs(rows []code.LayerRow) (midx, graph []string) {
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
// multi-pack-index layer — named by its own checksum, so the same bytes — and
// the same commit-graph layer over the same base. Another record publishing
// the same bytes again (new parts, a new position) is the same layer: a copy
// holding it holds what the rows name.
func sameLayer(a, b code.LayerRow) bool {
	return a.Layer.LayerID == b.Layer.LayerID && a.Layer.GraphID == b.Layer.GraphID && a.Layer.BaseID == b.Layer.BaseID
}

// sameRow reports whether a and b are one row of the rows' chain: one layer of
// one chain (sameLayer) named by the record that added it. An installed
// layer's row is the one it was installed from.
func sameRow(a, b code.LayerRow) bool {
	return sameLayer(a, b) && a.Added == b.Added
}

// sameRows reports whether a and b are one chain, row for row (sameRow).
func sameRows(a, b []code.LayerRow) bool {
	return slices.EqualFunc(a, b, sameRow)
}

// layerPacks is every pack checksum rows' layers name.
func layerPacks(rows []code.LayerRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		for _, cs := range r.Layer.Packs {
			out[cs] = true
		}
	}
	return out
}

// prefixOf is how many layers of installed a change ev keeps, as d details
// it — THE ONE CUT, which both chains, the swap and every placement read, so
// the multi-pack-index chain and the commit-graph chain are never cut at two
// places: the longest prefix whose every layer names none of d's packs, has no
// file d names failing, and — for EventLayersMoved — is, layer for layer, the
// chain d wants. A layer is valid only over its exact base, so nothing above
// the first layer cut is kept.
func prefixOf(installed []code.LayerRow, ev Event, d eventDetail) int {
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

// recordInstalled makes rows what inc holds of the chain — its in-memory
// record and the declarations' records the state persists — keeping the
// cache's total in step through setDerived; e.mu is held. THE ONE WRITE OF
// AN INCARNATION'S INSTALLED CHAIN: each declaration's record lists the ids
// its chain file lists (derivedState.Layers) and counts its files' bytes from
// the rows — every layer's sections and the chain file — so a boot restores
// both halves of what the copy holds without measuring a file, and a
// declaration with no layer has no entry. held is the packs the copy holds,
// by checksum, at the figures their rows count — every pack rows names is
// among them, since nothing is installed over a pack the copy lacks
// (installable) — and the packs the chain then holds keep those figures
// whatever becomes of their rows (incarnation.installedPacks).
func (c *Cache) recordInstalled(inc *incarnation, rows []code.LayerRow, held map[string]livePack) {
	was := inc.installedPacks
	inc.installed = slices.Clone(rows)
	inc.installedPacks = map[string]livePack{}
	for cs := range layerPacks(rows) {
		inc.installedPacks[cs] = held[cs]
	}
	if !maps.EqualFunc(was, inc.installedPacks, func(livePack, livePack) bool { return true }) {
		// The packs the chain names moved, and the tail is every live pack
		// it does not name.
		recountTail(inc)
	}
	c.recordChains(inc)
}

// recordChains makes the declarations' records of inc's chain what it holds
// — its installed rows and its tip — keeping the cache's total in step through
// setDerived; e.mu is held. Each record lists the ids its chain file lists
// (derivedState.Layers, and the graph's Tip) and counts its files' bytes —
// every installed layer's sections from its row, the tip's file as the write
// that made it found it, and the chain file — so a boot restores both halves
// of what the copy holds without measuring a file, and a declaration with no
// file has no entry. The graph's record is made by the install where a layer
// carries a graph, and by the tip's write where only the tip does
// (StepTipGraph): its Step is the write of its chain file's first line. It
// also carries the digest of the tail the last write covered (TipOver), so
// the next boot knows the tip it keeps needs no write while the tail is the
// same.
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
func (c *Cache) setInstalled(e *entry, inc *incarnation, rows []code.LayerRow, held map[string]livePack) {
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

// packFigures is the packs snap places in objects/pack, by checksum, at the
// figures their rows count: what a full round's copy holds there once its
// install has run — what a layer may name, and what an index covers, neither
// of which ever reaches the dropping view.
func (s snapshot) packFigures() map[string]livePack {
	places := s.places()
	out := make(map[string]livePack, len(s.packs))
	for _, p := range s.packs {
		if places[p.GitChecksum] == placeLive {
			out[p.GitChecksum] = liveOf(p.Ref(), code.PackLive)
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

// dropTip takes inc's tip off what it records: none held, and none written
// over any tail, so the maintainer writes one at once (GRAPH FIRST) over what
// is left of the tail — its file and its chain line the caller's to take out,
// its declaration's record the caller's to write again (recordChains). e.mu is
// held.
func dropTip(inc *incarnation) {
	inc.tip, inc.tipOver, inc.tipWritten = tipLayer{}, tailDigest{}, false
}

// tipGoes reports whether a change ev the installed chain answers Prefix,
// keeping k of installed's layers, takes the node's tip with it — every one
// but a retirement that keeps the whole chain beneath it: a layer cut is a
// base gone, the chain moving is a base the tip was not written over, a pack
// removed may take commits the tip names, an expunge's commit takes the
// commits it expunged out of the repository, and a file found failing is cut
// with the tip above it, where a retirement merges packs and drops no commit,
// and the tip names commits by id (X04).
func tipGoes(installed []code.LayerRow, k int, ev Event) bool {
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
// in-place write of them was in flight at m, and none has begun or ended
// since (incarnation.chainSeq). A chain file's verdict reached between the two
// is the file's; otherwise it is no verdict, the expectation and the file
// perhaps of two different moments. e.mu is held.
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

// bootInstalled is the chain inc holds as its record names it, read against
// the rows' chain: the longest prefix of chain whose every layer is, in turn,
// the next the multi-pack-index record lists and — where it carries one — the
// next the commit-graph record lists. What a boot's copy holds is known by
// its ids alone until the rows name them; a layer the rows no longer name is
// held by nobody's account, and its files go with the round's placement
// (cutChain). e.mu is held.
func bootInstalled(inc *incarnation, chain []code.LayerRow) []code.LayerRow {
	midx, graph := inc.derived[DerivedLayers].Layers, inc.derived[DerivedGraphLayers].Layers
	var out []code.LayerRow
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

// setChain records chain as the rows' published chain of inc, as a snapshot
// served it (code.RepoSnapshot.LayersServed), and drops every refusal of a
// layer the rows no longer name at the record that added it; e.mu is held.
// The generation moves only where chain is not the chain inc read before:
// every full round serves the chain, and one serving the very chain an
// installer's plan was made over is no move of it — counted as one, it threw
// away whatever the plan had fetched.
func setChain(inc *incarnation, chain []code.LayerRow) {
	if !inc.chainRead || !sameRows(inc.chain, chain) {
		inc.chainGen++
	}
	inc.chain, inc.chainRead = slices.Clone(chain), true
	for id, r := range inc.refused {
		i := slices.IndexFunc(chain, func(row code.LayerRow) bool { return row.Layer.LayerID == id })
		if i < 0 || chain[i].Added != r.added {
			delete(inc.refused, id)
		}
	}
}

// refuse records that this node will not install row while the rows name it
// at its record — logged code_layer_refused once an incarnation a layer —
// and counts it; e.mu is held.
func (c *Cache) refuse(e *entry, inc *incarnation, row code.LayerRow, outcome layerOutcome, detail string) {
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

// chainPlan is what a round or the installer decided of one incarnation's
// chain before a byte moves: what it holds (installed), the longest prefix of
// the rows' chain it holds whole and sound (kept), and the layers above it to
// fetch, in chain order (want) — stopping at the first that is refused, from
// a newer git, or over a pack the copy does not hold, since nothing is
// installed over a base it lacks.
type chainPlan struct {
	installed, kept, want []code.LayerRow
	// failing is the layers with a file a check or a scrub found failing that
	// is still the file found so, by id: cut, and fetched again.
	failing map[string]bool
	// rewrite: a chain file is not what the incarnation's record says — it
	// failed its check, or is absent where layers are installed, or present
	// where none are — so the chain files are written again even where
	// nothing else of the chain moves. dropTip: the node's tip the copy holds
	// is no file to serve — failing, gone, or not the one checked — and goes,
	// the chain files written again without it, even where nothing else
	// moves.
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
func (p chainPlan) rows(fetched []*stagedLayer) []code.LayerRow {
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
func (p chainPlan) moves(rows []code.LayerRow, fetched []*stagedLayer) bool {
	return p.rewrite || p.dropTip || len(fetched) > 0 || !sameChain(rows, p.installed)
}

// placeChain is a FULL round's placement of the chain it planned (planChain),
// once its install has fetched what it could (layers, in chain order): the
// kept prefix and every layer fetched up to the first that was not — a layer
// is valid only over its exact base, so whatever was fetched above a failure
// goes from tmp/ — and nothing where that is what the copy holds already.
//
//   - Chain files alone not the record's, or a tip alone that goes, are
//     written again IN PLACE, even on a copy a reader may hold — one rename
//     each — and anything of the two directories no layer kept names goes,
//     the tip's file with it (cutChain).
//   - Any other change to a copy a reader may hold is answered as a SWAP's
//     layer step (swap.go): the step returned, which the round's swap takes.
//   - On one nobody holds — a boot's first round, a rebuild's product — it is
//     placed IN PLACE (placeInPlace), the canary reading it first.
//
// A layer the round could not fetch is the installer's: bytes not its row's
// are refused at once (corrupt); a part the store does not hold is asked of
// the log's head by the installer's own try (refuseFailed), since the round
// holds the repository a read at the head would queue behind; anything else
// is the installer's to try again behind its wait. It answers the swap's
// step, or whether it wrote into the copy here. e.mu is not held; the round's
// write lock is.
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

// roundLayerFailed is a full round's layer it could not fetch, as its install
// answered it (placeChain): refused at once where its bytes are not its row's,
// and otherwise the installer's — which the round's end kicks, since the copy
// then wants it (installDue) — where a part the store does not hold is asked
// of the log's head, and anything else tried again behind its wait, counted
// failed as the installer's tries are. Logged code_layer_install_failed either
// way.
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
// the incarnation's own (setChain): kept is the longest prefix of that chain
// the incarnation holds, its every file present at its row's size, held to
// its Check by the round's check — which vouches every file it finds sound,
// and hashes every one this process has not vouched — and named failing by no
// finding still at the identity it failed at (found: the round's check's and
// every scrub's waiting on the repair mark, which a scrub makes of a file this
// process had vouched); want is installable above it. On a boot's copy what
// it holds is its record's, read against the rows' chain (bootInstalled), and
// made the incarnation's, so the placement cuts what the record held past it.
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
	// What the files are now, read before the lock: a finding still at the
	// identity it failed at, and every file of the rows' chain.
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
	// The node's tip the copy holds — a boot's copy's as its record names it —
	// and its file as it is now.
	e.mu.Lock()
	tipID := recordedTip(inc)
	e.mu.Unlock()
	var tipFile fileID
	if tipID != "" {
		tipFile = lstatID(filepath.Join(inc.dir, tipRel(tipID)))
	}
	holds := func(r code.LayerRow) bool {
		return !slices.ContainsFunc(r.Layer.Packs, func(cs string) bool { _, ok := p.held[cs]; return !ok })
	}
	e.mu.Lock()
	first := !inc.chainRead
	setChain(inc, snap.layers)
	if first && len(inc.installed) == 0 {
		// A BOOT'S COPY holds what its record names, as far as the rows'
		// chain names it in turn and the copy holds every pack each layer
		// names: a layer is valid only over its exact base and its packs.
		boot := bootInstalled(inc, inc.chain)
		if i := slices.IndexFunc(boot, func(r code.LayerRow) bool { return !holds(r) }); i >= 0 {
			boot = boot[:i]
		}
		// And the node's tip its record names, where the copy keeps every
		// layer the tip was written over and holds its file: a tip is valid
		// only over its exact base. Held to its check by the round's check
		// like every file a previous process left, and dropped below where it
		// failed. With it, the tail the last write covered (TipOver), so a
		// tail the restart left as it was owes the tip nothing.
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
	// THE NODE'S TIP stays only as the file this process wrote or its round's
	// check found sound, still at that identity and named failing by no
	// finding; any other is dropped, with the chain files written again over
	// what the copy keeps, and the maintainer writes the tip again at once.
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
	if i := slices.IndexFunc(inc.chain[:k], func(r code.LayerRow) bool {
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
// as files this process wrote or checked at the identity they have now (ids,
// read by the caller before it took e.mu) — what the round's check leaves
// vouched of a sound layer. e.mu is held.
func layerSound(inc *incarnation, row code.LayerRow, ids map[string]fileID) bool {
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

// installable is the layers of above — the rows' chain past what inc keeps —
// this node may fetch, in chain order: up to the first refused while the rows
// name it at the same record, written by a git newer than this node's (refused
// newer_git, before a byte moves), or naming a pack holds says the copy will
// not hold. Nothing is installed over a base the copy lacks. e.mu is held.
func (c *Cache) installable(e *entry, inc *incarnation, above []code.LayerRow, holds func(cs string) bool) []code.LayerRow {
	var out []code.LayerRow
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

// stagedLayer is one layer's files in tmp/ — fetched from the object store
// and held to their row, or linked from the maintainer's own (AdoptLayer) —
// until a placement renames them in: tmp/layer-<id>-<v7>/{midx,bitmap,graph}.
// Whatever of it no placement takes is removed, and counted out of tmp/'s
// bytes.
type stagedLayer struct {
	row code.LayerRow
	dir string
	// paths is the three files, in the order of code.LayerRef.Sections; the
	// graph's "" where the layer carries none.
	paths   [3]string
	adopted bool
	// base is the commit-graph layers the layer was checked over, root first.
	base []string
	// at is when it was staged, on Options.Now's clock: an adoption no
	// round's chain names within objstore.RecordWithin is discarded. began is
	// the same moment as a time.Now() reading: how long its install took, to
	// its placement, is measured from it (crewlet.code.cache.layer.install).
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
func (c *Cache) stageLayer(row code.LayerRow) (*stagedLayer, [3]*os.File, error) {
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

// fetchParts reads every part of st's layer from the object store into the
// files it cuts (code.FetchLayerPart: each part through the store's verified
// read, laid back where StoreLayer took it from) — ALL AT ONCE, one goroutine
// a part, each taking its GET's slot from slot and giving it back the moment
// its read ends — and answers the first failure, which ends the rest.
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
			if err = code.FetchLayerPart(ctx, c.objects, st.row.Layer, i, writers, buf); err != nil {
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

// finishLayer makes st's files durable — every sync in flight at once, as a
// pack's three are (syncStaged) — closes them, and holds them to the row
// over the commit-graph layers base names below it (code.CheckLayer): the
// one rule the maintainer's adoption and every install share, hashed a
// piece at a time in a slot of the read lane hold takes — a round's, as its
// check of the files a copy keeps is (gitproc.Pool.HoldRead), or the
// installer's, in its one share of the lane (installHold) — and stopped with
// ctx. Then the layer is fetched and checked and nothing is placed: the fault
// point InstallAfterFetch.
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
		if err := c.faults.Hit(ctx, codefault.InstallAfterFetch, 0); err != nil {
			return fmt.Errorf("cache: layer %s: %w", st.row.Layer.LayerID, err)
		}
	}
	return nil
}

// checkStaged holds st's files to its row over base (code.CheckLayer), each
// trailer hashed as p says and stopped with ctx: a refused hold and an ended
// ctx are answered as they came, never as code.ErrBadLayer.
func checkStaged(ctx context.Context, st *stagedLayer, base []string, p code.Pieces) error {
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
	if err := code.CheckLayer(ctx, st.row.Layer, readers[0], readers[1], readers[2], base, p); err != nil {
		return fmt.Errorf("cache: layer %s: %w", st.row.Layer.LayerID, err)
	}
	return nil
}

// baseOf is the commit-graph layers below chain[i], root first: what layer
// i's commit-graph layer names as its BASE (code.CheckLayer).
func baseOf(chain []code.LayerRow, i int) []string {
	_, graph := chainIDs(chain[:i])
	return graph
}

// layerFailure is what a layer's fetch failing says of the layer: missing, a
// part the store holds no object for; corrupt, bytes that are not its row's
// or files CheckLayer refuses — both definite — and failed for anything else,
// the store failing to answer or this node's disk, tried again.
func layerFailure(err error) layerOutcome {
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return layerMissing
	case errors.Is(err, objstore.ErrCorrupt), errors.Is(err, code.ErrBadLayer):
		return layerCorrupt
	}
	return layerFailed
}

// layerFetch is one layer a round or the installer fetched: staged and
// checked, or the failure that stopped the chain at it.
type layerFetch struct {
	row     code.LayerRow
	staged  *stagedLayer
	err     error
	outcome layerOutcome
	// took is how long the fetch ran before it failed: what
	// crewlet.code.cache.layer.install observes of a layer no placement took.
	took time.Duration
}

// placeInPlace makes rows the chain inc holds, IN PLACE: on an incarnation
// nobody holds — a boot's first round, a rebuild's product — and only there,
// since a pack taken out under a reader would be met half way. In order: own
// mode's files go and the chain is cut below the first layer of what it holds
// that rows does not keep as it is — one fetched again included, whose files
// the fetch replaces (dropFor, EventLayersMoved: before anything of the new
// chain is visible); every fetched layer's files are renamed in; both chain
// files are written over rows; and, where the placement added a layer the copy
// did not hold, the CANARY reads through the chain's bitmap — its verdict that
// the chain is unsound refuses every layer it added as corrupt, a canary that
// read nothing of it leaves them to the installer, and either puts the chain
// back to the layers below the first of them, before any reader is handed the
// incarnation. It answers the chain placed. held is the packs the copy holds,
// by checksum, with their rows' figures (recordInstalled). e.mu is not held;
// the round's write lock is.
func (c *Cache) placeInPlace(ctx context.Context, e *entry, inc *incarnation, rows []code.LayerRow,
	fetched []*stagedLayer, held map[string]livePack, tail int64, tips []string) ([]code.LayerRow, error) {

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
			// A layer above its base is valid only over that base, so every
			// layer from the first added up goes with it: refused as corrupt
			// where the canary found the chain unsound, and otherwise — a
			// canary that read nothing of it — the installer's to try again,
			// which the round's end kicks (installDue) while the round
			// answers from the layers below.
			first := slices.IndexFunc(rows, func(r code.LayerRow) bool { return sameLayer(r, added[0]) })
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
				if slices.ContainsFunc(added, func(r code.LayerRow) bool { return sameLayer(r, st.row) }) {
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

// placed records rows as the chain inc holds once a placement made it so in
// place: every file of rows vouched at the identity it has now — written by
// this process from files held to their rows, or checked by its round — and
// the chain files with them, the incarnation's record set (setInstalled),
// and every staged layer of fetched gone from tmp/. e.mu is not held.
func (c *Cache) placed(e *entry, inc *incarnation, rows []code.LayerRow, fetched []*stagedLayer,
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
		if slices.ContainsFunc(rows, func(r code.LayerRow) bool { return sameLayer(r, st.row) }) {
			in = append(in, st)
		}
		st.remove(c)
	}
	c.installedLayers(e.id, in)
}

// moveLayerIn renames st's files into the incarnation (or the staged one) at
// dir, under the names git opens them by, and answers the paths it placed,
// relative to dir. The directories are made where missing; each is synced
// before anything names what landed: by the chain files' write that follows
// in place, and with the rest of a swap's staging (syncStaged).
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
func (c *Cache) linkLayerIn(row code.LayerRow, from, to string) ([]string, error) {
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

// trashRels moves the files at rels, relative to dir, into trash/: what a
// placement that did not complete had renamed in. A file already gone is no
// failure, and one that will not move is logged — the next full round's
// placement makes the chain directories exactly the chain's again.
func (c *Cache) trashRels(dir string, rels []string) {
	for _, rel := range rels {
		path := filepath.Join(dir, rel)
		if err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path)); err != nil &&
			!errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", path, "error", err)
		}
	}
}

// writeChains writes both chain files of the incarnation (or the staged one)
// at dir over rows — the commit-graph's ending in the node's tip where tip
// names one (a swap carrying it, maintain.go) — each by writeChain. sync is
// writeChain's.
func (c *Cache) writeChains(dir string, rows []code.LayerRow, tip string, sync func(dir string) error) error {
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

// writeChain writes the chain file at rel of the incarnation (or the staged
// one) at dir listing ids, a line each — staged in tmp/, durable, renamed in
// — or, for none, removes it and its directory where that is left empty: an
// incarnation with no chain holds no file of one. sync makes the directory
// the chain file lands in durable, before the file is renamed in — so every
// layer file renamed into it before is durable before a chain file names it
// — and after: syncDir for an incarnation a reader may open now or once it is
// current, and unsynced for a swap's staging, which nothing reads before its
// rename and which syncs every directory it wrote in once, with the rest of
// it (syncStaged), and for the node's tip's chain written over the layers
// installed (maintain.go's writeTip), which names nothing new.
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
	// The layers' files renamed in before the chain file are made durable
	// by this sync of their directory too: the rule "what a file names is
	// durable before that file is renamed in" (durable.go).
	if err = sync(filepath.Dir(path)); err != nil {
		return removeStaged(staged, err)
	}
	if err = c.place(staged, path); err != nil {
		return err
	}
	return sync(filepath.Dir(path))
}

// cutChain cuts inc's installed chain to its first k layers IN PLACE — the
// chain files written over what is kept first, so no reader opening them
// from then on names what goes, then every file of the two chain directories
// that no kept layer owns moved into trash/ (what a layer above the cut held,
// the node's tip, and anything a crash or a previous process left there under
// a layer's name that no record accounts for) — and records what is kept.
// e.mu is not held; the caller holds the repository's write lock, and has
// stopped a maintenance run writing the chain (dropFor; a full round stops
// every run). dropFor's one cut, for every change a chain answers Prefix.
//
// THE NODE'S TIP GOES WITH EVERY CUT (tipGoes): a layer cut takes the base it
// stood on, a chain placed over the kept ones is a base it was not written
// over, a pack removed may take commits it names, and chain files written
// again are a repair, in which a node-written tip is the one file worth
// nothing to keep — the maintainer writes it again at once, over the tail
// alone.
//
// THE CUT IS MARKED IN FLIGHT (chainWrite) from before its chain files are
// written to after its record names what they list: a check, a scrub or
// Verify of a copy a reader holds reads that record and hashes the chain
// files beside the cut, holding no lock it takes, and a file of one moment
// held to a record of the other would be named damaged, its repair a round
// that writes again what was sound.
func (c *Cache) cutChain(e *entry, inc *incarnation, k int, why Event) error {
	e.mu.Lock()
	installed := slices.Clone(inc.installed)
	ended := chainWrite(inc)
	e.mu.Unlock()
	// What the chain files name from their write on is what the record names
	// however the cut ends: no tip, and no tail a tip covers. Ended under the
	// same lock as that record, so no reader takes the record's word without
	// its mark.
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

// canary is the read a placement makes through the chain's bitmap before any
// reader is handed it: one bitmapped walk of every object, in the read lane,
// under the read's bound plus a walk of the tail the chain leaves outside
// (gitproc.HistoryWalk of tail objects) — `rev-list --objects
// --use-bitmap-index --count`, from every ref of dir (--all) or, where tips is
// given, from those object ids (--stdin): a full round places its chain in
// place before it writes the refs it read, so its walk starts from the rows'
// tips, every object of which its install has just put in the copy. A layer
// file names no base, so one placed over the wrong base passes
// `multi-pack-index verify` and fails the first bitmapped read ("invalid MIDX
// object position, MIDX is likely corrupt"): what this reads, and what
// code.CheckLayer, reading each file alone, cannot.
//
// ITS VERDICT IS GIT'S, AND ONLY GIT'S: errChainUnsound where git read the
// chain and exited with a status that is not its answer (gitproc.ExitError)
// or named damage (gitproc.Result.Damage), and nil where it read it whole.
// Any other failure — no read slot within SlotWait while the requests the
// lane serves fill it, the deadline, the cache stopping — is no verdict: git
// never finished a read of the chain, and what stopped it says nothing of the
// layers. The placement then places none of what it added — a swap fails at
// this step as at a write its disk failed, and a placement in place puts the
// chain back below them — and they are the installer's to try again behind
// its wait. Taken for a verdict, they were refused corrupt: kept off the node
// until the maintainer's next publication or a restart, a busy node's layers
// counted as damage, and a copy whose root went that way left in own mode,
// its whole index the maintainer's to write on a node already short of cores.
//
// WHAT IT REPORTS IS THE PLACEMENT'S: it runs naming a reader of inc
// (gitproc.Cmd.Repairer) whose reports the observer holds rather than makes
// inc's scrub due, and which this consumes — the placement it judges is the
// answer to whatever it met. Run at dir, inc's own directory for a placement
// in place or a swap's staged one, which the observer routes to inc as the
// swap's (swapAt).
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
	// Each argument list a literal at its run, so the writers gate reads it.
	// THROUGH THE DROPPING VIEW where dir holds one: the walk reads what the
	// refs reach, as every reader the chain serves does, and a ref a push
	// revived onto a dropping object before its expunge's begin reaches one
	// (heldView) — read without it, the layer would be refused for an object
	// the copy holds.
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
func tailObjects(rows []code.LayerRow, live map[string]int64) int64 {
	in := layerPacks(rows)
	var n int64
	for cs, objects := range live {
		if !in[cs] {
			n += objects
		}
	}
	return n
}

// Layers is the published chain the incarnation s pins holds the rows'
// account of — the chain as of s.Position, root first, the layers this node
// may not have installed among them — copied under its entry's lock: what a
// repository's maintainer plans a publication over, beside the packs. nil for
// a Synced this cache did not hand out.
func (c *Cache) Layers(s Synced) []code.LayerRow {
	if c.handedOut(s, "a read of the published chain") != nil {
		return nil
	}
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	return slices.Clone(s.inc.chain)
}

// THE INSTALLER: one goroutine a cache, started by Start, that installs the
// layers a delta named and the copy does not hold — behind the serving, since
// no reader waits on a layer.
//
//   - IT HOLDS EACH REPOSITORY ONCE, in one of three places: DUE a try, in
//     the order it became so — kicked by a round whose chain moved or by an
//     adoption, or its wait ended — WAITING out a backoff, the earliest end
//     first, or RUNNING. A pass moves every wait that has ended among the due
//     and takes up the due while a task is free, so it costs what it moves
//     and takes up, never what the installer holds: a burst of publications
//     over thousands of repositories, or a store's outage, holds thousands,
//     and a pass runs at every kick — every task's end and every round of a
//     repository held, one waiting out its backoff included — under the lock
//     every round's kick and every full round's install waits on. A walk of
//     every repository held at every pass drained 10 000 in about Q²/32
//     steps, three million, and took ten thousand more in hand at every kick
//     of one waiting.
//   - IT FETCHES AT MOST FetchConcurrency PARTS AT ONCE ACROSS THE
//     REPOSITORIES it installs, taking the next from the queue whenever one
//     ends, through the node's fetch slots (NodeFetches): half the node's
//     fetches for background installs and the other half for the rounds a
//     reader waits on — the round's own share, FetchConcurrency — never one
//     repository at a time: most layers are ONE part (a layer over a tail of
//     a few tens of thousands of objects is some 37 bytes an object, a few
//     MiB), so a node catching up after a restore, or after a burst of
//     publications over thousands of repositories, would wait a first byte
//     (100–200 ms on S3) and a placement a repository in series — 10 000
//     one-part installs are 17 to 33 minutes that way, about a minute
//     sixteen wide.
//   - ITS WORK IN THE READ LANE IS ONE SLOT AT A TIME ACROSS THEM ALL FOR
//     EACH KIND (installReadSlots): the pieces of its layers' checks in one,
//     its placements' canaries in the other. Sixteen tasks keep sixteen GETs
//     in flight; they were never sixteen cores of hashing to take from the
//     requests that lane serves.
//   - ONE REPOSITORY IS INSTALLED BY ONE TASK AT A TIME, its layers fetched in
//     chain order and placed together, by a swap, under the repository's write
//     lock: a round queues behind it, and it behind a round.
//   - A FAILURE: a part the store does not hold is asked about once at the
//     log's head (a linearizable sync) and refused `missing` only while the
//     rows still name the layer; bytes that are not the row's are refused
//     `corrupt`; anything else — a miss whose read at the head failed among
//     it, which decided nothing — waits 1 minute, doubling to
//     installRetryCap, and the repository's next round that serves its chain
//     clears the wait.
//     A layer the rows no longer name — one a publication replaced — keeps
//     serving, its packs held, while its replacement cannot be placed, and is
//     cut once that has lasted installRetryCap.
type layerInstaller struct {
	// share is the parts in flight across every repository it installs, and
	// tasks the repositories it installs at once: both a round's share, since
	// a task fetches at least a part. checks and canaries are its shares of
	// the node's read lane, installReadSlots each, which every task's work
	// there takes in turn: its checks' pieces the first, its placements'
	// canaries the second.
	share    *semaphore.Weighted
	tasks    int
	checks   *semaphore.Weighted
	canaries *semaphore.Weighted
	kicks    chan struct{}

	mu sync.Mutex
	// repos is every repository the installer holds, each with its place and
	// its backoff: due is the DUE in order, waiting the WAITING by when each
	// one's wait ends, and running counts the RUNNING. staged is the layers
	// staged in tmp/ for a placement to take in place of a GET, by repository
	// and layer id: those the repositories' maintainer handed this node
	// (AdoptLayer), and those the installer fetched for a placement that
	// found the copy moved (holdFetched) — each over the base it was held to,
	// until a placement takes it, a plan of its repository no longer wants
	// the installer's own (dropFetched), or the janitor discards it
	// (expireStagedLayers).
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

// installWait is one repository's place in the installer's backoff: the
// failures in a row, when the next try may be made (Options.Now's clock), and
// every layer of its copy's installed chain the rows no longer named at its
// last try, each with since when it has stood in for a replacement that could
// not be placed.
//
// A STALE LAYER'S CLOCK IS ITS OWN, keyed by the row it was installed from
// (sameRow), never the repository's: an episode can end where the
// installer does not see it — a full round placing the replacement itself, a
// cut, the rows naming the layer again — and a clock kept for the repository
// outlived it, so the next publication's stale layer was cut at its first
// failed fetch, by an episode hours over: a root replacement then dropped the
// copy to own mode, bitmapless and its whole index the maintainer's to write.
// Keyed by the layer, a clock no layer of the installed chain still answers to
// is no clock, and each one runs from the try that first saw its layer stale.
type installWait struct {
	failures int
	retryAt  time.Time
	stale    []staleLayer
}

// staleLayer is one installed layer the rows no longer name — the row it was
// installed from — and since when the installer has seen it so.
type staleLayer struct {
	row   code.LayerRow
	since time.Time
}

// staleSince is when the oldest of stale has stood in since by w's clocks —
// each layer of stale w recorded at an earlier try, the rest now — and stale's
// clocks as w keeps them from now on: one for each layer of stale, and none
// for a layer no longer stale.
func (w *installWait) staleSince(stale []code.LayerRow, now time.Time) (time.Time, []staleLayer) {
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

// installRetryBase is the installer's first wait after a try that failed for
// a reason that may pass — the store failing to answer, this node's disk —
// doubled after each one in a row, to installRetryCap: A MINUTE, since a
// layer's absence costs a copy speed and never correctness, so nothing is
// gained by asking a failing store sooner, and a store back within a minute
// costs one wait.
const installRetryBase = time.Minute

// installRetryCap is the most the installer waits between tries of one
// repository, and how long a layer the rows no longer name may keep serving
// while its replacement cannot be placed: THIRTY MINUTES (a decision, not a
// measurement). A layer refused here is replaced by its maintainer's next
// publication, which reaches this node as new rows through a delta — a round,
// which ends the wait (kickInstall's fresh) — rather than through this retry,
// so the retry serves only a store that failed to answer: at most one GET a
// repository a node every half hour, about 5.6 a second at 10 000
// repositories; and a retired pack a stale layer holds stays at most this
// long past its retirement before the stale layer is cut.
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

// installReadSlots is how many slots of the node's read lane each kind of the
// installer's own work holds at once, across every repository it installs:
// the pieces of its layers' checks (CheckPieceBytes in a
// gitproc.Pool.HoldRead), and its placements' canaries (a git read) — ONE
// each, as the scrub holds one, the cache's other work behind the serving.
// The read lane is a core a slot for the work requests wait on — a seat's git
// read, a fetch's check of its wants (Missing), a round's check of the files
// a copy keeps — and the installer's sixteen tasks are there to keep sixteen
// GETs in flight, not to hash on sixteen cores: bounded by its tasks alone,
// its checks and canaries held up to sixteen slots — the whole lane of a node
// of sixteen cores or fewer — through a mass install (a burst of root
// publications, the republish after a restore, when every node installs every
// root), each request queued behind a piece of up to 0.54 s read cold. One
// slot hashes about a gigabyte a second (BenchmarkVerify's 0.95–1.08 ms a
// mebibyte), about what sixteen GETs bring at the S3 fake's 80 MB/s a
// connection (FetchConcurrency's measurement), so it paces a node's installs
// about as its downloads do.
//
// A SLOT FOR EACH KIND, never one for both: a canary runs inside its
// placement's swap, under the repository's write lock, which every round of
// the repository waits for — the sync a fetch after a push waits on among
// them. In one slot with the checks it waited behind up to fifteen other
// tasks' pieces, about a second warm and several read cold, with that lock
// held. In its own it waits only behind other placements' canaries, 0.004 s
// each over a chain with no tail.
//
// A JUDGMENT CALL, not a measurement: a node of many cores with a link far
// wider than sixteen connections bring would install faster with more, at the
// price of what its requests wait for.
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

// takeInstallRead takes the installer's share of the node's read lane for its
// own work about to run there — its canaries' share where canary says the
// work is a placement's canary, else its checks' (installReadSlots) — waiting
// in turn with every other task's work of the kind, and answers the call that
// gives it back, once however often it is called. It holds no slot of the
// lane itself: a piece's hold takes one after it (installHold), and a
// canary's git is admitted to one as it starts, so the installer never holds
// a slot while it waits for its share.
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
// in (code.Pieces.Hold): its checks' share of the read lane (takeInstallRead),
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

// kickInstall holds id for the installer and wakes it: due at once where it
// held none of it, as it was where it holds it — waiting out its backoff, or
// due — and once more as its task ends where one runs it. fresh says a round
// of the repository served its chain, which ends its wait (a new chain is a
// new try): a waiting one is due at once, and a running one's next try
// waits for nothing. O(log n) at most, never a walk of what the installer
// holds. Its lock is a leaf: called with an entry's mu held or not.
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

// installDue reports whether inc has work for the installer: a converged copy
// holding a layer the rows' chain no longer names there, or one whose next
// layer of the rows' chain above what it holds is not refused while the rows
// name it at that record. A copy whose chain stops at a refused layer has none
// until the rows move. e.mu is held.
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

// installPass moves every repository whose wait has ended among the due, then
// starts a task (installRepo, then settleInstall) for each of the due in
// order while fewer than its tasks run, and answers when the earliest wait
// still holding one back ends — zero when none does — the repositories it
// took up, and the tasks it started, which end as they finish. It takes in
// hand only what it moves and takes up. The tasks run on goroutines of their
// own, registered as the cache's work (enter), so Stop waits for them; the
// loop waits for none.
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
	// stop is the fetch that stopped the chain, nil where none did;
	// undecided the read at the log's head that failed where a missing part
	// could not be decided; swapFailed a placement whose swap failed at a
	// step — a write this node's disk failed, or a canary that read nothing;
	// and stale the layers of the installed chain the rows no longer name
	// still standing in, as installed.
	stop       *layerFetch
	undecided  error
	swapFailed bool
	stale      []code.LayerRow
}

// installRepo is one repository's install, in four steps:
//
//  1. THE PLAN, of the copy current now, pinned (no use, as the maintainer's
//     pin is none): the prefix of its chain the rows still name, kept, and
//     what is installable above it (installable).
//  2. EACH LAYER of it fetched in chain order, every part at once, holding
//     no lock — or taken from what is staged for it (claimStagedLayer: the
//     maintainer's adoption, or an earlier try's fetch), no GET — stopping
//     at the first that fails. A fetch an earlier try staged that the plan
//     does not install goes (dropFetched).
//  3. THE PLACEMENT, under the repository's write lock: the copy still
//     current and its chain the one planned for, else the downloads are
//     staged for the next plan (holdFetched) and the repository is due
//     again; then a SWAP (swap.go's layer step) to the kept prefix and what
//     was fetched — a layer the rows no longer name is cut by it — the
//     canary reading the staged chain.
//  4. THE FAILURE, if one stopped the fetches: a part the store does not hold
//     is asked about at the log's head once, refused `missing` while the rows
//     still name it there; bytes not its row's are refused `corrupt`; and
//     anything else — a miss the head could not be read about included — is
//     tried again after its wait. A layer the rows no longer name that has
//     stood in for its replacement for installRetryCap with nothing fetched
//     is cut.
//
// It answers how the try ended, for its task to settle. A copy gone, or with
// nothing due, ends the repository's wait with the try: whatever it waited
// out — failures in a row, a stale layer's clock — was an episode now over,
// ended where the installer does not see it (a full round placing the chain,
// a cut, an eviction), and a wait kept would be the next episode's from its
// first try.
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

	// THE PLACEMENT: what was fetched over what is kept, a stale layer cut
	// by it — or, with nothing fetched, the stale layers cut alone once the
	// oldest of them has stood in for its replacement for installRetryCap.
	cut := len(stale) > 0 && len(fetched) == 0 && c.stuckFor(id, stale, c.now()) >= installRetryCap
	swapFailed := false
	if len(fetched) > 0 || cut {
		rows := slices.Clone(kept)
		for _, st := range fetched {
			rows = append(rows, st.row)
		}
		switch c.placeBySwap(ctx, e, inc, gen, rows, fetched) {
		case placementMoved:
			// Planned over a copy or a chain no longer there: planned again,
			// what was fetched staged for that plan.
			c.holdFetched(id, fetched)
			fetched = nil
			return installEnd{moved: true}
		case placementMade:
			// Counted by the swap that placed them (swapped).
			fetched = nil
			stale = nil
		case placementRefused:
			// The canary refused what it added — every layer the installer
			// fetches is one its copy does not hold — which is definite: the
			// next plan stops below it.
		default:
			// A step of the swap failing — this node's disk failing a write,
			// or the canary reading nothing of the chain (no read slot within
			// SlotWait, its deadline), which is no verdict — logged
			// code_cache_swap_failed by it, and every layer it was to place
			// counted failed there: tried again behind the wait.
			swapFailed = true
		}
	}

	// WHAT IS LEFT: a definite failure decided, and the repository queued
	// again behind its wait — a failure that may pass (a miss the log's head
	// could not decide among them), a placement that did not, or a stale
	// layer still standing in for a replacement — or nothing.
	end := installEnd{stop: stop, swapFailed: swapFailed, stale: stale}
	if stop != nil && stop.outcome != layerFailed {
		end.undecided = c.refuseFailed(ctx, e, *stop)
	}
	return end
}

// stuckFor is how long the oldest of stale — the layers of id's installed
// chain the rows no longer name — has stood in for a replacement the
// installer could not place, by the clock each was given at the try that
// first saw it stale (installWait.staleSince): zero for a layer no try has.
func (c *Cache) stuckFor(id string, stale []code.LayerRow, now time.Time) time.Duration {
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

// settleInstall records how id's try ended (end), its task done: the
// repository let go — its wait forgotten — or held once more, among the due or
// behind a wait; held once more at once, whatever else, where a kick came
// while the task ran (again), its wait ended where that kick was fresh.
//
//   - Planned over a copy or a chain that moved (end.moved): due again, its
//     wait as it was.
//   - The copy gone or nothing due (end.forget), or done — nothing stopped
//     it, no swap failed, no stale layer left — let go.
//   - A try that failed for a reason that may pass waits installWindow of its
//     failures in a row: the store or this node's disk failing (the stop's
//     outcome failed), a swap that did not complete, or a part the store does
//     not hold while the read at the log's head that decides it failed
//     (end.undecided, that read's error) — nothing was decided, and taken up
//     again at once the repository would cost a GET, a staging and a failed
//     round at every pass for as long as the head cannot be read.
//   - A definite refusal is due again at once, the refused layer now blocking.
//   - The layers the rows no longer name still standing in for their
//     replacement (end.stale, as installed) wait until the oldest of them has
//     stood installRetryCap, when their cut is due — each by its own clock,
//     kept from the try that first saw it stale, and a clock for a layer no
//     longer stale dropped.
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

// fetchForInstall is one layer of the installer's: the maintainer's adoption
// of it when one is registered for this very layer, else fetched — every part
// at once, each a unit of the installer's share and one of the node's fetch
// slots — and held to its row over base.
func (c *Cache) fetchForInstall(ctx context.Context, id string, row code.LayerRow, base []string) layerFetch {
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
	err = c.fetchParts(ctx, st, files, func(ctx context.Context) ([]byte, func(), error) {
		if err := share.Acquire(ctx, 1); err != nil {
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

// refuseFailed decides a definite failure the installer met: a part the store
// does not hold is asked about once at the log's head — a linearizable sync,
// a round like any other — and refused `missing` on the copy current then
// only while its rows still name the layer at the same record (a publication
// that replaced it since leaves nothing to refuse); bytes that are not the
// row's are refused `corrupt` while the rows name it. It answers nil once it
// decided — a refusal, or none to make — and the head read's failure where it
// could not: a miss the head did not decide is neither refused nor cleared,
// and its try is one that may pass (settleInstall), asked again after its
// wait.
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
	if e.dead || inc == nil || !slices.ContainsFunc(inc.chain, func(r code.LayerRow) bool {
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

// placeBySwap places rows as the chain of inc by a swap into its next
// incarnation (swap.go's layer step), under the repository's write lock, and
// answers what became of it: made, the copy or its chain moved under the
// install since it planned at gen — which leaves the downloads unplaced — the
// canary refusing what it added, or the swap failing.
func (c *Cache) placeBySwap(ctx context.Context, e *entry, inc *incarnation, gen uint64, rows []code.LayerRow,
	fetched []*stagedLayer) chainPlacement {

	if err := c.acquireWrite(c.life, e); err != nil {
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
func sameChain(a, b []code.LayerRow) bool {
	return slices.EqualFunc(a, b, sameLayer)
}

// layerStep is a placement a swap makes (swap.go): the chain the next
// incarnation holds, root first — the layers it keeps of the one it leaves,
// linked, and those fetched, renamed in from tmp/ — and whether it is the
// installer's, whose canary runs in the installer's share of the read lane
// for canaries (takeInstallRead) rather than as a round's, which a reader
// waits on.
type layerStep struct {
	rows      []code.LayerRow
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
func (s *layerStep) adds(installed []code.LayerRow) []code.LayerRow {
	var out []code.LayerRow
	for _, r := range s.rows {
		if !slices.ContainsFunc(installed, func(i code.LayerRow) bool { return sameLayer(i, r) }) {
			out = append(out, r)
		}
	}
	return out
}

// claimStagedLayer takes the layer staged for row — registered for this
// repository, this very layer over this very base: the maintainer's adoption,
// or the installer's own fetch a moved placement held — for an install, a
// round's or the installer's, which places it in place of a GET; or nil.
func (c *Cache) claimStagedLayer(id string, row code.LayerRow, base []string) *stagedLayer {
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

// holdFetched stages the layers the installer fetched for id's copy, which a
// placement that found the copy or its chain moved since its plan did not
// place, for the plan that follows it: the next try's, or a full round's
// install, which claims each it still wants over the same base
// (claimStagedLayer) rather than downloading it again — a root layer of the
// largest repository the defaults admit is some 2.6 GB, and a busy
// repository's compactions swap its copy a few tens of seconds apart, so
// thrown away at every move it was downloaded again and again. Where the very
// layer over the very base is staged already (an adoption that came while it
// downloaded), that one is kept and the fetched copy goes; anything else
// staged under its id is the older staging, and goes for it, as an adoption
// replaces an older one.
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

// dropFetched takes out of tmp/ every layer the installer fetched for id's
// copy and staged for a later plan (holdFetched) that the plan made now —
// want, each layer over the base beside it — does not install: the rows
// moved past it, or the copy holds it already. The maintainer's adoptions
// stay: a copy's chain may not have read their rows yet, and the janitor
// discards one no round's chain named in time.
func (c *Cache) dropFetched(id string, want []code.LayerRow, bases [][]string) {
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

// expireStagedLayers discards every staged layer no placement has taken
// within objstore.RecordWithin of its staging: an adoption no round's chain
// named in that time is a function of a record that never applied — the
// publication refused, or its node gone before it — whose parts are the
// collector's, and a fetch a moved placement held that long has outlived
// every plan of its copy. The janitor's, at every pass.
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
