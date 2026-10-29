package codetest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE PUBLISHED LAYERS AS A CASE READS THEM (ADR-0061): every cache synced at
// the log's head holds an installed chain that is a PREFIX of the rows' chain,
// in the rows' order, every installed layer's files passing the check every
// git node holds a layer to (code.CheckLayer) over packs the copy holds — M2's
// rows invariant, extended to the layers, which every case's convergence check
// asks of every copy (cacheHolds).

// layerSettle is how long a convergence check waits for a copy's installer to
// place, or refuse, every layer the rows name past the copy's installed prefix:
// a delta hands a new layer to the installer and answers at once, and the
// installer fetches it behind the serving — seconds here, so a minute is room
// for a loaded machine.
const layerSettle = time.Minute

// The files of an installed chain, relative to a copy's directory.
var (
	layersDir     = filepath.Join("objects", "pack", "multi-pack-index.d")
	layersChain   = filepath.Join(layersDir, "multi-pack-index-chain")
	graphsDir     = filepath.Join("objects", "info", "commit-graphs")
	graphsChain   = filepath.Join(graphsDir, "commit-graph-chain")
	packFilesDir  = filepath.Join("objects", "pack")
	errNotSettled = errors.New("a layer the rows name is neither installed nor refused")
)

// settledCopy is n's copy of repoID synced linearizable once its installer has
// placed or refused every layer the rows name past its installed prefix — or,
// past layerSettle, as it stands, with errNotSettled — and its chain as the
// copy holds it. The caller releases the Synced.
func settledCopy(ctx context.Context, n Node, repoID string) (cache.Synced, cache.Chain, error) {
	deadline := time.Now().Add(layerSettle)
	for {
		s, err := n.Cache().Sync(ctx, repoID, Linearizable)
		if err != nil {
			return cache.Synced{}, cache.Chain{}, fmt.Errorf("sync: %w", err)
		}
		ch := n.Cache().Chain(s)
		if chainSettled(ch) {
			return s, ch, nil
		}
		if time.Now().After(deadline) {
			return s, ch, fmt.Errorf("%w: layer %s of a chain of %d, %d installed, a minute on", errNotSettled,
				ch.Rows[ch.Installed].Layer.LayerID, len(ch.Rows), ch.Installed)
		}
		s.Release()
		select {
		case <-ctx.Done():
			return cache.Synced{}, cache.Chain{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// chainSettled reports whether a copy holds every layer of its rows' chain, or
// refuses the first it does not: nothing left for its installer to do.
func chainSettled(ch cache.Chain) bool {
	if ch.Installed == len(ch.Rows) {
		return true
	}
	_, refused := ch.Refused[ch.Rows[ch.Installed].Layer.LayerID]
	return refused
}

// LayersHold reports what is wrong with the published chain the copy at dir
// holds installed, as ch says it holds it: its multi-pack-index-chain must list
// exactly the rows' installed prefix in the rows' order, its
// commit-graph-chain must begin with those layers' graph layers (the node's
// own tip may follow), and every installed layer's three files must pass
// code.CheckLayer over packs the copy holds byte for byte — a layer placed
// over a pack the copy lacks, or a chain file listing the layers in another
// order, is a copy git reads wrongly. nil when it all holds.
func LayersHold(ctx context.Context, dir string, ch cache.Chain) error {
	installed := ch.Rows[:ch.Installed]
	var errs []error
	listed, err := chainFile(filepath.Join(dir, layersChain))
	if err != nil {
		return err
	}
	if want := rowLayerIDs(installed); !slices.Equal(listed, want) {
		errs = append(errs, fmt.Errorf("its multi-pack-index-chain lists %v, and the layers it holds installed "+
			"are %v, root first", listed, want))
	}
	graphs, err := chainFile(filepath.Join(dir, graphsChain))
	if err != nil {
		return err
	}
	var base []string
	for i, row := range installed {
		if err := layerHolds(ctx, dir, row.Layer, base); err != nil {
			errs = append(errs, fmt.Errorf("installed layer %d, %s: %w", i, row.Layer.LayerID, err))
		}
		if row.Layer.GraphID != "" {
			base = append(base, row.Layer.GraphID)
		}
	}
	if len(graphs) < len(base) || !slices.Equal(graphs[:len(base)], base) {
		errs = append(errs, fmt.Errorf("its commit-graph-chain lists %v, and the graph layers it holds installed "+
			"are %v, root first", graphs, base))
	}
	return errors.Join(errs...)
}

// CopyHoldsItsLayers is the layer half of every case's convergence check over
// node n's copy of repoID ([LayersHold]), synced linearizable once its
// installer has placed or refused every layer the rows name.
func CopyHoldsItsLayers(ctx context.Context, n Node, repoID string) error {
	s, ch, err := settledCopy(ctx, n, repoID)
	if s.Dir == "" {
		return err
	}
	defer s.Release()
	return errors.Join(err, LayersHold(ctx, s.Dir, ch))
}

// layerHolds holds one installed layer's files to its row, over the base
// graph layers below it, and its packs to the copy's own.
func layerHolds(ctx context.Context, dir string, l code.LayerRef, baseGraphs []string) error {
	var errs []error
	for _, cs := range l.Packs {
		for _, ext := range []string{".pack", ".idx"} {
			if _, err := os.Stat(filepath.Join(dir, packFilesDir, "pack-"+cs+ext)); err != nil {
				errs = append(errs, fmt.Errorf("names pack-%s, and the copy holds no pack-%s%s: %w", cs, cs, ext,
					err))
			}
		}
	}
	open := func(rel string) (*os.File, error) {
		f, err := os.Open(filepath.Join(dir, rel))
		if err != nil {
			return nil, fmt.Errorf("its file %s: %w", rel, err)
		}
		return f, nil
	}
	midx, err := open(filepath.Join(layersDir, "multi-pack-index-"+l.LayerID+".midx"))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	defer midx.Close()
	bitmap, err := open(filepath.Join(layersDir, "multi-pack-index-"+l.LayerID+".bitmap"))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	defer bitmap.Close()
	var graph io.ReaderAt
	if l.GraphID != "" {
		g, err := open(filepath.Join(graphsDir, "graph-"+l.GraphID+".graph"))
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		defer g.Close()
		graph = g
	}
	if err := code.CheckLayer(ctx, l, midx, bitmap, graph, baseGraphs, code.Pieces{}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// chainFile is the ids a chain file lists, in its order; none where it is
// absent.
func chainFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []string
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		if line := strings.TrimSpace(lines.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, lines.Err()
}

// rowLayerIDs is the layer ids of rows, in their order.
func rowLayerIDs(rows []code.LayerRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Layer.LayerID
	}
	return out
}

// chainOn is RepoName's published chain as node n's copy holds it, synced
// linearizable once its installer has placed or refused every layer the rows
// name.
func chainOn(c *Case, n Node) cache.Chain {
	c.T.Helper()
	s, ch, err := settledCopy(c.Ctx, n, c.Rows().Repo.ID)
	if err != nil {
		c.T.Fatalf("%s's copy of %s: %v", n.ID(), RepoName, err)
	}
	defer s.Release()
	return ch
}

// publicationOf is RepoName as the publication due listing through node n
// names it — or, for a repository the listing does not name, holding no pack
// outside its chain (a REPAIR's), as its rows stand, which a maintainer's pass
// reads off its own cache.
func publicationOf(c *Case, n Node) code.PublicationCandidate {
	c.T.Helper()
	a, err := n.Read(c.Ctx, code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: Linearizable})
	if err != nil {
		c.T.Fatalf("the publication due listing through %s: %v", n.ID(), err)
	}
	for _, cand := range a.Publish {
		if cand.Name == RepoName {
			return cand
		}
	}
	rows := c.Rows()
	cand := code.PublicationCandidate{ID: rows.Repo.ID, Name: RepoName, LivePacks: int64(len(rows.Packs)),
		Version: rows.Position}
	for _, p := range rows.Packs {
		cand.LiveObjects += p.Objects
	}
	return cand
}

// maintainerOf is the git node that maintains RepoName, as each node's
// compactor asks: by the publication's charge.
func maintainerOf(c *Case) Node {
	c.T.Helper()
	for _, n := range c.Fleet.Nodes() {
		yes, err := maintains(n, publicationOf(c, n))
		if err != nil {
			c.T.Fatalf("whether %s maintains %s: %v", n.ID(), RepoName, err)
		}
		if yes {
			return n
		}
	}
	c.T.Fatalf("no git node maintains %s", RepoName)
	return nil
}

// maintains is whether n's compactor takes itself for cand's maintainer, asked
// as each of its passes asks: by the publication's charge, off one listing of
// the git nodes for every repository it judges.
func maintains(n Node, cand code.PublicationCandidate) (bool, error) {
	yes, err := n.Maintainers().MaintainerOf(map[string]int64{cand.ID: n.Cache().PublishCharge(cand.LiveObjects)})
	return yes[cand.ID], err
}

// publishAtMaintainer is one publication of RepoName by its maintainer — the
// unit a pass runs — asked again while it answers busy (a maintenance slot
// the maintainer holds a moment) or waiting (a layer of the chain its own
// installer has not placed yet), for a minute at the most; it answers the
// maintainer and the outcome.
func publishAtMaintainer(c *Case) (Node, compact.Outcome) {
	c.T.Helper()
	n := maintainerOf(c)
	return n, publishOn(c, n)
}

// tryPublishOn is node n's publication of cand asked again while it answers
// busy — the node's maintainer keeps the one maintenance slot a publication
// takes for a moment at a time — for a minute at the most: for a goroutine a
// fault point holds, so it reports through nothing but its answer.
func tryPublishOn(ctx context.Context, n Node, cand code.PublicationCandidate) (compact.Outcome, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := n.Compactor().Publish(ctx, cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// publishOn is node n's publication of RepoName, asked again while it answers
// busy or waiting, for a minute at the most.
func publishOn(c *Case, n Node) compact.Outcome {
	c.T.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := n.Compactor().Publish(c.Ctx, publicationOf(c, n))
		if (out != compact.OutcomeBusy && out != compact.OutcomeWaiting) || time.Now().After(deadline) {
			if err != nil && out != compact.OutcomeApplied {
				c.T.Logf("%s's publication of %s: %s, %v", n.ID(), RepoName, out, err)
			}
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustPublish is publishAtMaintainer, failing the case unless it applied.
func mustPublish(c *Case) Node {
	c.T.Helper()
	n, out := publishAtMaintainer(c)
	if out != compact.OutcomeApplied {
		c.T.Fatalf("%s's publication of %s: %s, want applied", n.ID(), RepoName, out)
	}
	return n
}

// chainRows is RepoName's published chain as its rows hold it, read through
// node n's copy at the log's head.
func chainRows(c *Case, n Node) []code.LayerRow { return chainOn(c, n).Rows }

// loseLayerPart deletes part i of the top layer of RepoName's chain from the
// object store — a store that lost what a publication stored — and answers the
// layer.
func loseLayerPart(c *Case, n Node, i int) code.LayerRef {
	c.T.Helper()
	rows := chainRows(c, n)
	if len(rows) == 0 {
		c.T.Fatalf("%s has no published layer to lose a part of", RepoName)
	}
	l := rows[len(rows)-1].Layer
	if err := c.Fleet.Objects().Delete(c.Ctx, l.Parts[i].Object.Name()); err != nil {
		c.T.Fatalf("delete part %d of layer %s: %v", i, l.LayerID, err)
	}
	return l
}

// spoilLayerPart flips one byte of part i of the top layer of RepoName's
// chain in the object store — a store that hands back other bytes than it
// took, which its hash check refuses — and answers the layer.
func spoilLayerPart(c *Case, n Node, i int) code.LayerRef {
	c.T.Helper()
	rows := chainRows(c, n)
	if len(rows) == 0 {
		c.T.Fatalf("%s has no published layer to spoil a part of", RepoName)
	}
	l := rows[len(rows)-1].Layer
	name := l.Parts[i].Object.Name()
	ctx, cancel := context.WithTimeout(c.Ctx, time.Minute)
	defer cancel()
	r, err := c.Fleet.Objects().Get(ctx, name, 0, -1)
	if err != nil {
		c.T.Fatalf("read part %d of layer %s: %v", i, l.LayerID, err)
	}
	raw, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		c.T.Fatalf("read part %d of layer %s: %v", i, l.LayerID, err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := c.Fleet.Objects().Delete(ctx, name); err != nil {
		c.T.Fatalf("replace part %d of layer %s: %v", i, l.LayerID, err)
	}
	if err := c.Fleet.Objects().Put(ctx, name, bytes.NewReader(raw), objstore.PutMeta{}); err != nil {
		c.T.Fatalf("replace part %d of layer %s: %v", i, l.LayerID, err)
	}
	return l
}

// freshNode is node n killed, its cache directory removed and restarted: a
// git node that joins the fleet with nothing, whose first round of each
// repository is a full one. A topology that kills and restarts its git nodes
// together (one process's, SoloFleet) restarts every other node too, so a
// handle to any node taken before it names a node that is gone: a case
// resolves such a handle again by its id (nodeByID).
func freshNode(c *Case, n Node) Node {
	c.T.Helper()
	id, dir := n.ID(), n.CacheDir()
	c.Fleet.Kill(c.T, id)
	if err := os.RemoveAll(dir); err != nil {
		c.T.Fatalf("remove %s: %v", dir, err)
	}
	c.Fleet.Restart(c.T, id)
	return nodeByID(c, id)
}

// nodeByID is the running git node called id, as the fleet runs it now.
func nodeByID(c *Case, id string) Node {
	c.T.Helper()
	for _, m := range c.Fleet.Nodes() {
		if m.ID() == id {
			return m
		}
	}
	c.T.Fatalf("no running git node %s", id)
	return nil
}
